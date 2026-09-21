// Package core is the one place a mutation happens. Every command, whoever
// calls it, opens one immediate transaction, checks the actor may do it,
// applies the change, writes the activity row, commits, and only then publishes
// the applied event. The browser, the websocket, the REST API and the MCP
// server are four callers of the same functions with a different actor.
package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/store"
)

// The two kinds of thing that can make a change. An API token and an MCP
// client are not among them: they are a way for a person to act, recorded in
// Via. Only the document mirror's watcher has no person behind it.
const (
	KindUser = "user"
	KindFile = "file"
)

// Actor is who is making the change. ID and Name are the person, which is what
// the board shows; Via is how they reached it, empty for a browser session,
// "token:<name>" for the API and "mcp:<client>" for MCP.
type Actor struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Via  string `json:"via,omitempty"`
}

// Event is one applied command. Seq is the activity row's id, so a client that
// has seen up to a sequence number can ask for the rest of the stream from the
// same table the panel reads.
type Event struct {
	Seq         int64           `json:"seq"`
	Proposition int64           `json:"proposition"`
	Entity      string          `json:"entity"`
	EntityID    int64           `json:"entity_id"`
	Action      string          `json:"action"`
	Actor       Actor           `json:"actor"`
	Before      json.RawMessage `json:"before,omitempty"`
	After       json.RawMessage `json:"after,omitempty"`
	At          int64           `json:"at"`
	// Replayed says this command arrived under a key the actor had already
	// spent, so nothing was applied and what follows is the event that key
	// produced the first time. A client that draws an event as it arrives must
	// not draw this one: the payload is the row as it was when it was applied,
	// which may be older than what the client holds now.
	Replayed bool `json:"replayed,omitempty"`
	// Key is the client's key this command was applied under, as it was spent,
	// so the second command of a run carries the numbered form. It is how a
	// client that drew a row before the server had one recognizes the real row
	// when it arrives, by whichever road it arrives on: the answer to its own
	// command, the broadcast to the room, or the stream it reads after being
	// away. Without it only a client's own answer could be matched, and a
	// second tab, or the same tab after a reload, would draw the thing twice.
	//
	// It goes to everybody in the room, because one event is one message. A key
	// is spent against the actor who chose it, so nobody else can replay a
	// command with it or collide with it; all it says is that the row was made
	// by a client that named the change.
	Key string `json:"key,omitempty"`
}

var (
	// ErrForbidden is every authorization refusal: the role, the membership and
	// the actor that no longer exists all come back as this, because telling a
	// caller which one it was tells it about rows it may not read.
	ErrForbidden = errors.New("not allowed")
	// ErrNotUndoable is an activity row whose before cannot safely be put back.
	ErrNotUndoable = errors.New("that change cannot be undone")
	// ErrNotFound is re-exported so callers need not import store for it.
	ErrNotFound = store.ErrNotFound
	// ErrKey is a client's key that is not one: it is the shape of a malformed
	// request rather than a refusal about anything in the workspace.
	ErrKey = errors.New("a key is 1 to 64 letters, digits, hyphens or underscores")
)

// ConflictError is what a versioned text field refuses a stale write with. It
// carries the value in the database so the editor can offer keep mine and take
// theirs without a second round trip.
type ConflictError struct {
	Entity   string `json:"entity"`
	EntityID int64  `json:"entity_id"`
	Field    string `json:"field"`
	Version  int64  `json:"version"`
	Current  string `json:"current"`
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s %d %s changed under you", e.Entity, e.EntityID, e.Field)
}

// Reader returns one row whole, by the name core knows it under. board sets
// it, because core must not know the board's types, and undo publishes what it
// returns: a tab replaces the row it holds with the payload of an event, so
// half a row would take the rest of it away.
type Reader func(ctx context.Context, q store.Querier, entity string, id int64) (any, error)

// Service holds the database and the bus every command publishes on.
type Service struct {
	Rejected atomic.Uint64
	DB       *store.DB
	Bus      *Bus
	// publishing keeps SQLite's commit order through delivery to subscribers.
	publishing sync.Mutex
	// Now is the clock, replaced in tests.
	Now func() time.Time
	// Read is how undo reads back the entity it restored.
	Read Reader
	// Allow is a rule core cannot know, asked inside the transaction before
	// anything is written. board uses it to refuse every change to an archived
	// proposition but the two that still mean something on one. Undo asks it;
	// board's own commands ask the same function directly.
	Allow func(ctx context.Context, tx *sql.Tx, proposition int64, entity, action string) error
}

func New(db *store.DB, bus *Bus) *Service {
	return &Service{DB: db, Bus: bus, Now: time.Now}
}

// Change is what a command did: the row it touched and the row as it was and
// as it now is. Before and After are marshaled into the activity row and the
// event, keyed by column name so undo can put them back without a translation
// table.
type Change struct {
	Entity   string
	EntityID int64
	Action   string
	Before   any
	After    any
	// Proposition names the proposition the change belongs to when that is not
	// the one the command was authorized against, which is how creating a
	// proposition files itself under the row it just made.
	Proposition int64
	// Detached writes the activity row with no proposition at all, which is
	// what a proposition delete needs: a row pointing at the proposition it
	// records the deletion of would cascade away with it.
	Detached bool
}

// Do is the shape of every mutation. apply runs inside an immediate
// transaction with the actor already authorized for need (one of auth.CanEdit
// or auth.CanDelete) on proposition, which is zero when the command creates the
// proposition itself.
func (s *Service) Do(ctx context.Context, a Actor, proposition int64, need string,
	apply func(context.Context, *sql.Tx) (Change, error)) (result Event, resultErr error) {
	defer func() {
		var conflict *ConflictError
		if errors.Is(resultErr, ErrForbidden) || errors.As(resultErr, &conflict) {
			s.Rejected.Add(1)
		}
	}()
	// Inside Together, every command shares the one transaction and neither
	// commits nor publishes: the outer call does both, once, if all of them
	// got through.
	group, joined := ctx.Value(groupKey{}).(*group)
	tx := group.tx()
	if !joined {
		// The DSN sets _txlock=immediate, so this takes the write lock now
		// rather than deadlocking later on an upgrade.
		opened, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return Event{}, err
		}
		defer opened.Rollback()
		tx = opened
	}

	if err := authorise(ctx, tx, a, proposition, need); err != nil {
		return Event{}, err
	}
	// The key is spent after the authorization and before anything is applied,
	// so somebody who may not write here is refused rather than told what
	// somebody else's key did, and so a command that has already happened does
	// not happen again.
	mine := spend(ctx, a)
	if mine != "" {
		e, found, err := replayed(ctx, tx, a.ID, mine)
		if err != nil {
			return Event{}, err
		}
		if found {
			// Keys are actor-wide, so reauthorize the original event as well as the
			// proposition named by this attempt.
			replayNeed := auth.CanRead
			if e.Proposition == 0 {
				// Detached core events are deletes whose proposition row is gone.
				replayNeed = auth.CanSettings
			}
			if err := authorise(ctx, tx, a, e.Proposition, replayNeed); err != nil {
				return Event{}, err
			}
			return e, nil
		}
	}
	change, err := apply(ctx, tx)
	if err != nil {
		return Event{}, err
	}

	at := s.Now().Unix()
	before, err := marshal(change.Before)
	if err != nil {
		return Event{}, err
	}
	after, err := marshal(change.After)
	if err != nil {
		return Event{}, err
	}
	prop := proposition
	if change.Proposition != 0 {
		prop = change.Proposition
	}
	filed := prop
	if change.Detached {
		filed = 0
	}
	seq, err := insertActivity(ctx, tx, a, filed, change, before, after, at)
	if err != nil {
		return Event{}, err
	}
	if mine != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_keys
			(actor_id, key, activity_id, created_at) VALUES (?, ?, ?, ?)`,
			a.ID, mine, seq, at); err != nil {
			return Event{}, err
		}
	}
	e := Event{
		Seq: seq, Proposition: prop, Entity: change.Entity, EntityID: change.EntityID,
		Action: change.Action, Actor: a, Before: before, After: after, At: at, Key: mine,
	}
	if joined {
		group.events = append(group.events, e)
		return e, nil
	}
	if err := s.commit(tx, []Event{e}); err != nil {
		return Event{}, err
	}
	return e, nil
}

type groupKey struct{}

// group is the transaction a run of commands shares and the events they are
// waiting to publish.
type group struct {
	open   *sql.Tx
	events []Event
}

// tx is nil safe, so Do can ask for the shared transaction before it knows
// whether there is one.
func (g *group) tx() *sql.Tx {
	if g == nil {
		return nil
	}
	return g.open
}

// Together runs several commands in one transaction, so a refusal partway
// through leaves none of them applied. A form that saves a section is one of
// these: adding a column and then failing to remove another used to leave the
// added one behind and say the save was refused. Nothing is published until
// the transaction commits, because until then nothing has happened.
func (s *Service) Together(ctx context.Context, run func(context.Context) error) error {
	if _, joined := ctx.Value(groupKey{}).(*group); joined {
		return run(ctx)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g := &group{open: tx}
	if err := run(context.WithValue(ctx, groupKey{}, g)); err != nil {
		return err
	}
	return s.commit(tx, g.events)
}

func (s *Service) commit(tx *sql.Tx, events []Event) error {
	s.publishing.Lock()
	defer s.publishing.Unlock()
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, e := range events {
		s.Bus.Publish(e)
	}
	return nil
}

// authorise resolves the actor's role and, for anyone but an owner, checks that
// the actor is a member of the proposition.
func authorise(ctx context.Context, tx *sql.Tx, a Actor, proposition int64, need string) error {
	role, userID, err := resolve(ctx, tx, a)
	if err != nil {
		return err
	}
	if !auth.Can(role, need) {
		return ErrForbidden
	}
	if proposition == 0 || role == auth.RoleOwner || a.Kind == KindFile {
		return nil
	}
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM proposition_members WHERE proposition_id = ? AND user_id = ?`,
		proposition, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	return err
}

// resolve turns an actor into the role its permissions come from and the user
// whose membership counts, which for an API token or an MCP client is the
// person who owns it. The document watcher acts as an editor on any
// proposition, because a file on disk has no membership to check.
func resolve(ctx context.Context, tx *sql.Tx, a Actor) (role string, userID int64, err error) {
	if a.Kind == KindFile {
		return auth.RoleEditor, 0, nil
	}
	if a.Kind != KindUser {
		return "", 0, ErrForbidden
	}
	err = tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, a.ID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrForbidden
	}
	return role, a.ID, err
}

func marshal(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func insertActivity(ctx context.Context, tx *sql.Tx, a Actor, proposition int64,
	c Change, before, after json.RawMessage, at int64) (int64, error) {
	var prop any
	if proposition != 0 {
		prop = proposition
	}
	null := func(m json.RawMessage) any {
		if m == nil {
			return nil
		}
		return string(m)
	}
	var via any
	if a.Via != "" {
		via = a.Via
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO activity
		(proposition_id, actor_kind, actor_id, via, entity, entity_id, action, before_json, after_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		prop, a.Kind, strconv.FormatInt(a.ID, 10), via, c.Entity,
		strconv.FormatInt(c.EntityID, 10), c.Action, null(before), null(after), at)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Since reads the stream out of the activity table, which is what the long poll
// fallback serves and what a tab that missed messages catches up with.
// The client's key is read back with the row, because a tab catching up on
// what it missed has to recognize a row it drew itself as surely as one that
// arrived while it was listening. One activity row has at most one key: a key
// is written beside the row the command that spent it made, and no other
// command can write it again.
func (s *Service) Since(ctx context.Context, proposition, seq int64, limit int) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id, a.actor_kind, a.actor_id, coalesce(u.name, ''),
		coalesce(a.via, ''), a.entity, a.entity_id, a.action, a.before_json, a.after_json, a.created_at,
		coalesce(k.key, '')
		FROM activity a LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
		LEFT JOIN client_keys k ON k.activity_id = a.id
		WHERE a.proposition_id = ? AND a.id > ? ORDER BY a.id LIMIT ?`, proposition, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var actorID, entityID string
		var before, after sql.NullString
		if err := rows.Scan(&e.Seq, &e.Actor.Kind, &actorID, &e.Actor.Name, &e.Actor.Via,
			&e.Entity, &entityID, &e.Action, &before, &after, &e.At, &e.Key); err != nil {
			return nil, err
		}
		e.Proposition = proposition
		e.Actor.ID, _ = strconv.ParseInt(actorID, 10, 64)
		e.EntityID, _ = strconv.ParseInt(entityID, 10, 64)
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
