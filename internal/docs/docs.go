// Package docs is the document under the board: an ordered list of blocks in
// SQLite, merged on the server when two people edit one block at once, and
// mirrored to markdown on disk where an editor or an agent with a shell can
// change it back. Every mutation is a core command, so the browser, the
// websocket, the REST API and MCP all reach the same function.
package docs

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/markdown"
	"github.com/davidtorcivia/theses/internal/store"
)

// Reasons a revision is kept, as the column allows them.
const (
	ReasonManual    = "manual"
	ReasonPeriodic  = "periodic"
	ReasonPreImport = "pre-import"
)

// RevisionEvery is how often a document being edited is snapshotted. The timer
// is armed by an edit and re-arms only while edits keep arriving, so a document
// nobody is touching costs nothing.
const RevisionEvery = 10 * time.Minute

// Nullable columns are pointers so a JSON payload round-trips a NULL as null.
// Undo writes the before payload straight back into the row, and a deleted_at
// that came back as 0 would be a tombstone dated the epoch.

// Document is one tab above the document area. revision counts every change to
// its blocks and is what the markdown mirror writes into the file.
type Document struct {
	ID          int64   `json:"id"`
	Proposition int64   `json:"proposition_id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Position    float64 `json:"position"`
	CreatedBy   *int64  `json:"created_by"`
	CreatedAt   int64   `json:"created_at"`
	Revision    int64   `json:"revision"`
}

// Block is one paragraph, heading or list. Position is a fractional index, so
// an insert or a move never renumbers its neighbours.
type Block struct {
	ID        int64  `json:"id"`
	Document  int64  `json:"document_id"`
	Position  string `json:"position"`
	Text      string `json:"text"`
	Version   int64  `json:"version"`
	UpdatedBy *int64 `json:"updated_by"`
	UpdatedAt int64  `json:"updated_at"`
	DeletedAt *int64 `json:"deleted_at"`
}

// Doc is a document with the blocks that are still in it.
type Doc struct {
	Document
	Blocks []Block `json:"blocks"`
}

// Revision is one snapshot in the history list.
type Revision struct {
	ID        int64  `json:"id"`
	Document  int64  `json:"document_id"`
	Markdown  string `json:"markdown"`
	CreatedBy *int64 `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	Reason    string `json:"reason"`
}

type Service struct {
	*core.Service
	// Template is what the first document of a proposition starts from, read at
	// the moment it is created so changing the setting changes the next one.
	Template func() string
	// Every is the gap between periodic revisions, a field so a test does not
	// have to wait ten minutes for one.
	Every time.Duration
	// Debounce is how long the watcher waits for a file to settle, a field for
	// the same reason.
	Debounce time.Duration

	log *slog.Logger
	// root is the directory the markdown mirror lives under, which is
	// data/docs. Empty turns the mirror and its watcher off, which is what
	// every test that is not about the mirror wants.
	root string

	mu sync.Mutex
	// pending is the documents with a periodic revision timer armed. The entry
	// is deleted when a timer fires with nothing edited since the last one, so
	// the timers stop when the editing does.
	pending map[int64]*timer
	// written is the hash of the bytes this process last wrote to each mirror
	// file, and the document that file belongs to. An fsnotify event whose
	// content hashes to what is recorded here is this process hearing its own
	// write; a path that is not in here at all was never written by us and is
	// never imported.
	written map[string]mirrored
}

type timer struct {
	stop  *time.Timer
	actor core.Actor
	// edited is set by every command and cleared by the tick that acts on it.
	edited bool
}

type mirrored struct {
	document int64
	hash     string
}

// New builds the service and hangs the two document entities off core, so that
// undo reads them back the way the commands do and the archived rule board
// installed applies to them as well. board.New must have run first: it is what
// sets Allow, and a document command asks it before writing anything.
func New(c *core.Service, dir string, template func() string, log *slog.Logger) *Service {
	previous := c.Read
	c.Read = func(ctx context.Context, q store.Querier, entity string, id int64) (any, error) {
		switch entity {
		case "document":
			return GetDocument(ctx, q, id)
		case "block":
			return GetBlock(ctx, q, id)
		}
		if previous != nil {
			return previous(ctx, q, entity, id)
		}
		return nil, core.ErrNotFound
	}
	return &Service{
		Service: c, Template: template, Every: RevisionEvery, Debounce: Debounce,
		log: log, root: dir,
		pending: map[int64]*timer{}, written: map[string]mirrored{},
	}
}

// Markdown is a document as one string: its blocks in order, separated by blank
// lines. It is what a revision stores and what the mirror writes under the
// front matter.
func Markdown(blocks []Block) string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Text)
	}
	return strings.Join(out, "\n\n")
}

// RenderDocument is the document as HTML, rendered on the server by goldmark.
// The whole document goes through one pass so that a footnote defined in one
// block can be referenced from another.
func RenderDocument(blocks []Block) template.HTML {
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		texts = append(texts, b.Text)
	}
	return markdown.RenderDocument(texts)
}

const documentColumns = `id, proposition_id, name, slug, position, created_by, created_at, revision`

func scanDocument(row interface{ Scan(...any) error }) (Document, error) {
	var d Document
	var by sql.NullInt64
	err := row.Scan(&d.ID, &d.Proposition, &d.Name, &d.Slug, &d.Position, &by, &d.CreatedAt, &d.Revision)
	d.CreatedBy = number(by)
	return d, err
}

func number(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// GetDocument reads one document row, which is what every document event
// carries so a tab can replace the one it holds.
func GetDocument(ctx context.Context, q store.Querier, id int64) (Document, error) {
	d, err := scanDocument(q.QueryRowContext(ctx,
		`SELECT `+documentColumns+` FROM documents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, core.ErrNotFound
	}
	return d, err
}

const blockColumns = `id, document_id, position, text, version, updated_by, updated_at, deleted_at`

func scanBlock(row interface{ Scan(...any) error }) (Block, error) {
	var b Block
	var by, deleted sql.NullInt64
	err := row.Scan(&b.ID, &b.Document, &b.Position, &b.Text, &b.Version, &by, &b.UpdatedAt, &deleted)
	b.UpdatedBy, b.DeletedAt = number(by), number(deleted)
	return b, err
}

// GetBlock reads one block, tombstoned or not: undo reads back a block it has
// just restored, and the delete event carries the row it tombstoned.
func GetBlock(ctx context.Context, q store.Querier, id int64) (Block, error) {
	b, err := scanBlock(q.QueryRowContext(ctx,
		`SELECT `+blockColumns+` FROM blocks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Block{}, core.ErrNotFound
	}
	return b, err
}

// ListDocuments is one proposition's documents in tab order.
func ListDocuments(ctx context.Context, q store.Querier, proposition int64) ([]Document, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+documentColumns+` FROM documents WHERE proposition_id = ? ORDER BY position, id`,
		proposition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Blocks is one document's live blocks in order. Tombstones are left out:
// nothing but undo and the activity log has any use for them.
func Blocks(ctx context.Context, q store.Querier, document int64) ([]Block, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+blockColumns+` FROM blocks
		WHERE document_id = ? AND deleted_at IS NULL ORDER BY position, id`, document)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Block{}
	for rows.Next() {
		b, err := scanBlock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Load is every document of a proposition with its blocks, which is what the
// page is rendered with and what the socket then keeps up to date.
func Load(ctx context.Context, q store.Querier, proposition int64) ([]Doc, error) {
	documents, err := ListDocuments(ctx, q, proposition)
	if err != nil {
		return nil, err
	}
	out := make([]Doc, 0, len(documents))
	for _, d := range documents {
		blocks, err := Blocks(ctx, q, d.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, Doc{Document: d, Blocks: blocks})
	}
	return out, nil
}

// HistoryLimit is how many revisions the history list shows. A document is
// snapshotted at most once every ten minutes it is worked on, so this is a long
// way back and still one small query.
const HistoryLimit = 50

// ListRevisions is the history list, newest first, with the markdown each
// revision holds so the page can diff two of them without asking again.
func ListRevisions(ctx context.Context, q store.Querier, document int64) ([]Revision, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, document_id, markdown, created_by, created_at, reason
		FROM document_revisions WHERE document_id = ? ORDER BY id DESC LIMIT ?`, document, HistoryLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		var by sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Document, &r.Markdown, &by, &r.CreatedAt, &r.Reason); err != nil {
			return nil, err
		}
		r.CreatedBy = number(by)
		out = append(out, r)
	}
	return out, rows.Err()
}

// The queries that answer which proposition a row belongs to, which is what a
// command is authorised against and what a read is allowed by.
const (
	documentScope = `SELECT proposition_id FROM documents WHERE id = ?`
	blockScope    = `SELECT d.proposition_id FROM blocks b JOIN documents d ON d.id = b.document_id WHERE b.id = ?`
)

func scopeOf(ctx context.Context, q store.Querier, query string, id int64) (int64, error) {
	var proposition int64
	err := q.QueryRowContext(ctx, query, id).Scan(&proposition)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, core.ErrNotFound
	}
	return proposition, err
}

// PropositionOfDocument and PropositionOfBlock are how a surface that was given
// an id finds the proposition to check it against.
func PropositionOfDocument(ctx context.Context, q store.Querier, id int64) (int64, error) {
	return scopeOf(ctx, q, documentScope, id)
}

func PropositionOfBlock(ctx context.Context, q store.Querier, id int64) (int64, error) {
	return scopeOf(ctx, q, blockScope, id)
}

// DocumentOfBlock is the document a block belongs to.
func DocumentOfBlock(ctx context.Context, q store.Querier, id int64) (int64, error) {
	return scopeOf(ctx, q, `SELECT document_id FROM blocks WHERE id = ?`, id)
}

// readable is the one visibility test, the same board uses: an owner reads
// every proposition, everybody else reads the ones they are a member of. A
// proposition somebody is not a member of and one that does not exist answer
// the same way, because the rule is that they are not told it is there.
func (s *Service) readable(ctx context.Context, u *store.User, proposition int64) error {
	ok, err := board.Readable(ctx, s.DB, u, proposition)
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrNotFound
	}
	return nil
}

// Documents is one proposition's documents and blocks, as this person may read
// them.
func (s *Service) Documents(ctx context.Context, u *store.User, proposition int64) ([]Doc, error) {
	if err := s.readable(ctx, u, proposition); err != nil {
		return nil, err
	}
	return Load(ctx, s.DB, proposition)
}

// Document is one document with its blocks, as this person may read it.
func (s *Service) Document(ctx context.Context, u *store.User, id int64) (Doc, error) {
	proposition, err := PropositionOfDocument(ctx, s.DB, id)
	if err != nil {
		return Doc{}, err
	}
	if err := s.readable(ctx, u, proposition); err != nil {
		return Doc{}, err
	}
	d, err := GetDocument(ctx, s.DB, id)
	if err != nil {
		return Doc{}, err
	}
	blocks, err := Blocks(ctx, s.DB, id)
	if err != nil {
		return Doc{}, err
	}
	return Doc{Document: d, Blocks: blocks}, nil
}

// History is one document's revisions, as this person may read them.
func (s *Service) History(ctx context.Context, u *store.User, document int64) ([]Revision, error) {
	proposition, err := PropositionOfDocument(ctx, s.DB, document)
	if err != nil {
		return nil, err
	}
	if err := s.readable(ctx, u, proposition); err != nil {
		return nil, err
	}
	return ListRevisions(ctx, s.DB, document)
}
