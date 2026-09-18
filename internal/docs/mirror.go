package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

var blockComment = regexp.MustCompile(`^<!-- block (\d+) v(\d+) -->$`)

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
	dir = filepath.Join(s.root, fmt.Sprintf("%d-%s", number, Slug(title)))
	path = filepath.Join(dir, Slug(slug)+".md")
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
	content := render(d, blocks, conflicted)
	sum := hashOf(content)

	s.mu.Lock()
	was, known := s.written[path]
	s.mu.Unlock()
	if known && was.hash == sum && !force {
		return nil
	}
	if !force {
		if current, err := os.ReadFile(path); err == nil && (!known || hashOf(current) != was.hash) {
			return nil
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// The hash is recorded before the bytes land, so that the watcher cannot
	// see the write before it knows the write was ours.
	s.mu.Lock()
	s.written[path] = mirrored{document: document, hash: sum}
	s.mu.Unlock()
	if err := writeAtomic(path, content); err != nil {
		return err
	}
	// A rename moved the file; the one it used to be is not the mirror of
	// anything now.
	s.forgetOthers(document, path)
	return nil
}

// writeAtomic writes through a temporary file in the same directory and renames
// it over the target, so a reader never sees half a document and a crash never
// leaves one.
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
	return os.Rename(name, path)
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
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
}

type fileDoc struct {
	Proposition int64
	Document    int64
	Revision    int64
	Blocks      []fileBlock
}

// ErrNotMirror is a file with no usable front matter, which is not a document
// this process wrote and has no business being imported.
var ErrNotMirror = errors.New("that file is not a document mirror")

// parseMirror reads a markdown file back into the blocks it stands for. The
// body is cut on blank lines, exactly as Paragraphs cuts a block, so a file
// written by render and read back here is the same list of blocks.
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

	for _, chunk := range blankLine.Split(body, -1) {
		var block fileBlock
		lines := strings.Split(strings.Trim(chunk, "\n"), "\n")
		for len(lines) > 0 {
			if m := blockComment.FindStringSubmatch(strings.TrimSpace(lines[0])); m != nil {
				block.ID, _ = strconv.ParseInt(m[1], 10, 64)
				block.Version, _ = strconv.ParseInt(m[2], 10, 64)
				lines = lines[1:]
				continue
			}
			// A marker this process wrote onto a block it could not take from
			// the file is not part of the text.
			if strings.TrimSpace(lines[0]) == conflictMarker {
				lines = lines[1:]
				continue
			}
			break
		}
		block.Text = strings.TrimSpace(strings.Join(lines, "\n"))
		if block.ID == 0 && block.Text == "" {
			continue
		}
		doc.Blocks = append(doc.Blocks, block)
	}
	return doc, nil
}
