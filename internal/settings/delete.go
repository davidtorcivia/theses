package settings

import (
	"context"
	"errors"
	"fmt"

	"github.com/davidtorcivia/theses/internal/store"
)

// Delete removes a stored value, so that the key reads as its registered
// default and IsSet says it is not set. It is how an integration is
// disconnected: SetAs refuses to write an empty secret on purpose, because an
// empty field in a saved form means keep what is stored, and there has to be
// some other way to say get rid of it.
//
// Deleting a key that was never set is not an error and writes no activity
// row: pressing Disconnect twice is not two events.
func (s *Settings) Delete(ctx context.Context, key string, actor Actor) error {
	def, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("settings: unknown key %s", key)
	}

	// Same lock as SetAs, for the same reason: the row and the cache are
	// written together so two writers cannot leave them disagreeing.
	s.mu.Lock()
	defer s.mu.Unlock()

	before := ""
	old, err := store.GetSetting(ctx, s.db, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("%w: read %s: %w", ErrStorage, key, err)
	case def.Secret:
		before = `{"set":true}`
	default:
		before = old.ValueJSON
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("%w: delete %s: %w", ErrStorage, key, err)
	}
	if err := store.InsertActivity(ctx, tx, actor.Kind, actor.ID, actor.Via,
		"setting", key, "clear", before, ""); err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}

	delete(s.present, key)
	delete(s.values, key)
	return nil
}
