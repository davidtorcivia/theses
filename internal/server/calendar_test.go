package server

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/davidtorcivia/theses/internal/auth"
)

func TestCalendarSubscription(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	prop, err := h.srv.board.CreateProposition(ctx, h.owner(), "Episode, one; \\ café\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.db.ExecContext(ctx, `UPDATE propositions SET target_date='2028-02-29' WHERE id=?`, prop.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.db.ExecContext(ctx, `INSERT INTO production_plans(proposition_id,record_date,edit_date) VALUES(?,'2028-02-27','2028-02-28')`, prop.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	subscriberID := h.owner().ID
	create := func() string {
		t.Helper()
		res, page := h.postBack("/profile/calendar", url.Values{"csrf": {h.csrf("/profile")}, "action": {"create"}})
		match := regexp.MustCompile(`/calendar/([a-f0-9]{64})/production.ics`).FindStringSubmatch(page)
		if res.StatusCode != 200 || match == nil {
			t.Fatalf("create: %d %s", res.StatusCode, page)
		}
		_, again := h.get("/profile")
		if strings.Contains(again, match[1]) {
			t.Fatal("token shown twice")
		}
		var stored []byte
		if err := h.db.QueryRowContext(ctx, `SELECT token_hash FROM calendar_subscriptions WHERE user_id=?`, subscriberID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(match[1]))
		if string(stored) != string(hash[:]) {
			t.Fatal("token not hashed")
		}
		return match[0]
	}
	read := func(path string, want int) string {
		t.Helper()
		res, err := http.Get(h.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("feed status %d want %d: %s", res.StatusCode, want, body)
		}
		if res.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatal("feed may be cached")
		}
		if len(res.Cookies()) != 0 {
			t.Fatal("feed sets a cookie")
		}
		if want == 200 && res.Header.Get("Content-Type") != "text/calendar; charset=utf-8" {
			t.Fatal("not calendar")
		}
		return strings.ReplaceAll(string(body), "\r\n ", "")
	}
	path := create()
	body := read(path, 200)
	if strings.Count(body, "BEGIN:VEVENT") != 3 || !strings.Contains(body, "DTSTART;VALUE=DATE:20280229\r\nDTEND;VALUE=DATE:20280301") || !strings.Contains(body, `SUMMARY:Release: Episode\, one\; \\ café\nsecond line`) {
		t.Fatalf("invalid feed %s", body)
	}
	if body != read(path, 200) {
		t.Fatal("unchanged feed unstable")
	}
	uid := regexp.MustCompile(`UID:([^\r]+)-Release@theses`).FindString(body)
	_, err = h.db.ExecContext(ctx, `UPDATE propositions SET target_date='2028-03-02' WHERE id=?`, prop.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	updated := read(path, 200)
	if !strings.Contains(updated, uid) || !strings.Contains(updated, "SEQUENCE:3") || !strings.Contains(updated, "DTSTART;VALUE=DATE:20280302") {
		t.Fatalf("date update: %s", updated)
	}
	if res, _ := h.post("/profile/calendar", url.Values{"action": {"revoke"}}); res.StatusCode != 403 {
		t.Fatal("mutation lacks CSRF protection")
	}
	replacement := create()
	read(path, 404)
	if strings.Contains(read(replacement, 200), uid) {
		t.Fatal("replacement reused subscription event identity")
	}
	_, _ = h.postBack("/profile/calendar", url.Values{"csrf": {h.csrf("/profile")}, "action": {"revoke"}})
	read(replacement, 404)
	read("/calendar/bad/production.ics", 404)
	// An editor's feed follows current membership and role, not creation-time access.
	member := h.as("calendarreader", "Calendar reader", auth.RoleEditor)
	var memberID int64
	if err := h.db.QueryRowContext(ctx, `SELECT id FROM users WHERE handle='calendarreader'`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	h.client = member
	subscriberID = memberID
	path = create()
	if strings.Contains(read(path, 200), "BEGIN:VEVENT") {
		t.Fatal("private episode leaked")
	}
	if _, err := h.srv.board.AddMember(ctx, h.owner(), prop.EntityID, memberID); err != nil {
		t.Fatal(err)
	}
	if strings.Count(read(path, 200), "BEGIN:VEVENT") != 3 {
		t.Fatal("member missing episode")
	}
	_, err = h.db.ExecContext(ctx, `DELETE FROM proposition_members WHERE proposition_id=? AND user_id=?`, prop.EntityID, memberID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(path, 200), "BEGIN:VEVENT") {
		t.Fatal("revoked membership leaked")
	}
	h.setRole(t, memberID, auth.RoleOwner)
	if strings.Count(read(path, 200), "BEGIN:VEVENT") != 3 {
		t.Fatal("owner missing episode")
	}
	h.setRole(t, memberID, auth.RoleGuest)
	if strings.Contains(read(path, 200), "BEGIN:VEVENT") {
		t.Fatal("demoted owner leaked episode")
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, memberID); err != nil {
		t.Fatal(err)
	}
	read(path, 404)
}

func TestCalendarEscapingAndFolding(t *testing.T) {
	if got := calendarText("Episode\x00bad\x01\x7f\t\n"); got != "Episodebad\t\\n" {
		t.Fatalf("control characters: %q", got)
	}
	original := "SUMMARY:" + calendarText(strings.Repeat("é,;\\\r\n", 40)+"\rEND:VEVENT")
	var out strings.Builder
	calendarLine(&out, original)
	for _, line := range strings.Split(out.String(), "\r\n") {
		if len(line) > 75 || !utf8.ValidString(line) {
			t.Fatalf("bad folded line: %q", line)
		}
	}
	if strings.TrimSuffix(strings.ReplaceAll(out.String(), "\r\n ", ""), "\r\n") != original {
		t.Fatal("fold changed content")
	}
	out.Reset()
	calendarLine(&out, "SUMMARY:"+strings.Repeat(string([]byte{0x80}), 200))
	if !utf8.ValidString(out.String()) || !strings.Contains(out.String(), "�") {
		t.Fatal("invalid UTF-8 not repaired")
	}
	for _, pattern := range []string{"", "GET /calendar/{token}/production.ics"} {
		r := httptest.NewRequest("GET", "/calendar/secret/production.ics", nil)
		r.Pattern = pattern
		if strings.Contains(logPath(r), "secret") {
			t.Fatal("secret in logs")
		}
	}
}

func TestCalendarRevisionTriggers(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	p, err := h.srv.board.CreateProposition(ctx, h.owner(), "Scheduled")
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		sql      string
		revision int
	}{
		{`UPDATE propositions SET title='Renamed' WHERE id=?`, 1},
		{`UPDATE propositions SET title='Renamed' WHERE id=?`, 1},
		{`INSERT INTO production_plans(proposition_id,record_date) VALUES(?,'2026-12-31')`, 2},
		{`UPDATE production_plans SET next_action='Write' WHERE proposition_id=?`, 2},
		{`UPDATE production_plans SET edit_date='2027-01-01' WHERE proposition_id=?`, 3},
		{`DELETE FROM production_plans WHERE proposition_id=?`, 4},
	}
	for _, step := range steps {
		if _, err := h.db.ExecContext(ctx, step.sql, p.EntityID); err != nil {
			t.Fatal(err)
		}
		var revision, updated int64
		if err := h.db.QueryRowContext(ctx, `SELECT calendar_revision,calendar_updated_at FROM propositions WHERE id=?`, p.EntityID).Scan(&revision, &updated); err != nil {
			t.Fatal(err)
		}
		if revision != int64(step.revision) || updated <= 0 {
			t.Fatalf("revision %d expected %d, timestamp %d", revision, step.revision, updated)
		}
	}
}
