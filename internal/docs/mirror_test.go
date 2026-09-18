package docs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mirrorFixture is one fixture with the mirror on and its file already written.
func mirrorFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := setup(t, t.TempDir())
	if err := f.Mirror(context.Background(), f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	_, path, err := f.paths(context.Background(), f.doc)
	if err != nil {
		t.Fatal(err)
	}
	return f, path
}

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestMirrorWritesAFileThatParsesBackToTheSameBlocks(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	if base := filepath.Base(filepath.Dir(path)); base != "1-tidal-power" {
		t.Fatalf("the directory is %q, want 1-tidal-power", base)
	}
	if base := filepath.Base(path); base != "research.md" {
		t.Fatalf("the file is %q, want research.md", base)
	}

	content := read(t, path)
	file, err := parseMirror([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	document, err := GetDocument(ctx, f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	if file.Document != f.doc || file.Proposition != f.prop || file.Revision != document.Revision {
		t.Fatalf("the front matter is %+v, want document %d revision %d", file, f.doc, document.Revision)
	}
	blocks := f.blocks(t)
	if !sameAs(file, blocks) {
		t.Fatalf("the file parses back to %+v, want %+v", file.Blocks, blocks)
	}
	for i, item := range file.Blocks {
		if item.Version != blocks[i].Version {
			t.Fatalf("block %d carries version %d, want %d", item.ID, item.Version, blocks[i].Version)
		}
	}
	// One block per paragraph, separated by blank lines, and nothing else.
	if strings.Contains(content, "\n\n\n") {
		t.Fatalf("the file has a run of blank lines:\n%s", content)
	}
}

func TestMirrorRefusesAPathOutsideTheDocsDirectory(t *testing.T) {
	for _, tc := range []struct{ name, root, path string }{
		{"a sibling", "/data/docs", "/data/backups/db.age"},
		{"one above", "/data/docs", "/data/docs/../secrets.md"},
		{"the root itself is fine", "/data/docs", "/data/docs/1-x/research.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := within(filepath.FromSlash(tc.root), filepath.FromSlash(tc.path))
			want := !strings.HasPrefix(tc.name, "the root itself")
			if (err != nil) != want {
				t.Fatalf("within(%q, %q) = %v", tc.root, tc.path, err)
			}
			if want && !errors.Is(err, ErrOutside) {
				t.Fatalf("got %v, want ErrOutside", err)
			}
		})
	}
}

func TestMirrorDoesNotClobberAPendingHandEdit(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	// Somebody saves the file; the import for it has not run yet.
	edited := strings.Replace(read(t, path), "## Is it true?", "## Is it true, though?", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	// A change in the browser would otherwise write over it.
	b := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["editor"], b[0].ID, b[0].Version, "# Changed in the browser"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != edited {
		t.Fatal("the mirror wrote over an edit it had not imported yet")
	}

	// And the import that follows takes both changes.
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	if after[0].Text != "# Changed in the browser" {
		t.Fatalf("the browser's change was lost: %q", after[0].Text)
	}
	if after[1].Text != "## Is it true, though?" {
		t.Fatalf("the hand edit was lost: %q", after[1].Text)
	}
}

func TestImportIgnoresItsOwnWriteAndAStrangerFile(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	// The bytes on disk are what the service recorded writing, and an import of
	// them touches nothing at all: no revision, and no write of its own, which
	// is what keeps the watcher from hearing itself for ever.
	f.mu.Lock()
	recorded := f.written[path]
	f.mu.Unlock()
	if recorded.hash != hashOf([]byte(read(t, path))) {
		t.Fatal("the mirror did not record the hash of what it wrote")
	}
	before := len(revisionsOf(t, f))
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if n := len(revisionsOf(t, f)); n != before {
		t.Fatalf("importing this process's own write kept %d revisions", n-before)
	}
	again, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(stat.ModTime()) {
		t.Fatal("importing this process's own write rewrote the file")
	}

	stranger := filepath.Join(filepath.Dir(path), "notes-from-the-shell.md")
	if err := os.WriteFile(stranger, []byte("Just a file somebody left here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, stranger); err != nil {
		t.Fatal(err)
	}
	if read(t, stranger) != "Just a file somebody left here.\n" {
		t.Fatal("a file this process never wrote was rewritten")
	}
	if len(f.blocks(t)) != 3 {
		t.Fatal("a file this process never wrote became blocks")
	}
}

func TestImportAppliesEditsInsertsAndDeletes(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	content := read(t, path)
	// Change one block, add a paragraph under it, and take the last one out.
	content = strings.Replace(content, "## Is it true?", "## Is it true, though?\n\nA new paragraph.", 1)
	content = strings.Replace(content, lineFor(t, path, blocks[2].ID)+"\n"+blocks[2].Text+"\n", "", 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	if len(after) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(after), after)
	}
	if after[1].Text != "## Is it true, though?" || after[2].Text != "A new paragraph." {
		t.Fatalf("the import landed as %+v", after)
	}
	if after[1].ID != blocks[1].ID {
		t.Fatal("the edited block was replaced rather than set")
	}
	// The pre-import revision holds the document as it was.
	list := revisionsOf(t, f)
	if len(list) != 1 || list[0].Reason != ReasonPreImport {
		t.Fatalf("revisions are %+v", list)
	}
	if list[0].Markdown != Markdown(blocks) {
		t.Fatalf("the pre-import revision holds %q", list[0].Markdown)
	}
	// A hand edit has no person behind it.
	if after[1].UpdatedBy != nil {
		t.Fatalf("the file's edit was attributed to user %d", *after[1].UpdatedBy)
	}
	var kind string
	if err := f.db.QueryRowContext(ctx,
		`SELECT actor_kind FROM activity WHERE entity = 'block' AND action = 'set' ORDER BY id DESC LIMIT 1`).
		Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "file" {
		t.Fatalf("the import was recorded as %q, want file", kind)
	}
	// The file comes back matching the database, with the new block's id in it.
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if n := len(revisionsOf(t, f)); n != 1 {
		t.Fatalf("importing the rewritten file recorded %d revisions", n)
	}
}

// lineFor is the id comment the mirror wrote above one block.
func lineFor(t *testing.T, path string, id int64) string {
	t.Helper()
	for _, line := range strings.Split(read(t, path), "\n") {
		if m := blockComment.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[1] == strconv.FormatInt(id, 10) {
			return strings.TrimRight(line, "\r")
		}
	}
	t.Fatalf("no comment for block %d in %s", id, path)
	return ""
}

// A file written against a revision the database has moved past must not take
// a block away: the block may have been added in the browser after the file was
// written, and those two look the same from disk.
func TestImportKeepsABlockTheStaleFileIsMissing(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	// The person at the terminal has the file open and deletes nothing yet.
	stale := read(t, path)

	// Meanwhile a block is added in the browser.
	inserted, err := f.InsertBlock(ctx, f.who["editor"], f.doc, blocks[2].ID, "Added in the browser.")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, true); err != nil {
		t.Fatal(err)
	}

	// Now the terminal saves what it had, which has neither the new block nor
	// the current revision.
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	if len(after) != 4 {
		t.Fatalf("a stale file took a block away: %+v", after)
	}
	if !strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("the rewritten file has no conflict marker:\n%s", read(t, path))
	}

	// Deleting it again, now against the fresh file, goes through.
	content := read(t, path)
	comment := lineFor(t, path, inserted.EntityID)
	content = strings.Replace(content, comment+"\n"+conflictMarker+"\nAdded in the browser.\n", "", 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if n := len(f.blocks(t)); n != 3 {
		t.Fatalf("the second delete left %d blocks", n)
	}
}

// A block both sides rewrote differently stays as the database has it and comes
// back marked, so the person at the terminal sees exactly what did not go in.
func TestImportLeavesAConflictingBlockAloneAndMarksIt(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	stale := strings.Replace(read(t, path), "## Is it true?", "## Is it true at the terminal?", 1)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}

	after := f.blocks(t)
	if after[1].Text != "## Is it true in the browser?" {
		t.Fatalf("the database gave way to the file: %q", after[1].Text)
	}
	content := read(t, path)
	if !strings.Contains(content, conflictMarker) {
		t.Fatalf("no conflict marker:\n%s", content)
	}
	if !strings.Contains(content, "## Is it true in the browser?") {
		t.Fatalf("the rewritten file does not hold what the database has:\n%s", content)
	}
}

// A stale file whose edit merges cleanly with the database is applied, which is
// the whole reason the block comment carries the version.
func TestImportMergesAStaleEditOnAnotherLine(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version,
		"The tide is high and the moon is full."); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, true); err != nil {
		t.Fatal(err)
	}
	stale := read(t, path)
	current, err := GetBlock(ctx, f.db, blocks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.SetBlock(ctx, f.who["owner"], blocks[1].ID, current.Version,
		"The tide is low and the moon is full."); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, true); err != nil {
		t.Fatal(err)
	}

	// The terminal edits the other half of the sentence in the version it had.
	stale = strings.Replace(stale, "The tide is high and the moon is full.",
		"The tide is high and the moon is new.", 1)
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	after, err := GetBlock(ctx, f.db, blocks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Text != "The tide is low and the moon is new." {
		t.Fatalf("the merge gave %q", after.Text)
	}
}

func TestRenameAndDeleteMoveTheFile(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	if _, err := f.RenameDocument(ctx, f.who["editor"], f.doc, "Show notes"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(filepath.Dir(path), "show-notes.md")
	if _, err := os.Stat(renamed); err != nil {
		t.Fatalf("the renamed document is not at %s: %v", renamed, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old file is still there")
	}

	f.Unmirror(f.doc)
	if _, err := os.Stat(renamed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a deleted document kept its file")
	}
}

// The watcher end to end: a file saved on disk becomes blocks, and the file the
// watcher itself writes does not come back round as an import.
func TestRunImportsAHandEditAndNotItsOwnWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := setup(t, t.TempDir())
	f.Debounce = 40 * time.Millisecond
	f.Every = 0

	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()

	_, path, err := f.paths(ctx, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the mirror to be written", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})

	// A change in the browser reaches the file.
	blocks := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version, "# From the browser"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the browser's change to reach the file", func() bool {
		content, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(content), "# From the browser")
	})

	// A change on disk reaches the database.
	content := strings.Replace(read(t, path), "# From the browser", "# From the terminal", 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hand edit to reach the database", func() bool {
		b, err := GetBlock(ctx, f.db, blocks[0].ID)
		return err == nil && b.Text == "# From the terminal"
	})

	// And nothing went round in a loop: one import, one pre-import revision.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := len(revisionsOf(t, f)); n > 1 {
			t.Fatalf("the watcher imported its own writes: %d revisions", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

// An edit made while the process was down is read in at the next start rather
// than written over by the first thing anybody does in the browser.
func TestRunImportsAnEditMadeWhileTheProcessWasDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, path := mirrorFixture(t)
	f.Debounce = 40 * time.Millisecond
	f.Every = 0

	edited := strings.Replace(read(t, path), "## Is it true?", "## Is it true, though?", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	// A restart: the process remembers nothing about what it wrote.
	f.mu.Lock()
	f.written = map[string]mirrored{}
	f.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	waitFor(t, "the edit made while the process was down", func() bool {
		return f.blocks(t)[1].Text == "## Is it true, though?"
	})
	if list := revisionsOf(t, f); len(list) != 1 || list[0].Reason != ReasonPreImport {
		t.Fatalf("revisions are %+v", list)
	}
	cancel()
	<-done
}

// The file an import leaves behind carries a marker on every block the database
// would not give up, and the events that import published must not take them
// off again a moment later.
func TestRunKeepsTheConflictMarkersItWrote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := setup(t, t.TempDir())
	f.Debounce = 40 * time.Millisecond
	f.Every = 0

	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	_, path, err := f.paths(ctx, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the mirror to be written", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
	stale := read(t, path)

	blocks := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version,
		"## Is it true in the browser?"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the browser's change to reach the file", func() bool {
		return strings.Contains(read(t, path), "## Is it true in the browser?")
	})

	// The terminal saves the version it had, with its own change to that block
	// and a paragraph at the end. The paragraph goes in, which is what makes
	// this worth testing: the commands the import applies publish events of
	// their own, and writing the file for one of those would take the marker
	// off the block the import could not apply.
	if err := os.WriteFile(path,
		[]byte(strings.Replace(stale, "## Is it true?", "## Is it true at the terminal?", 1)+
			"\nAnd a paragraph typed into the file.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the paragraph typed into the file", func() bool {
		blocks := f.blocks(t)
		return blocks[len(blocks)-1].Text == "And a paragraph typed into the file."
	})
	waitFor(t, "the conflict marker", func() bool {
		return strings.Contains(read(t, path), conflictMarker)
	})
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !strings.Contains(read(t, path), conflictMarker) {
			t.Fatal("the marker was written and then taken off again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.blocks(t)[1].Text != "## Is it true in the browser?" {
		t.Fatalf("the database gave way to the file: %q", f.blocks(t)[1].Text)
	}
	cancel()
	<-done
}

// Two documents whose names are the same for the first sixty characters get
// different slugs, and the mirror must keep them: running the slug through Slug
// again would cut the number that made it unique off the end and write both
// documents to one file.
func TestMirrorKeepsTwoLongNamesApart(t *testing.T) {
	ctx := context.Background()
	f := setup(t, t.TempDir())
	long := strings.Repeat("severn", 10) // sixty characters, no trailing hyphen
	var ids []int64
	for _, name := range []string{long + " one", long + " two"} {
		e, err := f.CreateDocument(ctx, f.who["editor"], f.prop, name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.EntityID)
		if _, err := f.InsertBlock(ctx, f.who["editor"], e.EntityID, 0, "In "+name+"."); err != nil {
			t.Fatal(err)
		}
		if err := f.Mirror(ctx, e.EntityID, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		_, path, err := f.paths(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if seen[path] {
			t.Fatalf("two documents mirror to %s", path)
		}
		seen[path] = true
		file, err := parseMirror([]byte(read(t, path)))
		if err != nil {
			t.Fatal(err)
		}
		if file.Document != id {
			t.Fatalf("%s holds document %d, want %d", path, file.Document, id)
		}
	}
}

// A write that does not land must leave behind no record of having landed. One
// failed rename used to stop the document mirroring for good: every later write
// read the file, found it different from the hash it thought it had written,
// took that for a hand edit and skipped.
func TestMirrorRecoversFromAWriteThatDidNotLand(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	original := read(t, path)
	blocks := f.blocks(t)

	// The file is replaced by a directory of the same name, which no rename
	// can be completed over.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "in the way"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version, "# Written while blocked"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err == nil {
		t.Fatal("the mirror reported a write it could not make")
	}

	// The obstruction goes and the file is put back as it was, which is what
	// the mirror should still believe it last wrote.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["editor"], after[0].ID, after[0].Version, "# Written after the block cleared"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), "# Written after the block cleared") {
		t.Fatalf("the mirror never wrote again:\n%s", read(t, path))
	}
}

// The markers an import leaves survive every write but the next import's, so a
// change made in the browser to some other block does not take away the only
// notice the person at the terminal has.
func TestMirrorKeepsTheMarkersUntilAnImportClearsThem(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	stale := strings.Replace(read(t, path), "## Is it true?", "## Is it true at the terminal?", 1)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("no conflict marker:\n%s", read(t, path))
	}

	// A change to another block rewrites the file, and the marker stays.
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[2].ID, blocks[2].Version, "## Who pays for it?"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	marked := read(t, path)
	if !strings.Contains(marked, conflictMarker) {
		t.Fatalf("an unrelated change took the marker off:\n%s", marked)
	}
	if !strings.Contains(marked, "## Who pays for it?") {
		t.Fatalf("the unrelated change did not reach the file:\n%s", marked)
	}

	// The terminal takes the marker out, agreeing with what the database has,
	// and the next import clears it for good.
	if err := os.WriteFile(path, []byte(strings.Replace(marked, conflictMarker+"\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("the marker outlived the conflict:\n%s", read(t, path))
	}
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, f.blocks(t)[0].Version, "# Later still"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("the marker came back:\n%s", read(t, path))
	}
}

// Every document found on disk at start is read back, however many there are.
// A queue with room for sixteen used to drop the rest, and because their files
// were already recorded as unknown they then stopped mirroring altogether.
func TestRunImportsEveryFileItFindsAtStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := setup(t, t.TempDir())
	f.Debounce = 40 * time.Millisecond
	f.Every = 0

	const documents = 20
	paths := map[int64]string{}
	for i := 0; i < documents; i++ {
		e, err := f.CreateDocument(ctx, f.who["editor"], f.prop, fmt.Sprintf("Note %d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Mirror(ctx, e.EntityID, nil, false); err != nil {
			t.Fatal(err)
		}
		_, path, err := f.paths(ctx, e.EntityID)
		if err != nil {
			t.Fatal(err)
		}
		paths[e.EntityID] = path
		if err := os.WriteFile(path,
			[]byte(read(t, path)+fmt.Sprintf("\nTyped into file %d.\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A restart: the process remembers nothing about what it wrote.
	f.mu.Lock()
	f.written = map[string]mirrored{}
	f.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	waitFor(t, "every file to be read back", func() bool {
		for id := range paths {
			blocks, err := Blocks(ctx, f.db, id)
			if err != nil || len(blocks) == 0 {
				return false
			}
			if !strings.HasPrefix(blocks[len(blocks)-1].Text, "Typed into file ") {
				return false
			}
		}
		return true
	})
	cancel()
	<-done
}

// Paragraphs the file adds go in in the order the file has them, whether or not
// one of them is a heading. Text between two comments is cut into blocks here,
// and following only the first of them would put whatever came next in the file
// in among them.
func TestImportKeepsAddedParagraphsInOrder(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	if err := os.WriteFile(path,
		[]byte(read(t, path)+"\nFirst new line.\n## Second as a heading.\n\nThird paragraph.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	blocks := f.blocks(t)
	var got []string
	for _, b := range blocks[len(blocks)-3:] {
		got = append(got, b.Text)
	}
	want := []string{"First new line.", "## Second as a heading.", "Third paragraph."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the file's new paragraphs landed as %v, want %v", got, want)
	}
}

// After a restart nothing remembers which blocks were in conflict but the file,
// so the import that reads it back has to take the markers from it rather than
// rub them out on its way past.
func TestImportKeepsTheMarkersItFindsAfterARestart(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	stale := strings.Replace(read(t, path), "## Is it true?", "## Is it true at the terminal?", 1)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("no conflict marker to start with:\n%s", read(t, path))
	}

	// A restart: the file is registered the way catchUp registers one it found
	// already there, with nothing known about what is in it.
	f.mu.Lock()
	f.written = map[string]mirrored{path: {document: f.doc}}
	f.mu.Unlock()

	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	if !strings.Contains(after, conflictMarker) {
		t.Fatalf("the restart took the marker off:\n%s", after)
	}
	// And the rewrite still happened, which is what puts the remembered hash
	// back in step with the file.
	f.mu.Lock()
	recorded := f.written[path]
	f.mu.Unlock()
	if recorded.hash != hashOf([]byte(after)) {
		t.Fatal("the import left the recorded hash out of step with the file")
	}
	if !recorded.conflicted[f.blocks(t)[1].ID] {
		t.Fatalf("the marker was written but not remembered: %+v", recorded.conflicted)
	}
}

// A marker is remembered by block id, and a block can go. Deleting one and
// undoing the delete must not bring its marker back with it.
func TestMirrorForgetsAMarkerOnABlockThatIsGone(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	stale := strings.Replace(read(t, path), "## Is it true?", "## Is it true at the terminal?", 1)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("no conflict marker to start with:\n%s", read(t, path))
	}

	deleted, err := f.DeleteBlock(ctx, f.who["editor"], blocks[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("the marker outlived the block it was on:\n%s", read(t, path))
	}

	if _, err := f.Undo(ctx, f.who["editor"], deleted.Seq); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	back := read(t, path)
	if strings.Contains(back, conflictMarker) {
		t.Fatalf("undoing the delete brought the marker back:\n%s", back)
	}
	if !strings.Contains(back, "## Is it true in the browser?") {
		t.Fatalf("the restored block is not in the file:\n%s", back)
	}
}
