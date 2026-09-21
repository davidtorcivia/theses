package notify

import (
	"context"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/settings"
)

func TestParseDate(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 9, 8, 14, 0, 0, 0, loc)
	tests := []struct {
		value string
		want  string // the date, or "" for one that cannot be read
	}{
		{"2026-09-28", "2026-09-28"},
		{"28 Sep 2026", "2026-09-28"},
		{"28 September 2026", "2026-09-28"},
		{"Sep 28 2026", "2026-09-28"},
		{"28 Sep", "2026-09-28"}, // no year is this year, as the board assumes
		{"next Tuesday", ""},
		{"", ""},
		{"soon", ""},
	}
	for _, tt := range tests {
		got, ok := parseDate(tt.value, at, loc)
		if !ok {
			if tt.want != "" {
				t.Errorf("parseDate(%q) could not be read, want %s", tt.value, tt.want)
			}
			continue
		}
		if tt.want == "" {
			t.Errorf("parseDate(%q) = %s, want nothing", tt.value, got.Format("2006-01-02"))
			continue
		}
		if got.Format("2006-01-02") != tt.want {
			t.Errorf("parseDate(%q) = %s, want %s", tt.value, got.Format("2006-01-02"), tt.want)
		}
	}
}

// due puts a date on the card and a target on the proposition.
func (f *fixture) dates(t *testing.T, due, target string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE cards SET due_date = ? WHERE id = 7`, due); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE propositions SET target_date = ? WHERE id = 3`, target); err != nil {
		t.Fatal(err)
	}
}

func TestTickFiresDatesOnceADay(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}},
		"due", "overdue", "release")

	day := time.Unix(now, 0).UTC()
	f.dates(t, day.AddDate(0, 0, 1).Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02"))

	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 2 {
		t.Fatalf("wrote %d rows, want one due and one release: %+v", len(got), got)
	}
	events := map[string]bool{got[0].Event: true, got[1].Event: true}
	if !events["due"] || !events["release"] {
		t.Errorf("events = %v", events)
	}

	// Again the same day is nothing: the pass remembers the date it ran on.
	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 2 {
		t.Fatalf("the pass ran twice on one day and wrote %d rows", len(got))
	}
}

func TestTickFiresOverdueTheMorningAfterAndNotAgain(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "due", "overdue")

	day := time.Unix(now, 0).UTC()
	f.dates(t, day.AddDate(0, 0, -1).Format("2006-01-02"), "")
	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 || got[0].Event != "overdue" {
		t.Fatalf("outbox = %+v, want one overdue", got)
	}

	// A week late is not news every morning.
	f.s.Now = func() int64 { return now + 7*24*60*60 }
	if err := f.set.SetAs(context.Background(), "notify.last_tick", []string{"0"}, settings.System()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 1 {
		t.Fatalf("a card that has been late for a week fired again: %+v", got)
	}
}

func TestTickWaitsForTheDigestHour(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "due")
	day := time.Unix(now, 0).UTC()
	f.dates(t, day.AddDate(0, 0, 1).Format("2006-01-02"), "")

	f.s.Now = func() int64 { return time.Date(2026, 9, 8, 6, 0, 0, 0, time.UTC).Unix() }
	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Fatalf("the pass ran before the digest hour")
	}
	if settings.Get[int](f.set, "notify.last_tick") != 0 {
		t.Error("the pass recorded a day it did not run")
	}
}

// The pass runs a few minutes before the digest, so that a digest channel gets
// "due tomorrow" in the digest of the day it is sent rather than a day late.
func TestTickCatchesTheSameDaysDigest(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindEmail, Digest: true}, "due")
	day := time.Unix(now, 0).UTC()
	f.dates(t, day.AddDate(0, 0, 1).Format("2006-01-02"), "")

	f.s.Now = func() int64 { return time.Date(2026, 9, 8, 7, 56, 0, 0, time.UTC).Unix() }
	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.outbox(t)
	if len(got) != 1 {
		t.Fatalf("wrote %d rows, want one due", len(got))
	}
	want := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC).Unix()
	if got[0].NextAt != want {
		t.Errorf("next_at = %d, want %d: today's digest, not tomorrow's", got[0].NextAt, want)
	}
}

func TestTickSkipsDoneCards(t *testing.T) {
	f := newFixture(t)
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "due")
	day := time.Unix(now, 0).UTC()
	f.dates(t, day.AddDate(0, 0, 1).Format("2006-01-02"), "")
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE cards SET done_at = unixepoch() WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Fatalf("a card that is done was called due: %+v", got)
	}
}
