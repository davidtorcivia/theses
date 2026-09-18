package server

import (
	"context"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// fakeSMTP is the smallest dialogue net/smtp needs, with an accept loop. It is
// a copy of the one in internal/mail's tests, which is package-private there.
type fakeSMTP struct {
	port int

	mu   sync.Mutex
	data []string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := &fakeSMTP{}
	f.port, _ = strconv.Atoi(port)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	tp := textproto.NewConn(conn)
	tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(line); {
		case strings.HasPrefix(cmd, "EHLO"):
			tp.PrintfLine("250-fake")
			tp.PrintfLine("250 HELP")
		case cmd == "DATA":
			tp.PrintfLine("354 go ahead")
			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.data = append(f.data, string(b))
			f.mu.Unlock()
			tp.PrintfLine("250 OK")
		case cmd == "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("250 OK")
		}
	}
}

func (f *fakeSMTP) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.data...)
}

// deadPort is a port nothing listens on, so a dial to it fails at once.
func deadPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	n, _ := strconv.Atoi(port)
	return n
}

const smtpPassword = "hunter2hunter2"

func (h *harness) configureMail(port int) {
	h.Helper()
	res, body := h.post("/settings", url.Values{
		"csrf": {h.csrf("/settings")}, "mail.host": {"127.0.0.1"},
		"mail.port": {strconv.Itoa(port)}, "mail.tls": {"none"},
		"mail.from": {"THESES <theses@example.fm>"}, "mail.password": {smtpPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		h.Fatalf("saving the mail settings gave %d:\n%s", res.StatusCode, body)
	}
}

func TestSettingsSaysMailIsNotConfigured(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	_, body := h.get("/settings")
	if !strings.Contains(body, "mail is not configured") {
		t.Errorf("the mail section does not say it is unconfigured:\n%s", body)
	}
	if !strings.Contains(body, "Outbox: 0 waiting") {
		t.Errorf("the outbox state is missing:\n%s", body)
	}
}

func TestTestSendGoesThroughTheConfiguredServer(t *testing.T) {
	f := startFakeSMTP(t)
	h := newHarness(t)
	h.setupOwner()
	h.configureMail(f.port)

	res, body := h.post("/settings/test/mail", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Sent to dt@example.fm.") {
		t.Fatalf("test send gave %d:\n%s", res.StatusCode, body)
	}
	got := f.received()
	if len(got) != 1 || !strings.Contains(got[0], "THESES test message") {
		t.Errorf("the server received %v", got)
	}
	// It went straight out rather than into the queue.
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM mail_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the test message was queued as well: %d rows", n)
	}
}

func TestTestSendShowsTheErrorWithoutThePassword(t *testing.T) {
	h := newHarness(t)
	h.setupOwner()
	h.configureMail(deadPort(t))

	res, body := h.post("/settings/test/mail", url.Values{"csrf": {h.csrf("/settings")}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a refused connection gave %d", res.StatusCode)
	}
	if !strings.Contains(body, "mail: dial 127.0.0.1:") {
		t.Errorf("the failure is not shown inline:\n%s", body)
	}
	if strings.Contains(body, smtpPassword) {
		t.Error("the SMTP password is on the page")
	}
}

func TestRetryNowPutsUnsentRowsBackAtTheFront(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.setupOwner()
	if _, err := h.db.ExecContext(ctx, `INSERT INTO mail_outbox
		(to_addr, subject, body_text, attempts, last_error, created_at, next_at)
		VALUES ('ana@example.fm', 'Old', 'Old', 4, 'refused', unixepoch() - 90000, unixepoch() + 3600)`); err != nil {
		t.Fatal(err)
	}
	_, body := h.get("/settings")
	if !strings.Contains(body, "1 given up") {
		t.Fatalf("a row past the day is not reported as given up:\n%s", body)
	}

	_, body = h.post("/settings/mail/retry", url.Values{"csrf": {h.csrf("/settings")}})
	if !strings.Contains(body, "back at the front of the queue") {
		t.Fatalf("retry said nothing:\n%s", body)
	}
	var attempts int
	var next, created int64
	if err := h.db.QueryRowContext(ctx,
		`SELECT attempts, next_at, created_at FROM mail_outbox`).Scan(&attempts, &next, &created); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || next > created {
		t.Errorf("attempts = %d, next_at = %d, created_at = %d", attempts, next, created)
	}
	if !strings.Contains(body, "1 waiting") {
		t.Errorf("the panel still counts it as given up:\n%s", body)
	}
}
