package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

// Debounce is how long the watcher waits after the last change to a file before
// reading it. An editor writing a file is several events: a temporary file, a
// rename, sometimes a truncate and a write. Two seconds is long enough for all
// of them to settle and short enough that saving and switching to the browser
// shows the change already there.
const Debounce = 2 * time.Second

// fileActor is who a hand edit is recorded as. It has no user behind it, which
// is the whole reason the activity table has an actor kind at all.
var fileActor = core.Actor{Kind: core.KindFile}

var errMirrorStopped = errors.New("the document mirror is not running")

type mirrorPause struct {
	run   func() error
	reply chan error
}

type mirrorRun struct {
	watcher *fsnotify.Watcher
	sub     *core.Subscription

	pendMu  sync.Mutex
	pending map[string]bool
	wake    chan struct{}
	timers  map[string]*time.Timer
}

func (m *mirrorRun) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *mirrorRun) take() []string {
	m.pendMu.Lock()
	defer m.pendMu.Unlock()
	paths := make([]string, 0, len(m.pending))
	for path := range m.pending {
		paths = append(paths, path)
		delete(m.pending, path)
	}
	return paths
}

func (m *mirrorRun) close() {
	for path, timer := range m.timers {
		timer.Stop()
		delete(m.timers, path)
	}
	m.pendMu.Lock()
	clear(m.pending)
	m.pendMu.Unlock()
	m.sub.Close()
	_ = m.watcher.Close()
}

func (s *Service) startMirror(ctx context.Context) (*mirrorRun, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	m := &mirrorRun{
		watcher: watcher,
		sub:     s.Bus.Subscribe(0),
		pending: map[string]bool{},
		wake:    make(chan struct{}, 1),
		timers:  map[string]*time.Timer{},
	}
	for _, path := range s.catchUp(ctx, watcher) {
		if err := s.Import(ctx, path); err != nil {
			s.log.Warn("document not imported", "err", err)
		}
	}
	return m, nil
}

// Run keeps the markdown mirror and the database in step, in one goroutine: it
// writes a file for every applied command and imports a file somebody else
// wrote. It returns when ctx is done.
func (s *Service) Run(ctx context.Context) {
	defer s.Stop()
	close(s.mirrorStarted)
	defer close(s.mirrorDone)
	if s.root == "" {
		<-ctx.Done()
		return
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		s.log.Error("documents directory", "err", err)
		<-ctx.Done()
		return
	}
	run, err := s.startMirror(ctx)
	if err != nil {
		s.log.Error("document watcher", "err", err)
		return
	}
	defer func() { run.close() }()

	for {
		select {
		case <-ctx.Done():
			return
		case request := <-s.mirrorPause:
			run.close()
			// Periodic revisions refer to rows in the database being replaced. They
			// must not wake later and write a snapshot for a reused document id.
			s.pauseRevisionTimers()
			s.mu.Lock()
			clear(s.written)
			s.mu.Unlock()

			swapErr := request.run()
			if ctx.Err() != nil {
				request.reply <- errors.Join(swapErr, ctx.Err())
				return
			}
			next, resumeErr := s.startMirror(ctx)
			if resumeErr == nil {
				run = next
				resumeErr = s.verifyMirrors(ctx)
			}
			s.resumeRevisionTimers()
			request.reply <- errors.Join(swapErr, resumeErr)
			if next == nil {
				return
			}
		case e, ok := <-run.sub.C:
			if !ok {
				return
			}
			if run.sub.Dropped() > 0 {
				// Subscribe first so changes during the rescan wait on the new channel;
				// events queued around the drop are no longer ordered and are discarded.
				old := run.sub
				run.sub = s.Bus.Subscribe(0)
				old.Close()
				s.reconcile(ctx, run.watcher)
				continue
			}
			s.applied(ctx, run.watcher, e)
		case ev, ok := <-run.watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			path := filepath.Clean(ev.Name)
			if !strings.HasSuffix(path, ".md") {
				continue
			}
			if t, ok := run.timers[path]; ok {
				t.Reset(s.Debounce)
				continue
			}
			current := run
			current.timers[path] = time.AfterFunc(s.Debounce, func() {
				current.pendMu.Lock()
				current.pending[path] = true
				current.pendMu.Unlock()
				current.poke()
			})
		case <-run.wake:
			for _, path := range run.take() {
				delete(run.timers, path)
				if err := s.Import(ctx, path); err != nil {
					s.log.Warn("document not imported", "err", err)
				}
			}
		case err, ok := <-run.watcher.Errors:
			if !ok {
				return
			}
			s.log.Warn("document watcher", "err", err)
		}
	}
}

// WithMirrorPaused runs a database and filesystem swap while Run owns no
// watches or cached hashes for the old tree, then catches the installed tree up
// before returning. The callback runs directly when mirroring is disabled.
func (s *Service) WithMirrorPaused(ctx context.Context, run func() error) error {
	if s.root == "" {
		return run()
	}
	// Production starts Run before listening. Refusing before that point keeps a
	// background restore from waiting forever when a caller forgot the worker.
	select {
	case <-s.mirrorStarted:
	default:
		return errMirrorStopped
	}
	select {
	case <-s.mirrorDone:
		return errMirrorStopped
	default:
	}

	request := mirrorPause{run: run, reply: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.mirrorDone:
		return errMirrorStopped
	case s.mirrorPause <- request:
	}
	// Once Run accepts the request it owns the callback and the caller must not
	// return early: Restore would otherwise unfreeze writes and remove its staged
	// files while the callback was still swapping them.
	return <-request.reply
}

// verifyMirrors turns a known catch-up write failure into a restore failure.
// Existing hand-edited files remain valid: this only requires one file for
// every document row after the swap.
func (s *Service) verifyMirrors(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM documents`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var document int64
		if err := rows.Scan(&document); err != nil {
			return err
		}
		_, path, err := s.paths(ctx, document)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("document %d was not mirrored after restore: %w", document, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("document %d was not mirrored after restore: %s is not a file", document, path)
		}
	}
	return rows.Err()
}

// catchUp brings the mirror up to date with the database at start and returns
// the files it found already there, for the caller to read back: one written by
// an earlier run may hold an edit made while the process was down.
func (s *Service) catchUp(ctx context.Context, watcher *fsnotify.Watcher) []string {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM documents`)
	if err != nil {
		s.log.Error("documents not mirrored", "err", err)
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			s.log.Error("documents not mirrored", "err", err)
			return nil
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("documents not mirrored", "err", err)
		return nil
	}
	var found []string
	for _, id := range ids {
		_, path, err := s.paths(ctx, id)
		if err != nil {
			s.log.Warn("document not mirrored", "document", id, "err", err)
			continue
		}
		content, err := readMirror(path)
		if err == nil {
			// A file that is exactly what this version would write for the
			// document as it stands holds nothing to import. Importing it
			// anyway reads the blocks back through the parser, which splits a
			// block holding a blank line into two and treats the second as a
			// new block, mentions and all. Writing it again records the hash.
			same, err := s.unchanged(ctx, id, content)
			if err != nil {
				s.log.Warn("document file not read", "document", id, "err", err)
			}
			if same {
				if err := s.Mirror(ctx, id, nil, true); err != nil {
					s.log.Warn("document not mirrored", "document", id, "err", err)
					continue
				}
				s.watch(watcher, filepath.Dir(path))
				continue
			}
		}
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			// The file is this document's, but nothing here wrote it, so what
			// is in it is unknown: an empty hash matches nothing, which makes
			// the import read it rather than mistake it for our own write and
			// stops the next command writing over an edit made while the
			// process was down. A file that could not be read is one of these
			// too, because unread is unknown.
			d, err := GetDocument(ctx, s.DB, id)
			if err != nil {
				s.log.Warn("document not mirrored", "document", id, "err", err)
				continue
			}
			generation, err := s.documentGeneration(ctx, id, d.Proposition)
			if err != nil {
				s.log.Warn("document generation not read", "document", id, "err", err)
				continue
			}
			s.mu.Lock()
			s.written[path] = mirrored{document: id, proposition: d.Proposition, generation: generation}
			s.mu.Unlock()
			s.watch(watcher, filepath.Dir(path))
			found = append(found, path)
			continue
		}
		if err := s.Mirror(ctx, id, nil, false); err != nil {
			s.log.Warn("document not mirrored", "document", id, "err", err)
			continue
		}
		s.watch(watcher, filepath.Dir(path))
	}
	return found
}

// reconcile rebuilds mirrors after a bus hole. A tracked file stays attached
// only while its document, proposition, create event and path all match.
func (s *Service) reconcile(ctx context.Context, watcher *fsnotify.Watcher) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.proposition_id,
		coalesce((SELECT max(a.id) FROM activity a
			WHERE a.proposition_id = d.proposition_id AND a.entity = 'document'
				AND a.entity_id = CAST(d.id AS TEXT) AND a.action = 'create'), 0)
		FROM documents d`)
	if err != nil {
		s.log.Error("document mirrors not reconciled", "err", err)
		return
	}
	type mirrorRow struct {
		proposition int64
		generation  int64
		path        string
	}
	live := map[int64]mirrorRow{}
	for rows.Next() {
		var document, proposition, generation int64
		if err := rows.Scan(&document, &proposition, &generation); err != nil {
			rows.Close()
			s.log.Error("document mirrors not reconciled", "err", err)
			return
		}
		live[document] = mirrorRow{proposition: proposition, generation: generation}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.log.Error("document mirrors not reconciled", "err", err)
		return
	}
	if err := rows.Close(); err != nil {
		s.log.Error("document mirrors not reconciled", "err", err)
		return
	}
	for document, row := range live {
		_, path, err := s.paths(ctx, document)
		if err != nil {
			s.log.Warn("document path not reconciled", "document", document, "err", err)
			return
		}
		row.path = path
		live[document] = row
	}

	s.mu.Lock()
	var remove []string
	for path, m := range s.written {
		current, ok := live[m.document]
		switch {
		case !ok:
			remove = append(remove, path)
			delete(s.written, path)
		case current.proposition != m.proposition || current.generation != m.generation || path != current.path:
			// Preserve edited stale files, but detach them from the replacement row.
			content, err := readMirror(path)
			if errors.Is(err, os.ErrNotExist) || err == nil && m.hash != "" && hashOf(content) == m.hash {
				remove = append(remove, path)
			}
			delete(s.written, path)
		}
	}
	s.mu.Unlock()
	for _, path := range remove {
		if err := within(s.root, path); err != nil {
			continue
		}
		if err := waitOut(func() error { return os.Remove(path) }); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("stale document mirror not removed", "err", err)
		}
	}
	for document := range live {
		if err := s.Mirror(ctx, document, nil, false); err != nil {
			s.log.Warn("document not reconciled", "document", document, "err", err)
			continue
		}
		if dir, _, err := s.paths(ctx, document); err == nil && watcher != nil {
			s.watch(watcher, dir)
		}
	}
}

// unchanged reports a file that is what render writes for the document now.
// A file carrying a conflict marker is not, so it is read back the ordinary way.
func (s *Service) unchanged(ctx context.Context, document int64, content []byte) (bool, error) {
	d, err := GetDocument(ctx, s.DB, document)
	if err != nil {
		return false, err
	}
	blocks, err := Blocks(ctx, s.DB, document)
	if err != nil {
		return false, err
	}
	return hashOf(content) == hashOf(render(d, blocks, nil)), nil
}

// applied writes the file behind one command. fsnotify watches directories, so
// a proposition's directory is added the first time something is written into
// it.
func (s *Service) applied(ctx context.Context, watcher *fsnotify.Watcher, e core.Event) {
	// An import's own commands need no file written: it wrote one itself when
	// it finished, with the conflict markers on the blocks the database would
	// not give up, and writing again here would take those markers off.
	if e.Actor.Kind == core.KindFile {
		return
	}
	var document int64
	switch e.Entity {
	case "proposition":
		if e.Action != "delete" {
			return
		}
		var proposition board.Proposition
		if err := json.Unmarshal(e.Before, &proposition); err != nil || proposition.Number == 0 {
			s.log.Warn("deleted proposition not unmirrored", "proposition", e.EntityID)
			return
		}
		s.UnmirrorProposition(proposition.Number)
		return
	case "document":
		document = e.EntityID
		if e.Action == "delete" {
			s.Unmirror(document)
			return
		}
	case "block":
		document = documentOf(e)
	default:
		return
	}
	if document == 0 {
		return
	}
	if err := s.Mirror(ctx, document, nil, false); err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			s.log.Warn("document not mirrored", "document", document, "err", err)
		}
		return
	}
	if _, path, err := s.paths(ctx, document); err == nil {
		s.watch(watcher, filepath.Dir(path))
	}
}

// documentOf reads the document a block event belongs to out of the row it
// carries. A delete carries both sides; a create carries only the after.
func documentOf(e core.Event) int64 {
	for _, payload := range []([]byte){e.After, e.Before} {
		if len(payload) == 0 {
			continue
		}
		var b Block
		if err := json.Unmarshal(payload, &b); err == nil && b.Document != 0 {
			return b.Document
		}
	}
	return 0
}

func (s *Service) watch(watcher *fsnotify.Watcher, dir string) {
	if err := watcher.Add(dir); err != nil {
		s.log.Warn("document directory not watched", "err", err)
	}
}

// Import applies a hand edited markdown file. It is only ever asked about a
// path this process has itself written, so a stray file dropped into the docs
// directory is left where it is rather than turned into a document.
func (s *Service) Import(ctx context.Context, path string) error {
	s.mu.Lock()
	was, ours := s.written[path]
	s.mu.Unlock()
	if !ours {
		return nil
	}
	content, err := readMirror(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// The watcher hears its own writes. A file that hashes to what this process
	// last put there has nothing in it that did not come from the database.
	if hashOf(content) == was.hash {
		return nil
	}
	file, err := parseMirror(content)
	if err != nil {
		// Somebody has taken the front matter off. Their file, their call: it
		// is left alone rather than rewritten over.
		s.log.Warn("document file has no front matter", "err", err)
		return nil
	}
	if file.Document != was.document {
		s.log.Warn("document file names another document", "document", file.Document)
		return nil
	}

	var conflicted map[int64]bool
	err = s.Together(ctx, func(ctx context.Context) error {
		blocks, err := Blocks(ctx, s.Querier(ctx), was.document)
		if err != nil {
			return err
		}
		document, err := GetDocument(ctx, s.Querier(ctx), file.Document)
		if err != nil {
			return err
		}
		items := file.items()
		if sameAs(items, blocks) {
			conflicted = markedIn(file)
			return nil
		}
		if _, err := s.CreateRevision(ctx, fileActor, document.ID, ReasonPreImport); err != nil {
			return err
		}
		// Missing blocks are deletions only when the file saw this exact revision.
		fresh := file.Revision == document.Revision
		conflicted, _, _, err = s.applyItems(ctx, fileActor, document.ID, items, blocks, func(Block) missing {
			if fresh {
				return dropMissing
			}
			return markMissing
		})
		return err
	})
	if err != nil {
		return err
	}
	return s.Mirror(ctx, file.Document, conflicted, true)
}

// An item is one paragraph as a save says the document should read: the block
// it belongs to, the version that save was written from, and the text to put
// there. An id of nought is a paragraph with no block behind it, which goes in
// as a new one after whatever came before it.
type item struct {
	ID      int64
	Version int64
	Text    string
}

// An applied is what one item became: the block its text stands in now and the
// version it stands at, and whether the block ended up holding something other
// than what the item sent, which is the merge having folded somebody else's
// words in. The two together are what lets a caller press save again knowing
// exactly what that would assert.
type applied struct {
	Ref    BlockRef
	Merged bool
}

// missing is what becomes of a block the document still has that no item names.
type missing int

const (
	// keepMissing leaves it alone: the save was not written against it, so it
	// is somebody else's paragraph and none of this save's business.
	keepMissing missing = iota
	// dropMissing deletes it, which is a paragraph taken out.
	dropMissing
	// markMissing leaves it where it is and reports it, which is a paragraph
	// taken out of a block that has moved on since the save was written.
	markMissing
)

// applyItems writes a list of paragraphs over a document's blocks and reports
// the blocks it could not take, which are the ones somebody else changed while
// the save was being written. It is the whole of what an import applies and the
// whole of what a source save applies: the two differ in how they arrive at the
// list and in what they make of a block the list does not name, which is what
// gone answers.
//
// Both callers hold an immediate write transaction that stabilizes snapshot
// reads and rolls every command back on refusal.
//
// wrote says whether any command ran, which is how a source save tells a list
// that changed nothing from one that changed something: a set whose text the
// block already holds is not a command, and neither is a conflict.
//
// done is one entry per item, in item order, and is what a source save answers
// with so that the next one need guess nothing. An item whose text holds more
// than one paragraph, which only an import has, is answered by the first block
// of them; an import ignores the whole list.
func (s *Service) applyItems(ctx context.Context, a core.Actor, document int64, items []item,
	blocks []Block, gone func(Block) missing) (conflicted map[int64]bool, wrote bool, done []applied, err error) {
	live := map[int64]Block{}
	for _, b := range blocks {
		live[b.ID] = b
	}
	conflicted = map[int64]bool{}
	seen := map[int64]bool{}
	var after int64
	for _, item := range items {
		current, known := live[item.ID]
		if !known {
			// A paragraph with no block is one block, whatever happens to it,
			// so the answer for it is settled in this branch. A block made for
			// it holds exactly what was sent, so nothing was merged into it.
			done = append(done, applied{})
			// A paragraph somebody added, or one whose block the browser
			// deleted while the file was open. Either way the words are in the
			// file and not in the database, so they go in as a new block: a
			// block put back is undone with one more delete, and words thrown
			// away are gone.
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			// The paragraphs are cut here rather than left to the command.
			// InsertBlock answers with the first block when the text it is
			// given holds more than one paragraph, and following that one
			// would put the next chunk of the file in among them.
			for n, part := range Paragraphs(item.Text) {
				e, err := s.InsertBlock(ctx, a, document, after, "", part, false)
				if err != nil {
					return nil, false, nil, err
				}
				if n == 0 {
					done[len(done)-1].Ref, _ = rowOf(e, item.ID)
				}
				wrote = true
				after = e.EntityID
			}
			continue
		}
		seen[item.ID] = true
		after = item.ID
		// The block as it stands is the answer unless something below writes
		// to it: a conflict leaves it exactly here, holding their words and
		// none of this item's, which is reported as a conflict and not as a
		// merge.
		done = append(done, applied{Ref: BlockRef{ID: current.ID, Version: current.Version}})
		// An item carrying the block's own text is nothing to do, and saying so
		// here rather than after the cut is what keeps a block whose text
		// Paragraphs would cut, which is what a save made while somebody is
		// typing leaves behind, from being cut by a save that had nothing to
		// say about it.
		if item.Text == current.Text {
			continue
		}
		// The paragraphs are cut here for the same reason they are on the
		// branch above: a set whose text holds more than one paragraph writes
		// the first to the named block and puts the rest in after it, and
		// answers with the named one, so whatever came next in the file would
		// land in among them.
		parts := Paragraphs(item.Text)
		if len(parts) == 0 {
			parts = []string{""}
		}
		if parts[0] != current.Text {
			// The version the item carries is the version whoever wrote it
			// started from, so this is the same stale set the browser sends and
			// it goes through the same merge.
			if e, err := s.SetBlock(ctx, a, item.ID, item.Version, parts[0], false); err != nil {
				var clash *core.ConflictError
				if !errors.As(err, &clash) {
					return nil, false, nil, err
				}
				conflicted[item.ID] = true
			} else {
				ref, stored := rowOf(e, item.ID)
				done[len(done)-1] = applied{Ref: ref, Merged: stored != parts[0]}
				wrote = true
			}
		}
		// What was written under the block is words that are in the file and
		// not in the database, so it goes in whether or not the block itself
		// would take its own change, which is the rule the branch above uses.
		for _, part := range parts[1:] {
			e, err := s.InsertBlock(ctx, a, document, after, "", part, false)
			if err != nil {
				return nil, false, nil, err
			}
			wrote = true
			after = e.EntityID
		}
	}

	for _, b := range blocks {
		if seen[b.ID] {
			continue
		}
		switch gone(b) {
		case dropMissing:
			if _, err := s.DeleteBlock(ctx, a, b.ID); err != nil {
				return nil, false, nil, err
			}
			wrote = true
		case markMissing:
			conflicted[b.ID] = true
		}
	}
	return conflicted, wrote, done, nil
}

// rowOf is the block a command wrote and the text it left in it, read out of
// the row the event carries rather than asked for again, since the transaction
// it was written in has not committed and no other connection can see it yet.
// An event without a readable row answers with the id it was about at no
// version, which is a base the next save cannot line up against and refuses on
// rather than guesses.
func rowOf(e core.Event, fallback int64) (BlockRef, string) {
	var b Block
	if err := json.Unmarshal(e.After, &b); err == nil && b.ID != 0 {
		return BlockRef{ID: b.ID, Version: b.Version}, b.Text
	}
	if e.EntityID != 0 {
		return BlockRef{ID: e.EntityID}, ""
	}
	return BlockRef{ID: fallback}, ""
}

// markedIn is the blocks the file already carries a conflict marker on, which
// after a restart is all this process knows about them.
func markedIn(file fileDoc) map[int64]bool {
	out := map[int64]bool{}
	for _, item := range file.Blocks {
		if item.Conflicted && item.ID != 0 {
			out[item.ID] = true
		}
	}
	return out
}

// sameAs reports whether a list of items already says exactly what the database
// holds, which is every event the watcher hears about its own writes and every
// save, from the file or from the source view, that changed nothing.
func sameAs(items []item, blocks []Block) bool {
	if len(items) != len(blocks) {
		return false
	}
	for i, item := range items {
		if item.ID != blocks[i].ID || item.Text != blocks[i].Text {
			return false
		}
	}
	return true
}

// CheckMirror exercises startup reconciliation without serving or watching the workspace.
func (s *Service) CheckMirror(ctx context.Context) error {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	for _, path := range s.catchUp(ctx, watcher) {
		if err := s.Import(ctx, path); err != nil {
			return err
		}
	}
	return s.verifyMirrors(ctx)
}
