package server

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// queued returns the one row in the outbox, or fails.
func (h *harness) queued() (to, subject, body string) {
	h.Helper()
	rows, err := h.db.QueryContext(context.Background(),
		`SELECT to_addr, subject, body_text FROM mail_outbox`)
	if err != nil {
		h.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&to, &subject, &body); err != nil {
			h.Fatal(err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		h.Fatal(err)
	}
	if n != 1 {
		h.Fatalf("%d rows in the outbox, want one", n)
	}
	return to, subject, body
}

func TestPasswordResetQueuesTheLink(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.signOut()

	res, body := h.post("/reset", url.Values{"csrf": {h.csrf("/reset")}, "who": {"dt"}})
	if res.StatusCode != 200 || !strings.Contains(body, "If that account exists, mail is on its way.") {
		t.Fatalf("reset gave %d without the neutral line", res.StatusCode)
	}

	to, subject, text := h.queued()
	if to != "dt@example.fm" {
		t.Errorf("to = %q", to)
	}
	if !strings.Contains(subject, "Reset your THESES password") {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(text, "http://localhost:8080/reset/") {
		t.Errorf("the queued message does not carry the link:\n%s", text)
	}
	if strings.Contains(h.log.String(), "/reset/") {
		t.Error("the reset link reached the log")
	}
}

func TestPasswordResetWithoutAnAddressQueuesNothing(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE users SET email = '' WHERE handle = 'dt'`); err != nil {
		t.Fatal(err)
	}
	h.signOut()

	h.post("/reset", url.Values{"csrf": {h.csrf("/reset")}, "who": {"dt"}})
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM mail_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows queued for an account with nowhere to send", n)
	}
}

func TestInvitationQueuesItsMailAndStillShowsTheLinkOnce(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()

	_, page := h.post("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.fm"}, "role": {"editor"},
	})
	m := inviteLinkRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the team panel did not show the link once:\n%s", page)
	}

	to, subject, text := h.queued()
	if to != "mara@example.fm" {
		t.Errorf("to = %q", to)
	}
	if !strings.Contains(subject, "David Torcivia invited you to THESES") {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(text, m[1]) {
		t.Errorf("the queued message carries a different link than the panel:\n%s", text)
	}
	if !strings.Contains(text, "editor") {
		t.Errorf("the queued message does not name the role:\n%s", text)
	}
}

func TestInvitationResendQueuesTheNewLink(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.post("/settings/team/invite", url.Values{
		"csrf": {h.csrf("/settings")}, "email": {"mara@example.fm"}, "role": {"editor"},
	})
	if _, err := h.db.ExecContext(context.Background(), `DELETE FROM mail_outbox`); err != nil {
		t.Fatal(err)
	}

	var id int64
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT id FROM invitations`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, page := h.post("/settings/team/invite/"+itoa(id)+"/resend", url.Values{"csrf": {h.csrf("/settings")}})
	m := inviteLinkRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("resend did not show the new link:\n%s", page)
	}
	to, _, text := h.queued()
	if to != "mara@example.fm" || !strings.Contains(text, m[1]) {
		t.Errorf("resend queued %q with:\n%s", to, text)
	}
}
