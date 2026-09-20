package core

import (
	"context"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/store"
)

// logRow is one activity row a compaction test starts from. ago is how long
// before the service's clock it was written, which is what decides whether the
// run it belongs to is old enough to fold.
type logRow struct {
	entity, entityID, action, actor, via string
	ago                                  time.Duration
	before, after                        string
	undone                               bool
}

// kept is one row a compaction test expects to find afterwards.
type kept struct {
	id            int64
	before, after string
}

func TestCompactFoldsRunsOfTypedSaves(t *testing.T) {
	setOn := func(block, actor string, ago time.Duration, before, after string) logRow {
		return logRow{entity: "block", entityID: block, action: "set",
			actor: actor, ago: ago, before: before, after: after}
	}
	set := func(actor string, ago time.Duration, before, after string) logRow {
		return setOn("7", actor, ago, before, after)
	}
	const day = 24 * time.Hour

	for _, tc := range []struct {
		name string
		rows []logRow
		want []kept
	}{
		{
			name: "a run of five keeps the last row with the first before",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				set("1", 3*day-30*time.Second, "t1", "t2"),
				set("1", 3*day-60*time.Second, "t2", "t3"),
				set("1", 3*day-90*time.Second, "t3", "t4"),
				set("1", 3*day-120*time.Second, "t4", "t5"),
			},
			want: []kept{{5, "t0", "t5"}},
		},
		{
			name: "each actor's saves are a run of their own",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				set("1", 3*day-30*time.Second, "t1", "t2"),
				set("2", 3*day-60*time.Second, "t2", "t3"),
				set("2", 3*day-90*time.Second, "t3", "t4"),
			},
			want: []kept{{2, "t0", "t2"}, {4, "t2", "t4"}},
		},
		{
			name: "two people typing turn about leave nothing to fold",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				set("2", 3*day-30*time.Second, "t1", "t2"),
				set("1", 3*day-60*time.Second, "t2", "t3"),
				set("2", 3*day-90*time.Second, "t3", "t4"),
			},
			want: []kept{{1, "t0", "t1"}, {2, "t1", "t2"}, {3, "t2", "t3"}, {4, "t3", "t4"}},
		},
		{
			name: "the same person through a token is a different actor",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				set("1", 3*day-30*time.Second, "t1", "t2"),
				{entity: "block", entityID: "7", action: "set", actor: "1", via: "token:agent",
					ago: 3*day - 60*time.Second, before: "t2", after: "t3"},
				{entity: "block", entityID: "7", action: "set", actor: "1", via: "token:agent",
					ago: 3*day - 90*time.Second, before: "t3", after: "t4"},
			},
			want: []kept{{2, "t0", "t2"}, {4, "t2", "t4"}},
		},
		{
			name: "another change to the same block ends the run",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				set("1", 3*day-30*time.Second, "t1", "t2"),
				{entity: "block", entityID: "7", action: "move", actor: "1",
					ago: 3*day - 60*time.Second, before: "m0", after: "m1"},
				set("1", 3*day-90*time.Second, "t2", "t3"),
				set("1", 3*day-120*time.Second, "t3", "t4"),
			},
			want: []kept{{2, "t0", "t2"}, {3, "m0", "m1"}, {5, "t2", "t4"}},
		},
		{
			name: "a save of another block does not end the run",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				{entity: "block", entityID: "8", action: "set", actor: "1",
					ago: 3*day - 15*time.Second, before: "o0", after: "o1"},
				set("1", 3*day-30*time.Second, "t1", "t2"),
			},
			want: []kept{{2, "o0", "o1"}, {3, "t0", "t2"}},
		},
		{
			name: "a pause longer than ten minutes ends the run",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				set("1", 3*day-11*time.Minute, "t1", "t2"),
				set("1", 3*day-11*time.Minute-30*time.Second, "t2", "t3"),
			},
			want: []kept{{1, "t0", "t1"}, {3, "t1", "t3"}},
		},
		// The two edges, each with the block a second the other side of it, so
		// that moving the comparison either way fails the case rather than
		// quietly widening or narrowing what is folded.
		{
			name: "a gap of exactly ten minutes joins a run and a second more breaks it",
			rows: []logRow{
				setOn("7", "1", 3*day, "a0", "a1"),
				setOn("7", "1", 3*day-compactGap, "a1", "a2"),
				setOn("8", "1", 3*day, "b0", "b1"),
				setOn("8", "1", 3*day-compactGap-time.Second, "b1", "b2"),
			},
			want: []kept{{2, "a0", "a2"}, {3, "b0", "b1"}, {4, "b1", "b2"}},
		},
		{
			name: "a run reaching the cutoff is left whole and one a second older folds",
			rows: []logRow{
				setOn("7", "1", CompactAfter+30*time.Second, "a0", "a1"),
				setOn("7", "1", CompactAfter, "a1", "a2"),
				setOn("8", "1", CompactAfter+31*time.Second, "b0", "b1"),
				setOn("8", "1", CompactAfter+time.Second, "b1", "b2"),
			},
			want: []kept{{1, "a0", "a1"}, {2, "a1", "a2"}, {4, "b0", "b2"}},
		},
		{
			name: "a run of one is left where it is",
			rows: []logRow{set("1", 3*day, "t0", "t1")},
			want: []kept{{1, "t0", "t1"}},
		},
		{
			name: "rows younger than the window are untouched",
			rows: []logRow{
				set("1", time.Hour, "t0", "t1"),
				set("1", time.Hour-30*time.Second, "t1", "t2"),
				set("1", time.Hour-60*time.Second, "t2", "t3"),
			},
			want: []kept{{1, "t0", "t1"}, {2, "t1", "t2"}, {3, "t2", "t3"}},
		},
		{
			name: "a run straddling the window is left whole until all of it is old",
			rows: []logRow{
				set("1", 2*day+5*time.Minute, "t0", "t1"),
				set("1", 2*day+time.Minute, "t1", "t2"),
				set("1", 2*day-3*time.Minute, "t2", "t3"),
			},
			want: []kept{{1, "t0", "t1"}, {2, "t1", "t2"}, {3, "t2", "t3"}},
		},
		{
			name: "an undone save is kept and ends the run either side of it",
			rows: []logRow{
				set("1", 3*day, "t0", "t1"),
				{entity: "block", entityID: "7", action: "set", actor: "1", undone: true,
					ago: 3*day - 30*time.Second, before: "t1", after: "t2"},
				set("1", 3*day-60*time.Second, "t2", "t3"),
				set("1", 3*day-90*time.Second, "t3", "t4"),
			},
			want: []kept{{1, "t0", "t1"}, {2, "t1", "t2"}, {4, "t2", "t4"}},
		},
		{
			name: "other entities and other actions are never folded",
			rows: []logRow{
				{entity: "card", entityID: "3", action: "set", actor: "1", ago: 3 * day, before: "c0", after: "c1"},
				{entity: "card", entityID: "3", action: "set", actor: "1", ago: 3*day - 30*time.Second, before: "c1", after: "c2"},
				{entity: "block", entityID: "7", action: "insert", actor: "1", ago: 3*day - 60*time.Second, before: "", after: "b1"},
				{entity: "block", entityID: "7", action: "insert", actor: "1", ago: 3*day - 90*time.Second, before: "", after: "b2"},
			},
			want: []kept{{1, "c0", "c1"}, {2, "c1", "c2"}, {3, "", "b1"}, {4, "", "b2"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1_700_000_000, 0)
			s := New(store.OpenTemp(t), NewBus())
			s.Now = func() time.Time { return now }
			for _, r := range tc.rows {
				writeLogRow(t, s, now, r)
			}

			removed, err := s.Compact(ctx, CompactAfter)
			if err != nil {
				t.Fatal(err)
			}
			if want := len(tc.rows) - len(tc.want); removed != want {
				t.Errorf("Compact removed %d rows, want %d", removed, want)
			}
			assertLog(t, s, tc.want)

			// A second pass has nothing left to fold, which is what makes this
			// safe to call on a timer.
			again, err := s.Compact(ctx, CompactAfter)
			if err != nil {
				t.Fatal(err)
			}
			if again != 0 {
				t.Errorf("a second pass removed %d rows", again)
			}
			assertLog(t, s, tc.want)
		})
	}
}

func writeLogRow(t *testing.T, s *Service, now time.Time, r logRow) {
	t.Helper()
	null := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	var undone any
	if r.undone {
		undone = now.Unix()
	}
	if _, err := s.DB.ExecContext(context.Background(), `INSERT INTO activity
		(actor_kind, actor_id, via, entity, entity_id, action, before_json, after_json, created_at, undone_at)
		VALUES ('user', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.actor, null(r.via), r.entity, r.entityID, r.action,
		null(r.before), null(r.after), now.Add(-r.ago).Unix(), undone); err != nil {
		t.Fatal(err)
	}
}

func assertLog(t *testing.T, s *Service, want []kept) {
	t.Helper()
	rows, err := s.DB.QueryContext(context.Background(),
		`SELECT id, coalesce(before_json, ''), coalesce(after_json, '') FROM activity ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []kept
	for rows.Next() {
		var k kept
		if err := rows.Scan(&k.id, &k.before, &k.after); err != nil {
			t.Fatal(err)
		}
		got = append(got, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("the log holds %v, want %v", got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("row %d is %v, want %v", i, got[i], k)
		}
	}
}
