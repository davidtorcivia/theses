package core

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// A client's key is how a command says it is the same command as one that may
// already have arrived. The browser puts one on every frame it sends, because a
// socket that dies with a frame in flight cannot tell whether the server
// applied it; an agent puts one on a request it may have to send again. The
// server remembers which activity row a key produced and answers the second
// arrival with the first one's event, having applied nothing.
//
// The key belongs to the actor, so one client cannot spend another's, and it is
// only honored for a person: the markdown watcher is not a client and has
// nothing to retry.

type keyKey struct{}

// run is one client key and how many commands have spent it. A single command
// in the browser's sense can be several core commands, in a transaction or not:
// a paste of three paragraphs is three inserts, a new document is a row and its
// blocks. The second and every one after it take the key with a number on the
// end, so that a replay of a sequence is answered command by command rather
// than answering all of them with the first one's event.
//
// What this guarantees is that a replay applies nothing that was applied
// before, because a replay never spends more numbers than the first run did:
// every command it reaches has an original to be answered with. What it does
// not guarantee is that each number lands on the command that first took it. A
// hit shortens the sequence: CreateDocument's first hit leaves the text it
// starts from empty, so the blocks it would have written spend no numbers, and
// the commands after it take numbers their originals did not. That is safe for
// as long as nothing after a hit reads the payload of the event it was
// answered with. Two callers do read one: files.record, which wants the row it
// made, and board.CreateProposition, which seeds under the id it was given;
// both read it off the event the command itself returned, which is always that
// command's own original.
//
// What keeps that true today is that every command running more than one Do
// runs them inside Together, so they commit as one and a replay either finds
// all their keys or none, or, like the MCP add_link, has a first Do that always
// runs exactly once before the rest. A command that ran several Do calls
// outside Together, and whose replay could be shortened by one of the inner
// ones hitting, would break this in silence: its later commands would be
// answered with events belonging to earlier ones. Anything of that shape needs
// a key of its own per step rather than a counter.
type run struct {
	key string
	n   int
}

// maxKey is as long as a key may be, which is comfortably more than the
// hyphenated UUID the browser sends.
const maxKey = 64

// KeyLife is how long a key is remembered. It is the window in which a client
// can still be replaying: an outbox drains on the next connection, and a
// device that has been shut for a day has had its commands applied or has not.
// Past it a repeat is applied again, so this is what makes a retry safe rather
// than a record of what was done.
const KeyLife = 24 * time.Hour

// WithKey puts a client's key in the context for the commands that follow, or
// refuses one that is not a key. It validates here rather than leaving each
// surface to do it, so a malformed key is a malformed request everywhere and
// never something quietly ignored.
func WithKey(ctx context.Context, key string) (context.Context, error) {
	if key == "" || len(key) > maxKey {
		return nil, ErrKey
	}
	for _, c := range []byte(key) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return nil, ErrKey
		}
	}
	return context.WithValue(ctx, keyKey{}, &run{key: key}), nil
}

// spend answers the key this command is to be remembered under, or "" when
// there is no key or the actor is not a person.
func spend(ctx context.Context, a Actor) string {
	r, ok := ctx.Value(keyKey{}).(*run)
	if !ok || a.Kind != KindUser {
		return ""
	}
	r.n++
	if r.n == 1 {
		return r.key
	}
	return r.key + "#" + strconv.Itoa(r.n)
}

// replayed answers the event this key already produced, if it produced one. The
// row is read back out of the activity log rather than kept beside the key,
// because the log already holds every field an event carries and a second copy
// could disagree with it.
func replayed(ctx context.Context, tx *sql.Tx, actorID int64, key string) (Event, bool, error) {
	var e Event
	var proposition sql.NullInt64
	var actor, entityID string
	var before, after sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.proposition_id, a.actor_kind, a.actor_id,
		coalesce(u.name, ''), coalesce(a.via, ''), a.entity, a.entity_id, a.action,
		a.before_json, a.after_json, a.created_at
		FROM client_keys k JOIN activity a ON a.id = k.activity_id
		LEFT JOIN users u ON a.actor_kind = 'user' AND u.id = a.actor_id
		WHERE k.actor_id = ? AND k.key = ?`, actorID, key).
		Scan(&e.Seq, &proposition, &e.Actor.Kind, &actor, &e.Actor.Name, &e.Actor.Via,
			&e.Entity, &entityID, &e.Action, &before, &after, &e.At)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	// The proposition is the one the row was filed under, which is zero for the
	// handful of changes filed under none: a proposition's own delete is one,
	// and a replay of one says nothing about which proposition it was.
	e.Proposition = proposition.Int64
	e.Actor.ID, _ = strconv.ParseInt(actor, 10, 64)
	e.EntityID, _ = strconv.ParseInt(entityID, 10, 64)
	if before.Valid {
		e.Before = []byte(before.String)
	}
	if after.Valid {
		e.After = []byte(after.String)
	}
	// The key goes back on the answer too. Nothing was applied, so it says which
	// command is being answered rather than what was done, and a client that
	// drew a row under this key can take the row in the payload for it without
	// waiting for the stream to bring the same row round again.
	e.Key = key
	e.Replayed = true
	return e, true, nil
}

// PruneKeys forgets the keys nobody can still be replaying. It runs on the
// sweep that already looks for abandoned uploads rather than on a timer of its
// own, because one hourly pass over the housekeeping is enough for both.
func (s *Service) PruneKeys(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM client_keys WHERE created_at < ?`, s.Now().Add(-KeyLife).Unix())
	return err
}
