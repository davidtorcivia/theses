package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/legal"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/store"
)

// The workspace defaults from the settings registry, which cannot import legal.
const testSubject = "{title} is now available"
const testBody = "Hello {name},\n\nThank you for taking part in {brand}. The episode is now available:\n\n{url}\n\nThank you,\n{brand}"

func TestLegalReleaseLifecycle(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	actor := h.owner()
	prop := h.proposition("Recorded conversations")
	release := legal.Release{Proposition: prop, State: "NY", Title: "Street voices", Kind: "street", Brand: "Example show", RightsHolder: "Example LLC", Details: "Market square", EmailSubject: testSubject, EmailBody: testBody}
	e, err := h.srv.api.Legal.Save(ctx, actor, release)
	if err != nil {
		t.Fatal(err)
	}
	release, err = h.srv.api.Legal.Get(ctx, actor, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.board.DeleteProposition(ctx, actor, prop); !errors.Is(err, board.ErrLegalReleases) {
		t.Fatalf("delete with release: %v", err)
	}
	path := "/legal/" + release.Token
	if res, body := h.get(fmt.Sprintf("/releases/%d", release.ID)); res.StatusCode != 200 || !strings.Contains(body, "Download print-quality SVG") {
		t.Fatalf("manage %d: %s", res.StatusCode, body)
	}
	if res, body := h.get(path + "/qr.svg?download=1"); res.StatusCode != 200 || !strings.Contains(body, "Example show") || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("qr %d: %s", res.StatusCode, body)
	}
	jar, _ := cookiejar.New(nil)
	anon := &harness{T: t, srv: h.srv, db: h.db, http: h.http, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	res, body := anon.get(path)
	if res.StatusCode != 200 || !strings.Contains(body, "Example LLC") || !strings.Contains(body, "State of New York") {
		t.Fatalf("public %d: %s", res.StatusCode, body)
	}
	receipt := regexp.MustCompile(`name="receipt" value="([^"]+)"`).FindStringSubmatch(body)[1]
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "version": {"1"}, "agreement_digest": {release.Agreement().Digest()}, "receipt": {receipt}, "person": {"0", "1"}, "person_0_name": {"Ada Lovelace"}, "person_0_date": {time.Now().Format("2006-01-02")}, "person_0_email": {"ada@example.com"}, "person_0_notify": {"on"}, "person_0_consent": {"on"}, "person_1_name": {"Grace Hopper"}, "person_1_date": {time.Now().Format("2006-01-02")}, "person_1_email": {"grace@example.com"}}
	if res, _ = anon.post(path, form); res.StatusCode != 422 {
		t.Fatalf("missing second consent %d", res.StatusCode)
	}
	var n int
	n = legalCount(t, h, "SELECT count(*) FROM legal_submissions")
	if n != 0 {
		t.Fatal("partial group saved")
	}
	form.Set("person_1_consent", "on")
	if res, body = anon.post(path, form); res.StatusCode != 303 {
		t.Fatalf("sign %d: %s", res.StatusCode, body)
	}
	receiptPath := res.Header.Get("Location")
	if res, body = anon.get(receiptPath); res.StatusCode != 200 || !strings.Contains(body, "Ada Lovelace") || !strings.Contains(body, "Grace Hopper") {
		t.Fatalf("receipt %d: %s", res.StatusCode, body)
	}
	if res, _ = anon.post(path, form); res.StatusCode != 303 {
		t.Fatalf("retry %d", res.StatusCode)
	}
	n = legalCount(t, h, "SELECT count(*) FROM legal_submissions")
	if n != 1 {
		t.Fatalf("duplicates: %d", n)
	}
	clock := h.srv.api.Legal.Now
	h.srv.api.Legal.Now = func() time.Time { return time.Now().AddDate(0, 0, 4) }
	if res, _ = anon.post(path, form); res.StatusCode != 303 {
		t.Fatalf("late retry %d", res.StatusCode)
	}
	h.srv.api.Legal.Now = clock
	form.Set("person_0_name", "Changed signer")
	if res, body = anon.post(path, form); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("receipt reused with different data %d", res.StatusCode)
	}
	fresh := regexp.MustCompile(`name="receipt" value="([^"]+)"`).FindStringSubmatch(body)[1]
	if fresh == receipt {
		t.Fatal("corrected form kept the spent receipt")
	}
	form.Set("receipt", fresh)
	if res, _ = anon.post(path, form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("corrected resubmission %d", res.StatusCode)
	}
	release.State = "GA"
	release.Body = "Updated wording for {rights_holder}"
	release.RightsHolder = "Updated LLC"
	if _, err = h.srv.api.Legal.Save(ctx, actor, release); err != nil {
		t.Fatal(err)
	}
	if res, body = anon.get(receiptPath + "?download=1"); res.StatusCode != 200 || !strings.Contains(body, "New York") || strings.Contains(body, "Updated wording") {
		t.Fatalf("immutable receipt %d: %s", res.StatusCode, body)
	}
	if res, body = anon.get(path); res.StatusCode != 200 || !strings.Contains(body, "State of Georgia") || !strings.Contains(body, "Updated wording for Updated LLC") {
		t.Fatalf("new agreement %d: %s", res.StatusCode, body)
	}
	form.Set("receipt", legal.Token())
	if res, _ = anon.post(path, form); res.StatusCode != 422 {
		t.Fatalf("stale version accepted: %d", res.StatusCode)
	}
	if _, err = h.srv.api.Legal.Notify(ctx, actor, release.ID, 2, "https://example.com/ep", testSubject, testBody); !errors.Is(err, mail.ErrNotConfigured) {
		t.Fatalf("notify without mail: %v", err)
	}
	for key, value := range map[string]string{"mail.host": "smtp.example.com", "mail.from": "studio@example.com"} {
		if err = h.srv.settings.Set(ctx, key, []string{value}, actor.ID); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := h.srv.api.Legal.Preview(ctx, actor, release.ID, "https://example.com/ep", testSubject, testBody)
	if err != nil || len(messages) != 1 || messages[0].Email != "ada@example.com" || !strings.Contains(messages[0].Body, "https://example.com/ep") {
		t.Fatalf("preview %+v %v", messages, err)
	}
	if _, err = h.srv.api.Legal.Notify(ctx, actor, release.ID, 2, "https://example.com/ep", testSubject, testBody, "stale-preview"); !errors.Is(err, legal.ErrRecipientsChanged) {
		t.Fatalf("stale preview: %v", err)
	}
	for range 2 {
		retryCtx, keyErr := core.WithKey(ctx, "notify-once")
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		if _, err = h.srv.api.Legal.Notify(retryCtx, actor, release.ID, 2, "https://example.com/ep", testSubject, testBody, legal.MessageDigest(messages)); err != nil {
			t.Fatal(err)
		}
	}
	n = legalCount(t, h, "SELECT count(*) FROM mail_outbox WHERE to_addr='ada@example.com'")
	if n != 1 {
		t.Fatalf("notification count %d", n)
	}
	n = legalCount(t, h, "SELECT count(*) FROM mail_outbox WHERE to_addr='grace@example.com'")
	if n != 0 {
		t.Fatal("sent to participant without opt-in")
	}
	if res, _ = anon.get(fmt.Sprintf("/releases/%d/export", release.ID)); res.StatusCode != 303 {
		t.Fatalf("anonymous export %d", res.StatusCode)
	}
	if res, _ = anon.get(path + "/receipt/" + legal.Token()); res.StatusCode != 404 {
		t.Fatalf("guessed receipt %d", res.StatusCode)
	}
	if res, _ = anon.post(path, url.Values{"person": {"0"}}); res.StatusCode != 403 {
		t.Fatalf("csrf %d", res.StatusCode)
	}
	for _, role := range []string{auth.RoleGuest, auth.RoleEditor} {
		id, err := store.CreateUser(ctx, h.db, &store.User{Handle: role, Email: role + "@example.com", Name: role, Initials: "X", Colour: "#111", Role: role, PasswordHash: "x"})
		if err != nil {
			t.Fatal(err)
		}
		outsider := core.Actor{Kind: core.KindUser, ID: id}
		if _, err = h.srv.api.Legal.Get(ctx, outsider, release.ID); err == nil {
			t.Fatal("outsider read release")
		}
		if _, err = h.srv.api.Legal.Submissions(ctx, outsider, release.ID); err == nil {
			t.Fatal("outsider read signatures")
		}
		if _, err = h.srv.api.Legal.Notify(ctx, outsider, release.ID, 2, "https://example.com/ep", testSubject, testBody); err == nil {
			t.Fatal("outsider sent email")
		}
	}
	release, err = h.srv.api.Legal.Get(ctx, actor, release.ID)
	if err != nil {
		t.Fatal(err)
	}
	release.Closed = true
	if _, err = h.srv.api.Legal.Save(ctx, actor, release); err != nil {
		t.Fatal(err)
	}
	_, err = h.srv.api.Legal.Sign(ctx, release.Token, legal.Token(), 3, []legal.Person{{Name: "Test", Date: time.Now().Format("2006-01-02"), Consent: true}}, release.Agreement().Digest())
	if !errors.Is(err, legal.ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
	release.Proposition = h.proposition("Other production")
	release.Version = 3
	if _, err = h.srv.api.Legal.Save(ctx, actor, release); !errors.Is(err, legal.ErrInvalid) {
		t.Fatalf("signed reassignment: %v", err)
	}
	var activity string
	rows, err := h.db.QueryContext(ctx, "SELECT coalesce(after_json,'') FROM activity WHERE entity IN ('legal_release','legal_submission')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err = rows.Scan(&activity); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(activity, "ada@example.com") || strings.Contains(activity, "Ada Lovelace") || strings.Contains(activity, receipt) {
			t.Fatal("private data in activity")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLegalValidation(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	a := h.owner()
	p := h.proposition("Episode")
	r := legal.Release{Proposition: p, State: "GA", Title: "Interview", Kind: "interview", Brand: "Show", RightsHolder: "Example LLC", EmailSubject: testSubject, EmailBody: testBody}
	event, err := h.srv.api.Legal.Save(ctx, a, r)
	if err != nil {
		t.Fatal(err)
	}
	r, err = h.srv.api.Legal.Get(ctx, a, event.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	valid := legal.Person{Name: "Ada", Date: time.Now().Format("2006-01-02"), Consent: true}
	for name, change := range map[string]func(*legal.Person){"empty name": func(p *legal.Person) { p.Name = " " }, "invalid email": func(p *legal.Person) { p.Email = "broken" }, "notification without email": func(p *legal.Person) { p.Notify = true }, "no consent": func(p *legal.Person) { p.Consent = false }, "old date": func(p *legal.Person) { p.Date = "2000-01-01" }, "future date": func(p *legal.Person) { p.Date = "2100-01-01" }, "invalid date": func(p *legal.Person) { p.Date = "2026-99-99" }} {
		t.Run(name, func(t *testing.T) {
			person := valid
			change(&person)
			_, err := h.srv.api.Legal.Sign(ctx, r.Token, legal.Token(), 1, []legal.Person{person}, r.Agreement().Digest())
			if !errors.Is(err, legal.ErrInvalid) {
				t.Fatalf("%v", err)
			}
		})
	}
	for _, people := range [][]legal.Person{nil, make([]legal.Person, 21)} {
		if _, err = h.srv.api.Legal.Sign(ctx, r.Token, legal.Token(), 1, people, r.Agreement().Digest()); !errors.Is(err, legal.ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err = h.srv.api.Legal.Sign(ctx, r.Token, "bad", 1, []legal.Person{valid}, r.Agreement().Digest()); !errors.Is(err, legal.ErrInvalid) {
		t.Fatal(err)
	}
	for _, u := range []string{"javascript:alert(1)", "https://user:pass@example.com", "relative"} {
		if _, err = h.srv.api.Legal.Preview(ctx, a, r.ID, u, "subject", "body"); !errors.Is(err, legal.ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err = h.db.ExecContext(ctx, "UPDATE legal_releases SET body=? WHERE id=?", "Restored branch wording", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.srv.api.Legal.Sign(ctx, r.Token, legal.Token(), r.Version, []legal.Person{valid}, r.Agreement().Digest()); !errors.Is(err, legal.ErrChanged) {
		t.Fatalf("same version, different agreement: %v", err)
	}
	stale := r
	stale.Version = 0
	if _, err = h.srv.api.Legal.Save(ctx, a, stale); !errors.Is(err, legal.ErrChanged) {
		t.Fatal(err)
	}
	for _, state := range []string{"", "XX"} {
		bad := r
		bad.State = state
		if _, err = h.srv.api.Legal.Save(ctx, a, bad); !errors.Is(err, legal.ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err = h.srv.api.Legal.Save(ctx, core.Actor{Kind: core.KindPublic}, r); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("public edit %v", err)
	}
	res, body := h.get(fmt.Sprintf("/app/legal/releases/%d", r.ID))
	if res.StatusCode != 200 {
		t.Fatal(body)
	}
	var data struct {
		Release legal.Release `json:"release"`
	}
	if err = json.Unmarshal([]byte(body), &data); err != nil || data.Release.ID != r.ID {
		t.Fatal(body)
	}
}

func TestLegalManualEmailForm(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	actor := h.owner()
	prop := h.proposition("Episode")
	e, err := h.srv.api.Legal.Save(ctx, actor, legal.Release{Proposition: prop, State: "NY", Title: "Voices", Brand: "Radio", RightsHolder: "Example LLC", Kind: "street", EmailSubject: testSubject, EmailBody: testBody})
	if err != nil {
		t.Fatal(err)
	}
	release, err := h.srv.api.Legal.Get(ctx, actor, e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.srv.api.Legal.Sign(ctx, release.Token, legal.Token(), 1, []legal.Person{{Name: "Ada", Date: time.Now().Format("2006-01-02"), Consent: true, Email: "ada@example.com", Notify: true}}, release.Agreement().Digest())
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"mail.host": "smtp.example.com", "mail.from": "studio@example.com"} {
		if err = h.srv.settings.Set(ctx, key, []string{value}, actor.ID); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/releases/%d/notify", release.ID)
	form := url.Values{"csrf": {h.csrf(fmt.Sprintf("/releases/%d", release.ID))}, "version": {"1"}, "key": {"mail-form"}, "episode_url": {"https://example.com/ep"}, "email_subject": {"A note for {name}"}, "email_body": {"Listen to {title}: {url}"}}
	res, body := h.post(path, form)
	if res.StatusCode != 200 || !strings.Contains(body, "A note for Ada") {
		t.Fatalf("preview %d: %s", res.StatusCode, body)
	}
	var n int
	n = legalCount(t, h, "SELECT count(*) FROM mail_outbox WHERE to_addr='ada@example.com'")
	if n != 0 {
		t.Fatal("preview queued email")
	}
	digest := regexp.MustCompile(`name="preview_hash" value="([^"]+)"`).FindStringSubmatch(body)
	if len(digest) != 2 {
		t.Fatal("missing preview digest")
	}
	form.Set("preview_hash", digest[1])
	form.Set("send", "yes")
	for range 2 {
		res, body = h.post(path, form)
		if res.StatusCode != 200 || !strings.Contains(body, "Notifications queued") {
			t.Fatalf("send %d: %s", res.StatusCode, body)
		}
	}
	var subject, mailBody string
	err = h.db.QueryRowContext(ctx, "SELECT subject,body_text FROM mail_outbox WHERE to_addr='ada@example.com'").Scan(&subject, &mailBody)
	if err != nil || subject != "A note for Ada" || mailBody != "Listen to Voices: https://example.com/ep" {
		t.Fatalf("queued mail %q %q: %v", subject, mailBody, err)
	}
	n = legalCount(t, h, "SELECT count(*) FROM mail_outbox WHERE to_addr='ada@example.com'")
	if n != 1 {
		t.Fatalf("duplicate messages %d", n)
	}
	api := fmt.Sprintf("/app/legal/releases/%d/", release.ID)
	csrf := h.csrf(fmt.Sprintf("/releases/%d", release.ID))
	request := `{"version":1,"url":"https://example.com/ep","subject":"New","body":"{url}"}`
	res, body = h.send("POST", api+"preview", csrf, request)
	var preview struct {
		PreviewHash string `json:"preview_hash"`
	}
	if err = json.Unmarshal([]byte(body), &preview); res.StatusCode != 200 || err != nil || preview.PreviewHash == "" {
		t.Fatalf("api preview %d: %s", res.StatusCode, body)
	}
	if _, err = h.srv.api.Legal.Sign(ctx, release.Token, legal.Token(), 1, []legal.Person{{Name: "Grace", Date: time.Now().Format("2006-01-02"), Consent: true, Email: "grace@example.com", Notify: true}}, release.Agreement().Digest()); err != nil {
		t.Fatal(err)
	}
	withHash := strings.Replace(request, "{", `{"preview_hash":"`+preview.PreviewHash+`",`, 1)
	if res, body = h.send("POST", api+"notify", csrf, withHash); res.StatusCode != http.StatusConflict || !strings.Contains(body, "preview again") {
		t.Fatalf("api notify after recipients changed %d: %s", res.StatusCode, body)
	}
	if n = legalCount(t, h, "SELECT count(*) FROM mail_outbox WHERE to_addr='grace@example.com'"); n != 0 {
		t.Fatal("stale preview queued mail")
	}
}

func TestLegalSignRateLimitKeepsForm(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	ctx := context.Background()
	e, err := h.srv.api.Legal.Save(ctx, h.owner(), legal.Release{Proposition: h.proposition("Episode"), State: "NY", Title: "Voices", Brand: "Radio", RightsHolder: "Example LLC", Kind: "street", EmailSubject: testSubject, EmailBody: testBody})
	if err != nil {
		t.Fatal(err)
	}
	release, err := h.srv.api.Legal.Get(ctx, h.owner(), e.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	anon := &harness{T: t, srv: h.srv, db: h.db, http: h.http, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	path := "/legal/" + release.Token
	_, body := anon.get(path)
	receipt := legal.Token()
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "version": {"1"}, "agreement_digest": {release.Agreement().Digest()}, "receipt": {receipt}, "person": {"0"}, "person_0_name": {"Ada Lovelace"}, "person_0_date": {time.Now().Format("2006-01-02")}}
	var res *http.Response
	for range 40 {
		if res, body = anon.post(path, form); res.StatusCode == http.StatusTooManyRequests {
			break
		}
	}
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" || !strings.Contains(body, "Too many submissions") || !strings.Contains(body, `value="Ada Lovelace"`) || !strings.Contains(body, `name="receipt" value="`+receipt+`"`) {
		t.Fatalf("rate limit %d: %s", res.StatusCode, body)
	}
}

func legalCount(t *testing.T, h *harness, query string) int {
	t.Helper()
	var count int
	if err := h.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
