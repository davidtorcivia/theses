// Package files is the material beside the board: the links somebody pasted
// and the files somebody uploaded, both belonging to one proposition. Every
// mutation is a core command with the same actor, the same archived guard and
// the same membership rule the board uses, so the browser, the REST API and
// MCP reach the same function.
//
// The app never receives file bytes. It hands the browser a presigned URL, the
// browser talks to the bucket, and the server verifies afterwards that the
// object is there and the size it was promised.
package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/blob"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

// Service is the links and files commands. Bucket answers which bucket a
// folder's objects live in, so that the Recordings folder can be sent to a
// second bucket without anything here knowing about settings. HTTP is the
// outbound client link metadata is fetched with, safehttp's in production.
type Service struct {
	CompletionFailures atomic.Uint64
	*core.Service
	Bucket func(ctx context.Context, folder string) (*blob.Client, error)
	HTTP   *http.Client
	// ponytail: serialize upload setup, not file bytes; use per-key locks if setup throughput matters.
	createMu sync.Mutex
}

// New registers this package's entities with core and returns the service. It
// must be called after board.New, which sets the reader it chains onto and the
// archived rule both packages share.
func New(c *core.Service, bucket func(context.Context, string) (*blob.Client, error), client *http.Client) *Service {
	board := c.Read
	c.Read = func(ctx context.Context, q store.Querier, entity string, id int64) (any, error) {
		switch entity {
		case "link":
			return GetLink(ctx, q, id)
		case "file":
			return GetFile(ctx, q, id)
		}
		if board == nil {
			return nil, core.ErrNotFound
		}
		return board(ctx, q, entity, id)
	}
	return &Service{Service: c, Bucket: bucket, HTTP: client}
}

var (
	// ErrKind is a link kind or a folder that is not one of the ones offered.
	ErrKind = errors.New("that is not one of the kinds this list uses")
	// ErrQuestion is a question that is not one of the four.
	ErrQuestion = errors.New("that is not one of the four questions")
	// ErrURL is something pasted into the link field that is not a web address.
	ErrURL = errors.New("that is not an http or https address")
	// ErrState is an operation on a file that is not in the state for it: a
	// download of something still uploading, a second completion.
	ErrState = errors.New("that upload is not in a state for this")
	// ErrSize is an object that is not the size the upload said it would be.
	ErrSize = errors.New("the object in the bucket is not the size this upload declared")
	// ErrSwept is a completion for an upload the sweep has already abandoned.
	ErrSwept = errors.New("that upload was abandoned and its parts are gone")
	// ErrNoBucket is object storage that has not been configured yet.
	ErrNoBucket = errors.New("object storage is not set up yet; a workspace owner does that in settings")
	// ErrCrossBucket is a move between folders that live in different buckets.
	ErrCrossBucket = errors.New("that folder is in another bucket; download it and upload it again")
	// ErrPart is a part number outside the ones this upload has.
	ErrPart = errors.New("that part number is not in this upload")
	// ErrBadSize is an upload that declares nothing to send or more than the
	// largest object this app takes.
	ErrBadSize = fmt.Errorf("a file has to be between 1 byte and %d bytes", int64(maxFileSize))
)

// Kinds a link may be. The list is the one the drawer offers and the one
// internal/links guesses from, plus the two it never guesses but a person may
// choose.
var Kinds = []string{"article", "paper", "book", "essay", "video", "project", "dataset", "thread"}

// Folders a file may be filed under. Recordings is the one that may live in a
// bucket of its own.
var Folders = []string{"Documents", "Reading", "Recordings", "Art"}

// Recordings is the folder the second bucket, when there is one, holds.
const Recordings = "Recordings"

func known(value string, of []string) bool {
	for _, v := range of {
		if v == value {
			return true
		}
	}
	return false
}

func question(q string) (any, error) {
	if q == "" {
		return nil, nil
	}
	if !known(q, board.Questions) {
		return nil, ErrQuestion
	}
	return q, nil
}

// A Link is one row of the links list. TextForSearch is not in the JSON: it is
// a page's readable text kept for FTS5, it is large, and no client displays it.
type Link struct {
	ID           int64   `json:"id"`
	Proposition  int64   `json:"proposition_id"`
	URL          string  `json:"url"`
	CanonicalURL string  `json:"canonical_url"`
	Title        string  `json:"title"`
	Author       string  `json:"author"`
	Year         string  `json:"year"`
	Kind         string  `json:"kind"`
	Note         string  `json:"note_md"`
	Question     *string `json:"question"`
	AddedBy      *int64  `json:"added_by"`
	CreatedAt    int64   `json:"created_at"`
	FetchedAt    *int64  `json:"fetched_at"`
	// Citation is built from the fields above rather than stored, and it is on
	// the row rather than on a view of it so that every surface carries it: a
	// tab replaces the link it holds with the payload of an event, and a shape
	// that only the HTTP answer had would disappear the moment one arrived.
	Citation string `json:"citation"`
}

// A File is one row of the files list. The object key is in it because the
// drawer shows where a thing landed in the bucket, and knowing the key is not
// a way to read the object: every download is a presigned GET.
type File struct {
	CommentRevision int64  `json:"comment_revision"`
	Note            string `json:"note_md"`
	Tags            string `json:"tags"`
	MetadataVersion int64  `json:"metadata_version"`
	ID              int64  `json:"id"`
	Proposition     int64  `json:"proposition_id"`
	Name            string `json:"name"`
	Folder          string `json:"folder"`
	Kind            string `json:"kind"`
	Size            int64  `json:"size"`
	ObjectKey       string `json:"object_key"`
	VersionOf       *int64 `json:"version_of"`
	DurationMS      *int64 `json:"duration_ms"`
	Width           *int64 `json:"width"`
	Height          *int64 `json:"height"`
	UploadedBy      *int64 `json:"uploaded_by"`
	State           string `json:"state"`
	CreatedAt       int64  `json:"created_at"`
}

// Ready is whether the object is in the bucket and verified.
func (f File) Ready() bool { return f.State == stateReady }

const (
	stateUploading = "uploading"
	stateReady     = "ready"
)

const linkColumns = `id, proposition_id, url, canonical_url, title, author, year, kind,
	note_md, question, added_by, created_at, fetched_at`

func scanLink(row interface{ Scan(...any) error }) (Link, error) {
	var l Link
	var question sql.NullString
	var by, fetched sql.NullInt64
	err := row.Scan(&l.ID, &l.Proposition, &l.URL, &l.CanonicalURL, &l.Title, &l.Author,
		&l.Year, &l.Kind, &l.Note, &question, &by, &l.CreatedAt, &fetched)
	l.Question, l.AddedBy, l.FetchedAt = text(question), number(by), number(fetched)
	l.Citation = Citation(l)
	return l, err
}

const fileColumns = `id, proposition_id, name, folder, kind, size, object_key, version_of,
	duration_ms, width, height, uploaded_by, state, created_at, note_md, tags, metadata_version, comment_revision`

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var f File
	var versionOf, duration, width, height, by sql.NullInt64
	err := row.Scan(&f.ID, &f.Proposition, &f.Name, &f.Folder, &f.Kind, &f.Size, &f.ObjectKey,
		&versionOf, &duration, &width, &height, &by, &f.State, &f.CreatedAt, &f.Note, &f.Tags, &f.MetadataVersion, &f.CommentRevision)
	f.VersionOf, f.DurationMS = number(versionOf), number(duration)
	f.Width, f.Height, f.UploadedBy = number(width), number(height), number(by)
	return f, err
}

// Nullable columns are pointers so that a JSON payload round-trips a NULL as
// null: undo writes a before payload straight back into the row, and a missing
// year that came back as 0 would be a year of zero.

func text(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

func number(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func GetLink(ctx context.Context, q store.Querier, id int64) (Link, error) {
	l, err := scanLink(q.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM links WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, core.ErrNotFound
	}
	return l, err
}

func GetFile(ctx context.Context, q store.Querier, id int64) (File, error) {
	f, err := scanFile(q.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM files WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, core.ErrNotFound
	}
	return f, err
}

// ListLinks is the links pane: newest first, which is where a paste lands.
func (s *Service) ListLinks(ctx context.Context, a core.Actor, proposition int64) ([]Link, error) {
	if err := visible(ctx, s.DB, a, proposition); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+linkColumns+` FROM links WHERE proposition_id = ? ORDER BY created_at DESC, id DESC`,
		proposition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListFiles is the files pane. Superseded versions are left out: they are
// listed in the drawer of the file that replaced them, under Versions. Only a
// replacement that arrived counts, or an upload nobody finished would take the
// file it was replacing out of the list with it.
func (s *Service) ListFiles(ctx context.Context, a core.Actor, proposition int64) ([]File, error) {
	if err := visible(ctx, s.DB, a, proposition); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+fileColumns+` FROM files
		WHERE proposition_id = ? AND id NOT IN
			(SELECT version_of FROM files WHERE version_of IS NOT NULL AND state = 'ready')
		ORDER BY created_at DESC, id DESC`, proposition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Versions is the chain under one file, newest first: what it replaced, what
// that replaced, and so on. The file itself is not in the list.
func (s *Service) Versions(ctx context.Context, a core.Actor, id int64) ([]File, error) {
	f, err := s.readable(ctx, a, id)
	if err != nil {
		return nil, err
	}
	out := []File{}
	for f.VersionOf != nil {
		older, err := GetFile(ctx, s.DB, *f.VersionOf)
		if errors.Is(err, core.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		// A chain is built one link at a time by replacing the head, so it
		// cannot loop; the length check is the cheap guard against a row
		// somebody edited by hand.
		if len(out) > 100 {
			break
		}
		out = append(out, older)
		f = older
	}
	return out, nil
}

// ReadFile is one file, refusing a proposition the reader may not see.
func (s *Service) ReadFile(ctx context.Context, a core.Actor, id int64) (File, error) {
	return s.readable(ctx, a, id)
}

// readable reads one file and checks the reader may see its proposition.
func (s *Service) readable(ctx context.Context, a core.Actor, id int64) (File, error) {
	f, err := GetFile(ctx, s.DB, id)
	if err != nil {
		return File{}, err
	}
	return f, visible(ctx, s.DB, a, f.Proposition)
}

// visible is the read rule, and it answers ErrNotFound rather than ErrForbidden
// for everything: an owner reads every proposition, everybody else reads the
// ones they are a member of and is not told the others are there.
func visible(ctx context.Context, q store.Querier, a core.Actor, proposition int64) error {
	if a.Kind == core.KindFile {
		return nil
	}
	if a.Kind != core.KindUser || proposition == 0 {
		return core.ErrNotFound
	}
	var role string
	err := q.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, a.ID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	if err != nil {
		return err
	}
	if role == auth.RoleOwner {
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT count(*) FROM propositions WHERE id = ?`, proposition).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return core.ErrNotFound
		}
		return nil
	}
	var one int
	err = q.QueryRowContext(ctx,
		`SELECT 1 FROM proposition_members WHERE proposition_id = ? AND user_id = ?`,
		proposition, a.ID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	return err
}

// do is core.Do with the two rules this package shares with the board asked
// first: a proposition the actor may not see does not exist, and one that has
// been archived is read only. Both are asked inside the transaction, so the
// answer cannot go stale between the check and the write.
func (s *Service) do(ctx context.Context, a core.Actor, proposition int64, need, entity, action string,
	apply func(context.Context, *sql.Tx) (core.Change, error)) (core.Event, error) {
	// Asked once before the transaction and once inside it. The first is for
	// the answer the caller gets: core's own membership check refuses with
	// ErrForbidden, and somebody who is not a member is meant to be told the
	// proposition is not there. The second is the one that counts, because a
	// membership can be taken away between them.
	if err := visible(ctx, s.DB, a, proposition); err != nil {
		return core.Event{}, err
	}
	return s.Do(ctx, a, proposition, need, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if err := visible(ctx, tx, a, proposition); err != nil {
			return core.Change{}, err
		}
		// The archived rule is about what a person may still do to a
		// proposition that has been put away. The file actor is not a person
		// and has nothing to put away: it is the sweep clearing up after an
		// upload nobody finished, and an archived proposition is exactly where
		// one is most likely to be left. Without this the sweep refused itself
		// on the first such row and did so again every hour.
		if s.Allow != nil && a.Kind != core.KindFile {
			if err := s.Allow(ctx, tx, proposition, entity, action); err != nil {
				return core.Change{}, err
			}
		}
		return apply(ctx, tx)
	})
}

// propositionOf answers which proposition a row belongs to, which is what the
// command is authorized against. Nothing ever moves a link or a file to another
// proposition, so reading it outside the transaction is safe.
func (s *Service) propositionOf(ctx context.Context, query string, id int64) (int64, error) {
	var proposition int64
	err := s.DB.QueryRowContext(ctx, query, id).Scan(&proposition)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, core.ErrNotFound
	}
	return proposition, err
}

const (
	linkScope = `SELECT proposition_id FROM links WHERE id = ?`
	fileScope = `SELECT proposition_id FROM files WHERE id = ?`
	cardScope = `SELECT proposition_id FROM cards WHERE id = ?`
)

// by is the actor as a column value: the person, or NULL for the sweep.
func by(a core.Actor) any {
	if a.Kind == core.KindUser && a.ID != 0 {
		return a.ID
	}
	return nil
}

func (s *Service) now() int64 { return s.Now().Unix() }

// Attachments.

// A Join is one row of card_links or card_files, which is the whole payload of
// an attach or a detach.
type Join struct {
	CardID int64 `json:"card_id"`
	LinkID int64 `json:"link_id,omitempty"`
	FileID int64 `json:"file_id,omitempty"`
}

// AttachLink and the three that follow are the card drawer's picker. The card
// and the thing attached have to be in the same proposition, which is checked
// here rather than left to the join table: the tables have no constraint that
// could say so.
func (s *Service) AttachLink(ctx context.Context, a core.Actor, card, link int64) (core.Event, error) {
	return s.attach(ctx, a, "card_link", "attach", card, link)
}

func (s *Service) DetachLink(ctx context.Context, a core.Actor, card, link int64) (core.Event, error) {
	return s.attach(ctx, a, "card_link", "detach", card, link)
}

func (s *Service) AttachFile(ctx context.Context, a core.Actor, card, file int64) (core.Event, error) {
	return s.attach(ctx, a, "card_file", "attach", card, file)
}

func (s *Service) DetachFile(ctx context.Context, a core.Actor, card, file int64) (core.Event, error) {
	return s.attach(ctx, a, "card_file", "detach", card, file)
}

func (s *Service) attach(ctx context.Context, a core.Actor, entity, action string, card, id int64) (core.Event, error) {
	table, column, scope := "card_links", "link_id", linkScope
	if entity == "card_file" {
		table, column, scope = "card_files", "file_id", fileScope
	}
	proposition, err := s.propositionOf(ctx, cardScope, card)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, entity, action, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		var other int64
		err := tx.QueryRowContext(ctx, scope, id).Scan(&other)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && other != proposition) {
			return core.Change{}, core.ErrNotFound
		}
		if err != nil {
			return core.Change{}, err
		}
		row := Join{CardID: card}
		if entity == "card_link" {
			row.LinkID = id
		} else {
			row.FileID = id
		}
		statement := `INSERT OR IGNORE INTO ` + table + ` (card_id, ` + column + `) VALUES (?, ?)`
		change := core.Change{Entity: entity, EntityID: card, Action: action, After: row}
		if action == "detach" {
			statement = `DELETE FROM ` + table + ` WHERE card_id = ? AND ` + column + ` = ?`
			change = core.Change{Entity: entity, EntityID: card, Action: action, Before: row}
		}
		_, err = tx.ExecContext(ctx, statement, card, id)
		return change, err
	})
}

// Attachments are the links and files hanging off the cards of one proposition,
// which is what the board draws on the cards and what the card drawer lists.
type Attachments struct {
	Links []Join `json:"links"`
	Files []Join `json:"files"`
}

func (s *Service) Attachments(ctx context.Context, a core.Actor, proposition int64) (Attachments, error) {
	out := Attachments{Links: []Join{}, Files: []Join{}}
	if err := visible(ctx, s.DB, a, proposition); err != nil {
		return out, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT j.card_id, j.link_id FROM card_links j
		JOIN cards c ON c.id = j.card_id WHERE c.proposition_id = ? ORDER BY j.card_id, j.link_id`, proposition)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var j Join
		if err := rows.Scan(&j.CardID, &j.LinkID); err != nil {
			return out, err
		}
		out.Links = append(out.Links, j)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	fileRows, err := s.DB.QueryContext(ctx, `SELECT j.card_id, j.file_id FROM card_files j
		JOIN cards c ON c.id = j.card_id WHERE c.proposition_id = ? ORDER BY j.card_id, j.file_id`, proposition)
	if err != nil {
		return out, err
	}
	defer fileRows.Close()
	for fileRows.Next() {
		var j Join
		if err := fileRows.Scan(&j.CardID, &j.FileID); err != nil {
			return out, err
		}
		out.Files = append(out.Files, j)
	}
	return out, fileRows.Err()
}

// downloadTTL is how long a download link lives. Long enough to click and for a
// large object to start arriving, short enough that a URL copied out of the
// network tab is no use tomorrow.
const downloadTTL = 15 * time.Minute
