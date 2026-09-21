package core

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/store"
)

func TestBusFansOutPerProposition(t *testing.T) {
	bus := NewBus()
	one := bus.Subscribe(1)
	two := bus.Subscribe(2)
	all := bus.Subscribe(0)
	defer one.Close()
	defer two.Close()
	defer all.Close()

	bus.Publish(Event{Seq: 7, Proposition: 1, Entity: "card", Action: "move"})

	if e := <-one.C; e.Seq != 7 {
		t.Errorf("subscriber on 1 got seq %d, want 7", e.Seq)
	}
	if e := <-all.C; e.Seq != 7 {
		t.Errorf("subscriber on every proposition got seq %d, want 7", e.Seq)
	}
	select {
	case e := <-two.C:
		t.Errorf("subscriber on 2 got an event for 1: %+v", e)
	default:
	}
}

func TestConcurrentCommandsPublishInCommitOrder(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	bus := NewBus()
	sub := bus.Subscribe(0)
	defer sub.Close()
	s := New(db, bus)
	id, err := store.CreateUser(ctx, db, &store.User{
		Handle: "ada", Email: "ada@example.com", Name: "Ada Lovelace",
		Initials: "AL", Colour: "#111", Role: auth.RoleOwner, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	who := Actor{Kind: KindUser, ID: id, Name: "Ada Lovelace"}

	const commands = 32
	start := make(chan struct{})
	errs := make(chan error, commands)
	var wg sync.WaitGroup
	for range commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Do(ctx, who, 0, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (Change, error) {
				return Change{Entity: "test", Action: "set", After: map[string]any{"set": true}}, nil
			})
			errs <- err
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent commands did not finish")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var previous int64
	for range commands {
		var e Event
		select {
		case e = <-sub.C:
		case <-time.After(2 * time.Second):
			t.Fatal("committed event was not published")
		}
		if e.Seq <= previous {
			t.Fatalf("event %d was published after %d", e.Seq, previous)
		}
		previous = e.Seq
	}
}

// A subscriber that has stopped reading loses events rather than stopping
// everybody else's, which is the whole reason the sends are non-blocking.
func TestBusDropsASlowSubscriber(t *testing.T) {
	bus := NewBus()
	slow := bus.Subscribe(1)
	quick := bus.Subscribe(1)
	defer slow.Close()
	defer quick.Close()

	for i := 0; i < busBuffer+5; i++ {
		bus.Publish(Event{Seq: int64(i), Proposition: 1})
		<-quick.C
	}
	if got := slow.Dropped(); got != 5 {
		t.Errorf("dropped %d events, want 5", got)
	}
	if len(slow.ch) != busBuffer {
		t.Errorf("slow subscriber holds %d events, want %d", len(slow.ch), busBuffer)
	}
}

// An actor that is neither a person nor the file watcher has no role to look
// up, and neither has a person who is no longer an account.
func TestActorsWithoutARoleAreRefused(t *testing.T) {
	db := store.OpenTemp(t)
	s := New(db, NewBus())
	never := func(context.Context, *sql.Tx) (Change, error) {
		t.Error("the command ran for an actor that should have been refused")
		return Change{}, nil
	}
	for _, a := range []Actor{{Kind: "token", ID: 1}, {Kind: KindUser, ID: 404}} {
		if _, err := s.Do(context.Background(), a, 0, auth.CanEdit, never); err != ErrForbidden {
			t.Errorf("actor %+v got %v, want ErrForbidden", a, err)
		}
	}
}

// Together is one transaction behind several commands: a refusal partway
// through leaves none of them applied, and nothing is published either,
// because until the transaction commits nothing has happened.
func TestTogetherRollsBackAndPublishesOnlyOnCommit(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	bus := NewBus()
	sub := bus.Subscribe(0)
	defer sub.Close()
	s := New(db, bus)

	id, err := store.CreateUser(ctx, db, &store.User{
		Handle: "grace", Email: "grace@example.com", Name: "Grace Hopper",
		Initials: "GH", Colour: "#111", Role: auth.RoleOwner, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	who := Actor{Kind: KindUser, ID: id, Name: "Grace Hopper"}
	write := func(ctx context.Context, name string) error {
		_, err := s.Do(ctx, who, 0, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (Change, error) {
			_, err := tx.ExecContext(ctx, `INSERT INTO propositions
				(number, title, status, position, created_at) VALUES (?, ?, 'idea', 'V', 0)`,
				len(name), name)
			return Change{Entity: "proposition", Action: "create", After: map[string]any{"title": name}}, err
		})
		return err
	}

	refused := errors.New("no")
	err = s.Together(ctx, func(ctx context.Context) error {
		if err := write(ctx, "one"); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Together returned %v", err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM propositions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d propositions left behind by a refused run", rows)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM activity`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d activity rows left behind by a refused run", rows)
	}
	select {
	case e := <-sub.C:
		t.Errorf("a refused run published %+v", e)
	default:
	}

	// A run that gets through commits once and publishes what it did.
	if err := s.Together(ctx, func(ctx context.Context) error {
		if err := write(ctx, "two"); err != nil {
			return err
		}
		return write(ctx, "three")
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM propositions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("%d propositions after a run that got through, want 2", rows)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-sub.C:
		default:
			t.Errorf("only %d of 2 events were published", i)
		}
	}
}

func TestPublicActorOnlySignsReleases(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTemp(t)
	s := New(db, NewBus())
	_, err := db.ExecContext(ctx, `INSERT INTO propositions(id,number,title,status,position,created_at) VALUES(1,1,'Episode','idea','a',1)`)
	if err != nil {
		t.Fatal(err)
	}
	a := Actor{Kind: KindPublic, Name: "Public participant"}
	cases := []struct {
		need    string
		prop    int64
		change  Change
		allowed bool
	}{
		{SignRelease, 1, Change{Entity: "legal_submission", Action: "sign"}, true},
		{auth.CanEdit, 1, Change{Entity: "legal_submission", Action: "sign"}, false},
		{SignRelease, 0, Change{Entity: "legal_submission", Action: "sign"}, false},
		{SignRelease, 1, Change{Entity: "card", Action: "sign"}, false},
		{SignRelease, 1, Change{Entity: "legal_submission", Action: "edit"}, false},
		{SignRelease, 1, Change{Entity: "legal_submission", Action: "sign", Detached: true}, false},
		{SignRelease, 1, Change{Entity: "legal_submission", Action: "sign", Proposition: 2}, false},
	}
	for _, c := range cases {
		_, err := s.Do(ctx, a, c.prop, c.need, func(context.Context, *sql.Tx) (Change, error) { return c.change, nil })
		if (err == nil) != c.allowed {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}
