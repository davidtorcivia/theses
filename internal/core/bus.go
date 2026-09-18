package core

import "sync"

// busBuffer is how far behind a subscriber may fall. A tab that has stopped
// reading is a tab whose socket is already dead; the realtime hub notices the
// drop and tells it to reload from the activity table rather than pretending
// nothing was missed.
const busBuffer = 64

// Bus is the in-process fan-out of applied events. Subscribers take a
// proposition, or zero for every proposition, which is what a notifier wants.
// Sends never block: a subscriber that is not reading loses the event and has
// its drop counted.
type Bus struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

type Subscription struct {
	C           <-chan Event
	ch          chan Event
	bus         *Bus
	proposition int64
	dropped     int64
}

func NewBus() *Bus { return &Bus{subs: map[*Subscription]struct{}{}} }

// Subscribe returns a subscription to one proposition, or to all of them when
// proposition is zero. Close it when done or the bus keeps sending to it.
func (b *Bus) Subscribe(proposition int64) *Subscription {
	ch := make(chan Event, busBuffer)
	s := &Subscription{C: ch, ch: ch, bus: b, proposition: proposition}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if _, ok := s.bus.subs[s]; ok {
		delete(s.bus.subs, s)
		close(s.ch)
	}
}

// Dropped is how many events this subscription was too slow to take.
func (s *Subscription) Dropped() int64 {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.dropped
}

// Publish fans an event out to every interested subscriber. It holds the lock
// over the sends, which is safe because no send can block.
func (b *Bus) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if s.proposition != 0 && s.proposition != e.Proposition {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.dropped++
		}
	}
}
