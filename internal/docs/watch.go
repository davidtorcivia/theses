package docs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

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

// Run keeps the markdown mirror and the database in step, in one goroutine: it
// writes a file for every applied command and imports a file somebody else
// wrote. It returns when ctx is done.
func (s *Service) Run(ctx context.Context) {
	defer s.Stop()
	if s.root == "" {
		<-ctx.Done()
		return
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		s.log.Error("documents directory", "err", err)
		<-ctx.Done()
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.log.Error("document watcher", "err", err)
		<-ctx.Done()
		return
	}
	defer watcher.Close()

	// The subscription is opened before the files are written, so a command
	// applied while the mirror is catching up is waiting on the channel rather
	// than missed by both.
	sub := s.Bus.Subscribe(0)
	defer sub.Close()

	// A debounced import is held in a set rather than sent down a channel: a
	// channel with room for sixteen drops the seventeenth, and a dropped import
	// is a document that never gets read back. The channel here only wakes the
	// loop, and losing a wake costs nothing because the work is still in the
	// set when the next one arrives.
	var pendMu sync.Mutex
	pending := map[string]bool{}
	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	take := func() []string {
		pendMu.Lock()
		defer pendMu.Unlock()
		paths := make([]string, 0, len(pending))
		for path := range pending {
			paths = append(paths, path)
			delete(pending, path)
		}
		return paths
	}

	timers := map[string]*time.Timer{}
	// Whatever was already on disk is read before the loop starts, so that no
	// number of documents can outrun anything.
	for _, path := range s.catchUp(ctx, watcher) {
		if err := s.Import(ctx, path); err != nil {
			s.log.Warn("document not imported", "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub.C:
			if !ok {
				return
			}
			s.applied(ctx, watcher, e)
		case ev, ok := <-watcher.Events:
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
			if t, ok := timers[path]; ok {
				t.Reset(s.Debounce)
				continue
			}
			timers[path] = time.AfterFunc(s.Debounce, func() {
				pendMu.Lock()
				pending[path] = true
				pendMu.Unlock()
				poke()
			})
		case <-wake:
			for _, path := range take() {
				delete(timers, path)
				if err := s.Import(ctx, path); err != nil {
					s.log.Warn("document not imported", "err", err)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			s.log.Warn("document watcher", "err", err)
		}
	}
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
		if _, err := os.Stat(path); err == nil {
			// The file is this document's, but nothing here wrote it, so what
			// is in it is unknown: an empty hash matches nothing, which makes
			// the import read it rather than mistake it for our own write and
			// stops the next command writing over an edit made while the
			// process was down.
			s.mu.Lock()
			s.written[path] = mirrored{document: id}
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

	document, err := GetDocument(ctx, s.DB, file.Document)
	if err != nil {
		return err
	}
	blocks, err := Blocks(ctx, s.DB, document.ID)
	if err != nil {
		return err
	}
	if sameAs(file, blocks) {
		// Whitespace, a reordered comment, an editor's newline at the end.
		// Nothing is applied, but the file is still rewritten, because that is
		// what puts the hash this process remembers back in step with what is
		// on disk. The markers the file already carries go back on it: after a
		// restart the file is the only thing that remembers them, and dropping
		// them here would take away the one notice the person at the terminal
		// has that a block of theirs never went in.
		return s.Mirror(ctx, document.ID, markedIn(file), true)
	}

	// A bad hand edit is one restore away.
	if _, err := s.CreateRevision(ctx, fileActor, document.ID, ReasonPreImport); err != nil {
		return err
	}

	live := map[int64]Block{}
	for _, b := range blocks {
		live[b.ID] = b
	}
	conflicted := map[int64]bool{}
	seen := map[int64]bool{}
	var after int64
	for _, item := range file.Blocks {
		current, known := live[item.ID]
		if !known {
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
			for _, part := range Paragraphs(item.Text) {
				e, err := s.InsertBlock(ctx, fileActor, document.ID, after, part, false)
				if err != nil {
					return err
				}
				after = e.EntityID
			}
			continue
		}
		seen[item.ID] = true
		after = item.ID
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
			// The version in the comment is the version the person at the
			// terminal started from, so this is the same stale set the browser
			// sends and it goes through the same merge.
			if _, err := s.SetBlock(ctx, fileActor, item.ID, item.Version, parts[0], false); err != nil {
				var clash *core.ConflictError
				if !errors.As(err, &clash) {
					return err
				}
				conflicted[item.ID] = true
			}
		}
		// What was written under the block is words that are in the file and
		// not in the database, so it goes in whether or not the block itself
		// would take its own change, which is the rule the branch above uses.
		for _, part := range parts[1:] {
			e, err := s.InsertBlock(ctx, fileActor, document.ID, after, part, false)
			if err != nil {
				return err
			}
			after = e.EntityID
		}
	}

	// A block the file no longer has was either deleted at the terminal or
	// added in the browser after the file was written. Those look the same from
	// here, so the database only gives one up when the file was written against
	// the revision the database still holds; otherwise the block stays and the
	// file comes back with a marker on it. Deleting it again against the fresh
	// file goes through.
	fresh := file.Revision == document.Revision
	for _, b := range blocks {
		if seen[b.ID] {
			continue
		}
		if !fresh {
			conflicted[b.ID] = true
			continue
		}
		if _, err := s.DeleteBlock(ctx, fileActor, b.ID); err != nil {
			return err
		}
	}
	return s.Mirror(ctx, document.ID, conflicted, true)
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

// sameAs reports whether the file already says exactly what the database holds,
// which is every event the watcher hears about its own writes and every save
// that changed nothing.
func sameAs(file fileDoc, blocks []Block) bool {
	if len(file.Blocks) != len(blocks) {
		return false
	}
	for i, item := range file.Blocks {
		if item.ID != blocks[i].ID || item.Text != blocks[i].Text {
			return false
		}
	}
	return true
}
