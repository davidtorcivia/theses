package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/davidtorcivia/theses/internal/auth"
)

// undoable names the entities whose before can be put back and the columns it
// may write. Anything else refuses, and so do creates and deletes: putting a
// deleted row back would give it a new id and orphan everything that pointed at
// it, and unmaking a created one is a delete wearing a different name. The one
// delete that is undone is a tombstone, which never took the row away.
type undoSpec struct {
	table string
	// scope is the column an ordering key is unique within, empty for a table
	// ordered as a whole. Undo needs it to see whether the key it is about to
	// put back has been taken since.
	scope string
	cols  []string
	// versioned is a table whose rows carry the version a stale edit is refused
	// against. Putting old text back has to move that forward rather than
	// backward, or the editor that lost the race wins the next one.
	versioned bool
	// tombstone is a table whose delete sets a column rather than removing the
	// row, so undoing one is an ordinary column write.
	tombstone bool
}

var undoable = map[string]undoSpec{
	"proposition":    {table: "propositions", cols: []string{"title", "statement", "blurb", "status", "episode", "target_date", "position", "archived_at"}},
	"column":         {table: "columns", scope: "proposition_id", cols: []string{"name", "position"}},
	"card":           {table: "cards", scope: "column_id", cols: []string{"column_id", "position", "title", "description_md", "question", "due_date", "done_at"}, versioned: true},
	"checklist_item": {table: "checklist_items", scope: "card_id", cols: []string{"text", "done", "position"}},
	"document":       {table: "documents", scope: "proposition_id", cols: []string{"name", "slug", "position"}},
	"block":          {table: "blocks", scope: "document_id", cols: []string{"position", "text", "deleted_at"}, versioned: true, tombstone: true},
	// A link's fields are the ones a person reads and corrects. What a refetch
	// also wrote, the canonical URL, the time of the fetch and the page's text
	// for the search index, is not on the list and is left as the refetch left
	// it: the event contract keeps the search text out of the payload, so undo
	// has nothing to put back, and an undo of a refetch therefore restores the
	// fields that are shown and not the ones that are not.
	//
	// A file's are the two that do not describe the object in the bucket:
	// putting back a size, a key or a state would say something about the
	// bucket that is not true, and naming none of the columns a completion
	// writes is what makes a finished upload not undoable.
	"link": {table: "links", cols: []string{"title", "author", "year", "kind", "note_md", "question"}},
	"file": {table: "files", cols: []string{"name", "folder"}},
}

// Undo puts back the before of one activity row and marks the row undone. The
// undo is itself a command: it is authorized, recorded and published like any
// other edit, so a board watching sees it happen.
func (s *Service) Undo(ctx context.Context, a Actor, activityID int64) (Event, error) {
	var proposition sql.NullInt64
	var entity, entityID, action string
	var before, after sql.NullString
	var undoneAt sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT proposition_id, entity, entity_id, action, before_json, after_json, undone_at
		FROM activity WHERE id = ?`, activityID).
		Scan(&proposition, &entity, &entityID, &action, &before, &after, &undoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}

	return s.Do(ctx, a, proposition.Int64, auth.CanEdit, func(ctx context.Context, tx *sql.Tx) (Change, error) {
		// Whether this row can be put back is asked after the actor has been
		// authorized for the proposition it belongs to, not before. Answering
		// that a change cannot be undone to somebody who may not read the
		// proposition would tell them the row is there, and activity ids are
		// dense enough to walk. A refusal here rolls the transaction back and
		// writes no activity row of its own.
		spec, ok := undoable[entity]
		if !ok || !before.Valid || !after.Valid || undoneAt.Valid ||
			action == "create" || (action == "delete" && !spec.tombstone) || action == "undo" {
			return Change{}, ErrNotUndoable
		}
		id, err := strconv.ParseInt(entityID, 10, 64)
		if err != nil {
			return Change{}, ErrNotUndoable
		}
		fields, err := decode(before.String)
		if err != nil {
			return Change{}, err
		}
		applied, err := decode(after.String)
		if err != nil {
			return Change{}, err
		}
		// A change that moved none of the columns undo can write moved
		// something else: an assignee, a note. Putting the columns back would
		// mark the row undone without undoing anything.
		if unchanged(spec.cols, fields, applied) {
			return Change{}, ErrNotUndoable
		}

		// An undo is a write, so whatever rule the layer above has about
		// writing to this proposition applies to it, asked here rather than
		// before the transaction so the answer cannot go stale between.
		if s.Allow != nil {
			if err := s.Allow(ctx, tx, proposition.Int64, entity, "undo"); err != nil {
				return Change{}, err
			}
		}

		// The row has to still hold what the change left, or putting the
		// before back would throw away whatever came after it and, for a
		// position, put two rows on one ordering key. This is the same refusal
		// a stale text edit gets, so the editor offers the same choice.
		current, err := snapshot(ctx, tx, spec, id)
		if err != nil {
			return Change{}, err
		}
		for _, col := range spec.cols {
			if same(current[col], applied[col]) {
				continue
			}
			return Change{}, &ConflictError{
				Entity: entity, EntityID: id, Field: col,
				Version: version(current), Current: text(current[col]),
			}
		}

		// An ordering key is unique within its scope, and the key this undo
		// would put back may have been given to something else since the row
		// left it. Two rows on one key is an order that depends on which the
		// database happens to return first, so this is a refusal rather than a
		// thing to sort out afterwards. The scope is the one the row is going
		// back to, which for a card is the column it came from.
		if key, ok := fields["position"]; ok {
			where, args := "position = ? AND id <> ?", []any{key, id}
			if spec.scope != "" {
				scope, ok := fields[spec.scope]
				if !ok {
					scope = current[spec.scope]
				}
				where = spec.scope + " = ? AND " + where
				args = append([]any{scope}, args...)
			}
			var taken int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM `+spec.table+` WHERE `+where, args...).Scan(&taken); err != nil {
				return Change{}, err
			}
			if taken > 0 {
				return Change{}, &ConflictError{
					Entity: entity, EntityID: id, Field: "position",
					Version: version(current), Current: text(current["position"]),
				}
			}
		}

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

		was, err := s.whole(ctx, tx, entity, spec, id)
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
		// The version is what stale edits are refused against, so putting the
		// old text back has to move it forward, not backward.
		if spec.versioned {
			set = append(set, "version = version + 1")
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+spec.table+` SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...); err != nil {
			return Change{}, err
		}

		now, err := s.whole(ctx, tx, entity, spec, id)
		if err != nil {
			return Change{}, err
		}
		return Change{Entity: entity, EntityID: id, Action: "undo", Before: was, After: now}, nil
	})
}

// whole reads the row the way the commands do, so that an undo's payload is
// the same shape as every other event's and a tab can replace what it holds
// with it. Without a Reader it falls back to the columns undo itself writes.
func (s *Service) whole(ctx context.Context, tx *sql.Tx, entity string, spec undoSpec, id int64) (any, error) {
	if s.Read != nil {
		row, err := s.Read(ctx, tx, entity, id)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return snapshot(ctx, tx, spec, id)
}

// snapshot reads one row as the map that goes into an activity payload. It is
// generic so that core needs to know nothing about the board's row types.
func snapshot(ctx context.Context, tx *sql.Tx, spec undoSpec, id int64) (map[string]any, error) {
	all := append([]string{"id"}, spec.cols...)
	if spec.versioned {
		all = append(all, "version")
	}
	if spec.scope != "" && !slices.Contains(all, spec.scope) {
		all = append(all, spec.scope)
	}
	dest := make([]any, len(all))
	values := make([]any, len(all))
	for i := range dest {
		dest[i] = &values[i]
	}
	err := tx.QueryRowContext(ctx,
		`SELECT `+strings.Join(all, ", ")+` FROM `+spec.table+` WHERE id = ?`, id).Scan(dest...)
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

// same compares one column's stored value with the same column in an activity
// payload. A bool goes into an INTEGER column and comes back as one, and JSON
// has no integers of its own, so both are brought to the same shape first.
func same(stored, recorded any) bool {
	return text(stored) == text(recorded)
}

func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
	case []byte:
		return string(t)
	}
	return fmt.Sprint(v)
}

func version(row map[string]any) int64 {
	n, _ := row["version"].(int64)
	return n
}

// unchanged reports whether two payloads agree on every column undo writes.
func unchanged(cols []string, before, after map[string]any) bool {
	for _, col := range cols {
		if !same(before[col], after[col]) {
			return false
		}
	}
	return true
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
