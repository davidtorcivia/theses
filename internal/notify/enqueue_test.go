package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

var testKey = []byte("a secret key of at least thirty-two bytes")

// now is the moment every test runs at unless it says otherwise: an afternoon,
// so nothing is inside a night time quiet window by accident.
var now = time.Date(2026, 9, 8, 14, 26, 0, 0, time.UTC).Unix()

type fixture struct {
	db  *store.DB
	set *settings.Settings
	s   *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := store.OpenTemp(t)
	set, err := settings.Open(context.Background(), db, testKey)
	if err != nil {
		t.Fatal(err)
	}
	// UTC everywhere, so a quiet window means the same thing on any machine.
	if err := set.Set(context.Background(), "workspace.timezone", []string{"UTC"}, 0); err != nil {
		t.Fatal(err)
	}
	s := New(db, set, slog.New(slog.NewTextHandler(io.Discard, nil)), "https://example.com")
	s.Now = func() int64 { return now }
	return &fixture{db: db, set: set, s: s}
}

// user makes an account and returns its id.
func (f *fixture) user(t *testing.T, handle string) int64 {
	t.Helper()
	res, err := f.db.ExecContext(context.Background(), `INSERT INTO users
		(handle, email, name, initials, colour, role, password_hash, created_at)
		VALUES (?, ?, ?, ?, '#1100ff', 'editor', 'x', unixepoch())`,
		handle, handle+"@example.com", handle, "AL")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// channel makes a verified channel and, unless events is empty, the rules that
// tick it for them.
func (f *fixture) channel(t *testing.T, c Channel, events ...string) Channel {
	t.Helper()
	c.VerifiedAt = now
	saved, err := SaveChannel(context.Background(), f.db, f.set, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_rules
			(user_id, event, channel_id, enabled) VALUES (?, ?, ?, 1)`, c.UserID, e, saved.ID); err != nil {
			t.Fatal(err)
		}
	}
	return saved
}

type row struct {
	ID        int64
	ChannelID int64
	NextAt    int64
	Collapse  string
	Event     string
	Payload   payload
}

func (f *fixture) outbox(t *testing.T) []row {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT id, channel_id, next_at, collapse, event, payload_json FROM notification_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		var body string
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.NextAt, &r.Collapse, &r.Event, &body); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(body), &r.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// move is a card move by ada with everybody in assignees on the card.
func (f *fixture) move(t *testing.T, actor int64, cardID int64) core.Event {
	t.Helper()
	return core.Event{
		Entity: "card", Action: "move", Proposition: 3,
		Actor:  core.Actor{Kind: core.KindUser, ID: actor, Name: "Ada Lovelace"},
		Before: card(t, map[string]any{"id": cardID}),
		After:  card(t, map[string]any{"id": cardID}),
	}
}

func (f *fixture) onCard(t *testing.T, cardID int64, users ...int64) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO propositions (id, number, title, status, position, created_at)
		 VALUES (3, 10, 'Ten', 'idea', 'a', unixepoch()) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO columns (id, proposition_id, name, position) VALUES (2, 3, 'Research', 'a')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO cards (id, proposition_id, column_id, position, title, created_at)
		 VALUES (?, 3, 2, 'a', 'Fix the intro', unixepoch()) ON CONFLICT DO NOTHING`, cardID); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if _, err := f.db.ExecContext(context.Background(),
			`INSERT OR IGNORE INTO card_assignees (card_id, user_id) VALUES (?, ?)`, cardID, u); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActorIsNeverNotifiedOfTheirOwnChange(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, ada, grace)
	f.channel(t, Channel{UserID: ada, Kind: KindPushover, Config: Config{UserKey: "k"}}, "moved")
	f.channel(t, Channel{UserID: grace, Kind: KindPushover, Config: Config{UserKey: "k"}}, "moved")

	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("wrote %d rows, want 1 (Ada moved her own card)", len(got))
	}
	if got[0].Payload.Items[0].URL != "https://example.com/p/3" {
		t.Errorf("link = %q", got[0].Payload.Items[0].URL)
	}
}

func TestAChannelNobodyHasVerifiedGetsNothing(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	c := f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE notification_channels SET verified_at = NULL WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Fatalf("wrote %d rows to an unverified channel", len(got))
	}
}

func TestACellNobodyTickedGetsNothing(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "assigned")
	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Fatalf("wrote %d rows for an event nobody ticked", len(got))
	}
}

func TestBurstsCollapseIntoOneRow(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")

	for range 5 {
		if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
			t.Fatal(err)
		}
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("five moves in a minute wrote %d rows, want 1", len(got))
	}
	if len(got[0].Payload.Items) != 5 {
		t.Errorf("row carries %d lines, want 5", len(got[0].Payload.Items))
	}
	if got[0].NextAt != now+60 {
		t.Errorf("next_at = %d, want %d", got[0].NextAt, now+60)
	}
	if n := note(got[0].Payload); n.Title != "Moved: Fix the intro, and 4 more" {
		t.Errorf("title = %q", n.Title)
	}
}

// A row whose turn has come may be halfway through the worker's batch, so a
// line appended to it would be marked sent without ever going out.
func TestARowThatIsDueIsNotCollapsedInto(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")

	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	first := f.outbox(t)[0]
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE notification_outbox SET next_at = ? WHERE id = ?`, now-1, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 2 {
		t.Fatalf("wrote %d rows, want 2: the due one must be left alone", len(got))
	}
}

func TestQuietHoursHoldARowUntilTheWindowEnds(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	// 23:30 UTC on the day of, inside a window that crosses midnight.
	night := time.Date(2026, 9, 8, 23, 30, 0, 0, time.UTC).Unix()
	f.s.Now = func() int64 { return night }
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"},
		QuietFrom: "23:00", QuietTo: "07:00"}, "moved")

	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("wrote %d rows", len(got))
	}
	want := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC).Unix()
	if got[0].NextAt != want {
		t.Errorf("next_at = %d, want %d (07:00 the next morning)", got[0].NextAt, want)
	}
}

// The one rule that overrides the rest: being named reaches somebody now.
func TestAMentionGetsThroughDigestAndQuietHours(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	night := time.Date(2026, 9, 8, 23, 30, 0, 0, time.UTC).Unix()
	f.s.Now = func() int64 { return night }
	f.channel(t, Channel{UserID: grace, Kind: KindEmail, Digest: true}, "mentioned")
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"},
		QuietFrom: "23:00", QuietTo: "07:00"}, "mentioned")

	e := core.Event{Entity: "comment", Action: "create", Proposition: 3,
		Actor: core.Actor{Kind: core.KindUser, ID: ada, Name: "Ada Lovelace"},
		After: raw(t, map[string]any{"id": 9, "card_id": 7, "body_md": "look at this @grace"})}
	if err := f.s.Handle(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	got := f.outbox(t)
	if len(got) != 2 {
		t.Fatalf("wrote %d rows, want one per channel: a mention is moved, not copied", len(got))
	}
	soon := 0
	perChannel := map[int64]int{}
	for _, r := range got {
		perChannel[r.ChannelID]++
		if r.NextAt <= night {
			soon++
		}
	}
	if soon != 1 {
		t.Fatalf("%d rows go out now, want exactly one: a mention always reaches one channel", soon)
	}
	for id, n := range perChannel {
		if n != 1 {
			t.Errorf("channel %d has %d rows, want 1: it must not arrive now and again later", id, n)
		}
	}
}

// Quiet hours hold a burst back; they do not turn it into a burst that all
// arrives at seven in the morning.
func TestABurstHeldByQuietHoursStillCollapses(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	night := time.Date(2026, 9, 8, 23, 30, 0, 0, time.UTC).Unix()
	f.s.Now = func() int64 { return night }
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"},
		QuietFrom: "23:00", QuietTo: "07:00"}, "moved")

	for range 5 {
		if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
			t.Fatal(err)
		}
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("five moves inside quiet hours wrote %d rows, want 1", len(got))
	}
	if len(got[0].Payload.Items) != 5 {
		t.Errorf("row carries %d lines, want 5", len(got[0].Payload.Items))
	}
	want := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC).Unix()
	if got[0].NextAt != want {
		t.Errorf("next_at = %d, want %d", got[0].NextAt, want)
	}
}

func TestAMentionReachesAnAccountWithNoRuleTicked(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}})

	e := core.Event{Entity: "comment", Action: "create", Proposition: 3,
		Actor: core.Actor{Kind: core.KindUser, ID: ada, Name: "Ada Lovelace"},
		After: raw(t, map[string]any{"id": 9, "card_id": 7, "body_md": "@grace"})}
	if err := f.s.Handle(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 || got[0].NextAt > now {
		t.Fatalf("outbox = %+v, want one row going out now", got)
	}
}

func TestDigestHoldsEverythingUntilTheMorning(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindEmail, Digest: true}, "moved", "done")

	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	done := f.move(t, ada, 7)
	done.Action = "done"
	if err := f.s.Handle(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("a day of a digest channel wrote %d rows, want 1", len(got))
	}
	if len(got[0].Payload.Items) != 2 {
		t.Errorf("digest carries %d lines, want 2", len(got[0].Payload.Items))
	}
	if got[0].Payload.Event != digestEvent {
		t.Errorf("event = %q, want %q", got[0].Payload.Event, digestEvent)
	}
	want := time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC).Unix()
	if got[0].NextAt != want {
		t.Errorf("next_at = %d, want %d (08:00 tomorrow)", got[0].NextAt, want)
	}
}

func TestWorkspaceWebhookFiresOnTheActorsOwnChange(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	f.onCard(t, 7, ada)
	f.channel(t, Channel{Kind: KindWebhook, Config: Config{
		URL: "https://example.com/hook", Events: []string{"moved"}}})

	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("wrote %d rows, want 1: a workspace webhook fires whoever did it", len(got))
	}
	if got[0].NextAt != now {
		t.Errorf("next_at = %d, want now: a workspace webhook has no quiet hours", got[0].NextAt)
	}
}

func TestWorkspaceWebhookCanWaitForOneColumn(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	f.onCard(t, 7, ada)
	f.channel(t, Channel{Kind: KindWebhook, Config: Config{
		URL: "https://example.com/hook", Events: []string{"moved"}, Column: "Publication"}})

	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Fatalf("fired on a move into Research, which is not Publication")
	}
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE columns SET name = 'Publication' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Handle(context.Background(), f.move(t, ada, 7)); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 1 {
		t.Fatalf("wrote %d rows after the card reached Publication, want 1", len(got))
	}
}

func TestQuietUntil(t *testing.T) {
	loc := time.UTC
	at := func(h, m int) int64 { return time.Date(2026, 9, 8, h, m, 0, 0, loc).Unix() }
	tests := []struct {
		name     string
		from, to string
		now      int64
		want     int64
	}{
		{"inside a window that crosses midnight, before it", "23:00", "07:00", at(23, 30),
			time.Date(2026, 9, 9, 7, 0, 0, 0, loc).Unix()},
		{"inside a window that crosses midnight, after it", "23:00", "07:00", at(3, 0), at(7, 0)},
		{"outside a window that crosses midnight", "23:00", "07:00", at(12, 0), 0},
		{"on the minute the window opens", "23:00", "07:00", at(23, 0),
			time.Date(2026, 9, 9, 7, 0, 0, 0, loc).Unix()},
		{"on the minute the window closes", "23:00", "07:00", at(7, 0), 0},
		{"inside a window inside one day", "09:00", "17:00", at(12, 0), at(17, 0)},
		{"outside a window inside one day", "09:00", "17:00", at(8, 0), 0},
		{"no window at all", "", "", at(3, 0), 0},
		{"a window of no length silences nothing", "23:00", "23:00", at(23, 0), 0},
		{"a window that is not a time silences nothing", "twenty three", "07:00", at(23, 30), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quietUntil(tt.now, loc, tt.from, tt.to); got != tt.want {
				t.Errorf("quietUntil = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNextDigest(t *testing.T) {
	loc := time.UTC
	morning := time.Date(2026, 9, 8, 7, 0, 0, 0, loc).Unix()
	if got, want := nextDigest(morning, loc, "08:00"),
		time.Date(2026, 9, 8, 8, 0, 0, 0, loc).Unix(); got != want {
		t.Errorf("before the hour = %d, want %d (today)", got, want)
	}
	evening := time.Date(2026, 9, 8, 20, 0, 0, 0, loc).Unix()
	if got, want := nextDigest(evening, loc, "08:00"),
		time.Date(2026, 9, 9, 8, 0, 0, 0, loc).Unix(); got != want {
		t.Errorf("after the hour = %d, want %d (tomorrow)", got, want)
	}
	if got, want := nextDigest(morning, loc, "not a time"),
		time.Date(2026, 9, 8, 8, 0, 0, 0, loc).Unix(); got != want {
		t.Errorf("an unreadable digest time = %d, want eight in the morning %d", got, want)
	}
}

func TestCheckWindow(t *testing.T) {
	for _, tt := range []struct {
		from, to string
		ok       bool
	}{
		{"", "", true},
		{"23:00", "07:00", true},
		{"23:00", "", false},
		{"", "07:00", false},
		{"25:00", "07:00", false},
		{"23:60", "07:00", false},
		{"2300", "07:00", false},
	} {
		if err := checkWindow(tt.from, tt.to); (err == nil) != tt.ok {
			t.Errorf("checkWindow(%q, %q) = %v", tt.from, tt.to, err)
		}
	}
}
