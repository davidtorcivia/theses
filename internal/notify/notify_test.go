package notify

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
)

// Watch takes what the bus carries and stops when its context does. What it
// does with an event is Handle, which every other test here drives directly,
// so this only pins the loop around it: no deadline decides whether it passes.
func TestWatchStopsWithItsContext(t *testing.T) {
	f := newFixture(t)
	ada := f.user(t, "ada")
	grace := f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")

	bus := core.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.s.Watch(ctx, bus)
	}()

	bus.Publish(f.move(t, ada, 7))
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the watcher did not stop with its context")
	}
}

func TestRecoveryCommitsQueueAndCursorTogether(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	ada, grace := f.user(t, "ada"), f.user(t, "grace")
	f.onCard(t, 7, grace)
	f.channel(t, Channel{UserID: grace, Kind: KindNtfy, Config: Config{Topic: "t"}}, "moved")
	if _, err := f.db.ExecContext(ctx, `UPDATE notification_cursor SET activity_id = (SELECT max(id) FROM activity)`); err != nil {
		t.Fatal(err)
	}
	e := f.move(t, ada, 7)
	r, err := f.db.ExecContext(ctx, `INSERT INTO activity (proposition_id, actor_kind, actor_id, entity, entity_id, action, before_json, after_json, created_at) VALUES (3, 'user', ?, 'card', '7', 'move', ?, ?, ?)`, ada, string(e.Before), string(e.After), now)
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := r.LastInsertId()
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER fail_cursor BEFORE UPDATE ON notification_cursor BEGIN SELECT RAISE(ABORT, 'cursor failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CatchUp(ctx); err == nil {
		t.Fatal("cursor failure ignored")
	}
	if len(f.outbox(t)) != 0 {
		t.Fatal("failed cursor committed notification")
	}
	if _, err := f.db.ExecContext(ctx, `DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var consumed atomic.Int64
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() { n, err := f.s.CatchUp(ctx); consumed.Add(int64(n)); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if consumed.Load() != 1 || len(f.outbox(t)) != 1 {
		t.Fatalf("duplicate or lost work: consumed=%d queued=%d", consumed.Load(), len(f.outbox(t)))
	}
	var cursor int64
	if err := f.db.QueryRowContext(ctx, `SELECT activity_id FROM notification_cursor`).Scan(&cursor); err != nil || cursor != seq {
		t.Fatalf("cursor=%d: %v", cursor, err)
	}
	// Restoring an older cursor must be observed without restarting the service.
	if _, err := f.db.ExecContext(ctx, `UPDATE notification_cursor SET activity_id = ?`, seq-1); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.CatchUp(ctx); err != nil || n != 1 {
		t.Fatalf("restored cursor: %d %v", n, err)
	}
	// Cascades may delete the highest event. A new event must still exceed it.
	if _, err := f.db.ExecContext(ctx, `DELETE FROM propositions WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	r, err = f.db.ExecContext(ctx, `INSERT INTO activity (actor_kind,entity,action,created_at) VALUES ('system','setting','set',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := r.LastInsertId()
	if next <= seq {
		t.Fatalf("event ID reused: %d after %d", next, seq)
	}
	if n, err := f.s.CatchUp(ctx); err != nil || n != 1 {
		t.Fatalf("nonmatching event: %d %v", n, err)
	}
}

func TestNoticeLinksTargetTheChangedItem(t *testing.T) {
	s := &Service{baseURL: "https://example.com"}
	for _, tc := range []struct {
		n    Notice
		want string
	}{
		{Notice{Proposition: 7, Card: 12, Entity: "comment", EntityID: 20}, "https://example.com/p/7#card-12"},
		{Notice{Proposition: 7, Entity: "block", EntityID: 20}, "https://example.com/p/7#block-20"},
		{Notice{Proposition: 7, Entity: "file", EntityID: 9}, "https://example.com/p/7#file-9"},
		{Notice{Proposition: 7, Entity: "proposition", EntityID: 7}, "https://example.com/p/7"},
	} {
		if got := s.noticeLink(tc.n); got != tc.want {
			t.Errorf("%+v: %s", tc.n, got)
		}
	}
}
