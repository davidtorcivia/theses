package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	send = func(_ context.Context, _ *Service, c Channel, _ string, n channel.Note) error {
		calls = append(calls, sent{channel: c.ID, note: n})
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

	// No fake sender: the real path runs and stops at the missing SMTP host,
	// which is a workspace that has not been set up rather than a failure.
	if err := f.s.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, _, _, done := f.state(t, id)
	if done {
		t.Fatal("a message went out with no mail server configured")
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0: the day of retries has not started", attempts)
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
		msg, ok := template("grace@example.com", n, "Workspace")
		if ok != tt.want {
			t.Errorf("%s has a template = %v, want %v", tt.event, ok, tt.want)
			continue
		}
		if ok && msg.Subject != tt.subject {
			t.Errorf("%s subject = %q, want %q", tt.event, msg.Subject, tt.subject)
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
