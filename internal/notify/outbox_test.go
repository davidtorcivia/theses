package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/notify/channel"
)

// sent is what the worker handed to a channel, recorded instead of delivered.
type sent struct {
	channel int64
	note    channel.Note
}

// fakeSender replaces the delivery for the length of one test. The senders
// themselves are covered in internal/notify/channel; what is under test here is
// the worker around them.
func fakeSender(t *testing.T, fail func(int) error) *[]sent {
	t.Helper()
	var calls []sent
	was := send
	send = func(_ context.Context, _ *Service, c Channel, _ string, p payload) error {
		calls = append(calls, sent{channel: c.ID, note: note(p)})
		if fail != nil {
			return fail(len(calls))
		}
		return nil
	}
	t.Cleanup(func() { send = was })
	return &calls
}

// state is one row's attempts, next_at, error and whether it went out.
func (f *fixture) state(t *testing.T, id int64) (attempts int, nextAt int64, lastError string, done bool) {
	t.Helper()
	var sentAt *int64
	err := f.db.QueryRowContext(context.Background(),
		`SELECT attempts, next_at, last_error, sent_at FROM notification_outbox WHERE id = ?`, id).
		Scan(&attempts, &nextAt, &lastError, &sentAt)
	if err != nil {
		t.Fatal(err)
	}
	return attempts, nextAt, lastError, sentAt != nil
}

// queueOne puts one row in the outbox for a channel and returns its id.
func (f *fixture) queueOne(t *testing.T, c Channel) int64 {
	t.Helper()
	f.onCard(t, 7, c.UserID)
	// Moved by somebody who is not on the card, so the row is for its owner.
	if err := f.s.Handle(context.Background(), f.move(t, 0, 7)); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("queued %d rows, want 1", len(got))
	}
	// Collapsible events wait a minute for the next one; the worker is what is
	// under test, so the row is made due.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE notification_outbox SET next_at = ? WHERE id = ?`, now, got[0].ID); err != nil {
		t.Fatal(err)
	}
	return got[0].ID
}

func TestWorkerSendsAndMarksSent(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	id := f.queueOne(t, c)
	calls := fakeSender(t, nil)

	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("sent %d messages, want 1", len(*calls))
	}
	if (*calls)[0].note.Body == "" || (*calls)[0].note.Tags == nil {
		t.Errorf("note = %+v, want a body and the event's tags", (*calls)[0].note)
	}
	if _, _, _, done := f.state(t, id); !done {
		t.Error("the row was not marked sent")
	}
}

func TestWorkerBacksOffAndGivesUpAfterADay(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	id := f.queueOne(t, c)
	fakeSender(t, func(int) error { return errors.New("ntfy: 502 Bad Gateway") })

	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, nextAt, lastError, done := f.state(t, id)
	if done {
		t.Fatal("a refused message was marked sent")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError != "ntfy: 502 Bad Gateway" {
		t.Errorf("last_error = %q", lastError)
	}
	if nextAt <= time.Now().Unix() {
		t.Errorf("next_at = %d, want a minute from now", nextAt)
	}

	// A day of failure later the row is not read at all, which is what bounds
	// the retries.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE notification_outbox SET next_at = ?, tried_at = ? WHERE id = ?`,
		time.Now().Unix(), time.Now().Add(-25*time.Hour).Unix(), id); err != nil {
		t.Fatal(err)
	}
	calls := fakeSender(t, func(int) error { return errors.New("still down") })
	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("tried %d times after a day of failure, want none", len(*calls))
	}
}

func TestWorkerRechecksTheChannelBeforeSending(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	id := f.queueOne(t, c)
	// Unverified between the enqueue and the send, which is the window a batch
	// read before the first delivery leaves open.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE notification_channels SET verified_at = NULL WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	calls := fakeSender(t, nil)

	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("sent to a channel that is no longer verified")
	}
	_, _, lastError, done := f.state(t, id)
	if done {
		t.Error("the row was marked sent")
	}
	if !strings.Contains(lastError, "not verified") {
		t.Errorf("last_error = %q, want it to say why", lastError)
	}
	// And it is not tried again for ever.
	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("an abandoned row was picked up again")
	}
}

func TestMailThatIsNotConfiguredCostsNoAttempt(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindEmail}, "moved")
	id := f.queueOne(t, c)
	// Queued just now, so the day it may wait for a mail server is ahead.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE notification_outbox SET created_at = unixepoch() WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	// No fake sender: the real path runs and stops at the missing SMTP host,
	// which is a workspace that has not been set up rather than a failure.
	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, _, why, done := f.state(t, id)
	if done {
		t.Fatal("a message went out with no mail server configured")
	}
	if attempts != 0 || why != "" {
		t.Errorf("attempts = %d, last_error %q, want 0 and none: the day of retries has not started", attempts, why)
	}
}

// A batch is twenty rows. Undeliverable email at the head of the queue must not
// be able to hold everything behind it.
func TestUnsendableMailDoesNotBlockTheQueue(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	post := f.channel(t, Channel{UserID: grace, Kind: KindEmail}, "moved")
	hook := f.channel(t, Channel{UserID: grace, Kind: KindWebhook,
		Config: Config{URL: "https://example.com/hook"}}, "moved")
	f.onCard(t, 7, grace)

	// More email rows than one batch holds, all due, all older than the webhook.
	for range batchSize + 5 {
		if _, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_outbox
			(channel_id, payload_json, created_at, next_at, event)
			VALUES (?, '{"event":"moved","title":"t","items":[{"text":"x"}]}', ?, ?, 'moved')`,
			post.ID, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_outbox
		(channel_id, payload_json, created_at, next_at, event)
		VALUES (?, '{"event":"moved","title":"t","items":[{"text":"x"}]}', ?, ?, 'moved')`,
		hook.ID, now, now); err != nil {
		t.Fatal(err)
	}

	// Mail is not configured, so every email row stops at the sender. The
	// webhook behind them still goes out.
	calls := fakeSender(t, func(int) error { return nil })
	was := send
	send = func(ctx context.Context, s *Service, c Channel, email string, p payload) error {
		if c.Kind == KindEmail {
			return mail.ErrNotConfigured
		}
		return was(ctx, s, c, email, p)
	}

	for range 3 {
		if err := f.s.once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(*calls) != 1 || (*calls)[0].channel != hook.ID {
		t.Fatalf("delivered %+v, want the one webhook message", *calls)
	}
}

// A row waiting on a mail server nobody has set up waits a day from its
// enqueue and is then given up, rather than piling up until one is set.
func TestUnconfiguredMailGivesUpAfterADay(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	post := f.channel(t, Channel{UserID: grace, Kind: KindEmail}, "moved")
	fakeSender(t, func(int) error { return mail.ErrNotConfigured })
	queued := map[string]int64{}
	for name, created := range map[string]time.Time{
		"fresh": time.Now(),
		"stale": time.Now().Add(-giveUpAfter - time.Minute),
	} {
		res, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_outbox
			(channel_id, payload_json, created_at, next_at, event)
			VALUES (?, '{"event":"moved","title":"t","items":[{"text":"x"}]}', ?, ?, 'moved')`,
			post.ID, created.Unix(), created.Unix())
		if err != nil {
			t.Fatal(err)
		}
		queued[name], _ = res.LastInsertId()
	}
	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, given := range map[string]bool{"fresh": false, "stale": true} {
		var tried *int64
		var why string
		if err := f.db.QueryRowContext(context.Background(),
			`SELECT tried_at, last_error FROM notification_outbox WHERE id = ?`, queued[name]).Scan(&tried, &why); err != nil {
			t.Fatal(err)
		}
		if (tried != nil) != given || given && why == "" {
			t.Errorf("%s row: tried_at %v, last_error %q, want given up %v", name, tried, why, given)
		}
	}
}

func TestSecretsAreRedactedFromStoredErrors(t *testing.T) {
	f := newFixture(t)
	c := Channel{Kind: KindWebhook, Config: Config{URL: "https://example.com/hook", Secret: "s3cret-token"}}
	err := f.s.redacted(context.Background(),
		errors.New(`webhook: post failed with secret "s3cret-token"`), c)
	if strings.Contains(err.Error(), "s3cret-token") {
		t.Fatalf("the secret survived redaction: %q", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("redacted error = %q", err)
	}
	if f.s.redacted(context.Background(), nil, c) != nil {
		t.Error("nil became an error")
	}
}

// A webhook URL and an ntfy topic are the credential for their destination,
// and net/http quotes the whole address in a transport error.
func TestAddressesAreRedactedFromStoredErrors(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name   string
		c      Channel
		err    error
		secret string
	}{
		{"a webhook URL", Channel{Kind: KindWebhook, Config: Config{URL: "https://hooks.example.com/services/T0/B1?token=abc"}},
			fmt.Errorf("webhook: %w", &url.Error{Op: "Post", URL: "https://hooks.example.com/services/T0/B1?token=abc", Err: errors.New("i/o timeout")}),
			"T0/B1?token=abc"},
		{"a webhook path alone", Channel{Kind: KindWebhook, Config: Config{URL: "https://hooks.example.com/services/T0/B1?token=abc"}},
			errors.New(`webhook: redirect to "/services/T0/B1?token=abc" refused`), "T0/B1?token=abc"},
		{"an ntfy topic", Channel{Kind: KindNtfy, Config: Config{Topic: "tides-4f9a2c"}},
			fmt.Errorf("ntfy: %w", &url.Error{Op: "Post", URL: "https://ntfy.example.com/tides-4f9a2c", Err: errors.New("i/o timeout")}),
			"tides-4f9a2c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.s.redacted(context.Background(), tc.err, tc.c).Error()
			if strings.Contains(got, tc.secret) || !strings.Contains(got, "i/o timeout") && !strings.Contains(got, "refused") {
				t.Fatalf("redacted error = %q", got)
			}
		})
	}
}

func TestTheSMTPPasswordIsRedactedToo(t *testing.T) {
	f := newFixture(t)
	c := Channel{Kind: KindEmail}
	err := f.s.redacted(context.Background(),
		errors.New("mail: 535 authentication failed for hunter2seventeen"), c, "hunter2seventeen")
	if strings.Contains(err.Error(), "hunter2seventeen") {
		t.Fatalf("the SMTP password survived redaction: %q", err)
	}
}

func TestBackoffIsBoundedByTheHour(t *testing.T) {
	for _, tt := range []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{7, 64 * time.Minute},
		{20, time.Hour},
		{1000, time.Hour},
	} {
		got := backoff(tt.attempts)
		if got > time.Hour {
			t.Errorf("backoff(%d) = %s, past the cap", tt.attempts, got)
		}
		if tt.attempts <= 6 && got != tt.want {
			t.Errorf("backoff(%d) = %s, want %s", tt.attempts, got, tt.want)
		}
	}
}

func TestNoteRendersOneLineAndMany(t *testing.T) {
	one := payload{Title: "Moved: a card", Items: []item{{Text: "Ada moved it", URL: "u"}}}
	if n := note(one); n.Body != "Ada moved it" || n.Title != "Moved: a card" || n.URL != "u" {
		t.Errorf("one item = %+v", n)
	}
	many := payload{Title: "Moved: a card", Items: []item{
		{Text: "one", URL: "u"}, {Text: "two", URL: "v"}, {Text: "three", URL: "w"}}}
	n := note(many)
	if n.Title != "Moved: a card, and 2 more" {
		t.Errorf("title = %q", n.Title)
	}
	if n.Body != "one\ntwo\nthree" {
		t.Errorf("body = %q", n.Body)
	}
	digest := payload{Event: digestEvent, Title: "THESES daily digest",
		Items: []item{{Text: "one"}, {Text: "two"}}}
	if got := note(digest).Title; got != "THESES daily digest" {
		t.Errorf("a digest is not counted in its own title: %q", got)
	}
	if got := note(payload{Title: "empty"}).Body; got != "" {
		t.Errorf("a payload with no lines = %q", got)
	}
}

func TestTemplateChoosesTheMailForTheEvent(t *testing.T) {
	for _, tt := range []struct {
		event   string
		want    bool
		subject string
	}{
		{"mentioned", true, "Ada Lovelace mentioned you in a note"},
		{"assigned", true, "Assigned to you: Fix the intro"},
		{digestEvent, true, "THESES daily digest"},
		{"moved", false, ""},
		{"file", false, ""},
	} {
		n := channel.Note{Event: tt.event, Actor: "Ada Lovelace", Entity: "comment",
			Title: "Assigned to you: Fix the intro", Body: "something happened"}
		msg, ok := template("grace@example.com", n, nil, "Workspace")
		if ok != tt.want {
			t.Errorf("%s has a template = %v, want %v", tt.event, ok, tt.want)
			continue
		}
		if ok && msg.Subject != tt.subject {
			t.Errorf("%s subject = %q, want %q", tt.event, msg.Subject, tt.subject)
		}
	}

	digest := payload{Event: digestEvent, Title: "THESES daily digest", Items: []item{
		{Text: "one", URL: "https://example.com/p/1"},
		{Text: "two", URL: "https://example.com/p/2"},
	}}
	msg, ok := template("grace@example.com", note(digest), digest.Items, "Workspace")
	if !ok {
		t.Fatal("a digest did not choose its mail template")
	}
	for _, url := range []string{"https://example.com/p/1", "https://example.com/p/2"} {
		if strings.Count(msg.Text, url) != 1 || strings.Count(msg.HTML, url) != 2 {
			t.Errorf("digest did not keep one item URL: %q / %q", msg.Text, msg.HTML)
		}
	}
}

func TestStateCountsWhatIsWaiting(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	id := f.queueOne(t, c)

	st, err := f.s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 1 || st.GivenUp != 0 {
		t.Errorf("state = %+v, want one waiting", st)
	}
	if err := f.s.abandon(context.Background(), id, "gave up"); err != nil {
		t.Fatal(err)
	}
	st, err = f.s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 0 || st.GivenUp != 1 || st.LastError != "gave up" {
		t.Errorf("state = %+v, want one given up", st)
	}
}
