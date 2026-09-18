package core

import (
	"context"
	"database/sql"
	"testing"

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
