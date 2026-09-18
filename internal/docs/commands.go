package docs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/frac"
	"github.com/davidtorcivia/theses/internal/merge"
	"github.com/davidtorcivia/theses/internal/store"
)

var (
	// ErrNameTaken is a second document with the same name in one proposition.
	// Names become the slug the mirror file is written under, so two of them
	// would be one file.
	ErrNameTaken = errors.New("there is already a document with that name here")
	// ErrReason is a revision asked for with a reason the column does not hold.
	ErrReason = errors.New("that is not a reason to keep a revision")
	// ErrTooManyDocuments is more tabs than the document area can draw.
	ErrTooManyDocuments = errors.New("that proposition has as many documents as it takes")
)

// maxDocuments is the number of tabs above the document. Past this the tabs
// wrap twice and the person meant to make a new proposition.
const maxDocuments = 50

// do is core.Do with the layer's own rule asked first, the way board's do is:
// a command that would write to an archived proposition never runs at all.
// Allow is board's, set when board.New ran, and the nil dereference if it did
// not is deliberate: a document service wired without one would otherwise write
// to archived propositions in silence.
func (s *Service) do(ctx context.Context, a core.Actor, proposition int64, need, entity, action string,
	apply func(context.Context, *sql.Tx) (core.Change, error)) (core.Event, error) {
	return s.Do(ctx, a, proposition, need, func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
		if err := s.Allow(ctx, tx, proposition, entity, action); err != nil {
			return core.Change{}, err
		}
		return apply(ctx, tx)
	})
}

// writer is the user column a write records, left NULL for the markdown
// watcher, which is a file rather than a person and has no row in users.
func writer(a core.Actor) any {
	if a.Kind == core.KindUser && a.ID != 0 {
		return a.ID
	}
	return nil
}

// Paragraphs is the one rule for cutting text into blocks: a blank line ends a
// block, and a heading is always a block of its own. It is used for the
// template a document starts from, for text pasted into one block, and for the
// paragraphs an import finds in the markdown file, so that a block written to
// disk and read back is the same block.
//
// ponytail: a fenced code block containing a blank line or a line starting with
// a hash is cut up by this. The upgrade path is a fence aware scan, worth it
// the day a document holds code.
func Paragraphs(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	out := []string{}
	for _, chunk := range blankLine.Split(text, -1) {
		var current []string
		flush := func() {
			if len(current) > 0 {
				out = append(out, strings.Join(current, "\n"))
				current = nil
			}
		}
		for _, line := range strings.Split(chunk, "\n") {
			if heading.MatchString(line) {
				flush()
				out = append(out, strings.TrimRight(line, " \t"))
				continue
			}
			current = append(current, line)
		}
		flush()
	}
	// Everything that was only whitespace falls out here, which is what keeps
	// a file with a trailing newline from growing an empty block each import.
	kept := out[:0]
	for _, p := range out {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	return kept
}

var (
	blankLine = regexp.MustCompile(`\n[ \t]*\n`)
	heading   = regexp.MustCompile(`^#{1,6} `)
	notSlug   = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slug is the name as it appears in a path: lower case, words joined by
// hyphens. A name with nothing usable in it becomes "untitled", because the
// mirror needs a filename either way.
func Slug(name string) string {
	s := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "untitled"
	}
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

// freeSlug returns the slug for name, with a number on the end if the plain one
// is already taken in this proposition. exclude is the document being renamed,
// which must not count as its own clash.
func freeSlug(ctx context.Context, tx *sql.Tx, proposition, exclude int64, name string) (string, error) {
	base := Slug(name)
	for n := 1; n <= maxDocuments+1; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM documents WHERE proposition_id = ? AND slug = ? AND id <> ?`,
			proposition, candidate, exclude).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return candidate, nil
		}
	}
	return "", ErrNameTaken
}

// Documents.

// CreateDocument makes a document and the blocks it starts from. The first
// document of a proposition starts from the workspace's template, with the
// proposition's own statement in place of the placeholder; every one after it
// starts from its own title, because a second document is not a second copy of
// the research outline.
func (s *Service) CreateDocument(ctx context.Context, a core.Actor, proposition int64, name string) (core.Event, error) {
	name, err := board.Field(name, board.MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if name == "" {
		return core.Event{}, board.ErrEmpty
	}
	template := ""
	if s.Template != nil {
		template = s.Template()
	}
	// The row and the blocks it starts from go in one transaction, so a
	// refusal partway through leaves no half made document, and the blocks go
	// in as ordinary insert commands rather than as raw rows: the text a block
	// holds at a version is recovered from the activity log, and a block that
	// never wrote one there could never be merged, only refused.
	var created core.Event
	err = s.Together(ctx, func(ctx context.Context) error {
		e, start, err := s.createDocumentRow(ctx, a, proposition, name, template)
		if err != nil {
			return err
		}
		created = e
		after := int64(0)
		for _, text := range Paragraphs(start) {
			block, err := s.insertOne(ctx, a, proposition, e.EntityID, after, text)
			if err != nil {
				return err
			}
			after = block.EntityID
		}
		return nil
	})
	return created, err
}

// createDocumentRow writes the document row and returns the text it starts
// from: the workspace's template for the first document of a proposition, with
// the proposition's own statement in place of the placeholder, and the title
// for every one after it, because a second document is not a second copy of the
// research outline.
func (s *Service) createDocumentRow(ctx context.Context, a core.Actor, proposition int64,
	name, template string) (core.Event, string, error) {
	var start string
	e, err := s.do(ctx, a, proposition, auth.CanEdit, "document", "create",
		func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
			var count int
			var statement string
			if err := tx.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM documents WHERE proposition_id = ?),
				(SELECT statement FROM propositions WHERE id = ?)`,
				proposition, proposition).Scan(&count, &statement); err != nil {
				return core.Change{}, err
			}
			if count >= maxDocuments {
				return core.Change{}, ErrTooManyDocuments
			}
			slug, err := freeSlug(ctx, tx, proposition, 0, name)
			if err != nil {
				return core.Change{}, err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO documents
				(proposition_id, name, slug, position, created_by, created_at)
				VALUES (?, ?, ?, (SELECT coalesce(max(position), 0) + 1 FROM documents WHERE proposition_id = ?),
				?, unixepoch())`, proposition, name, slug, proposition, writer(a))
			if err != nil {
				return core.Change{}, err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}

			start = "# " + name
			if count == 0 && strings.TrimSpace(template) != "" {
				start = strings.ReplaceAll(template, "{statement}", statement)
			}
			document, err := GetDocument(ctx, tx, id)
			if err != nil {
				return core.Change{}, err
			}
			return core.Change{Entity: "document", EntityID: id, Action: "create", After: document}, nil
		})
	return e, start, err
}

// document is the shape every command on one document has.
func (s *Service) document(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx, Document) error) (core.Event, error) {
	proposition, err := PropositionOfDocument(ctx, s.DB, id)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, need, "document", action,
		func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
			was, err := GetDocument(ctx, tx, id)
			if err != nil {
				return core.Change{}, err
			}
			if err := apply(ctx, tx, was); err != nil {
				return core.Change{}, err
			}
			change := core.Change{Entity: "document", EntityID: id, Action: action, Before: was}
			if action == "delete" {
				return change, nil
			}
			if change.After, err = GetDocument(ctx, tx, id); err != nil {
				return core.Change{}, err
			}
			return change, nil
		})
}

// RenameDocument moves the slug with the name, so the mirror file is named
// after what the tab says.
func (s *Service) RenameDocument(ctx context.Context, a core.Actor, id int64, name string) (core.Event, error) {
	name, err := board.Field(name, board.MaxLine)
	if err != nil {
		return core.Event{}, err
	}
	if name == "" {
		return core.Event{}, board.ErrEmpty
	}
	return s.document(ctx, a, id, auth.CanEdit, "rename", func(ctx context.Context, tx *sql.Tx, was Document) error {
		slug, err := freeSlug(ctx, tx, was.Proposition, id, name)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE documents SET name = ?, slug = ? WHERE id = ?`, name, slug, id)
		return err
	})
}

// DeleteDocument takes the blocks and the revisions with it, by the cascade.
func (s *Service) DeleteDocument(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.document(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx, _ Document) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, id)
		return err
	})
}

// Blocks.

// place returns the ordering key for a block that should sit directly after the
// block `after` in this document, or at the head when after is zero. Tombstoned
// blocks keep their keys and are counted here: an undo puts one back on the key
// it had, and a key given away in the meantime is a refusal it does not need to
// meet. exclude is the block being moved, which is not its own neighbor.
func place(ctx context.Context, tx *sql.Tx, document, after, exclude int64) (string, error) {
	lo := ""
	if after != 0 {
		err := tx.QueryRowContext(ctx,
			`SELECT position FROM blocks WHERE id = ? AND document_id = ?`, after, document).Scan(&lo)
		if errors.Is(err, sql.ErrNoRows) {
			return "", core.ErrNotFound
		}
		if err != nil {
			return "", err
		}
	}
	var hi sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT min(position) FROM blocks WHERE document_id = ? AND position > ? AND id <> ?`,
		document, lo, exclude).Scan(&hi); err != nil {
		return "", err
	}
	return frac.Between(lo, hi.String), nil
}

// LastBlock is the block an append goes after, or zero for an empty document.
func LastBlock(ctx context.Context, q store.Querier, document int64) (int64, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id FROM blocks
		WHERE document_id = ? AND deleted_at IS NULL ORDER BY position DESC, id DESC LIMIT 1`,
		document).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id.Int64, err
}

// InsertBlock puts a new block after another one, or at the head of the
// document when after is zero. Text holding more than one paragraph becomes
// more than one block, because a block is a paragraph.
func (s *Service) InsertBlock(ctx context.Context, a core.Actor, document, after int64, text string) (core.Event, error) {
	text, err := board.Field(text, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	proposition, err := PropositionOfDocument(ctx, s.DB, document)
	if err != nil {
		return core.Event{}, err
	}
	parts := Paragraphs(text)
	if len(parts) == 0 {
		parts = []string{""}
	}
	if len(parts) == 1 {
		e, err := s.insertOne(ctx, a, proposition, document, after, parts[0])
		if err == nil {
			s.touch(document, a)
		}
		return e, err
	}
	var first core.Event
	err = s.Together(ctx, func(ctx context.Context) error {
		for i, part := range parts {
			e, err := s.insertOne(ctx, a, proposition, document, after, part)
			if err != nil {
				return err
			}
			if i == 0 {
				first = e
			}
			after = e.EntityID
		}
		return nil
	})
	if err == nil {
		s.touch(document, a)
	}
	return first, err
}

// insertOne is told the proposition rather than looking it up, because the
// blocks a document starts from are written in the transaction that makes the
// document, where no other connection can yet see the row to look it up from.
func (s *Service) insertOne(ctx context.Context, a core.Actor, proposition, document, after int64, text string) (core.Event, error) {
	e, err := s.do(ctx, a, proposition, auth.CanEdit, "block", "insert",
		func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
			position, err := place(ctx, tx, document, after, 0)
			if err != nil {
				return core.Change{}, err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO blocks
				(document_id, position, text, updated_by, updated_at)
				VALUES (?, ?, ?, ?, unixepoch())`, document, position, text, writer(a))
			if err != nil {
				return core.Change{}, err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}
			block, err := GetBlock(ctx, tx, id)
			if err != nil {
				return core.Change{}, err
			}
			return core.Change{Entity: "block", EntityID: id, Action: "insert", After: block}, nil
		})
	return e, err
}

// block is the shape every command on one block has.
func (s *Service) block(ctx context.Context, a core.Actor, id int64, need, action string,
	apply func(context.Context, *sql.Tx, Block) error) (core.Event, error) {
	proposition, err := PropositionOfBlock(ctx, s.DB, id)
	if err != nil {
		return core.Event{}, err
	}
	var document int64
	e, err := s.do(ctx, a, proposition, need, "block", action,
		func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
			was, err := GetBlock(ctx, tx, id)
			if err != nil {
				return core.Change{}, err
			}
			// A tombstoned block is gone as far as every command is concerned.
			// Undo is what brings it back, and it writes the row itself.
			if was.DeletedAt != nil {
				return core.Change{}, core.ErrNotFound
			}
			document = was.Document
			if err := apply(ctx, tx, was); err != nil {
				return core.Change{}, err
			}
			// A delete carries the tombstoned row as well as the row before it:
			// undo puts a column back, and it has to see what it is undoing.
			now, err := GetBlock(ctx, tx, id)
			if err != nil {
				return core.Change{}, err
			}
			return core.Change{Entity: "block", EntityID: id, Action: action, Before: was, After: now}, nil
		})
	if err == nil {
		s.touch(document, a)
	}
	return e, err
}

// SetBlock writes a block's text. base is the version the editor started from:
// if it is the version the block still holds the text is applied, and if it is
// not the three way merge decides, with the text at base as the base. A merge
// that cannot be made honestly comes back as a conflict carrying the text the
// block holds now, so the editor offers keep mine and take theirs.
func (s *Service) SetBlock(ctx context.Context, a core.Actor, id, base int64, text string) (core.Event, error) {
	text, err := board.Field(text, board.MaxBody)
	if err != nil {
		return core.Event{}, err
	}
	parts := Paragraphs(text)
	if len(parts) == 0 {
		parts = []string{""}
	}
	if len(parts) == 1 {
		return s.setOne(ctx, a, id, base, parts[0])
	}
	proposition, err := PropositionOfBlock(ctx, s.DB, id)
	if err != nil {
		return core.Event{}, err
	}
	document, err := DocumentOfBlock(ctx, s.DB, id)
	if err != nil {
		return core.Event{}, err
	}
	// More than one paragraph went into one block, which is a paste. The first
	// stays where it is and the rest follow it, in one transaction so that a
	// conflict on the first leaves none of them behind.
	var first core.Event
	err = s.Together(ctx, func(ctx context.Context) error {
		e, err := s.setOne(ctx, a, id, base, parts[0])
		if err != nil {
			return err
		}
		first = e
		after := id
		for _, part := range parts[1:] {
			inserted, err := s.insertOne(ctx, a, proposition, document, after, part)
			if err != nil {
				return err
			}
			after = inserted.EntityID
		}
		return nil
	})
	return first, err
}

func (s *Service) setOne(ctx context.Context, a core.Actor, id, base int64, text string) (core.Event, error) {
	return s.block(ctx, a, id, auth.CanEdit, "set", func(ctx context.Context, tx *sql.Tx, was Block) error {
		applied := text
		if was.Version != base {
			basis, ok, err := baseText(ctx, tx, id, base)
			if err != nil {
				return err
			}
			var conflict bool
			if !ok {
				// Without the text the editor started from there is nothing to
				// merge against, and guessing would silently drop one side.
				conflict = true
			} else {
				applied, conflict = merge.Merge(basis, text, was.Text)
			}
			if conflict {
				return &core.ConflictError{Entity: "block", EntityID: id, Field: "text",
					Version: was.Version, Current: was.Text}
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE blocks
			SET text = ?, version = version + 1, updated_by = ?, updated_at = unixepoch()
			WHERE id = ?`, applied, writer(a), id)
		return err
	})
}

// baseText recovers the text a block held at a given version. Every block.set
// writes the whole row into the activity log as its after, so the text at a
// version is the after of the row that produced it; block.insert writes the
// same row at version one. Nothing else is kept: a per block column of previous
// text would be a second copy of what the log already holds, and it would hold
// only the one version back rather than every version an editor might have
// started from.
//
// ponytail: an activity log pruned one day would take the older bases with it,
// and a stale set whose base has been pruned becomes a conflict rather than a
// merge. The upgrade path is keeping the last few texts per block when that day
// comes.
func baseText(ctx context.Context, tx *sql.Tx, id, version int64) (string, bool, error) {
	var text string
	err := tx.QueryRowContext(ctx, `SELECT json_extract(after_json, '$.text') FROM activity
		WHERE entity = 'block' AND entity_id = ?
		  AND json_extract(after_json, '$.version') = ?
		ORDER BY id DESC LIMIT 1`, id, version).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return text, err == nil, err
}

// MoveBlock puts a block after another one in the same document, or at the head
// when after is zero. A move never conflicts with an edit: they write different
// columns and the version only guards the text.
func (s *Service) MoveBlock(ctx context.Context, a core.Actor, id, after int64) (core.Event, error) {
	return s.block(ctx, a, id, auth.CanEdit, "move", func(ctx context.Context, tx *sql.Tx, was Block) error {
		if after == id {
			return core.ErrNotFound
		}
		position, err := place(ctx, tx, was.Document, after, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE blocks SET position = ? WHERE id = ?`, position, id)
		return err
	})
}

// DeleteBlock tombstones a block. The row stays, keeping its ordering key and
// its text, so undo is a column put back rather than a row invented again.
func (s *Service) DeleteBlock(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.block(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx, _ Block) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE blocks SET deleted_at = unixepoch() WHERE id = ?`, id)
		return err
	})
}

// Revisions.

// CreateRevision stores the document as it is now. manual is the button,
// periodic is the timer while somebody is editing, and pre-import is what the
// markdown watcher takes before it applies anything from disk, so a bad hand
// edit is one restore away.
func (s *Service) CreateRevision(ctx context.Context, a core.Actor, document int64, reason string) (core.Event, error) {
	switch reason {
	case ReasonManual, ReasonPeriodic, ReasonPreImport:
	default:
		return core.Event{}, ErrReason
	}
	proposition, err := PropositionOfDocument(ctx, s.DB, document)
	if err != nil {
		return core.Event{}, err
	}
	return s.do(ctx, a, proposition, auth.CanEdit, "revision", "create",
		func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
			blocks, err := Blocks(ctx, tx, document)
			if err != nil {
				return core.Change{}, err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO document_revisions
				(document_id, markdown, created_by, created_at, reason)
				VALUES (?, ?, ?, unixepoch(), ?)`,
				document, Markdown(blocks), writer(a), reason)
			if err != nil {
				return core.Change{}, err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return core.Change{}, err
			}
			var r Revision
			var by sql.NullInt64
			if err := tx.QueryRowContext(ctx,
				`SELECT id, document_id, markdown, created_by, created_at, reason
				 FROM document_revisions WHERE id = ?`, id).
				Scan(&r.ID, &r.Document, &r.Markdown, &by, &r.CreatedAt, &r.Reason); err != nil {
				return core.Change{}, err
			}
			r.CreatedBy = number(by)
			return core.Change{Entity: "revision", EntityID: id, Action: "create", After: r}, nil
		})
}

// touch arms the periodic revision timer for a document somebody is editing.
// The first tick after an edit writes a revision and arms the next one; a tick
// with nothing edited since the last forgets the document, which is how the
// timers stop when the editing does.
func (s *Service) touch(document int64, a core.Actor) {
	if s.Every <= 0 || document == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.pending[document]; ok {
		t.edited, t.actor = true, a
		return
	}
	t := &timer{actor: a, edited: true}
	t.stop = time.AfterFunc(s.Every, func() { s.tick(document) })
	s.pending[document] = t
}

func (s *Service) tick(document int64) {
	s.mu.Lock()
	t, ok := s.pending[document]
	if !ok {
		s.mu.Unlock()
		return
	}
	edited, who := t.edited, t.actor
	t.edited = false
	if !edited {
		delete(s.pending, document)
		s.mu.Unlock()
		return
	}
	t.stop.Reset(s.Every)
	s.mu.Unlock()

	// The timer has no request behind it, so it carries a deadline of its own
	// rather than running until the process ends.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.CreateRevision(ctx, who, document, ReasonPeriodic); err != nil {
		s.log.Warn("periodic revision not kept", "document", document, "err", err)
	}
}

// Editing reports how many documents have a periodic revision timer armed,
// which is what a test asserts stops when the editing does.
func (s *Service) Editing() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Stop cancels every armed timer, for a process on its way out.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.pending {
		t.stop.Stop()
		delete(s.pending, id)
	}
}
