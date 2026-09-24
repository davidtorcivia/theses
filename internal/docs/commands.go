package docs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
	// ErrAfterBoth is an insert that names where it goes twice. The two are
	// different questions, an id and a key, and a caller that sent both has not
	// decided which it means.
	ErrAfterBoth = errors.New("an insert names after or after_key, not both")
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
// block, and a heading is always a block of its own, except inside a fenced
// code block, where neither cuts anything, because a fence cut in two is two
// halves of a fence. It is used for the template a document starts from, for
// text pasted into one block, and for the paragraphs an import finds in the
// markdown file, so that a block written to disk and read back is the same
// block.
func Paragraphs(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	out := []string{}
	var current []string
	// Everything that was only whitespace falls out here, which is what keeps a
	// file with a trailing newline from growing an empty block each import.
	flush := func() {
		if p := strings.TrimSpace(strings.Join(current, "\n")); p != "" {
			out = append(out, p)
		}
		current = nil
	}
	var f fence
	for _, line := range strings.Split(text, "\n") {
		switch {
		case f.track(line):
			current = append(current, line)
		case blank(line):
			flush()
		case heading.MatchString(line):
			flush()
			out = append(out, strings.TrimSpace(line))
		default:
			current = append(current, line)
		}
	}
	flush()
	return out
}

// blank is a line that ends a block, and the mirror file cuts blocks at the
// same one. Spaces and tabs only: a line holding a no-break space is a line
// with something on it, and the day it stopped being one every document holding
// one would be cut differently the next time anything touched it.
func blank(line string) bool { return strings.Trim(line, " \t") == "" }

// fence is where a line by line scan stands in a text: inside a fenced code
// block, holding the delimiter that opened it, or outside one. The web editor
// decides whether the caret is in code by the same rule, in step in
// web/static/app/blocktext.js.
type fence struct {
	char byte
	long int
}

// track takes the next line and answers whether it is code: the line that opens
// a fence, a line inside one, or the line that closes it. Those are the lines
// nothing else may cut at.
func (f *fence) track(line string) bool {
	char, long, info := fenceAt(line)
	switch {
	case char == 0:
		return f.char != 0
	case f.char == 0:
		// A backtick opener's info string holds no backtick, which is what
		// keeps a line of inline code from opening a fence that never closes.
		if char == '`' && strings.Contains(info, "`") {
			return false
		}
		f.char, f.long = char, long
	case char == f.char && long >= f.long && strings.TrimSpace(info) == "":
		f.char, f.long = 0, 0
	}
	return true
}

// fenceAt reads a line as a fence delimiter the way CommonMark does: up to
// three spaces, then at least three backticks or at least three tildes, and
// whatever follows them. A line that is not one answers with a zero character.
func fenceAt(line string) (char byte, long int, info string) {
	at := 0
	for at < 3 && at < len(line) && line[at] == ' ' {
		at++
	}
	if at == len(line) || (line[at] != '`' && line[at] != '~') {
		return 0, 0, ""
	}
	char = line[at]
	for at+long < len(line) && line[at+long] == char {
		long++
	}
	if long < 3 {
		return 0, 0, ""
	}
	return char, long, line[at+long:]
}

var (
	heading = regexp.MustCompile(`^#{1,6} `)
	notSlug = regexp.MustCompile(`[^a-z0-9]+`)
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
	// in as ordinary insert commands rather than as raw rows, because a block
	// that appeared in a document with nobody recorded as having written it is
	// a mutation the log does not hold.
	var created core.Event
	err = s.Together(ctx, func(ctx context.Context) error {
		e, start, err := s.createDocumentRow(ctx, a, proposition, name, template)
		if err != nil {
			return err
		}
		created = e
		after := int64(0)
		for _, text := range Paragraphs(start) {
			block, err := s.insertOne(ctx, a, proposition, e.EntityID, after, "", text)
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
			var statement, propositionTitle string
			if err := tx.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM documents WHERE proposition_id = ?),
				(SELECT statement FROM propositions WHERE id = ?),
				(SELECT title FROM propositions WHERE id = ?)`,
				proposition, proposition, proposition).Scan(&count, &statement, &propositionTitle); err != nil {
				return core.Change{}, err
			}
			// A proposition seeded the moment it was made has no statement yet,
			// and the template's first line would be a bare hash. The title is
			// what somebody typed, so it stands in until the statement exists.
			if strings.TrimSpace(statement) == "" {
				statement = propositionTitle
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
	proposition, err := PropositionOfDocument(ctx, s.Querier(ctx), id)
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
	return s.document(ctx, a, id, auth.CanDelete, "delete", func(ctx context.Context, tx *sql.Tx, was Document) error {
		if err := s.KeepDeleted(ctx, tx, was.Proposition, "document", id, was.Name); err != nil {
			return err
		}
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

// blockUnderKey is the block this actor's earlier command made, found by the
// key that command was sent under. It is how an insert names a block whose id
// the caller cannot know: a browser with no connection draws the block it has
// just made and queues the command that makes it, and a second block made below
// the first has only that key to point at.
//
// A command that made several blocks, which is a paste the server cut up, spent
// the key and then the key with a number on the end, one per block. What the
// key names is the last of them, because that is the block the next one belongs
// under.
//
// A key nobody spent, one spent by somebody else, and one whose command made no
// block are all the same answer: there is no such block. A key older than
// core.KeyLife has been forgotten, and the command that made the block is
// replayed rather than replied to, so it records the key afresh and this finds
// the block that replay made.
func blockUnderKey(ctx context.Context, tx *sql.Tx, a core.Actor, key string) (int64, error) {
	// The run's later keys are the key, a hash and a decimal number, so they sit
	// between the key with a hash on the end and the key with the character
	// after the last digit on it. It is a range rather than a pattern because
	// the key comes from a client: a pattern would have to be escaped, and a
	// caller sending wildcards would be matching keys it did not name.
	var id string
	err := tx.QueryRowContext(ctx, `SELECT a.entity_id
		FROM client_keys k JOIN activity a ON a.id = k.activity_id
		WHERE k.actor_id = ? AND (k.key = ? OR (k.key > ? AND k.key < ?))
		AND a.entity = 'block' AND a.action = 'insert'
		ORDER BY a.id DESC LIMIT 1`, a.ID, key, key+"#", key+"#:").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, core.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(id, 10, 64)
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
//
// afterKey is the other way of naming where it goes: the key this actor sent an
// earlier command under, meaning after the block that command made. A client
// that draws a block before the server has answered has no id to name, and two
// blocks made in a row with no connection are two commands in an outbox, the
// second of which names the first. Exactly one of the two may be given.
//
// whole is the same flag block.set has, and it is the editor splitting a block
// under somebody's caret: the text is stored exactly as it was sent and is
// always one block, so what comes back is what went up and the half paragraph
// they are in the middle of writing is not trimmed or cut up on the way.
func (s *Service) InsertBlock(ctx context.Context, a core.Actor, document, after int64, afterKey, text string, whole bool) (core.Event, error) {
	if after != 0 && afterKey != "" {
		return core.Event{}, ErrAfterBoth
	}
	if whole {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		if err := board.Fits(text, board.MaxBody); err != nil {
			return core.Event{}, err
		}
	} else {
		field, err := board.Field(text, board.MaxBody)
		if err != nil {
			return core.Event{}, err
		}
		text = field
	}
	proposition, err := PropositionOfDocument(ctx, s.Querier(ctx), document)
	if err != nil {
		return core.Event{}, err
	}
	parts := []string{text}
	if !whole {
		if parts = Paragraphs(text); len(parts) == 0 {
			parts = []string{""}
		}
	}
	if len(parts) == 1 {
		e, err := s.insertOne(ctx, a, proposition, document, after, afterKey, parts[0])
		if err == nil {
			s.touch(document, a)
		}
		return e, err
	}
	var first core.Event
	err = s.Together(ctx, func(ctx context.Context) error {
		for i, part := range parts {
			e, err := s.insertOne(ctx, a, proposition, document, after, afterKey, part)
			if err != nil {
				return err
			}
			if i == 0 {
				first = e
			}
			// Every paragraph after the first goes behind the one before it,
			// which is an id this transaction has just made and no longer a key.
			after = e.EntityID
			afterKey = ""
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
func (s *Service) insertOne(ctx context.Context, a core.Actor, proposition, document, after int64,
	afterKey, text string) (core.Event, error) {
	e, err := s.do(ctx, a, proposition, auth.CanEdit, "block", "insert",
		func(ctx context.Context, tx *sql.Tx) (core.Change, error) {
			if afterKey != "" {
				resolved, err := blockUnderKey(ctx, tx, a, afterKey)
				if err != nil {
					return core.Change{}, err
				}
				after = resolved
			}
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
	proposition, err := PropositionOfBlock(ctx, s.Querier(ctx), id)
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
//
// whole is a save made while somebody is typing. The text is stored exactly as
// it was sent: trimming the blank line they are in the middle of writing, or
// cutting the paragraph above the caret off into a block of its own, is what a
// save every few hundred milliseconds must not do. Everything else is the same,
// the merge included. A block saved this way keeps what was typed into it until
// an ordinary set, the API, MCP or an import from the markdown mirror touches
// it, and each of those trims and cuts as it always has.
func (s *Service) SetBlock(ctx context.Context, a core.Actor, id, base int64, text string, whole bool) (core.Event, error) {
	if whole {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		if err := board.Fits(text, board.MaxBody); err != nil {
			return core.Event{}, err
		}
		return s.setOne(ctx, a, id, base, text)
	}
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
	proposition, err := PropositionOfBlock(ctx, s.Querier(ctx), id)
	if err != nil {
		return core.Event{}, err
	}
	document, err := DocumentOfBlock(ctx, s.Querier(ctx), id)
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
			inserted, err := s.insertOne(ctx, a, proposition, document, after, "", part)
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

// baseText recovers the text a block held at a given version. The last twenty
// versions of every block are in block_texts, written by the triggers migration
// 004 put on the blocks table, so this is a primary key read.
//
// The activity log is the fallback, and it is there for the versions written
// before that migration ran: every block.set writes the whole row into the log
// as its after, so the text at a version is the after of the row that produced
// it, and block.insert writes the same row at version one. Nothing was
// backfilled. core.Compact folds runs of typed saves into one row, which takes
// the intermediate texts of that era away, so a base older than twenty versions
// and older than the fold is not recoverable and the set becomes a conflict
// carrying the text the block holds now. That is the honest answer: the editor
// is offered keep mine and take theirs rather than a merge against a guess.
func baseText(ctx context.Context, q store.Querier, id, version int64) (string, bool, error) {
	var text string
	err := q.QueryRowContext(ctx,
		`SELECT text FROM block_texts WHERE block_id = ? AND version = ?`, id, version).Scan(&text)
	if err == nil {
		return text, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	err = q.QueryRowContext(ctx, `SELECT json_extract(after_json, '$.text') FROM activity
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
// Which is why this asks for the standing to edit rather than the standing to
// delete: taking a paragraph out of a document is writing the document, and a
// researcher who may write one may take out the empty block they just left.
// Deleting the document itself is the other thing and still asks for delete.
func (s *Service) DeleteBlock(ctx context.Context, a core.Actor, id int64) (core.Event, error) {
	return s.block(ctx, a, id, auth.CanEdit, "delete", func(ctx context.Context, tx *sql.Tx, _ Block) error {
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
	proposition, err := PropositionOfDocument(ctx, s.Querier(ctx), document)
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
			r.CreatedBy = board.Number(by)
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
	if s.revisionPaused {
		return
	}
	if t, ok := s.pending[document]; ok {
		t.edited, t.actor = true, a
		return
	}
	t := &timer{actor: a, edited: true}
	t.stop = time.AfterFunc(s.Every, func() {
		s.mu.Lock()
		if s.pending[document] != t {
			s.mu.Unlock()
			return
		}
		s.revisionActive++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.revisionActive--
			s.revisionCond.Broadcast()
			s.mu.Unlock()
		}()
		s.tick(document, t)
	})
	s.pending[document] = t
}

func (s *Service) tick(document int64, expected *timer) {
	s.mu.Lock()
	t, ok := s.pending[document]
	if !ok || t != expected {
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
	// Saving as somebody types means an edit can leave the document reading
	// exactly as the last snapshot does: a word written and taken back again,
	// or text merged into what was already there. Storing that copy would push
	// the versions worth restoring to off the end of the history list.
	same, err := s.matchesNewestRevision(ctx, document)
	if err != nil {
		s.log.Warn("periodic revision not kept", "document", document, "err", err)
		return
	}
	if same {
		return
	}
	if _, err := s.CreateRevision(ctx, who, document, ReasonPeriodic); err != nil {
		s.log.Warn("periodic revision not kept", "document", document, "err", err)
	}
}

// matchesNewestRevision reports whether the document reads exactly as its
// newest revision does. A document with no revisions yet does not match: the
// first snapshot is always worth keeping.
func (s *Service) matchesNewestRevision(ctx context.Context, document int64) (bool, error) {
	var newest string
	err := s.DB.QueryRowContext(ctx, `SELECT markdown FROM document_revisions
		WHERE document_id = ? ORDER BY id DESC LIMIT 1`, document).Scan(&newest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	blocks, err := Blocks(ctx, s.DB, document)
	if err != nil {
		return false, err
	}
	return Markdown(blocks) == newest, nil
}

// Stop cancels every armed timer, for a process on its way out.
func (s *Service) Stop() {
	s.pauseRevisionTimers()
	s.resumeRevisionTimers()
}

func (s *Service) pauseRevisionTimers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisionPaused = true
	for id, t := range s.pending {
		t.stop.Stop()
		delete(s.pending, id)
	}
	for s.revisionActive > 0 {
		s.revisionCond.Wait()
	}
}

func (s *Service) resumeRevisionTimers() {
	s.mu.Lock()
	s.revisionPaused = false
	s.mu.Unlock()
}
