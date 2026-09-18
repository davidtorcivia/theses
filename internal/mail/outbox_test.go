package mail

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// loopingFake is startFake with an accept loop, because the worker opens one
// connection per row and the tests here send more than one.
type loopingFake struct {
	port int

	mu   sync.Mutex
	data []string
	to   []string
}

func startLoopingFake(t *testing.T) *loopingFake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := &loopingFake{}
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

func (f *loopingFake) serve(conn net.Conn) {
	defer conn.Close()
	tp := textproto.NewConn(conn)
	tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			tp.PrintfLine("250-fake")
			tp.PrintfLine("250 HELP")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			f.mu.Lock()
			f.to = append(f.to, angled(line))
			f.mu.Unlock()
			tp.PrintfLine("250 OK")
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

func (f *loopingFake) delivered() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.to...), append([]string(nil), f.data...)
}

func newTestOutbox(t *testing.T) (*Outbox, *store.DB, *settings.Settings) {
	t.Helper()
	db := store.OpenTemp(t)
	set, err := settings.Open(context.Background(), db, []byte("a settings key of at least thirty-two bytes"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewOutbox(db, set, log), db, set
}

func configure(t *testing.T, set *settings.Settings, host string, port int) {
	t.Helper()
	ctx := context.Background()
	for key, value := range map[string]string{
		"mail.host":     host,
		"mail.port":     strconv.Itoa(port),
		"mail.tls":      "none",
		"mail.from":     "THESES <theses@example.org>",
		"mail.password": "hunter2hunter2",
	} {
		if err := set.Set(ctx, key, []string{value}, 0); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
}

func countRows(t *testing.T, db *store.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM mail_outbox WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnqueueRollsBackWithItsTransaction(t *testing.T) {
	_, db, _ := newTestOutbox(t)
	ctx := context.Background()
	msg := Invite{To: "ana@example.com", Inviter: "DT", Role: "editor", URL: "https://x/invite/t", Expires: 7 * 24 * time.Hour}.Message()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, tx, msg); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `1=1`); n != 0 {
		t.Fatalf("an uncommitted row is already visible: %d", n)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `1=1`); n != 0 {
		t.Fatalf("rollback left %d rows", n)
	}

	// The same message through a committed transaction does land.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, tx, msg); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `to_addr = ?`, "ana@example.com"); n != 1 {
		t.Fatalf("commit left %d rows", n)
	}
}

func TestOutboxSendsAndMarksSent(t *testing.T) {
	f := startLoopingFake(t)
	o, db, set := newTestOutbox(t)
	configure(t, set, "127.0.0.1", f.port)
	ctx := context.Background()

	for _, to := range []string{"ana@example.com", "bo@example.com"} {
		if err := Enqueue(ctx, db, Reset{To: to, URL: "https://x/reset/t", Expires: time.Hour}.Message()); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.once(ctx); err != nil {
		t.Fatal(err)
	}

	to, data := f.delivered()
	if len(to) != 2 || to[0] != "ana@example.com" || to[1] != "bo@example.com" {
		t.Fatalf("recipients = %v", to)
	}
	if len(data) != 2 || !strings.Contains(data[0], "https://x/reset/t") {
		t.Fatalf("the body did not carry the link: %q", data)
	}
	if n := countRows(t, db, `sent_at IS NULL`); n != 0 {
		t.Fatalf("%d rows are still unsent", n)
	}
}

func TestOutboxRecordsFailureAndBacksOff(t *testing.T) {
	o, db, set := newTestOutbox(t)
	// A port nothing listens on: the dial fails at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	dead, _ := strconv.Atoi(port)
	configure(t, set, "127.0.0.1", dead)

	ctx := context.Background()
	if err := Enqueue(ctx, db, Reset{To: "ana@example.com", URL: "https://x/reset/t", Expires: time.Hour}.Message()); err != nil {
		t.Fatal(err)
	}
	if err := o.once(ctx); err != nil {
		t.Fatal(err)
	}

	var attempts int
	var lastError string
	var nextAt, createdAt int64
	if err := db.QueryRowContext(ctx,
		`SELECT attempts, last_error, next_at, created_at FROM mail_outbox`).
		Scan(&attempts, &lastError, &nextAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == "" || strings.Contains(lastError, "hunter2hunter2") {
		t.Errorf("last_error = %q", lastError)
	}
	if delay := nextAt - createdAt; delay < 55 || delay > 65 {
		t.Errorf("first retry is %ds away, want about 60", delay)
	}
	if n := countRows(t, db, `sent_at IS NULL`); n != 1 {
		t.Errorf("the row that failed was not kept: %d", n)
	}

	// Ready again, but past the day: the batch no longer sees it, and it stays
	// for inspection with its error.
	if _, err := db.ExecContext(ctx,
		`UPDATE mail_outbox SET created_at = unixepoch() - ?, next_at = unixepoch()`,
		int64(25*time.Hour/time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := o.once(ctx); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.QueryRowContext(ctx, `SELECT attempts FROM mail_outbox`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != attempts {
		t.Errorf("a row past the day was tried again: attempts %d", after)
	}
	st, err := o.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 0 || st.GivenUp != 1 || st.LastError == "" {
		t.Errorf("state = %+v", st)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	for _, c := range []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{7, time.Hour},
		{40, time.Hour},
	} {
		if got := backoff(c.attempts); got != c.want {
			t.Errorf("backoff(%d) = %s, want %s", c.attempts, got, c.want)
		}
	}
}

func TestOutboxLeavesRowsAloneWhileMailIsNotConfigured(t *testing.T) {
	o, db, _ := newTestOutbox(t)
	ctx := context.Background()
	if err := Enqueue(ctx, db, Reset{To: "ana@example.com", URL: "https://x/reset/t", Expires: time.Hour}.Message()); err != nil {
		t.Fatal(err)
	}
	if err := o.once(ctx); err != ErrNotConfigured {
		t.Fatalf("once = %v, want ErrNotConfigured", err)
	}
	var attempts int
	var sentAt *int64
	if err := db.QueryRowContext(ctx, `SELECT attempts, sent_at FROM mail_outbox`).Scan(&attempts, &sentAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || sentAt != nil {
		t.Errorf("attempts = %d, sent_at = %v; the row should be untouched", attempts, sentAt)
	}
	st, err := o.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 1 {
		t.Errorf("pending = %d, want 1", st.Pending)
	}
}

func TestRedactLeavesTextWithoutASecretAlone(t *testing.T) {
	if got := Redact("dial tcp: refused", ""); got != "dial tcp: refused" {
		t.Errorf("an empty secret changed the text: %q", got)
	}
	if got := Redact("auth: 535 hunter2 rejected", "hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("the secret survived: %q", got)
	}
}
