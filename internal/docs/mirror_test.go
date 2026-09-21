package docs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/board"
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

func TestImportRollsBackEveryBlockOnFailure(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	before := f.blocks(t)
	document, err := GetDocument(ctx, f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]Block(nil), before...)
	edited[0].Text = "A valid first edit."
	edited[1].Text = strings.Repeat("x", board.MaxBody+1)
	content := string(render(document, edited, nil))
	save(t, path, content)
	if err := f.Import(ctx, path); !errors.Is(err, board.ErrTooLong) {
		t.Fatalf("import oversized block: %v", err)
	}
	after := f.blocks(t)
	for i, b := range before {
		if after[i].Text != b.Text || after[i].Version != b.Version {
			t.Fatalf("block %d changed despite failed import", b.ID)
		}
	}
	var revisions int
	if err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM document_revisions WHERE document_id = ?`, f.doc).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 0 {
		t.Fatalf("failed import left %d revisions", revisions)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != content {
		t.Fatal("failed hand edit was overwritten")
	}
}

// save writes a file the way an editor that renames into place does. A truncate
// and a write are two events and a watcher can read between them, which the two
// seconds the mirror waits in earnest cover but the milliseconds these tests
// wait do not.
func save(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".saving"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitOut(func() error { return os.Rename(tmp, path) }); err != nil {
		t.Fatal(err)
	}
}

// read is how the tests look at a mirror file. It goes through the same wait
// the mirror's own reads do, because a test that polls a file the watcher is
// replacing under it is a second process as far as the platform is concerned.
func read(t *testing.T, path string) string {
	t.Helper()
	content, err := readMirror(path)
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
	if !sameAs(file.items(), blocks) {
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

// A fenced code block holds blank lines, and the file is a block per paragraph
// separated by blank lines, so the two only agree if the import reads a fence
// the way Paragraphs cuts one.
func TestMirrorRoundTripsAFencedBlock(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	blocks := f.blocks(t)
	code := "```go\nif tide > 0 {\n\n\treturn true\n}\n```"
	fenced, err := f.InsertBlock(ctx, f.who["editor"], f.doc, blocks[len(blocks)-1].ID, "", code, false)
	if err != nil {
		t.Fatal(err)
	}
	// A fence that is never closed is what a save made while somebody is still
	// typing leaves in a block, and the blocks under it keep their ids anyway.
	typing := "~~~\nstill typing"
	open, err := f.InsertBlock(ctx, f.who["editor"], f.doc, fenced.EntityID, "", typing, true)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := f.InsertBlock(ctx, f.who["editor"], f.doc, open.EntityID, "", "After.", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}

	file, err := parseMirror([]byte(read(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	if !sameAs(file.items(), f.blocks(t)) {
		t.Fatalf("the file parses back to %+v, want %+v", file.Blocks, f.blocks(t))
	}
	held := map[int64]string{}
	for _, item := range file.Blocks {
		held[item.ID] = item.Text
	}
	for _, want := range []struct {
		id   int64
		text string
	}{{fenced.EntityID, code}, {open.EntityID, typing}, {tail.EntityID, "After."}} {
		if held[want.id] != want.text {
			t.Fatalf("block %d comes back as %q, want %q", want.id, held[want.id], want.text)
		}
	}

	// And an edit made at the terminal applies through all of it: the blocks
	// under the fence that is never closed keep their ids rather than going in
	// again as new ones.
	was := f.blocks(t)
	save(t, path, strings.Replace(read(t, path), "## Who pays?", "## Who pays for it?", 1))
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	now := f.blocks(t)
	if len(now) != len(was) {
		t.Fatalf("the import left %d blocks, want %d: %+v", len(now), len(was), now)
	}
	for i, b := range now {
		if b.ID != was[i].ID {
			t.Fatalf("block %d of the document is now %d, want %d", i, b.ID, was[i].ID)
		}
		if b.Text != was[i].Text && b.Text != "## Who pays for it?" {
			t.Fatalf("block %d holds %q, want %q", b.ID, b.Text, was[i].Text)
		}
	}
	if held := f.blocks(t); held[len(held)-3].Text != code || held[len(held)-2].Text != typing {
		t.Fatalf("the import rewrote the code blocks: %+v", held[len(held)-3:])
	}
}

// Where the body is cut: at a blank line outside a fenced code block, and at
// every comment the file writes itself, which is the start of the next block
// wherever it stands and ends whatever fence is open.
func TestMirrorChunksCutsWhereParagraphsDoes(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"a blank line cuts", "A\n\nB", []string{"A", "B"}},
		{"a blank line inside a fence does not", "```\nA\n\nB\n```", []string{"```\nA\n\nB\n```"}},
		{"a line holding a no-break space is not blank", "A\n" + noBreakSpace + "\nB", []string{"A\n" + noBreakSpace + "\nB"}},
		{"a comment cuts", "<!-- block 7 v1 -->\nA\n\n<!-- block 9 v1 -->\nB",
			[]string{"<!-- block 7 v1 -->\nA", "<!-- block 9 v1 -->\nB"}},
		{"a comment with no blank line in front of it still cuts",
			"<!-- block 7 v1 -->\nA\n<!-- block 9 v1 -->\nB",
			[]string{"<!-- block 7 v1 -->\nA", "<!-- block 9 v1 -->\nB"}},
		{"a comment inside a fence cuts and ends the fence",
			"```\nA\n<!-- block 7 v1 -->\nB\n\nC",
			[]string{"```\nA", "<!-- block 7 v1 -->\nB", "C"}},
		{"a comment written with a backslash in front of it is text",
			"```\nA\n\\<!-- block 7 v1 -->\nB\n```",
			[]string{"```\nA\n\\<!-- block 7 v1 -->\nB\n```"}},
		{"the conflict marker is not a cut", "<!-- block 7 v1 -->\n<!-- conflict -->\nA",
			[]string{"<!-- block 7 v1 -->\n<!-- conflict -->\nA"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mirrorChunks(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("chunk %d is %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The two halves of the escape are each other's inverse, over the lines the
// parser reads as structure and the lines that only look close to one. An
// escaped line is never a boundary, and text that is not a structural line is
// written as it stands.
func TestMirrorEscapesTextThatReadsLikeStructure(t *testing.T) {
	for _, line := range []string{
		"<!-- block 3 v1 -->", "<!-- block 3 v1 -->  ", "<!-- block 3 v1 -->\t",
		"<!-- block 3 v1 -->\r", "\\<!-- block 3 v1 -->", "\\\\<!-- block 3 v1 -->",
		conflictMarker, conflictMarker + " \t", "\\" + conflictMarker, "<!-- block 0 v0 -->",
	} {
		if got := unescaped(escaped(line)); got != line {
			t.Fatalf("%q was written as %q and read back as %q", line, escaped(line), got)
		}
		if id, _ := commentOf(escaped(line)); id != 0 {
			t.Fatalf("%q was written as %q, which the file reads as block %d", line, escaped(line), id)
		}
		if marker(escaped(line)) {
			t.Fatalf("%q was written as %q, which the file reads as a conflict", line, escaped(line))
		}
	}
	// The same lines bare, which is what the file writes above a block: every
	// one of them is read as that block, whatever is on the end of it, so a
	// block whose comment an editor left a space or a tab on keeps its id and
	// its words rather than coming back as a block somebody added.
	for _, line := range []string{
		"<!-- block 3 v1 -->", "<!-- block 3 v1 --> ", "<!-- block 3 v1 -->  \t",
		"<!-- block 3 v1 -->\r",
	} {
		if id, version := commentOf(line); id != 3 || version != 1 {
			t.Fatalf("%q is read as block %d at version %d, want block 3 at version 1", line, id, version)
		}
	}
	for _, line := range []string{conflictMarker, conflictMarker + "  ", conflictMarker + "\r"} {
		if !marker(line) {
			t.Fatalf("%q is not read as a conflict", line)
		}
	}
	// Lines that only read a little like one of the file's own, which are
	// written and read back with nothing done to them. The parser does not take
	// any of these for structure either, which is what keeps the two in step.
	for _, line := range []string{
		"Plain words.", "<!-- block -->", "<!-- block 3 -->", "<!-- block 3 v1 --> and more",
		" <!-- block 3 v1 -->", "<!-- conflicted -->", "<!--block 3 v1-->",
	} {
		if got := escaped(line); got != line {
			t.Fatalf("%q was written as %q", line, got)
		}
		if got := unescaped(line); got != line {
			t.Fatalf("%q was read back as %q", line, got)
		}
		if id, _ := commentOf(line); id != 0 {
			t.Fatalf("%q is read as block %d", line, id)
		}
	}
}

// A block whose text reads like the file's own structure: the comment above a
// block, the marker on a conflicted one, a line already written with a
// backslash in front of one. render writes one more backslash in front of each
// and parseMirror takes one off, so the text is its own round trip whatever it
// quotes, and none of it can be read as a boundary.
func TestMirrorRoundTripsTextThatQuotesTheFormat(t *testing.T) {
	blocks := []Block{{ID: 1, Version: 1, Text: "Before."}, {ID: 2, Version: 4}, {ID: 3, Version: 1, Text: "After."}}
	for _, tc := range []struct{ name, text string }{
		{"a fenced quote of the next block's comment", "```\n<!-- block 3 v1 -->\ncode\n```"},
		{"a fenced quote of a later block's comment", "```\n<!-- block 9 v2 -->\ncode\n```"},
		{"a quote of a comment naming nothing", "```\n<!-- block 999999 v1 -->\ncode\n```"},
		{"a quoted comment in an ordinary paragraph", "As in:\n<!-- block 3 v1 -->\nwhich names a block."},
		{"a line that already starts with a backslash", "\\<!-- block 3 v1 -->"},
		{"two backslashes already", "\\\\<!-- block 3 v1 -->"},
		{"a quoted conflict marker", "```\n" + conflictMarker + "\n```"},
		{"a quoted comment on a line of its own", "<!-- block 3 v1 -->"},
		{"an unclosed fence", "~~~\nstill typing"},
		{"an unclosed fence over a quoted comment", "~~~\n<!-- block 3 v1 -->"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks[1].Text = tc.text
			file, err := parseMirror(render(Document{ID: 7, Proposition: 1, Revision: 2}, blocks, nil))
			if err != nil {
				t.Fatal(err)
			}
			if !sameAs(file.items(), blocks) {
				t.Fatalf("the file parses back to %+v, want %+v", file.Blocks, blocks)
			}
		})
	}
}

// The same, over every text three of those lines make: what render writes,
// parseMirror reads back as the block it was.
func TestMirrorRoundTripsQuotedFormatEverywhere(t *testing.T) {
	lines := []string{
		"Plain words.", "# A heading", "```", "~~~", "```\nA\n\nB\n```",
		"<!-- block 3 v1 -->", "<!-- block 999999 v1 -->", "\\<!-- block 3 v1 -->",
		conflictMarker, "\\" + conflictMarker,
	}
	blocks := []Block{{ID: 1, Version: 1, Text: "Before."}, {ID: 2, Version: 4}, {ID: 3, Version: 1, Text: "After."}}
	covered := 0
	var walk func(text string, left int)
	walk = func(text string, left int) {
		if text != "" {
			// A blank line outside a fenced code block is a block boundary and
			// always has been, so a block whose text holds one comes back as
			// more than one block. That is the rule this is not about.
			if !cutByABlankLine(text) {
				covered++
				blocks[1].Text = text
				file, err := parseMirror(render(Document{ID: 7, Proposition: 1, Revision: 2}, blocks, nil))
				if err != nil {
					t.Fatal(err)
				}
				if !sameAs(file.items(), blocks) {
					t.Fatalf("%q parses back to %+v", text, file.Blocks)
				}
			}
		}
		if left == 0 {
			return
		}
		for _, line := range lines {
			if text == "" {
				walk(line, left-1)
				continue
			}
			walk(text+"\n"+line, left-1)
		}
	}
	walk("", 3)
	if covered < 500 {
		t.Fatalf("only %d texts were covered", covered)
	}
}

// cutByABlankLine is a text the property above leaves out: one holding a blank
// line that no fence covers, which the importer has always read as the end of a
// block.
func cutByABlankLine(text string) bool {
	var f fence
	for _, line := range strings.Split(text, "\n") {
		if !f.track(line) && blank(line) {
			return true
		}
	}
	return false
}

// The document that quotes the comment of the block written directly under it,
// imported after a hand edit somewhere else in the file. Every block keeps its
// id and its words: the quote is escaped in the file, so it is not a boundary,
// and the real comment below it still is.
func TestImportKeepsABlockQuotingAnotherBlocksComment(t *testing.T) {
	ctx := context.Background()
	for _, next := range []bool{true, false} {
		name := "the block written next"
		which := 1
		if !next {
			name = "a block further down"
			which = 2
		}
		t.Run("quoting "+name, func(t *testing.T) {
			f, path := mirrorFixture(t)
			blocks := f.blocks(t)
			quote := "```\n" + lineFor(t, path, blocks[which].ID) + "\ncode\n```"
			if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version, quote, false); err != nil {
				t.Fatal(err)
			}
			if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
				t.Fatal(err)
			}
			was := f.blocks(t)
			if was[0].Text != quote {
				t.Fatalf("the code block went in as %q", was[0].Text)
			}

			save(t, path, strings.Replace(read(t, path), "## Who pays?", "## Who pays for it?", 1))
			if err := f.Import(ctx, path); err != nil {
				t.Fatal(err)
			}
			now := f.blocks(t)
			if len(now) != len(was) {
				t.Fatalf("the import left %d blocks, want %d: %+v", len(now), len(was), now)
			}
			for i, b := range now {
				if b.ID != was[i].ID {
					t.Fatalf("block %d of the document is now %d, want %d", i, b.ID, was[i].ID)
				}
				if b.Text != was[i].Text && b.Text != "## Who pays for it?" {
					t.Fatalf("block %d holds %q, want %q", b.ID, b.Text, was[i].Text)
				}
			}
			if now[len(now)-1].Text != "## Who pays for it?" {
				t.Fatalf("the edit in the file did not go in: %+v", now[len(now)-1])
			}
		})
	}
}

// A paragraph copied in the file, comment line and all, is a paragraph: the
// block it names keeps what it had, the copy goes in after it as a block of its
// own, and the rest of that save is applied with it.
func TestImportMakesANewBlockOfACopiedParagraph(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	was := f.blocks(t)

	copied := lineFor(t, path, was[2].ID) + "\n" + was[2].Text + "\n"
	save(t, path, strings.Replace(read(t, path), "## Is it true?", "## Is it true, though?", 1)+"\n"+copied)
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}

	now := f.blocks(t)
	if len(now) != len(was)+1 {
		t.Fatalf("the import left %d blocks, want %d: %+v", len(now), len(was)+1, now)
	}
	for i, b := range was {
		if now[i].ID != b.ID {
			t.Fatalf("block %d of the document is now %d, want %d", i, now[i].ID, b.ID)
		}
	}
	if now[1].Text != "## Is it true, though?" {
		t.Fatalf("the edit made in the same save did not go in: %+v", now[1])
	}
	if now[2].Text != was[2].Text {
		t.Fatalf("the block that was copied holds %q, want %q", now[2].Text, was[2].Text)
	}
	last := now[len(now)-1]
	if last.Text != was[2].Text || last.ID == was[2].ID {
		t.Fatalf("the copy came out as %+v, want %q in a block of its own", last, was[2].Text)
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
	if _, err := f.SetBlock(ctx, f.who["editor"], b[0].ID, b[0].Version, "# Changed in the browser", false); err != nil {
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
		line = strings.TrimRight(line, "\r")
		if named, _ := commentOf(line); named == id {
			return line
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
	inserted, err := f.InsertBlock(ctx, f.who["editor"], f.doc, blocks[2].ID, "", "Added in the browser.", false)
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?", false); err != nil {
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
		"The tide is high and the moon is full.", false); err != nil {
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
		"The tide is low and the moon is full.", false); err != nil {
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

func TestDeletingAPropositionRemovesItsMirrorsBeforeTheIDIsReused(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	oldDocument := f.doc
	manual := filepath.Join(filepath.Dir(path), "private-notes.md")
	if err := os.WriteFile(manual, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	deleted, err := f.board.DeleteProposition(ctx, f.who["owner"], f.prop)
	if err != nil {
		t.Fatal(err)
	}
	f.applied(ctx, nil, deleted)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the deleted proposition kept its mirror at %s", path)
	}
	if body, err := os.ReadFile(manual); err != nil || string(body) != "keep me" {
		t.Fatalf("deleting the proposition removed a manual file: %q, %v", body, err)
	}

	replacement, err := f.board.CreateProposition(ctx, f.who["owner"], "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.EntityID != f.prop {
		t.Fatalf("replacement proposition id = %d, want reused %d", replacement.EntityID, f.prop)
	}
	document, err := f.CreateDocument(ctx, f.who["owner"], replacement.EntityID, "Research")
	if err != nil {
		t.Fatal(err)
	}
	if document.EntityID != oldDocument {
		t.Fatalf("replacement document id = %d, want reused %d", document.EntityID, oldDocument)
	}
	if err := f.Mirror(ctx, document.EntityID, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the replacement proposition was not mirrored: %v", err)
	}
}

func TestDroppedEventReconciliationPreservesHandEditsAndRejectsAReusedDocumentID(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	manual := strings.Replace(read(t, path), "## Is it true?", "## Hand edit", 1)
	if err := os.WriteFile(path, []byte(manual), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version,
		"A browser edit whose event was dropped.", false); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(f.root, "999-deleted", "stale.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old proposition"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.written[stale] = mirrored{document: f.doc, proposition: f.prop + 1000, hash: hashOf([]byte("old proposition"))}
	movedEdit := filepath.Join(f.root, "998-old-title", "research.md")
	if err := os.MkdirAll(filepath.Dir(movedEdit), 0o755); err != nil {
		f.mu.Unlock()
		t.Fatal(err)
	}
	if err := os.WriteFile(movedEdit, []byte("manual edit in an old path"), 0o600); err != nil {
		f.mu.Unlock()
		t.Fatal(err)
	}
	f.written[movedEdit] = mirrored{document: f.doc, proposition: f.prop, hash: hashOf([]byte("what the app wrote"))}
	f.mu.Unlock()

	f.reconcile(ctx, nil)
	if got := read(t, path); got != manual {
		t.Fatal("reconciliation overwrote a hand edit that was waiting to import")
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciliation kept a stale mirror after its document id was reused: %v", err)
	}
	if body, err := os.ReadFile(movedEdit); err != nil || string(body) != "manual edit in an old path" {
		t.Fatalf("reconciliation removed a hand edit from an old path: %q, %v", body, err)
	}
	f.mu.Lock()
	_, tracked := f.written[movedEdit]
	f.mu.Unlock()
	if tracked {
		t.Fatal("old-path hand edit remained attached to a potentially reused document id")
	}
}

func TestDroppedDeleteDoesNotAttachAHandEditToAnIdenticalReplacement(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	f.mu.Lock()
	oldGeneration := f.written[path].generation
	f.mu.Unlock()
	if oldGeneration == 0 {
		t.Fatal("the original mirror has no document generation")
	}
	oldEdit := strings.Replace(read(t, path), "## Is it true?", "## Old hand edit", 1)
	if err := os.WriteFile(path, []byte(oldEdit), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.board.DeleteProposition(ctx, f.who["owner"], f.prop); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.board.CreateProposition(ctx, f.who["owner"], "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	document, err := f.CreateDocument(ctx, f.who["owner"], replacement.EntityID, "Research")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.EntityID != f.prop || document.EntityID != f.doc {
		t.Fatalf("replacement ids = proposition %d document %d, want reused %d and %d",
			replacement.EntityID, document.EntityID, f.prop, f.doc)
	}
	_, replacementPath, err := f.paths(ctx, document.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if replacementPath != path {
		t.Fatalf("replacement path = %q, want identical %q", replacementPath, path)
	}
	if document.Seq == oldGeneration {
		t.Fatalf("replacement reused document generation %d", oldGeneration)
	}

	f.reconcile(ctx, nil)
	if got := read(t, path); got != oldEdit {
		t.Fatal("reconciliation destroyed the old hand edit")
	}
	f.mu.Lock()
	_, tracked := f.written[path]
	f.mu.Unlock()
	if tracked {
		t.Fatal("old hand edit stayed attached to the replacement document generation")
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	for _, block := range f.blocks(t) {
		if strings.Contains(block.Text, "Old hand edit") {
			t.Fatal("old hand edit was imported into the identical replacement")
		}
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version, "# From the browser", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the browser's change to reach the file", func() bool {
		content, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(content), "# From the browser")
	})

	// A change on disk reaches the database.
	save(t, path, strings.Replace(read(t, path), "# From the browser", "# From the terminal", 1))
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

// legacyRender is compared against a file byte for byte, so it has to be what
// the version before this one wrote, down to where the conflict marker stands
// and the newline on the end.
func TestLegacyRenderIsWhatTheOlderVersionWrote(t *testing.T) {
	d := Document{ID: 7, Proposition: 3, Revision: 12}
	blocks := []Block{
		{ID: 1, Version: 2, Text: "# A heading"},
		{ID: 2, Version: 1, Text: "```\n<!-- block 1 v2 -->\ncode\n```"},
		{ID: 3, Version: 5, Text: "Last."},
	}
	want := "---\nproposition: 3\ndocument: 7\nrevision: 12\n---\n" +
		"\n<!-- block 1 v2 -->\n# A heading\n" +
		"\n<!-- block 2 v1 -->\n```\n<!-- block 1 v2 -->\ncode\n```\n" +
		"\n<!-- block 3 v5 -->\n<!-- conflict -->\nLast.\n"
	if got := string(legacyRender(d, blocks, map[int64]bool{3: true})); got != want {
		t.Fatalf("legacyRender wrote:\n%q\nwant:\n%q", got, want)
	}
}

// Every file on disk at the first start after this version is one the older
// version wrote, with nothing escaped. A document quoting the format is the one
// that matters: read back, the comment it quotes would be a block boundary and
// two blocks would be rewritten from a file nobody had touched. It is
// recognized by what that version would have written and simply written again.
func TestRunRewritesAFileTheOlderVersionWrote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := setup(t, t.TempDir())
	f.Debounce = 40 * time.Millisecond
	f.Every = 0

	blocks := f.blocks(t)
	quoted := fmt.Sprintf(blockCommentFmt, blocks[1].ID, blocks[1].Version)
	quote := "```\n" + quoted + "\ncode\n```"
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version, quote, false); err != nil {
		t.Fatal(err)
	}
	was := f.blocks(t)
	legacy(t, f, nil)

	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	_, path, err := f.paths(ctx, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the file to be written in this version's format", func() bool {
		return strings.Contains(read(t, path), "\\"+quoted)
	})

	now := f.blocks(t)
	if len(now) != len(was) {
		t.Fatalf("the start left %d blocks, want %d: %+v", len(now), len(was), now)
	}
	for i, b := range now {
		if b.ID != was[i].ID || b.Text != was[i].Text || b.Version != was[i].Version {
			t.Fatalf("block %d is %+v, want %+v", i, b, was[i])
		}
	}
	if n := len(revisionsOf(t, f)); n != 0 {
		t.Fatalf("the file was imported: %d revisions", n)
	}
	cancel()
	<-done
}

// The same file with a hand edit in it is not what that version wrote, so it is
// read back the way any other file changed while the process was down is.
func TestRunImportsAFileTheOlderVersionWroteAndSomebodyEdited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := setup(t, t.TempDir())
	f.Debounce = 40 * time.Millisecond
	f.Every = 0

	legacy(t, f, func(content string) string {
		return strings.Replace(content, "## Is it true?", "## Is it true, though?", 1)
	})
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

// legacy writes the document's file as the version before this one wrote it,
// which is the state of every mirror file the first time this version starts.
// edit is what somebody did to it while the process was down, or nil.
func legacy(t *testing.T, f *fixture, edit func(string) string) {
	t.Helper()
	ctx := context.Background()
	d, err := GetDocument(ctx, f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	dir, path, err := f.paths(ctx, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	content := string(legacyRender(d, f.blocks(t), nil))
	if edit != nil {
		content = edit(content)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
		"## Is it true in the browser?", false); err != nil {
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
	save(t, path, strings.Replace(stale, "## Is it true?", "## Is it true at the terminal?", 1)+
		"\nAnd a paragraph typed into the file.\n")
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
		if _, err := f.InsertBlock(ctx, f.who["editor"], e.EntityID, 0, "", "In "+name+".", false); err != nil {
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, blocks[0].Version, "# Written while blocked", false); err != nil {
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
	if _, err := f.SetBlock(ctx, f.who["editor"], after[0].ID, after[0].Version, "# Written after the block cleared", false); err != nil {
		t.Fatal(err)
	}
	if err := f.Mirror(ctx, f.doc, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), "# Written after the block cleared") {
		t.Fatalf("the mirror never wrote again:\n%s", read(t, path))
	}
}

func TestMirrorDoesNotOverwriteAnUnreadableUnknownFile(t *testing.T) {
	ctx := context.Background()
	f := setup(t, t.TempDir())
	_, path, err := f.paths(ctx, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const handEdit = "an unreadable hand edit\n"
	if err := os.WriteFile(path, []byte(handEdit), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("file permissions are not enforced")
	}

	if err := f.Mirror(ctx, f.doc, nil, false); err == nil {
		t.Fatal("mirror overwrote a file it could not inspect")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != handEdit {
		t.Fatalf("unreadable hand edit was replaced with %q", got)
	}
	f.mu.Lock()
	_, tracked := f.written[path]
	f.mu.Unlock()
	if tracked {
		t.Fatal("unreadable unknown file was claimed as a mirror")
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?", false); err != nil {
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[2].ID, blocks[2].Version, "## Who pays for it?", false); err != nil {
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[0].ID, f.blocks(t)[0].Version, "# Later still", false); err != nil {
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?", false); err != nil {
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
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version, "## Is it true in the browser?", false); err != nil {
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

// texts is a document's blocks in order, for comparing a whole document.
func texts(blocks []Block) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Text)
	}
	return out
}

// A sentence written straight under a heading is a block of its own, and it
// belongs between the heading and whatever the file has after it. The command
// that writes the heading's own change puts it there and answers with the
// heading, so following that answer would leave the next chunk of the file in
// between the two.
func TestImportKeepsAParagraphWrittenUnderABlockInPlace(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)

	if err := os.WriteFile(path, []byte(strings.Replace(read(t, path), "## Is it true?",
		"## Is it true?\nA note under it.\n\nBrand new paragraph.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Import(ctx, path); err != nil {
		t.Fatal(err)
	}
	got := texts(f.blocks(t))
	want := []string{"# The sea is a battery.", "## Is it true?", "A note under it.",
		"Brand new paragraph.", "## Who pays?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the document reads %v, want %v", got, want)
	}
}

// The same, for a block whose own change the database will not take. What was
// written under it is words that are in the file and nowhere else, so it goes
// in even though the block itself stays as the database has it and comes back
// marked.
func TestImportKeepsAParagraphWrittenUnderABlockItCouldNotSet(t *testing.T) {
	ctx := context.Background()
	f, path := mirrorFixture(t)
	blocks := f.blocks(t)

	// The terminal rewrites the heading and writes a note under it.
	stale := strings.Replace(read(t, path), "## Is it true?",
		"## Is it true at the terminal?\nA note under it.", 1)
	// The browser rewrites the same heading another way.
	if _, err := f.SetBlock(ctx, f.who["editor"], blocks[1].ID, blocks[1].Version,
		"## Is it true in the browser?", false); err != nil {
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

	got := texts(f.blocks(t))
	want := []string{"# The sea is a battery.", "## Is it true in the browser?",
		"A note under it.", "## Who pays?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the document reads %v, want %v", got, want)
	}
	if !strings.Contains(read(t, path), conflictMarker) {
		t.Fatalf("the block that would not take the change came back unmarked:\n%s", read(t, path))
	}
}

// A file operation refused because something else has the file open is worth
// trying again; anything else is not. The platform that refuses these is the
// one the tests and the gate run on, so the two errors it uses are named.
func TestWaitingOutAFileSomebodyElseHasOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		again bool
	}{
		// A rename refused for a handle is Access is denied, which is worth
		// waiting out only where it means that; everywhere else it is a
		// permission the process does not have.
		{"a rename over a file another handle holds", fs.ErrPermission, runtime.GOOS == "windows"},
		{"a read of a file being replaced", sharingViolation, true},
		{"a file that is not there", fs.ErrNotExist, false},
		{"a path that is not a directory", errors.New("not a directory"), false},
		{"no error at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inUse(tc.err); got != tc.again {
				t.Fatalf("inUse(%v) = %v, want %v", tc.err, got, tc.again)
			}
		})
	}

	// An operation that stops being refused goes through.
	tries := 0
	if err := waitOut(func() error {
		tries++
		if tries < 4 {
			return &os.LinkError{Op: "rename", Err: sharingViolation}
		}
		return nil
	}); err != nil {
		t.Fatalf("an operation that came good was given up on: %v", err)
	}
	if tries != 4 {
		t.Fatalf("it went round %d times", tries)
	}

	// Anything else comes back at once rather than a second later.
	tries = 0
	started := time.Now()
	if err := waitOut(func() error {
		tries++
		return fs.ErrNotExist
	}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	if tries != 1 {
		t.Fatalf("a missing file was tried %d times", tries)
	}
	if waited := time.Since(started); waited > inUseWait/2 {
		t.Fatalf("a missing file was waited on for %s", waited)
	}

	// And one that never lets go is given up on inside the budget.
	started = time.Now()
	if err := waitOut(func() error { return sharingViolation }); !errors.Is(err, sharingViolation) {
		t.Fatalf("got %v", err)
	}
	if waited := time.Since(started); waited < inUseWait || waited > 3*inUseWait {
		t.Fatalf("it waited %s, want about %s", waited, inUseWait)
	}
}

// A block whose stored text is exactly what a chunk of the file reads is now
// left alone rather than cut into paragraphs first. Two shapes of block hold
// text Paragraphs would cut, both of them written by a save made while somebody
// was typing, and only one of them survives the round trip to the file as one
// chunk.
func TestImportLeavesABlockThatReadsAsItIsStored(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		want         []string
	}{
		{
			// The file writes the two lines under one comment and the read back
			// cuts only at blank lines and at comments, so the chunk is the
			// stored text and nothing is done to it.
			name:   "a heading under a line keeps its block",
			stored: "Intro.\n# Heading",
			want:   []string{"Intro.\n# Heading"},
		},
		{
			// A blank line is where the file itself cuts, so this block comes
			// back as two chunks, the first of which is not the stored text,
			// and the import normalizes it as it always has.
			name:   "a blank line inside a block is still cut",
			stored: "Above.\n\nBelow.",
			want:   []string{"Above.", "Below."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, path := mirrorFixture(t)
			for _, b := range f.blocks(t) {
				if _, err := f.DeleteBlock(ctx, f.who["editor"], b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.InsertBlock(ctx, f.who["editor"], f.doc, 0, "", tc.stored, true); err != nil {
				t.Fatal(err)
			}
			if err := f.Mirror(ctx, f.doc, nil, true); err != nil {
				t.Fatal(err)
			}
			// A trailing newline is a file somebody touched and nothing else,
			// which is what makes the import read it back rather than take it
			// for its own write.
			save(t, path, read(t, path)+"\n")
			if err := f.Import(ctx, path); err != nil {
				t.Fatal(err)
			}
			if got := f.texts(t); !same(got, tc.want) {
				t.Fatalf("the document reads %q, want %q", got, tc.want)
			}
		})
	}
}
