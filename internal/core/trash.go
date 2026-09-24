package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
)

const TrashRetention = 7 * 24 * time.Hour

var ErrRestoreBusy = errors.New("backup or storage cleanup is running; retry restoration when it finishes")

var ErrRestoreConflict = errors.New("this item cannot be restored because a name, position, or related item changed; restore missing related items first")

// constraint reports a SQLite constraint failure, whose primary code is 19
// under every extended code.
func constraint(err error) bool {
	var sqlite interface{ Code() int }
	return errors.As(err, &sqlite) && sqlite.Code()&255 == 19
}

type trashTable struct{ table, where string }

var trashTables = map[string][]trashTable{
	"document":       {{"documents", "id=?"}, {"blocks", "document_id=?"}, {"document_revisions", "document_id=?"}},
	"card":           {{"cards", "id=?"}, {"card_assignees", "card_id=?"}, {"checklist_items", "card_id=?"}, {"comments", "card_id=?"}, {"card_links", "card_id=?"}, {"card_files", "card_id=?"}},
	"link":           {{"links", "id=?"}, {"card_links", "link_id=?"}},
	"file":           {{"files", "id=?"}, {"file_comments", "file_id=?"}, {"transcripts", "file_id=?"}, {"card_files", "file_id=?"}},
	"evidence":       {{"evidence", "id=?"}},
	"calendar_event": {{"calendar_events", "id=?"}},
}

type trashReference struct{ table, key, column, where string }

var trashReferences = map[string][]trashReference{
	"document": {{"evidence", "id", "block_id", "block_id IN (SELECT id FROM blocks WHERE document_id=?)"}},
	"link":     {{"evidence", "id", "link_id", "link_id=?"}},
	"file":     {{"evidence", "id", "file_id", "file_id=?"}, {"files", "id", "version_of", "version_of=?"}},
	"card":     {{"production_templates", "proposition_id", "card_id", "card_id=?"}},
}

type trashReferenceValue struct {
	ID     int64 `json:"id"`
	Target int64 `json:"target"`
}
type trashSnapshot struct {
	Tables     []trashRows             `json:"tables"`
	References [][]trashReferenceValue `json:"references"`
}

type trashRows struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}
type TrashItem struct {
	ID          int64  `json:"id"`
	Proposition int64  `json:"proposition_id"`
	Entity      string `json:"entity"`
	EntityID    int64  `json:"entity_id"`
	Title       string `json:"title"`
	DeletedAt   int64  `json:"deleted_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

// KeepDeleted snapshots a supported item inside its authorized delete transaction.
func (s *Service) KeepDeleted(ctx context.Context, tx *sql.Tx, prop int64, entity string, id int64, title string) error {
	tables, ok := trashTables[entity]
	if !ok {
		return ErrNotFound
	}
	snapshot := make([]trashRows, 0, len(tables))
	for _, table := range tables {
		rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table.table+" WHERE "+table.where, id)
		if err != nil {
			return err
		}
		data := trashRows{}
		data.Columns, err = rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		for rows.Next() {
			values := make([]any, len(data.Columns))
			ptrs := make([]any, len(values))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			data.Rows = append(data.Rows, values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		snapshot = append(snapshot, data)
	}
	refs := make([][]trashReferenceValue, 0, len(trashReferences[entity]))
	for _, ref := range trashReferences[entity] {
		rows, err := tx.QueryContext(ctx, "SELECT "+ref.key+","+ref.column+" FROM "+ref.table+" WHERE "+ref.where, id)
		if err != nil {
			return err
		}
		values := []trashReferenceValue{}
		for rows.Next() {
			var v trashReferenceValue
			if err := rows.Scan(&v.ID, &v.Target); err != nil {
				rows.Close()
				return err
			}
			values = append(values, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		refs = append(refs, values)
	}
	raw, err := json.Marshal(trashSnapshot{Tables: snapshot, References: refs})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM trash WHERE expires_at<=?", s.Now().Unix()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO trash(proposition_id,entity,entity_id,title,payload,deleted_at,expires_at) VALUES(?,?,?,?,?,?,?)`, prop, entity, id, title, string(raw), s.Now().Unix(), s.Now().Add(TrashRetention).Unix())
	return err
}

func (s *Service) Trash(ctx context.Context, a Actor, prop int64) ([]TrashItem, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := authorise(ctx, tx, a, prop, auth.CanRead); err != nil {
		return nil, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,proposition_id,entity,entity_id,title,deleted_at,expires_at FROM trash WHERE proposition_id=? AND restored_at IS NULL AND expires_at>? ORDER BY id DESC`, prop, s.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrashItem{}
	for rows.Next() {
		var item TrashItem
		if err := rows.Scan(&item.ID, &item.Proposition, &item.Entity, &item.EntityID, &item.Title, &item.DeletedAt, &item.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) DeletedItem(ctx context.Context, a Actor, id int64) (TrashItem, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return TrashItem{}, err
	}
	defer tx.Rollback()
	var item TrashItem
	err = tx.QueryRowContext(ctx, "SELECT id,proposition_id,entity,entity_id,title,deleted_at,expires_at FROM trash WHERE id=?", id).Scan(&item.ID, &item.Proposition, &item.Entity, &item.EntityID, &item.Title, &item.DeletedAt, &item.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return TrashItem{}, err
	}
	if err := authorise(ctx, tx, a, item.Proposition, auth.CanRead); err != nil {
		return TrashItem{}, ErrNotFound
	}
	return item, nil
}

func (s *Service) RestoreDeleted(ctx context.Context, a Actor, id int64) (Event, error) {
	if s.ReserveMaintenance != nil {
		release, err := s.ReserveMaintenance()
		if err != nil {
			return Event{}, ErrRestoreBusy
		}
		defer release()
	}

	var prop int64
	if err := s.DB.QueryRowContext(ctx, "SELECT proposition_id FROM trash WHERE id=?", id).Scan(&prop); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return Event{}, err
	}
	return s.Do(ctx, a, prop, auth.CanDelete, func(ctx context.Context, tx *sql.Tx) (Change, error) {
		var entity, raw string
		var entityID int64
		if err := tx.QueryRowContext(ctx, `SELECT entity,entity_id,payload FROM trash WHERE id=? AND proposition_id=? AND restored_at IS NULL AND expires_at>?`, id, prop, s.Now().Unix()).Scan(&entity, &entityID, &raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = ErrNotFound
			}
			return Change{}, err
		}
		if s.Allow != nil {
			if err := s.Allow(ctx, tx, prop, entity, "restore"); err != nil {
				return Change{}, err
			}
		}
		tables, ok := trashTables[entity]
		if !ok {
			return Change{}, ErrNotFound
		}
		var saved trashSnapshot
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&saved); err != nil {
			return Change{}, err
		}
		snapshot := saved.Tables
		if len(snapshot) != len(tables) || len(saved.References) != len(trashReferences[entity]) {
			return Change{}, ErrRestoreConflict
		}
		for i, data := range snapshot {
			table := tables[i].table
			columns, err := tx.QueryContext(ctx, "SELECT * FROM "+table+" LIMIT 0")
			if err != nil {
				return Change{}, err
			}
			known, err := columns.Columns()
			columns.Close()
			if err != nil {
				return Change{}, err
			}
			userKeys := map[string]string{}
			keys, err := tx.QueryContext(ctx, `SELECT "from",on_delete FROM pragma_foreign_key_list(?) WHERE "table"='users'`, table)
			if err != nil {
				return Change{}, err
			}
			for keys.Next() {
				var column, action string
				if err := keys.Scan(&column, &action); err != nil {
					keys.Close()
					return Change{}, err
				}
				userKeys[column] = action
			}
			err = keys.Err()
			keys.Close()
			if err != nil {
				return Change{}, err
			}
			quoted := make([]string, len(data.Columns))
			marks := make([]string, len(data.Columns))
			for j, col := range data.Columns {
				if !slices.Contains(known, col) {
					return Change{}, ErrRestoreConflict
				}
				quoted[j] = `"` + col + `"`
				marks[j] = "?"
			}
		rows:
			for _, row := range data.Rows {
				if table == "cards" || table == "documents" {
					scope := "column_id"
					if table == "documents" {
						scope = "proposition_id"
					}
					positionIndex, scopeIndex := slices.Index(data.Columns, "position"), slices.Index(data.Columns, scope)
					if len(row) != len(data.Columns) || positionIndex < 0 || scopeIndex < 0 {
						return Change{}, ErrRestoreConflict
					}
					var count int
					if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+scope+"=? AND position=?", row[scopeIndex], row[positionIndex]).Scan(&count); err != nil {
						return Change{}, err
					}
					if count > 0 {
						return Change{}, ErrRestoreConflict
					}
				}

				if len(row) != len(data.Columns) {
					return Change{}, ErrRestoreConflict
				}
				for j, value := range row {
					if n, ok := value.(json.Number); ok {
						if whole, err := n.Int64(); err == nil {
							row[j] = whole
						} else {
							real, err := n.Float64()
							if err != nil {
								return Change{}, err
							}
							row[j] = real
						}
					}
				}
				// Account deletion keeps its normal SET NULL and CASCADE semantics in trash.
				for j, col := range data.Columns {
					if action, ok := userKeys[col]; ok && row[j] != nil {
						var exists bool
						if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=?)", row[j]).Scan(&exists); err != nil {
							return Change{}, err
						}
						if !exists {
							if action == "CASCADE" {
								continue rows
							}
							if action == "SET NULL" {
								row[j] = nil
							}
						}
					}
				}
				// History and checklist IDs are internal and may have been reused after deletion.
				if table == "document_revisions" || table == "checklist_items" {
					for j, col := range data.Columns {
						if col == "id" {
							row[j] = nil
						}
						// Migration 021 respelled the template's a0; a card deleted before it still holds one.
						if col == "position" && row[j] == "a0" {
							row[j] = "a"
						}
					}
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" ("+strings.Join(quoted, ",")+") VALUES ("+strings.Join(marks, ",")+")", row...); err != nil {
					if constraint(err) {
						return Change{}, ErrRestoreConflict
					}
					return Change{}, err
				}
			}
		}
		for i, ref := range trashReferences[entity] {
			for _, v := range saved.References[i] {
				var current sql.NullInt64
				err := tx.QueryRowContext(ctx, "SELECT "+ref.column+" FROM "+ref.table+" WHERE "+ref.key+"=?", v.ID).Scan(&current)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return Change{}, err
				}
				if current.Valid && current.Int64 != v.Target {
					return Change{}, ErrRestoreConflict
				}
				if !current.Valid {
					suffix := ""
					if ref.table == "evidence" {
						suffix = ",version=version+1"
					}
					if ref.table == "files" {
						suffix = ",metadata_version=metadata_version+1"
					}
					if _, err := tx.ExecContext(ctx, "UPDATE "+ref.table+" SET "+ref.column+"=?"+suffix+" WHERE "+ref.key+"=?", v.Target, v.ID); err != nil {
						return Change{}, err
					}
				}
			}
		}
		// Restoring must advance versions so stale editors cannot overwrite restored work.
		versionColumn := ""
		switch entity {
		case "card", "evidence", "calendar_event":
			versionColumn = "version"
		case "file":
			versionColumn = "metadata_version"
		case "document":
			versionColumn = "revision"
		}
		if versionColumn != "" {
			if _, err := tx.ExecContext(ctx, "UPDATE "+tables[0].table+" SET "+versionColumn+"="+versionColumn+"+1 WHERE id=?", entityID); err != nil {
				return Change{}, err
			}
		}
		if entity == "document" {
			if _, err := tx.ExecContext(ctx, "UPDATE blocks SET version=version+1 WHERE document_id=?", entityID); err != nil {
				return Change{}, err
			}
		}
		if entity == "file" {
			if _, err := tx.ExecContext(ctx, "UPDATE files SET comment_revision=comment_revision+1 WHERE id=?", entityID); err != nil {
				return Change{}, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE file_comments SET version=version+1 WHERE file_id=?", entityID); err != nil {
				return Change{}, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE transcripts SET version=version+1 WHERE file_id=?", entityID); err != nil {
				return Change{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE trash SET restored_at=unixepoch() WHERE id=?", id); err != nil {
			return Change{}, err
		}
		after, err := s.Read(ctx, tx, entity, entityID)
		if err != nil {
			return Change{}, fmt.Errorf("read restored item: %w", err)
		}
		if entity == "document" {
			encoded, err := json.Marshal(after)
			if err != nil {
				return Change{}, err
			}
			var doc map[string]any
			if err = json.Unmarshal(encoded, &doc); err != nil {
				return Change{}, err
			}
			rows, err := tx.QueryContext(ctx, "SELECT id FROM blocks WHERE document_id=? AND deleted_at IS NULL ORDER BY position,id", entityID)
			if err != nil {
				return Change{}, err
			}
			ids := []int64{}
			for rows.Next() {
				var block int64
				if err := rows.Scan(&block); err != nil {
					rows.Close()
					return Change{}, err
				}
				ids = append(ids, block)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return Change{}, err
			}
			blocks := []any{}
			for _, block := range ids {
				v, err := s.Read(ctx, tx, "block", block)
				if err != nil {
					return Change{}, err
				}
				blocks = append(blocks, v)
			}
			doc["blocks"] = blocks
			after = doc
		}
		return Change{Entity: entity, EntityID: entityID, Action: "restore", After: after}, nil
	})
}
