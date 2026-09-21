// Package notify turns applied commands into notifications. It subscribes to
// the bus core publishes on, matches every event against every account's rules,
// resolves who the event is for, collapses bursts, writes one outbox row per
// channel, and delivers them with bounded retries the way the mail outbox does.
package notify

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

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

// Watch treats bus events as wakeups; only committed activity determines work.
func (s *Service) Watch(ctx context.Context, bus *core.Bus) {
	sub := bus.Subscribe(0)
	defer sub.Close()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		n, err := s.CatchUp(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("notification recovery", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			continue
		}
		if n == 100 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sub.C:
			if !ok {
				return
			}
		case <-tick.C:
		}
	}
}

// CatchUp commits each event's notifications with its cursor. The database
// owns the cursor so replacing it during restore also resets this consumer.
func (s *Service) CatchUp(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for ; n < 100; n++ {
		var e core.Event
		var before, after string
		err = tx.QueryRowContext(ctx, `SELECT a.id, coalesce(a.proposition_id, 0), a.actor_kind,
   CAST(a.actor_id AS INTEGER), coalesce(u.name, ''), coalesce(a.via, ''),
   a.entity, CAST(a.entity_id AS INTEGER), a.action, coalesce(a.before_json, ''),
   coalesce(a.after_json, ''), a.created_at
   FROM activity a LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
   WHERE a.id > (SELECT activity_id FROM notification_cursor WHERE id = 1)
   ORDER BY a.id LIMIT 1`).Scan(&e.Seq, &e.Proposition, &e.Actor.Kind, &e.Actor.ID,
			&e.Actor.Name, &e.Actor.Via, &e.Entity, &e.EntityID, &e.Action, &before, &after, &e.At)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return 0, err
		}
		e.Before, e.After = []byte(before), []byte(after)
		if err := s.queueTx(ctx, tx, e.Actor, Match(e)); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notification_cursor SET activity_id = ? WHERE id = 1`, e.Seq); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.Nudge()
	}
	return n, nil
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
