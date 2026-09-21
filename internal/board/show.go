package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

var showColumns = []string{"Backlog", "Next", "In progress", "Waiting", "Done"}

var showDocuments = []struct{ name, slug string }{
	{"Show overview", "show-overview"},
	{"Editorial guidelines", "editorial-guidelines"},
	{"Meeting notes", "meeting-notes"},
	{"Ideas", "ideas"},
}

type showDocument struct {
	ID          int64  `json:"id"`
	Proposition int64  `json:"proposition_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Position    int    `json:"position"`
	CreatedBy   *int64 `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	Revision    int64  `json:"revision"`
}

// EnsureShow creates the one permanent shared workspace. Once it exists this
// only repairs universal membership, so documents a user deleted stay deleted.
func EnsureShow(ctx context.Context, db *store.DB) (Proposition, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Proposition{}, err
	}
	defer tx.Rollback()

	p, err := GetShow(ctx, tx)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return Proposition{}, err
	}
	if err == nil {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO proposition_members (proposition_id, user_id)
			SELECT ?, id FROM users`, p.ID); err != nil {
			return Proposition{}, err
		}
		if err := tx.Commit(); err != nil {
			return Proposition{}, err
		}
		return GetShow(ctx, db)
	}

	position, err := place(ctx, tx, "propositions", "", 0, 0, 0)
	if err != nil {
		return Proposition{}, err
	}
	now := time.Now().Unix()
	id, err := nextPropositionID(ctx, tx)
	if err != nil {
		return Proposition{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO propositions
		(id, number, kind, title, status, position, created_at) VALUES (?, 0, 'show', 'Show', 'show', ?, ?)`,
		id, position, now)
	if err != nil {
		return Proposition{}, err
	}
	for _, name := range showColumns {
		position, err := last(ctx, tx, "columns", "proposition_id", id)
		if err != nil {
			return Proposition{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO columns (proposition_id, name, position) VALUES (?, ?, ?)`, id, name, position); err != nil {
			return Proposition{}, err
		}
	}
	p, err = GetProposition(ctx, tx, id)
	if err != nil {
		return Proposition{}, err
	}
	if err := showActivity(ctx, tx, id, "proposition", id, p, now); err != nil {
		return Proposition{}, err
	}
	for i, seed := range showDocuments {
		res, err := tx.ExecContext(ctx, `INSERT INTO documents
			(proposition_id, name, slug, position, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, seed.name, seed.slug, i+1, now)
		if err != nil {
			return Proposition{}, err
		}
		documentID, err := res.LastInsertId()
		if err != nil {
			return Proposition{}, err
		}
		d := showDocument{ID: documentID, Proposition: id, Name: seed.name, Slug: seed.slug,
			Position: i + 1, CreatedAt: now}
		if err := showActivity(ctx, tx, id, "document", documentID, d, now); err != nil {
			return Proposition{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Proposition{}, err
	}
	return p, nil
}

func nextPropositionID(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE proposition_sequence
		SET last_id = max(last_id, (SELECT coalesce(max(id), 0) FROM propositions)) + 1
		WHERE singleton = 1`); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT last_id FROM proposition_sequence WHERE singleton = 1`).Scan(&id)
	return id, err
}

func showActivity(ctx context.Context, tx *sql.Tx, proposition int64, entity string, entityID int64, after any, at int64) error {
	payload, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO activity
		(proposition_id, actor_kind, entity, entity_id, action, after_json, created_at)
		VALUES (?, 'system', ?, ?, 'create', ?, ?)`,
		proposition, entity, strconv.FormatInt(entityID, 10), string(payload), at)
	return err
}
