// Package board is the propositions and the board on top of each of them:
// the rows, the queries that read them, and one command per mutation. Every
// command goes through core, so the browser, the websocket, the API and MCP
// all reach the same function with a different actor.
package board

import (
	"context"
	"database/sql"
	"errors"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

// Questions are the four the show asks of everything. A card carries one or none.
var Questions = []string{"I", "II", "III", "IV"}

// Defaults are what a new proposition starts with. They come from the settings
// table, which core knows nothing about, so the server hands them in.
type Defaults struct {
	Status  string
	Columns []string
	// Statuses is the whole list the rail groups by, so a command can refuse a
	// status the board would have nowhere to draw. An empty list is a workspace
	// that names none, and then any word is taken.
	Statuses []string
}

type Service struct {
	*core.Service
	// Defaults is read at the moment a proposition is created, so changing the
	// setting changes the next one without a restart.
	Defaults func() Defaults
	// Seed fills a proposition the moment it is made, in the transaction that
	// made it. The documents every proposition starts with live a layer above
	// this package, so the wiring sets this rather than board calling them.
	Seed func(ctx context.Context, a core.Actor, proposition int64) error
}

func New(c *core.Service, defaults func() Defaults) *Service {
	c.Read = readEntity
	c.Allow = archived
	return &Service{Service: c, Defaults: defaults}
}

// readEntity is how core reads a board row back after an undo. Undo writes
// columns; an event carries the row, so it reads it the way the commands do.
func readEntity(ctx context.Context, q store.Querier, entity string, id int64) (any, error) {
	switch entity {
	case "proposition":
		return GetProposition(ctx, q, id)
	case "card":
		return GetCard(ctx, q, id)
	case "column":
		return readColumn(ctx, q, id)
	case "checklist_item":
		return readChecklistItem(ctx, q, id)
	}
	return nil, core.ErrNotFound
}

// Nullable columns are pointers so that a JSON payload round-trips a NULL as
// null. Undo writes the before payload straight back into the row, and a
// missing due date that came back as "" or 0 would be a due date of zero.

type Proposition struct {
	ID         int64   `json:"id"`
	Number     int64   `json:"number"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	Statement  string  `json:"statement"`
	Blurb      string  `json:"blurb"`
	Status     string  `json:"status"`
	Episode    *string `json:"episode"`
	TargetDate *string `json:"target_date"`
	Position   string  `json:"position"`
	CreatedAt  int64   `json:"created_at"`
	ArchivedAt *int64  `json:"archived_at"`
	Members    []int64 `json:"members"`
}

type Column struct {
	ID          int64  `json:"id"`
	Proposition int64  `json:"proposition_id"`
	Name        string `json:"name"`
	Position    string `json:"position"`
}

type Card struct {
	ID          int64           `json:"id"`
	Proposition int64           `json:"proposition_id"`
	ColumnID    int64           `json:"column_id"`
	Position    string          `json:"position"`
	Title       string          `json:"title"`
	Description string          `json:"description_md"`
	Question    *string         `json:"question"`
	DueDate     *string         `json:"due_date"`
	DoneAt      *int64          `json:"done_at"`
	CreatedAt   int64           `json:"created_at"`
	Version     int64           `json:"version"`
	Assignees   []int64         `json:"assignees"`
	Checklist   []ChecklistItem `json:"checklist"`
	Comments    []Comment       `json:"comments"`
}

type ChecklistItem struct {
	ID       int64  `json:"id"`
	CardID   int64  `json:"card_id"`
	Text     string `json:"text"`
	Done     bool   `json:"done"`
	Position string `json:"position"`
}

type Comment struct {
	ID        int64  `json:"id"`
	CardID    int64  `json:"card_id"`
	UserID    *int64 `json:"user_id"`
	Body      string `json:"body_md"`
	CreatedAt int64  `json:"created_at"`
}

// Board is one proposition's columns and cards, with the sequence number the
// rest of the stream continues from.
type Board struct {
	Proposition int64    `json:"proposition"`
	Columns     []Column `json:"columns"`
	Cards       []Card   `json:"cards"`
	Seq         int64    `json:"seq"`
}

func text(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// Number is a nullable integer column as the pointer a JSON payload carries.
func Number(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// value turns an empty string into a NULL, which is what clearing an episode,
// a due date or a question means.
func value(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const propositionColumns = `id, number, kind, title, statement, blurb, status, episode, target_date,
	position, created_at, archived_at`

func scanProposition(rows interface{ Scan(...any) error }) (Proposition, error) {
	var p Proposition
	var episode, target sql.NullString
	var archived sql.NullInt64
	err := rows.Scan(&p.ID, &p.Number, &p.Kind, &p.Title, &p.Statement, &p.Blurb, &p.Status,
		&episode, &target, &p.Position, &p.CreatedAt, &archived)
	p.Episode, p.TargetDate, p.ArchivedAt = text(episode), text(target), Number(archived)
	p.Members = []int64{}
	return p, err
}

// GetShow reads the workspace shared by every user.
func GetShow(ctx context.Context, q store.Querier) (Proposition, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM propositions WHERE kind = 'show'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposition{}, core.ErrNotFound
	}
	if err != nil {
		return Proposition{}, err
	}
	return GetProposition(ctx, q, id)
}

// ListPropositions reads the rail: every proposition in its own order, with its
// members. Archived ones are included and carry their archived_at.
func ListPropositions(ctx context.Context, q store.Querier) ([]Proposition, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+propositionColumns+` FROM propositions ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Proposition{}
	index := map[int64]int{}
	for rows.Next() {
		p, err := scanProposition(rows)
		if err != nil {
			return nil, err
		}
		index[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	members, err := q.QueryContext(ctx,
		`SELECT proposition_id, user_id FROM proposition_members ORDER BY proposition_id, user_id`)
	if err != nil {
		return nil, err
	}
	defer members.Close()
	for members.Next() {
		var propID, userID int64
		if err := members.Scan(&propID, &userID); err != nil {
			return nil, err
		}
		if i, ok := index[propID]; ok {
			out[i].Members = append(out[i].Members, userID)
		}
	}
	return out, members.Err()
}

func GetProposition(ctx context.Context, q store.Querier, id int64) (Proposition, error) {
	p, err := scanProposition(q.QueryRowContext(ctx,
		`SELECT `+propositionColumns+` FROM propositions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposition{}, core.ErrNotFound
	}
	if err != nil {
		return Proposition{}, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT user_id FROM proposition_members WHERE proposition_id = ? ORDER BY user_id`, id)
	if err != nil {
		return Proposition{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var u int64
		if err := rows.Scan(&u); err != nil {
			return Proposition{}, err
		}
		p.Members = append(p.Members, u)
	}
	return p, rows.Err()
}

func ListColumns(ctx context.Context, q store.Querier, proposition int64) ([]Column, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, proposition_id, name, position FROM columns WHERE proposition_id = ? ORDER BY position`,
		proposition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Column{}
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.ID, &c.Proposition, &c.Name, &c.Position); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const cardColumns = `id, proposition_id, column_id, position, title, description_md,
	question, due_date, done_at, created_at, version`

func scanCard(rows interface{ Scan(...any) error }) (Card, error) {
	var c Card
	var question, due sql.NullString
	var done sql.NullInt64
	err := rows.Scan(&c.ID, &c.Proposition, &c.ColumnID, &c.Position, &c.Title, &c.Description,
		&question, &due, &done, &c.CreatedAt, &c.Version)
	c.Question, c.DueDate, c.DoneAt = text(question), text(due), Number(done)
	c.Assignees, c.Checklist, c.Comments = []int64{}, []ChecklistItem{}, []Comment{}
	return c, err
}

// GetCard reads one card whole, which is what every card event carries so that
// a tab can replace the card it holds rather than patch it field by field.
func GetCard(ctx context.Context, q store.Querier, id int64) (Card, error) {
	c, err := scanCard(q.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, core.ErrNotFound
	}
	if err != nil {
		return Card{}, err
	}
	cards := map[int64]*Card{c.ID: &c}
	if err := fillCards(ctx, q, `card_id = ?`, id, cards); err != nil {
		return Card{}, err
	}
	return c, nil
}

// Load reads a whole board in four queries, which is the payload the page is
// rendered with and the state the websocket then keeps up to date.
func Load(ctx context.Context, q store.Querier, proposition int64) (Board, error) {
	b := Board{Proposition: proposition, Cards: []Card{}}
	// The sequence number is read before the rows it stands for, not after. A
	// change that lands in between is then replayed by the catch up rather
	// than skipped by it, and replaying one costs nothing: every event carries
	// the whole row.
	var seq sql.NullInt64
	if err := q.QueryRowContext(ctx,
		`SELECT max(id) FROM activity WHERE proposition_id = ?`, proposition).Scan(&seq); err != nil {
		return b, err
	}
	b.Seq = seq.Int64

	cols, err := ListColumns(ctx, q, proposition)
	if err != nil {
		return b, err
	}
	b.Columns = cols

	rows, err := q.QueryContext(ctx, `SELECT `+cardColumns+` FROM cards
		WHERE proposition_id = ? ORDER BY position`, proposition)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return b, err
		}
		b.Cards = append(b.Cards, c)
	}
	if err := rows.Err(); err != nil {
		return b, err
	}

	byID := make(map[int64]*Card, len(b.Cards))
	for i := range b.Cards {
		byID[b.Cards[i].ID] = &b.Cards[i]
	}
	where := `card_id IN (SELECT id FROM cards WHERE proposition_id = ?)`
	if err := fillCards(ctx, q, where, proposition, byID); err != nil {
		return b, err
	}
	return b, nil
}

// fillCards hangs the assignees, checklist and notes on the cards already read.
func fillCards(ctx context.Context, q store.Querier, where string, arg any, cards map[int64]*Card) error {
	assignees, err := q.QueryContext(ctx,
		`SELECT card_id, user_id FROM card_assignees WHERE `+where+` ORDER BY card_id, user_id`, arg)
	if err != nil {
		return err
	}
	defer assignees.Close()
	for assignees.Next() {
		var cardID, userID int64
		if err := assignees.Scan(&cardID, &userID); err != nil {
			return err
		}
		if c, ok := cards[cardID]; ok {
			c.Assignees = append(c.Assignees, userID)
		}
	}
	if err := assignees.Err(); err != nil {
		return err
	}

	items, err := q.QueryContext(ctx,
		`SELECT id, card_id, text, done, position FROM checklist_items WHERE `+where+
			` ORDER BY card_id, position`, arg)
	if err != nil {
		return err
	}
	defer items.Close()
	for items.Next() {
		var it ChecklistItem
		if err := items.Scan(&it.ID, &it.CardID, &it.Text, &it.Done, &it.Position); err != nil {
			return err
		}
		if c, ok := cards[it.CardID]; ok {
			c.Checklist = append(c.Checklist, it)
		}
	}
	if err := items.Err(); err != nil {
		return err
	}

	notes, err := q.QueryContext(ctx,
		`SELECT id, card_id, user_id, body_md, created_at FROM comments WHERE `+where+
			` ORDER BY card_id, created_at, id`, arg)
	if err != nil {
		return err
	}
	defer notes.Close()
	for notes.Next() {
		var n Comment
		var user sql.NullInt64
		if err := notes.Scan(&n.ID, &n.CardID, &user, &n.Body, &n.CreatedAt); err != nil {
			return err
		}
		n.UserID = Number(user)
		if c, ok := cards[n.CardID]; ok {
			c.Comments = append(c.Comments, n)
		}
	}
	return notes.Err()
}

// Readable is the one visibility test in the app: an owner reads every
// proposition, everybody else reads the ones they are a member of. The rail,
// the page, the socket, the settings page and the fallback all ask it, so a
// person who is not a member is not told a proposition exists.
func Readable(ctx context.Context, q store.Querier, u *store.User, proposition int64) (bool, error) {
	if u == nil || proposition == 0 {
		return false, nil
	}
	if u.Role == auth.RoleOwner {
		var n int
		err := q.QueryRowContext(ctx,
			`SELECT count(*) FROM propositions WHERE id = ?`, proposition).Scan(&n)
		return n == 1, err
	}
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM proposition_members WHERE proposition_id = ? AND user_id = ?`,
		proposition, u.ID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Memberships is the same test for a whole rail in one query: every
// proposition this person is a member of. An owner is a member of everything
// whether or not a row says so, so callers check the role as well.
func Memberships(ctx context.Context, q store.Querier, userID int64) (map[int64]bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT proposition_id FROM proposition_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
