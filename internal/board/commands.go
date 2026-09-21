package board

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/frac"
	"github.com/davidtorcivia/theses/internal/store"
)

var (
	// ErrColumnNotEmpty guards the one deletion that would take cards with it
	// without saying so.
	ErrColumnNotEmpty = errors.New("move the cards out of that column first")
	// ErrNotYours is deleting somebody else's note.
	ErrNotYours = errors.New("that is not yours to delete")
	// ErrEmpty is a title or a note with nothing in it.
	ErrEmpty = errors.New("that needs some text")
	// ErrArchived is an edit to a proposition that has been put away. Restoring
	// it and deleting it are the two things still allowed.
	ErrArchived = errors.New("that proposition is archived; restore it first")
	// ErrTooLong is a field with more in it than the field is for.
	ErrTooLong = errors.New("that is longer than this field takes")
	// ErrQuestion is a card filed under something that is not one of the four.
	ErrQuestion = errors.New("that is not one of the four questions")
	// ErrStatus is a proposition moved to a status the workspace does not have,
	// which the rail would have no column to draw it in.
	ErrStatus = errors.New("that is not one of the workspace's statuses")
	// ErrDueDate is a due date that is not a day. The board asks whether one
	// has passed, so anything the calendar does not have could never be late
	// and would sit on a card saying nothing for ever.
	ErrDueDate = errors.New("a due date is a day, written as 2006-01-02")
)

// What a field on the board holds. A status or a date is a word, a title is a
// line, a description or a note is the longest thing here. Past these the
// command refuses rather than storing something the column was never meant to
// carry and every board that draws it has to render.
const (
	MaxWord = 100
	MaxLine = 500
	MaxBody = 20000
	// maxAssignees is more people than the workspace has, so a create naming
	// more than this is not somebody assigning work.
	maxAssignees = 50
)

// Field trims a value and refuses one longer than the field takes. It is the
// one answer to how long a thing on the board may be, so every surface that
// takes text asks it rather than inventing a limit of its own.
func Field(value string, most int) (string, error) {
	value = strings.TrimSpace(value)
	if err := Fits(value, most); err != nil {
		return "", err
	}
	return value, nil
}

// Fits is the half of Field that still applies to a value stored exactly as it
// was sent. A block being saved as somebody types is not trimmed, because that
// would take the newline they just typed away from under their caret, but it is
// still no longer than a block may be.
func Fits(value string, most int) error {
	if utf8.RuneCountInString(value) > most {
		return ErrTooLong
	}
	return nil
}

// archived is the rule core cannot know: a proposition that has been put away
// is read only. Restoring it and deleting it are the two things that still
// mean something on one, and they are named by entity as well as action: on
// action alone a card delete, a column delete and a checklist item's removal
// all let themselves through, and archiving refused itself.
func archived(ctx context.Context, tx *sql.Tx, proposition int64, entity, action string) error {
	if proposition == 0 || (entity == "proposition" && (action == "restore" || action == "delete")) {
		return nil
	}
	var at sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT archived_at FROM propositions WHERE id = ?`, proposition).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	if err != nil {
		return err
	}
	if at.Valid {
		return ErrArchived
	}
	return nil
}

// do is core.Do with that rule asked first, so a command that would write to an
// archived proposition never runs at all.
func (s *Service) do(ctx context.Context, a core.Actor, proposition int64, need, entity, action string,
	apply func(context.Context, *sql.Tx) (core.Change, error)) (core.Event, error) {
	return s.Do(ctx, a, proposition, need, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if err := archived(ctx, tx, proposition, entity, action); err != nil {
			return core.Change{}, err
		}
		return apply(ctx, tx)
	})
}

// place returns the ordering key for a row that should sit directly after the
// row `after` within a scope. after zero puts it at the head; exclude is the
// row being moved, which must not be treated as its own neighbor.
func place(ctx context.Context, tx *sql.Tx, table, scope string, scopeID, after, exclude int64) (string, error) {
	where, args := "1 = 1", []any{}
	if scope != "" {
		where, args = scope+" = ?", []any{scopeID}
	}
	lo := ""
	if after != 0 {
		err := tx.QueryRowContext(ctx,
			`SELECT position FROM `+table+` WHERE id = ? AND `+where,
			append([]any{after}, args...)...).Scan(&lo)
		if errors.Is(err, sql.ErrNoRows) {
			return "", core.ErrNotFound
		}
		if err != nil {
			return "", err
		}
	}
	var hi sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT min(position) FROM `+table+` WHERE `+where+` AND position > ? AND id <> ?`,
		append(append([]any{}, args...), lo, exclude)...).Scan(&hi); err != nil {
		return "", err
	}
	return frac.Between(lo, hi.String), nil
}

func last(ctx context.Context, tx *sql.Tx, table, scope string, scopeID int64) (string, error) {
	where, args := "1 = 1", []any{}
	if scope != "" {
		where, args = scope+" = ?", []any{scopeID}
	}
	var top sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT max(position) FROM `+table+` WHERE `+where, args...).Scan(&top); err != nil {
		return "", err
	}
	return frac.Between(top.String, ""), nil
}

// propositionOf answers which proposition an entity belongs to, which is what
// the command is authorized against. It runs outside the transaction because
// nothing ever moves a card or a column to another proposition.
func propositionOf(ctx context.Context, q store.Querier, query string, id int64) (int64, error) {
	var proposition int64
	err := q.QueryRowContext(ctx, query, id).Scan(&proposition)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, core.ErrNotFound
	}
	return proposition, err
}

const (
	cardScope      = `SELECT proposition_id FROM cards WHERE id = ?`
	columnScope    = `SELECT proposition_id FROM columns WHERE id = ?`
	checklistScope = `SELECT c.proposition_id FROM checklist_items i JOIN cards c ON c.id = i.card_id WHERE i.id = ?`
	commentScope   = `SELECT c.proposition_id FROM comments m JOIN cards c ON c.id = m.card_id WHERE m.id = ?`
)

// Propositions.

// CreateProposition takes the next number, the columns the settings name, the
// actor as its first member and whatever Seed fills it with. The seed runs in
// the same transaction, so nobody ever opens a half made proposition.
func (s *Service) CreateProposition(ctx context.Context, a core.Actor, title string) (core.Event, error) {
	var created core.Event
	err := s.Together(ctx, func(ctx context.Context) error {
		e, err := s.createProposition(ctx, a, title)
		if err != nil {
			return err
		}
		created = e
		if s.Seed == nil {
			return nil
		}
		return s.Seed(ctx, a, e.EntityID)
	})
	if err != nil {
		return core.Event{}, err
	}
	return created, nil
}

func (s *Service) createProposition(ctx context.Context, a core.Actor, title string) (core.Event, error) {
	title, err := Field(title, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if title == "" {
		return core.Event{}, ErrEmpty
	}
	defaults := s.Defaults()
	return s.do(ctx, a, 0, auth.CanEdit, "proposition", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var number int64
		if err := tx.QueryRowContext(ctx,
			`SELECT coalesce(max(number), 0) + 1 FROM propositions`).Scan(&number); err != nil {
			return core.Change{}, err
		}
		position, err := last(ctx, tx, "propositions", "", 0)
		if err != nil {
			return core.Change{}, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO propositions
			(number, title, status, position, created_at) VALUES (?, ?, ?, ?, unixepoch())`,
			number, title, defaults.Status, position)
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		for _, name := range defaults.Columns {
			position, err := last(ctx, tx, "columns", "proposition_id", id)
			if err != nil {
				return core.Change{}, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO columns (proposition_id, name, position) VALUES (?, ?, ?)`,
				id, name, position); err != nil {
				return core.Change{}, err
			}
		}
		// The creator is a member, written here rather than by a second
		// command because every later command on this proposition is
		// authorized against the row, including the one that would add it.
		if a.Kind == core.KindUser && a.ID != 0 {
			if err := addMember(ctx, tx, id, a.ID); err != nil {
				return core.Change{}, err
			}
		}
		p, err := GetProposition(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "proposition", EntityID: id, Action: "create",
			After: p, Proposition: id}, nil
	})
}

// proposition is the shape every proposition command has: read it, change it,
// read it again, and hand core the two snapshots.
func (s *Service) proposition(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx, Proposition) error) (core.Event, error) {
	return s.do(ctx, a, id, need, "proposition", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := GetProposition(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if err := apply(ctx, tx, was); err != nil {
			return core.Change{}, err
		}
		change := core.Change{Entity: "proposition", EntityID: id, Action: action, Before: was}
		if action == "delete" {
			change.Detached = true
			return change, nil
		}
		if change.After, err = GetProposition(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return change, nil
	})
}

func (s *Service) EditProposition(ctx context.Context, a core.Actor, id int64, title, statement, blurb string) (core.Event, error) {
	title, err := Field(title, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if title == "" {
		return core.Event{}, ErrEmpty
	}
	if statement, err = Field(statement, MaxLine); err != nil {
		return core.Event{}, err
	}
	if blurb, err = Field(blurb, MaxBody); err != nil {
		return core.Event{}, err
	}
	return s.proposition(ctx, a, id, auth.CanEdit, "edit", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE propositions SET title = ?, statement = ?, blurb = ? WHERE id = ?`,
			title, statement, blurb, id)
		return err
	})
}

func (s *Service) SetStatus(ctx context.Context, a core.Actor, id int64, status string) (core.Event, error) {
	status, err := Field(status, MaxWord)
	if err != nil {
		return core.Event{}, err
	}
	if status == "" {
		return core.Event{}, ErrEmpty
	}
	// The rail groups by status, so a word that is not on the workspace's list
	// is a proposition it has nowhere to draw. A workspace that names no
	// statuses at all is one nothing can be checked against.
	if known := s.Defaults().Statuses; len(known) > 0 && !slices.Contains(known, status) {
		return core.Event{}, ErrStatus
	}
	return s.proposition(ctx, a, id, auth.CanEdit, "status", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		_, err := tx.ExecContext(ctx, `UPDATE propositions SET status = ? WHERE id = ?`, status, id)
		return err
	})
}

func (s *Service) Schedule(ctx context.Context, a core.Actor, id int64, episode, targetDate string) (core.Event, error) {
	episode, err := Field(episode, MaxWord)
	if err != nil {
		return core.Event{}, err
	}
	if targetDate, err = Field(targetDate, MaxWord); err != nil {
		return core.Event{}, err
	}
	if targetDate != "" {
		if _, err := time.Parse("2006-01-02", targetDate); err != nil {
			return core.Event{}, ErrDueDate
		}
	}
	return s.proposition(ctx, a, id, auth.CanEdit, "schedule", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		_, err := tx.ExecContext(ctx, `UPDATE propositions SET episode = ?, target_date = ? WHERE id = ?`,
			value(episode), value(targetDate), id)
		return err
	})
}

// MoveProposition puts one proposition directly after another in the rail.
func (s *Service) MoveProposition(ctx context.Context, a core.Actor, id, after int64) (core.Event, error) {
	return s.proposition(ctx, a, id, auth.CanEdit, "move", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		position, err := place(ctx, tx, "propositions", "", 0, after, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE propositions SET position = ? WHERE id = ?`, position, id)
		return err
	})
}

func (s *Service) ArchiveProposition(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.proposition(ctx, a, id, auth.CanEdit, "archive", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE propositions SET archived_at = unixepoch() WHERE id = ? AND archived_at IS NULL`, id)
		return err
	})
}

func (s *Service) RestoreProposition(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.proposition(ctx, a, id, auth.CanEdit, "restore", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		_, err := tx.ExecContext(ctx, `UPDATE propositions SET archived_at = NULL WHERE id = ?`, id)
		return err
	})
}

func (s *Service) DeleteProposition(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.proposition(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx, _ Proposition) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM propositions WHERE id = ?`, id)
		return err
	})
}

// Members.

func (s *Service) AddMember(ctx context.Context, a core.Actor, proposition, user int64) (core.Event, error) {
	return s.member(ctx, a, proposition, user, "add")
}

func (s *Service) RemoveMember(ctx context.Context, a core.Actor, proposition, user int64) (core.Event, error) {
	return s.member(ctx, a, proposition, user, "remove")
}

func (s *Service) member(ctx context.Context, a core.Actor, proposition, user int64, action string) (core.Event, error) {
	return s.do(ctx, a, proposition, auth.CanEdit, "member", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		row := map[string]any{"proposition_id": proposition, "user_id": user}
		if action == "add" {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE id = ?`, user).Scan(&exists); err != nil {
				return core.Change{}, err
			}
			if exists == 0 {
				return core.Change{}, core.ErrNotFound
			}
			if err := addMember(ctx, tx, proposition, user); err != nil {
				return core.Change{}, err
			}
			return core.Change{Entity: "member", EntityID: user, Action: "add", After: row}, nil
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM proposition_members WHERE proposition_id = ? AND user_id = ?`,
			proposition, user)
		if err != nil {
			return core.Change{}, err
		}
		// Taking off somebody who was never on is nothing happening, and an
		// activity row and an event saying it happened would be a lie every
		// board watching would draw.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return core.Change{}, core.ErrNotFound
		}
		return core.Change{Entity: "member", EntityID: user, Action: "remove", Before: row}, nil
	})
}

// addMember is the one statement that puts somebody on a proposition, so the
// create and the member command cannot come to disagree about what that means.
func addMember(ctx context.Context, tx *sql.Tx, proposition, user int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO proposition_members (proposition_id, user_id) VALUES (?, ?)`,
		proposition, user)
	return err
}

// Columns.

func (s *Service) CreateColumn(ctx context.Context, a core.Actor, proposition int64, name string) (core.Event, error) {
	name, err := Field(name, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if name == "" {
		return core.Event{}, ErrEmpty
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "column", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		position, err := last(ctx, tx, "columns", "proposition_id", proposition)
		if err != nil {
			return core.Change{}, err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO columns (proposition_id, name, position) VALUES (?, ?, ?)`,
			proposition, name, position)
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "column", EntityID: id, Action: "create",
			After: Column{ID: id, Proposition: proposition, Name: name, Position: position}}, nil
	})
}

func (s *Service) column(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx, Column) error) (core.Event, error) {
	proposition, err := propositionOf(ctx, s.DB, columnScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, need, "column", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := readColumn(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if err := apply(ctx, tx, was); err != nil {
			return core.Change{}, err
		}
		change := core.Change{Entity: "column", EntityID: id, Action: action, Before: was}
		if action == "delete" {
			return change, nil
		}
		if change.After, err = readColumn(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return change, nil
	})
}

func readColumn(ctx context.Context, q store.Querier, id int64) (Column, error) {
	var c Column
	err := q.QueryRowContext(ctx,
		`SELECT id, proposition_id, name, position FROM columns WHERE id = ?`, id).
		Scan(&c.ID, &c.Proposition, &c.Name, &c.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return Column{}, core.ErrNotFound
	}
	return c, err
}

func (s *Service) RenameColumn(ctx context.Context, a core.Actor, id int64, name string) (core.Event, error) {
	name, err := Field(name, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if name == "" {
		return core.Event{}, ErrEmpty
	}
	return s.column(ctx, a, id, auth.CanEdit, "rename", func(ctx context.Context, tx *sql.Tx, _ Column) error {
		_, err := tx.ExecContext(ctx, `UPDATE columns SET name = ? WHERE id = ?`, name, id)
		return err
	})
}

func (s *Service) MoveColumn(ctx context.Context, a core.Actor, id, after int64) (core.Event, error) {
	return s.column(ctx, a, id, auth.CanEdit, "move", func(ctx context.Context, tx *sql.Tx, was Column) error {
		position, err := place(ctx, tx, "columns", "proposition_id", was.Proposition, after, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE columns SET position = ? WHERE id = ?`, position, id)
		return err
	})
}

// DeleteColumn refuses a column with cards in it, because the cascade would
// take them without the person who pressed it being told.
func (s *Service) DeleteColumn(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.column(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx, _ Column) error {
		var cards int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM cards WHERE column_id = ?`, id).Scan(&cards); err != nil {
			return err
		}
		if cards > 0 {
			return ErrColumnNotEmpty
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM columns WHERE id = ?`, id)
		return err
	})
}

// Cards.

func (s *Service) CreateCard(ctx context.Context, a core.Actor, column int64, title string, assignees []int64) (core.Event, error) {
	title, err := Field(title, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if title == "" {
		return core.Event{}, ErrEmpty
	}
	if len(assignees) > maxAssignees {
		return core.Event{}, ErrTooLong
	}
	proposition, err := propositionOf(ctx, s.DB, columnScope, column)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "card", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		position, err := last(ctx, tx, "cards", "column_id", column)
		if err != nil {
			return core.Change{}, err
		}
		var by any
		if a.Kind == core.KindUser && a.ID != 0 {
			by = a.ID
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO cards
			(proposition_id, column_id, position, title, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, unixepoch())`, proposition, column, position, title, by)
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		for _, u := range assignees {
			if err := assignCard(ctx, tx, id, proposition, u); err != nil {
				return core.Change{}, err
			}
		}
		card, err := GetCard(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "card", EntityID: id, Action: "create", After: card}, nil
	})
}

// card is the shape every card command has.
func (s *Service) card(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx, Card) error) (core.Event, error) {
	proposition, err := propositionOf(ctx, s.DB, cardScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, need, "card", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := GetCard(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if err := apply(ctx, tx, was); err != nil {
			return core.Change{}, err
		}
		change := core.Change{Entity: "card", EntityID: id, Action: action, Before: was}
		if action == "delete" {
			return change, nil
		}
		if change.After, err = GetCard(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return change, nil
	})
}

// EditCardTitle and EditCardDescription take the version the editor started
// from. A card has one version across both fields, so an edit that began before
// somebody else's is refused with the text that is now there.
func (s *Service) EditCardTitle(ctx context.Context, a core.Actor, id, base int64, title string) (core.Event, error) {
	title, err := Field(title, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if title == "" {
		return core.Event{}, ErrEmpty
	}
	return s.card(ctx, a, id, auth.CanEdit, "edit", func(ctx context.Context, tx *sql.Tx, was Card) error {
		if was.Version != base {
			return &core.ConflictError{Entity: "card", EntityID: id, Field: "title",
				Version: was.Version, Current: was.Title}
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE cards SET title = ?, version = version + 1 WHERE id = ?`, title, id)
		return err
	})
}

func (s *Service) EditCardDescription(ctx context.Context, a core.Actor, id, base int64, description string) (core.Event, error) {
	description, err := Field(description, MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	return s.card(ctx, a, id, auth.CanEdit, "edit", func(ctx context.Context, tx *sql.Tx, was Card) error {
		if was.Version != base {
			return &core.ConflictError{Entity: "card", EntityID: id, Field: "description_md",
				Version: was.Version, Current: was.Description}
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE cards SET description_md = ?, version = version + 1 WHERE id = ?`,
			description, id)
		return err
	})
}

// MoveCard puts a card in a column, directly after another card or at the head
// when after is zero. Moves never conflict; the last one wins by server order.
func (s *Service) MoveCard(ctx context.Context, a core.Actor, id, column, after int64) (core.Event, error) {
	return s.card(ctx, a, id, auth.CanEdit, "move", func(ctx context.Context, tx *sql.Tx, was Card) error {
		var proposition int64
		err := tx.QueryRowContext(ctx, `SELECT proposition_id FROM columns WHERE id = ?`, column).Scan(&proposition)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && proposition != was.Proposition) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		position, err := place(ctx, tx, "cards", "column_id", column, after, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE cards SET column_id = ?, position = ? WHERE id = ?`, column, position, id)
		return err
	})
}

func (s *Service) AssignCard(ctx context.Context, a core.Actor, id, user int64) (core.Event, error) {
	return s.card(ctx, a, id, auth.CanEdit, "assign", func(ctx context.Context, tx *sql.Tx, card Card) error {
		return assignCard(ctx, tx, id, card.Proposition, user)
	})
}

// Owners can read every proposition; other assignees must be members.
// Repeated assignments remain idempotent.
func assignCard(ctx context.Context, tx *sql.Tx, card, proposition, user int64) error {
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO card_assignees (card_id, user_id)
		SELECT ?, id FROM users WHERE id = ? AND (role = 'owner' OR EXISTS (
			SELECT 1 FROM proposition_members
			WHERE proposition_id = ? AND user_id = users.id))`, card, user, proposition)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Either the user cannot read the proposition or they are already on the
		// card; only the first is worth refusing.
		var readable int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users
			WHERE id = ? AND (role = 'owner' OR EXISTS (
				SELECT 1 FROM proposition_members
				WHERE proposition_id = ? AND user_id = users.id))`,
			user, proposition).Scan(&readable); err != nil {
			return err
		}
		if readable == 0 {
			return core.ErrNotFound
		}
	}
	return nil
}

func (s *Service) UnassignCard(ctx context.Context, a core.Actor, id, user int64) (core.Event, error) {
	return s.card(ctx, a, id, auth.CanEdit, "unassign", func(ctx context.Context, tx *sql.Tx, _ Card) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM card_assignees WHERE card_id = ? AND user_id = ?`, id, user)
		return err
	})
}

func (s *Service) SetCardDue(ctx context.Context, a core.Actor, id int64, due string) (core.Event, error) {
	due, err := Field(due, MaxWord)
	if err != nil {
		return core.Event{}, err
	}
	// Clearing the date is the one empty value this takes. Everything else is
	// a calendar day, which also refuses the days a month does not have.
	if due != "" {
		if _, err := time.Parse("2006-01-02", due); err != nil {
			return core.Event{}, ErrDueDate
		}
	}
	return s.card(ctx, a, id, auth.CanEdit, "due", func(ctx context.Context, tx *sql.Tx, _ Card) error {
		_, err := tx.ExecContext(ctx, `UPDATE cards SET due_date = ? WHERE id = ?`,
			value(due), id)
		return err
	})
}

func (s *Service) SetCardQuestion(ctx context.Context, a core.Actor, id int64, question string) (core.Event, error) {
	question = strings.TrimSpace(question)
	if question != "" && !slices.Contains(Questions, question) {
		return core.Event{}, ErrQuestion
	}
	return s.card(ctx, a, id, auth.CanEdit, "question", func(ctx context.Context, tx *sql.Tx, _ Card) error {
		_, err := tx.ExecContext(ctx, `UPDATE cards SET question = ? WHERE id = ?`, value(question), id)
		return err
	})
}

func (s *Service) SetCardDone(ctx context.Context, a core.Actor, id int64, done bool) (core.Event, error) {
	action := "reopen"
	if done {
		action = "done"
	}
	return s.card(ctx, a, id, auth.CanEdit, action, func(ctx context.Context, tx *sql.Tx, _ Card) error {
		var at any
		if done {
			at = s.Now().Unix()
		}
		_, err := tx.ExecContext(ctx, `UPDATE cards SET done_at = ? WHERE id = ?`, at, id)
		return err
	})
}

func (s *Service) DeleteCard(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.card(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx, _ Card) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM cards WHERE id = ?`, id)
		return err
	})
}

// Checklist items.

func (s *Service) AddChecklistItem(ctx context.Context, a core.Actor, card int64, itemText string) (core.Event, error) {
	itemText, err := Field(itemText, MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if itemText == "" {
		return core.Event{}, ErrEmpty
	}
	proposition, err := propositionOf(ctx, s.DB, cardScope, card)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "checklist_item", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		position, err := last(ctx, tx, "checklist_items", "card_id", card)
		if err != nil {
			return core.Change{}, err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO checklist_items (card_id, text, position) VALUES (?, ?, ?)`,
			card, itemText, position)
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "checklist_item", EntityID: id, Action: "create",
			After: ChecklistItem{ID: id, CardID: card, Text: itemText, Position: position}}, nil
	})
}

func (s *Service) ToggleChecklistItem(ctx context.Context, a core.Actor, id int64, done bool) (core.Event, error) {
	return s.checklistItem(ctx, a, id, "toggle", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE checklist_items SET done = ? WHERE id = ?`, done, id)
		return err
	})
}

func (s *Service) RemoveChecklistItem(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.checklistItem(ctx, a, id, "delete", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM checklist_items WHERE id = ?`, id)
		return err
	})
}

func (s *Service) checklistItem(ctx context.Context, a core.Actor, id int64, action string,
	apply func(context.Context, *sql.Tx) error) (core.Event, error) {
	proposition, err := propositionOf(ctx, s.DB, checklistScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "checklist_item", action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		was, err := readChecklistItem(ctx, tx, id)
		if err != nil {
			return core.Change{}, err
		}
		if err := apply(ctx, tx); err != nil {
			return core.Change{}, err
		}
		change := core.Change{Entity: "checklist_item", EntityID: id, Action: action, Before: was}
		if action == "delete" {
			return change, nil
		}
		if change.After, err = readChecklistItem(ctx, tx, id); err != nil {
			return core.Change{}, err
		}
		return change, nil
	})
}

func readChecklistItem(ctx context.Context, q store.Querier, id int64) (ChecklistItem, error) {
	var it ChecklistItem
	err := q.QueryRowContext(ctx,
		`SELECT id, card_id, text, done, position FROM checklist_items WHERE id = ?`, id).
		Scan(&it.ID, &it.CardID, &it.Text, &it.Done, &it.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return ChecklistItem{}, core.ErrNotFound
	}
	return it, err
}

// Notes.

func (s *Service) PostComment(ctx context.Context, a core.Actor, card int64, body string) (core.Event, error) {
	body, err := Field(body, MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	if body == "" {
		return core.Event{}, ErrEmpty
	}
	proposition, err := propositionOf(ctx, s.DB, cardScope, card)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "comment", "create", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var by any
		if a.Kind == core.KindUser && a.ID != 0 {
			by = a.ID
		}
		at := s.Now().Unix()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO comments (card_id, user_id, body_md, created_at) VALUES (?, ?, ?, ?)`,
			card, by, body, at)
		if err != nil {
			return core.Change{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return core.Change{}, err
		}
		note := Comment{ID: id, CardID: card, Body: body, CreatedAt: at}
		if by != nil {
			user := a.ID
			note.UserID = &user
		}
		return core.Change{Entity: "comment", EntityID: id, Action: "create", After: note}, nil
	})
}

// DeleteComment removes your own note. Somebody else's stays where it is,
// whatever the role: the activity panel is a record, not a wall to moderate.
func (s *Service) DeleteComment(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	proposition, err := propositionOf(ctx, s.DB, commentScope, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "comment", "delete", func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var was Comment
		var user sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, card_id, user_id, body_md, created_at FROM comments WHERE id = ?`, id).
			Scan(&was.ID, &was.CardID, &user, &was.Body, &was.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Change{}, core.ErrNotFound
		}
		if err != nil {
			return core.Change{}, err
		}
		was.UserID = number(user)
		if a.Kind != core.KindUser || !user.Valid || user.Int64 != a.ID {
			return core.Change{}, ErrNotYours
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, id); err != nil {
			return core.Change{}, err
		}
		return core.Change{Entity: "comment", EntityID: id, Action: "delete", Before: was}, nil
	})
}
