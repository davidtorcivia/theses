package notify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
)

// logs replaces the service's logger with one a test can read back.
func (f *fixture) logs() *bytes.Buffer {
	var buf bytes.Buffer
	f.s.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// An emitter faster than the matcher loses events, because the bus never
// blocks. It has to say so when it happens, not once at shutdown.
func TestMissedEventsAreReportedAsTheyHappen(t *testing.T) {
	f := newFixture(t)
	buf := f.logs()
	bus := core.NewBus()
	sub := bus.Subscribe(0)
	defer sub.Close()

	// Nothing is reading, so everything past the buffer is thrown away.
	for i := range 200 {
		bus.Publish(core.Event{Seq: int64(i), Entity: "card", Action: "move"})
	}
	if sub.Dropped() == 0 {
		t.Fatal("the bus buffered two hundred events, so this tests nothing")
	}

	seen := int64(0)
	f.s.missed(sub, &seen)
	if !strings.Contains(buf.String(), "notifications missed events") {
		t.Fatalf("nothing was logged: %q", buf.String())
	}
	if seen != sub.Dropped() {
		t.Errorf("seen = %d, want %d", seen, sub.Dropped())
	}

	// And not again for the same drops.
	buf.Reset()
	f.s.missed(sub, &seen)
	if buf.Len() != 0 {
		t.Errorf("the same drops were reported twice: %q", buf.String())
	}
}

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
