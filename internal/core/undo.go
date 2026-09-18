package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
)

// undoable names the entities whose before can be put back and the columns it
// may write. Anything else refuses, and so do creates and deletes: putting a
// deleted row back would give it a new id and orphan everything that pointed at
// it, and unmaking a created one is a delete wearing a different name.
var undoable = map[string]struct {
	table string
	cols  []string
}{
	"proposition":    {"propositions", []string{"title", "statement", "blurb", "status", "episode", "target_date", "position", "archived_at"}},
	"column":         {"columns", []string{"name", "position"}},
	"card":           {"cards", []string{"column_id", "position", "title", "description_md", "question", "due_date", "done_at"}},
	"checklist_item": {"checklist_items", []string{"text", "done", "position"}},
}

// Undo puts back the before of one activity row and marks the row undone. The
// undo is itself a command: it is authorised, recorded and published like any
// other edit, so a board watching sees it happen.
func (s *Service) Undo(ctx context.Context, a Actor, activityID int64) (Event, error) {
	var proposition sql.NullInt64
	var entity, entityID, action string
	var before sql.NullString
	var undoneAt sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT proposition_id, entity, entity_id, action, before_json, undone_at
		FROM activity WHERE id = ?`, activityID).
		Scan(&proposition, &entity, &entityID, &action, &before, &undoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}

	spec, ok := undoable[entity]
	if !ok || !before.Valid || undoneAt.Valid || action == "create" || action == "delete" || action == "undo" {
		return Event{}, ErrNotUndoable
	}
	id, err := strconv.ParseInt(entityID, 10, 64)
	if err != nil {
		return Event{}, ErrNotUndoable
	}
	fields, err := decode(before.String)
	if err != nil {
		return Event{}, err
	}

	return s.Do(ctx, a, proposition.Int64, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (Change, error) {
		// Claiming the row and restoring it in the same transaction is what
		// makes two people pressing undo on the same change do it once.
		res, err := tx.ExecContext(ctx,
			`UPDATE activity SET undone_at = ? WHERE id = ? AND undone_at IS NULL`, s.Now().Unix(), activityID)
		if err != nil {
			return Change{}, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return Change{}, ErrNotUndoable
		}

		was, err := snapshot(ctx, tx, spec.table, spec.cols, id)
		if err != nil {
			return Change{}, err
		}

		set, args := []string{}, []any{}
		for _, col := range spec.cols {
			v, ok := fields[col]
			if !ok {
				continue
			}
			set = append(set, col+" = ?")
			args = append(args, v)
		}
		if len(set) == 0 {
			return Change{}, ErrNotUndoable
		}
		// A card's version is what stale edits are refused against, so putting
		// the old text back has to move it forward, not backward.
		if spec.table == "cards" {
			set = append(set, "version = version + 1")
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+spec.table+` SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...); err != nil {
			return Change{}, err
		}

		now, err := snapshot(ctx, tx, spec.table, spec.cols, id)
		if err != nil {
			return Change{}, err
		}
		return Change{Entity: entity, EntityID: id, Action: "undo", Before: was, After: now}, nil
	})
}

// snapshot reads one row as the map that goes into an activity payload. It is
// generic so that core needs to know nothing about the board's row types.
func snapshot(ctx context.Context, tx *sql.Tx, table string, cols []string, id int64) (map[string]any, error) {
	all := append([]string{"id"}, cols...)
	if table == "cards" {
		all = append(all, "version")
	}
	dest := make([]any, len(all))
	values := make([]any, len(all))
	for i := range dest {
		dest[i] = &values[i]
	}
	err := tx.QueryRowContext(ctx,
		`SELECT `+strings.Join(all, ", ")+` FROM `+table+` WHERE id = ?`, id).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(all))
	for i, c := range all {
		if b, ok := values[i].([]byte); ok {
			out[c] = string(b)
			continue
		}
		out[c] = values[i]
	}
	return out, nil
}

// decode reads an activity payload with numbers left as numbers, so that a unix
// time does not come back as a float and land in the column as one.
func decode(payload string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	for k, v := range raw {
		n, ok := v.(json.Number)
		if !ok {
			continue
		}
		if i, err := n.Int64(); err == nil {
			raw[k] = i
			continue
		}
		f, err := n.Float64()
		if err != nil {
			return nil, err
		}
		raw[k] = f
	}
	return raw, nil
}
