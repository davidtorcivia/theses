package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrOutside is a mirror path that would land somewhere other than under the
// docs directory. Every part of the path is made from something a person typed,
// so this is checked rather than assumed.
var ErrOutside = errors.New("that document would be written outside the docs directory")

// The file is a faithful copy of what the database holds, plus the two things a
// person editing it at a terminal needs: which document it is, and which
// revision of it they started from. Each block carries its id and the version
// it was at, so an edit made on disk maps onto exactly the same stale block.set
// the browser sends and goes through the same merge.
const (
	frontMatter     = "---"
	conflictMarker  = "<!-- conflict -->"
	blockCommentFmt = "<!-- block %d v%d -->"
)

// structural matches the lines the file carries as structure: the comment above
// a block and the marker on one an import could not take. The backslashes in
// front are how a line of somebody's text that reads like one of them is
// written, so that text and structure are never the same line. Every number of
// them matches, because escaping a line that is already escaped adds one more
// and reading it back takes that one off.
//
// A carriage return counts as trailing space. Nothing that reaches here has one
// today, because every text is normalized on the way in and the file is
// normalized on the way back, but a line that reads as structure after that and
// as text here would be a line written to the file bare and read back as a
// block boundary, and this is one regular expression rather than five places
// that have to keep normalizing.
var structural = regexp.MustCompile(`^(\\*)<!-- (?:block (\d+) v(\d+)|conflict) -->[ \t\r]*$`)

// commentOf is the block a line stands above and the version it was at, or a
// zero id for anything else: a line of text quoting one of these comments, the
// conflict marker, or an ordinary line. It is the one reader of a comment, so
// that the cut and the block it starts can never disagree about which lines are
// structure. No block is at id nought, so a line naming one is text.
func commentOf(line string) (id, version int64) {
	m := structural.FindStringSubmatch(line)
	if m == nil || m[1] != "" || m[2] == "" {
		return 0, 0
	}
	id, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || id == 0 {
		return 0, 0
	}
	version, _ = strconv.ParseInt(m[3], 10, 64)
	return id, version
}

// marker is the line an import writes onto a block it could not take from the
// file. It stands under that block's comment and is not part of the text.
func marker(line string) bool {
	m := structural.FindStringSubmatch(line)
	return m != nil && m[1] == "" && m[2] == ""
}

// escaped is a line of a block's text as the file carries it: one more
// backslash in front when it reads like a line the file writes itself, and the
// line as it stands otherwise.
func escaped(line string) string {
	if structural.MatchString(line) {
		return `\` + line
	}
	return line
}

// unescaped is the other half of that, and the two are a bijection, so text the
// app stores comes back as itself whatever it quotes.
func unescaped(line string) string {
	if m := structural.FindStringSubmatch(line); m != nil && m[1] != "" {
		return line[1:]
	}
	return line
}

// paths returns the file this document is mirrored to. The directory is the
// proposition, numbered and named the way an object key is, so a listing of
// data/docs reads like the rail.
//
// ponytail: renaming a proposition changes the directory, and the one it used
// to be is left where it is until somebody edits the document, at which point
// the file moves and the empty directory stays behind. Nothing imports from it,
// because the watcher only reads back paths it has itself written. The upgrade
// path is to move the directory on a proposition rename, which wants an event
// this package does not listen for yet.
func (s *Service) paths(ctx context.Context, document int64) (dir, path string, err error) {
	var number int64
	var title, slug string
	err = s.DB.QueryRowContext(ctx, `SELECT p.number, p.title, d.slug
		FROM documents d JOIN propositions p ON p.id = d.proposition_id WHERE d.id = ?`,
		document).Scan(&number, &title, &slug)
	if err != nil {
		return "", "", err
	}
	// The slug stored on the row is already a slug, and already the one thing
	// that tells two documents of a proposition apart. Running it through Slug
	// again would cut it back to sixty characters and take the number that
	// makes it unique off the end, so two documents would mirror to one file.
	dir = filepath.Join(s.root, fmt.Sprintf("%d-%s", number, Slug(title)))
	path = filepath.Join(dir, slug+".md")
	if err := within(s.root, path); err != nil {
		return "", "", err
	}
	return dir, path, nil
}

// within refuses a path that is not inside root. Slug already leaves nothing in
// a name but letters, digits and hyphens, so this is the second lock on a door
// that should already be shut.
func within(root, path string) error {
	r, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrOutside
	}
	return nil
}

// render is the whole file: the front matter, then one block per paragraph with
// its id comment above it, and a conflict marker on any block an import could
// not take from the file.
//
// The front matter needs no escaping of its own. It is four lines and a fence
// of hyphens either side, and the read below cuts at the first of those after
// the first line, which is always the one written here: every block comes after
// it, so a block holding a line of hyphens is a line in the body like any
// other.
func render(d Document, blocks []Block, conflicted map[int64]bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nproposition: %d\ndocument: %d\nrevision: %d\n%s\n",
		frontMatter, d.Proposition, d.ID, d.Revision, frontMatter)
	for _, block := range blocks {
		b.WriteString("\n")
		fmt.Fprintf(&b, blockCommentFmt+"\n", block.ID, block.Version)
		if conflicted[block.ID] {
			b.WriteString(conflictMarker + "\n")
		}
		for _, line := range strings.Split(block.Text, "\n") {
			b.WriteString(escaped(line))
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// legacyRender is render as the version before this one wrote it, with nothing
// escaped, kept whole so that a file can be compared against it byte for byte.
// Every mirror file on disk is one of these at the first start after this
// version is deployed, and reading one back through the parser would take a
// line of somebody's text that quotes a block comment for a block boundary,
// with no hand edit anywhere near it.
//
// ponytail: this is for one start per deployment, and it can be deleted once
// every deployment has run this version once. Its ceiling is a file the older
// version wrote that is no longer what it wrote: a hand edit made while the
// process was down, or a conflict marker, leaves it to be read back the
// ordinary way, which is right unless that file also quotes the format.
func legacyRender(d Document, blocks []Block, conflicted map[int64]bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nproposition: %d\ndocument: %d\nrevision: %d\n%s\n",
		frontMatter, d.Proposition, d.ID, d.Revision, frontMatter)
	for _, block := range blocks {
		b.WriteString("\n")
		fmt.Fprintf(&b, blockCommentFmt+"\n", block.ID, block.Version)
		if conflicted[block.ID] {
			b.WriteString(conflictMarker + "\n")
		}
		b.WriteString(block.Text)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Mirror writes one document to disk. force is what an import uses: it has just
// reconciled the file with the database and the file has to end up holding the
// result, new block ids and conflict markers and all.
//
// Without force, a file whose contents are not what this process last wrote is
// left alone. Somebody has edited it and the import for that edit has not run
// yet; overwriting here would take their work away and the watcher would then
// see its own write and have nothing to import.
func (s *Service) Mirror(ctx context.Context, document int64, conflicted map[int64]bool, force bool) error {
	if s.root == "" {
		return nil
	}
	dir, path, err := s.paths(ctx, document)
	if err != nil {
		return err
	}
	d, err := GetDocument(ctx, s.DB, document)
	if err != nil {
		return err
	}
	blocks, err := Blocks(ctx, s.DB, document)
	if err != nil {
		return err
	}
	s.mu.Lock()
	was, known := s.written[path]
	s.mu.Unlock()
	// Only an import knows what is in conflict, and it says so by forcing the
	// write. Every other write keeps the markers the last import left, because
	// they are the only notice the person at the terminal has that a block of
	// theirs did not go in, and an unrelated edit must not take it away.
	if !force {
		conflicted = was.conflicted
	}
	// A marker is remembered by block id, and a block can go: deleting one and
	// undoing the delete would otherwise bring back a marker for a conflict
	// that was settled long before. What is remembered is cut down to the
	// blocks the document still has, before it is written or recorded.
	conflicted = stillThere(conflicted, blocks)
	content := render(d, blocks, conflicted)
	sum := hashOf(content)

	if known && was.hash == sum && !force {
		return nil
	}
	if !force {
		if current, err := readMirror(path); err == nil && (!known || hashOf(current) != was.hash) {
			return nil
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// The hash is recorded before the bytes land, so that the watcher cannot
	// see the write before it knows the write was ours.
	s.mu.Lock()
	s.written[path] = mirrored{document: document, hash: sum, conflicted: conflicted}
	s.mu.Unlock()
	if err := writeAtomic(path, content); err != nil {
		// Nothing landed, so what this process last wrote is still what it
		// wrote before. Leaving the new hash recorded would make every later
		// write read the file, find it different from it, take that for a hand
		// edit waiting to be imported, and skip: one failed rename would stop
		// this document mirroring for the life of the process.
		s.mu.Lock()
		if known {
			s.written[path] = was
		} else {
			delete(s.written, path)
		}
		s.mu.Unlock()
		return err
	}
	// A rename moved the file; the one it used to be is not the mirror of
	// anything now.
	s.forgetOthers(document, path)
	return nil
}

// stillThere drops the blocks a remembered set of markers names that the
// document no longer has.
func stillThere(conflicted map[int64]bool, blocks []Block) map[int64]bool {
	if len(conflicted) == 0 {
		return conflicted
	}
	out := map[int64]bool{}
	for _, b := range blocks {
		if conflicted[b.ID] {
			out[b.ID] = true
		}
	}
	return out
}

// inUseWait is how long a file operation waits for whatever else has the file
// to let go. An editor saving the document, a sync client or an indexer holds
// it for a moment; a second is long enough to outlast that and short enough
// that the goroutine doing this, which also drains the event bus, is never held
// up for anything a person would notice.
const inUseWait = time.Second

// sharingViolation is what Windows answers when a file is open elsewhere. It is
// named here rather than kept behind a build tag because no file operation on
// any other platform returns it, so the test below is false everywhere else.
const sharingViolation = syscall.Errno(32)

// inUse reports whether an operation was refused because something else has the
// file open. Windows will not let a rename replace or a remove take away a file
// another handle holds, and will not open one that is in the middle of being
// replaced. Neither is a reason to give up: the handle goes in a moment.
//
// It answers those two with Access is denied and with a code of its own. Only
// the second means the same thing everywhere; a permission error on the machine
// this is deployed to is a permission the process does not have and never will,
// and waiting a second per file for it would cost the startup pass a second per
// document to learn nothing.
func inUse(err error) bool {
	if runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == sharingViolation
}

// waitOut runs a file operation again for as long as something else has the
// file. Anything else it fails with comes straight back, so a path that is
// wrong or a directory that cannot be written is one attempt, not a second of
// them.
func waitOut(op func() error) error {
	deadline := time.Now().Add(inUseWait)
	for delay := time.Millisecond; ; delay *= 2 {
		err := op()
		if err == nil || !inUse(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(delay)
	}
}

// readMirror reads a file the mirror owns. A read given up on because an editor
// still had the file open is a hand edit that is never read back at all:
// nothing further happens to the file, so no later event asks again.
func readMirror(path string) ([]byte, error) {
	var content []byte
	err := waitOut(func() error {
		var err error
		content, err = os.ReadFile(path)
		return err
	})
	return content, err
}

// writeAtomic writes through a temporary file in the same directory and renames
// it over the target, so a reader never sees half a document and a crash never
// leaves one. The rename waits out anything holding the file it replaces,
// because a write given up on is a change the document on disk never gets.
func writeAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return waitOut(func() error { return os.Rename(name, path) })
}

// forgetOthers drops the mirror files this document used to be written to,
// which is what a rename leaves behind.
func (s *Service) forgetOthers(document int64, keep string) {
	s.mu.Lock()
	stale := []string{}
	for path, m := range s.written {
		if m.document == document && path != keep {
			stale = append(stale, path)
			delete(s.written, path)
		}
	}
	s.mu.Unlock()
	for _, path := range stale {
		// The same wait the rename gets, for the same reason: a file left
		// behind because something had it open is one a later document that
		// slugs to this path would find in its way for good.
		err := waitOut(func() error { return os.Remove(path) })
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("old document file not removed", "err", err)
		}
	}
}

// Unmirror removes a deleted document's file.
func (s *Service) Unmirror(document int64) {
	s.forgetOthers(document, "")
}

// A fileBlock is one block as the markdown file carries it: the id and version
// from its comment, and the text under it. An id of zero is a paragraph
// somebody added, which has no block behind it yet.
type fileBlock struct {
	ID      int64
	Version int64
	Text    string
	// Conflicted is a block the file already carries a marker on, put there by
	// an earlier import. It is read back rather than assumed, because after a
	// restart the file is the only record of it.
	Conflicted bool
}

type fileDoc struct {
	Proposition int64
	Document    int64
	Revision    int64
	Blocks      []fileBlock
}

// items is the file as the list an import applies, which is the same list a
// source save builds from what the browser sent.
func (f fileDoc) items() []item {
	out := make([]item, 0, len(f.Blocks))
	for _, b := range f.Blocks {
		out = append(out, item{ID: b.ID, Version: b.Version, Text: b.Text})
	}
	return out
}

// ErrNotMirror is a file with no usable front matter, which is not a document
// this process wrote and has no business being imported.
var ErrNotMirror = errors.New("that file is not a document mirror")

// parseMirror reads a markdown file back into the blocks it stands for. The
// body is cut by mirrorChunks, which cuts where Paragraphs cuts, so a file
// written by render and read back here is the same list of blocks.
//
// A block named twice keeps its id the first time and loses it after that. The
// file is the one that says so, and two chunks cannot both be that block: the
// second is a paragraph somebody copied, comment line and all, and an id of
// nought is how the import is told to put it in as a block of its own.
func parseMirror(content []byte) (fileDoc, error) {
	text := strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(text, frontMatter+"\n") {
		return fileDoc{}, ErrNotMirror
	}
	head, body, ok := strings.Cut(text[len(frontMatter)+1:], "\n"+frontMatter+"\n")
	if !ok {
		return fileDoc{}, ErrNotMirror
	}
	var doc fileDoc
	for _, line := range strings.Split(head, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "proposition":
			doc.Proposition = n
		case "document":
			doc.Document = n
		case "revision":
			doc.Revision = n
		}
	}
	if doc.Document == 0 {
		return fileDoc{}, ErrNotMirror
	}

	seen := map[int64]bool{}
	for _, chunk := range mirrorChunks(body) {
		var block fileBlock
		lines := strings.Split(strings.Trim(chunk, "\n"), "\n")
		for len(lines) > 0 {
			if id, version := commentOf(lines[0]); id != 0 {
				block.ID, block.Version = id, version
			} else if !marker(lines[0]) {
				break
			} else {
				block.Conflicted = true
			}
			lines = lines[1:]
		}
		if seen[block.ID] {
			block.ID, block.Version = 0, 0
		}
		if block.ID != 0 {
			seen[block.ID] = true
		}
		for i, line := range lines {
			lines[i] = unescaped(line)
		}
		block.Text = strings.TrimSpace(strings.Join(lines, "\n"))
		if block.ID == 0 && block.Text == "" {
			continue
		}
		doc.Blocks = append(doc.Blocks, block)
	}
	return doc, nil
}

// mirrorChunks cuts the body into the runs of lines each block was written as:
// at a blank line, unless it is inside a fenced code block, which is where
// Paragraphs cuts too, so a block holding code comes back as the one block it
// went out as, and at every block comment, which is a boundary wherever it
// stands. A comment ends whatever fence is open, because it is the start of
// another block: a block holding a fence that is never closed, which is what a
// save made while somebody is typing leaves behind, cannot swallow the blocks
// written under it.
//
// Nothing a block holds can be read as one of those comments, because render
// writes a line of text that looks like one with a backslash in front. What is
// left is the hand edit: somebody who types a bare comment line into the file
// themselves has written a boundary, and the words on either side of it are all
// kept, but they land in the blocks the comments name. A conflict marker moved
// above its block's comment rather than under it is dropped rather than read as
// that block's, since it falls at the end of the block before it; render always
// writes one under the comment.
func mirrorChunks(body string) []string {
	out := []string{}
	var current []string
	var f fence
	// A run with nothing in it is no block: the body opens with the blank line
	// render writes above the first comment, and two boundaries in a row are
	// somebody's spacing rather than an empty block.
	cut := func() {
		if len(current) > 0 {
			out = append(out, strings.Join(current, "\n"))
			current = nil
		}
	}
	for _, line := range strings.Split(body, "\n") {
		id, _ := commentOf(line)
		switch {
		case id != 0:
			cut()
			f = fence{}
			current = append(current, line)
		case f.track(line):
			current = append(current, line)
		case blank(line):
			cut()
		default:
			current = append(current, line)
		}
	}
	cut()
	return out
}
