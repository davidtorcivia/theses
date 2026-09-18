// Package notify turns applied commands into notifications. It subscribes to
// the bus core publishes on, matches every event against every account's rules,
// resolves who the event is for, collapses bursts, writes one outbox row per
// channel, and delivers them with bounded retries the way the mail outbox does.
package notify

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/mail"
	"github.com/davidtorcivia/theses/internal/settings"
	"github.com/davidtorcivia/theses/internal/store"
)

// An Event is one row of the matrix on the profile page: what happened, how it
// reads there, and what it turns into on a channel that takes a priority and
// tags. Everything else reads this table, so the matcher, the page, the
// defaults and the API cannot drift apart.
type Event struct {
	Key   string
	Label string
	// Priority is channel.Note's: -1 low, 0 normal, 1 high.
	Priority int
	Tags     []string
	// Collapse is true for the events that arrive in bursts, where five in a
	// minute are worth one line rather than five messages.
	Collapse bool
}

var Events = []Event{
	{Key: "assigned", Label: "Assigned to a card", Priority: 1, Tags: []string{"inbox_tray"}},
	{Key: "unassigned", Label: "Unassigned from a card", Tags: []string{"outbox_tray"}},
	{Key: "mentioned", Label: "Mentioned in a note, a description or a document", Priority: 1, Tags: []string{"speech_balloon"}},
	{Key: "note", Label: "A note on a card I am on", Tags: []string{"memo"}},
	{Key: "due", Label: "A card I am on is due tomorrow", Priority: 1, Tags: []string{"alarm_clock"}},
	{Key: "overdue", Label: "A card I am on is overdue", Priority: 1, Tags: []string{"rotating_light"}},
	{Key: "moved", Label: "A card I am on was moved", Priority: -1, Tags: []string{"arrow_right"}, Collapse: true},
	{Key: "done", Label: "A card I am on was marked done", Tags: []string{"white_check_mark"}, Collapse: true},
	{Key: "block", Label: "A document block I wrote was changed", Tags: []string{"pencil2"}, Collapse: true},
	{Key: "status", Label: "A proposition I am on changed status", Tags: []string{"label"}},
	{Key: "file", Label: "A file landed in a proposition I am on", Tags: []string{"package"}, Collapse: true},
	{Key: "release", Label: "Release day tomorrow", Priority: 1, Tags: []string{"calendar"}},
}

// digestEvent is the key an outbox row carries once it has been put off until
// the digest. It is not a rule: nobody subscribes to it, and the matrix has no
// row for it.
const digestEvent = "digest"

var eventByKey = func() map[string]Event {
	m := make(map[string]Event, len(Events))
	for _, e := range Events {
		m[e.Key] = e
	}
	return m
}()

// LookupEvent returns the event a rule key names.
func LookupEvent(key string) (Event, bool) {
	e, ok := eventByKey[key]
	return e, ok
}

// Service is the notifier: the subscriber that fills the outbox, the worker
// that empties it and the daily tick that produces the events no command does.
type Service struct {
	db  *store.DB
	set *settings.Settings
	log *slog.Logger
	// mail builds the SMTP client from the settings as they stand, so a change
	// on the settings page reaches the next batch without a restart.
	mail *mail.Outbox
	// baseURL is what a deep link is built on.
	baseURL string
	// Now is the clock, replaced in tests.
	Now func() int64

	nudge chan struct{}
}

func New(db *store.DB, set *settings.Settings, log *slog.Logger, baseURL string) *Service {
	return &Service{
		db: db, set: set, log: log,
		mail:    mail.NewOutbox(db, set, log),
		baseURL: strings.TrimSuffix(baseURL, "/"),
		Now:     nowUnix,
		nudge:   make(chan struct{}, 1),
	}
}

// Watch fills the outbox from the bus until ctx is canceled. Nothing here
// talks to the network: it reads and writes the database and hands the sending
// to the worker, so an unreachable ntfy server cannot back the bus up.
//
// ponytail: the bus is in process, so events applied between a commit and a
// crash are never matched. The upgrade is a cursor over the activity table
// replayed by seq on start.
func (s *Service) Watch(ctx context.Context, bus *core.Bus) {
	sub := bus.Subscribe(0)
	defer sub.Close()
	dropped := int64(0)
	for {
		select {
		case <-ctx.Done():
			s.missed(sub, &dropped)
			return
		case e, ok := <-sub.C:
			if !ok {
				return
			}
			if err := s.Handle(ctx, e); err != nil && ctx.Err() == nil {
				s.log.Error("notify", "entity", e.Entity, "action", e.Action, "err", err)
			}
			s.missed(sub, &dropped)
		}
	}
}

// missed says so when the bus has thrown events away since the last look.
//
// The bus never blocks, so an emitter faster than this loop loses events rather
// than waiting for it. Read only at shutdown, that is a gap nobody sees until
// somebody asks why a notification never arrived.
func (s *Service) missed(sub *core.Subscription, seen *int64) {
	n := sub.Dropped()
	if n <= *seen {
		return
	}
	s.log.Warn("notifications missed events", "dropped", n-*seen, "total", n)
	*seen = n
}

// Handle matches one event and queues whatever it produces.
func (s *Service) Handle(ctx context.Context, e core.Event) error {
	matches := Match(e)
	if len(matches) == 0 {
		return nil
	}
	return s.queue(ctx, e.Actor, matches)
}

// link is the deep link a notification carries.
//
// ponytail: a card has no URL of its own yet, so everything points at the board
// it is on. When the board takes a card in its path this returns that instead.
func (s *Service) link(proposition int64) string {
	if proposition == 0 {
		return s.baseURL + "/"
	}
	return s.baseURL + "/p/" + strconv.FormatInt(proposition, 10)
}
