package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
)

func (s *Service) ProductionTemplate(ctx context.Context, a core.Actor, proposition int64) (core.Event, error) {
	existingTemplate := errors.New("template already exists")
	var existingEvent core.Event
	event, err := s.do(ctx, a, proposition, auth.CanEdit, "card", "template", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var existing sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT card_id FROM production_templates WHERE proposition_id=?`, proposition).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, err
		}
		if existing.Valid {
			card, err := GetCard(ctx, tx, existing.Int64)
			if err != nil {
				return core.Change{}, err
			}
			raw, err := json.Marshal(card)
			if err != nil {
				return core.Change{}, err
			}
			existingEvent = core.Event{Proposition: proposition, Entity: "card", EntityID: card.ID, Action: "template", After: raw, Replayed: true}
			return core.Change{}, existingTemplate
		}
		var column int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM columns WHERE proposition_id=? ORDER BY position LIMIT 1`, proposition).Scan(&column)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, ErrEmpty
		}
		if err != nil {
			return core.Change{}, err
		}
		position, err := last(ctx, tx, "cards", "column_id", column)
		if err != nil {
			return core.Change{}, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO cards(proposition_id,column_id,position,title,description_md,due_date,created_by,created_at)
   SELECT id,?,?,'Production checklist','Review each stage before release. Completing this checklist does not publish the episode.',target_date,?,unixepoch() FROM propositions WHERE id=?`, column, position, a.ID, proposition)
		if err != nil {
			return core.Change{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		if err := assignCard(ctx, tx, id, proposition, a.ID); err != nil {
			return core.Change{}, err
		}
		for i, label := range []string{"Review: sources checked and script approved", "Record: audio captured and backed up", "Edit: mix reviewed and final audio approved", "Publish: title, description, credits, and release time checked"} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO checklist_items(card_id,text,position) VALUES(?,?,?)`, id, label, fmt.Sprintf("a%d", i)); err != nil {
				return core.Change{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_templates(proposition_id,card_id) VALUES(?,?) ON CONFLICT(proposition_id) DO UPDATE SET card_id=excluded.card_id`, proposition, id); err != nil {
			return core.Change{}, err
		}
		card, err := GetCard(ctx, tx, id)
		return core.Change{Entity: "card", EntityID: id, Action: "template", After: card}, err
	})
	if errors.Is(err, existingTemplate) {
		return existingEvent, nil
	}
	return event, err
}
