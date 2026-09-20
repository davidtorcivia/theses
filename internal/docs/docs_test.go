package docs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/davidtorcivia/theses/internal/auth"
	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/store"
)

type fixture struct {
	*Service
	t     *testing.T
	board *board.Service
	db    *store.DB
	who   map[string]core.Actor
	prop  int64
	doc   int64
}

// discard is the logger a test hands the service: the warnings it writes are
// about the machine, not about the assertion.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup is one owner, one editor, one researcher, one guest and one outsider,
// with a proposition the first four are members of and one document in it. The
// board service is built first, because it is what installs the archived rule
// every document command asks before it writes.
func setup(t *testing.T, dir string) *fixture {
	t.Helper()
	ctx := context.Background()
	db := store.OpenTemp(t)
	c := core.New(db, core.NewBus())
	b := board.New(c, func() board.Defaults {
		return board.Defaults{Status: "idea", Statuses: []string{"idea", "recording"}, Columns: []string{"Research"}}
	})
	svc := New(c, dir, func() string { return "# {statement}\n\n## Is it true?\n## Who pays?" }, discard())

	f := &fixture{Service: svc, t: t, board: b, db: db, who: map[string]core.Actor{}}
	for _, u := range []struct{ handle, role string }{
		{"owner", auth.RoleOwner},
		{"editor", auth.RoleEditor},
		{"researcher", auth.RoleResearcher},
		{"guest", auth.RoleGuest},
		{"outsider", auth.RoleEditor},
	} {
		id, err := store.CreateUser(ctx, db, &store.User{
			Handle: u.handle, Email: u.handle + "@example.com", Name: u.handle,
			Initials: "XX", Colour: "#1100ff", Role: u.role, PasswordHash: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		f.who[u.handle] = core.Actor{Kind: core.KindUser, ID: id, Name: u.handle}
	}

	e, err := b.CreateProposition(ctx, f.who["owner"], "Tidal Power")
	if err != nil {
		t.Fatal(err)
	}
	f.prop = e.EntityID
	if _, err := b.EditProposition(ctx, f.who["owner"], f.prop, "Tidal Power", "The sea is a battery.", ""); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"editor", "researcher", "guest"} {
		if _, err := b.AddMember(ctx, f.who["owner"], f.prop, f.who[handle].ID); err != nil {
			t.Fatal(err)
		}
	}
	created, err := svc.CreateDocument(ctx, f.who["editor"], f.prop, "Research")
	if err != nil {
		t.Fatal(err)
	}
	f.doc = created.EntityID
	return f
}

func (f *fixture) blocks(t *testing.T) []Block {
	t.Helper()
	blocks, err := Blocks(context.Background(), f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func (f *fixture) user(t *testing.T, handle string) *store.User {
	t.Helper()
	u, err := store.UserByID(context.Background(), f.db, f.who[handle].ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The two characters some of the rows below turn on, written as their code
// points: neither can be seen in the source, and a tool that swallowed one on
// the way through would leave a row that tested nothing.
var (
	lineSeparator = string(rune(0x2028))
	noBreakSpace  = string(rune(0x00a0))
)

func TestParagraphs(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"one paragraph", "Just a line.", []string{"Just a line."}},
		{"blank line splits", "One.\n\nTwo.", []string{"One.", "Two."}},
		{"windows line endings", "One.\r\n\r\nTwo.", []string{"One.", "Two."}},
		{"stacked headings each stand alone", "# A\n## B\n## C", []string{"# A", "## B", "## C"}},
		{"a heading ends the paragraph before it", "Text.\n# A", []string{"Text.", "# A"}},
		{"a list stays one block", "- one\n- two", []string{"- one\n- two"}},
		{"whitespace only is nothing", "  \n\n\t", nil},
		{"a hash without a space is not a heading", "#hashtag", []string{"#hashtag"}},
		{"a blank line inside a fence does not cut", "```\nA\n\nB\n```", []string{"```\nA\n\nB\n```"}},
		{"a hash inside a tilde fence is not a heading", "~~~\n# A\n~~~", []string{"~~~\n# A\n~~~"}},
		{"a shorter fence does not close a longer one", "````\nA\n\n```\nB\n````", []string{"````\nA\n\n```\nB\n````"}},
		{"a tilde does not close a backtick fence", "```\n~~~\n\nA\n```\n\nAfter.",
			[]string{"```\n~~~\n\nA\n```", "After."}},
		{"a fence needs nothing but space after it to close", "```\nA\n``` and more\n\nB",
			[]string{"```\nA\n``` and more\n\nB"}},
		{"four spaces is not a fence", "    ```\n\nAfter.", []string{"```", "After."}},
		{"a backtick in the info string is not a fence", "```a``` b\n\nAfter.", []string{"```a``` b", "After."}},
		{"a fence that is never closed runs to the end", "```go\nA\n\n# B", []string{"```go\nA\n\n# B"}},
		{"two fences with a paragraph between them", "```\nA\n\nB\n```\n\nBetween.\n\n~~~\nC\n\nD\n~~~",
			[]string{"```\nA\n\nB\n```", "Between.", "~~~\nC\n\nD\n~~~"}},
		{"a fence under a heading is its own block", "# Title\n```\nA\n\nB\n```",
			[]string{"# Title", "```\nA\n\nB\n```"}},
		{"windows line endings inside a fence", "```\r\nA\r\n\r\nB\r\n```", []string{"```\nA\n\nB\n```"}},
		{"a closing fence with spaces after it closes", "```\nA\n```  \n\nB", []string{"```\nA\n```", "B"}},
		{"a line separator in the info string does not stop a fence opening",
			"```" + lineSeparator + "js\nA\n\n# B", []string{"```" + lineSeparator + "js\nA\n\n# B"}},
		{"a line holding a no-break space is not blank",
			"A\n" + noBreakSpace + "\nB", []string{"A\n" + noBreakSpace + "\nB"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Paragraphs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("block %d is %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Research", "research"},
		{"Show notes", "show-notes"},
		{"  Spaced  out  ", "spaced-out"},
		{"...", "untitled"},
		{"", "untitled"},
		{strings.Repeat("a", 80), strings.Repeat("a", 60)},
	} {
		if got := Slug(tc.in); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCreateDocumentSeedsTheTemplateOnceAndNumbersClashingSlugs(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")

	first := f.blocks(t)
	if len(first) != 3 || first[0].Text != "# The sea is a battery." {
		t.Fatalf("the first document is %+v", first)
	}
	if first[0].Position >= first[1].Position || first[1].Position >= first[2].Position {
		t.Fatalf("blocks are out of order: %q %q %q", first[0].Position, first[1].Position, first[2].Position)
	}

	// A second document is not a second copy of the research outline.
	second, err := f.CreateDocument(ctx, f.who["editor"], f.prop, "Research")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := Blocks(ctx, f.db, second.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Text != "# Research" {
		t.Fatalf("the second document is %+v", blocks)
	}
	d, err := GetDocument(ctx, f.db, second.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Slug != "research-2" {
		t.Fatalf("slug is %q, want research-2", d.Slug)
	}
}

func TestBlockCommandsAskMembershipAndTheArchivedRule(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		run  func(f *fixture, a core.Actor) error
	}{
		{"insert", func(f *fixture, a core.Actor) error {
			_, err := f.InsertBlock(ctx, a, f.doc, 0, "", "Hello.", false)
			return err
		}},
		{"set", func(f *fixture, a core.Actor) error {
			b := f.blocks(f.t)
			_, err := f.SetBlock(ctx, a, b[0].ID, b[0].Version, "Changed.", false)
			return err
		}},
		{"move", func(f *fixture, a core.Actor) error {
			b := f.blocks(f.t)
			_, err := f.MoveBlock(ctx, a, b[0].ID, b[2].ID)
			return err
		}},
		{"delete", func(f *fixture, a core.Actor) error {
			b := f.blocks(f.t)
			_, err := f.DeleteBlock(ctx, a, b[0].ID)
			return err
		}},
		{"revision", func(f *fixture, a core.Actor) error {
			_, err := f.CreateRevision(ctx, a, f.doc, ReasonManual)
			return err
		}},
		{"rename", func(f *fixture, a core.Actor) error {
			_, err := f.RenameDocument(ctx, a, f.doc, "Notes")
			return err
		}},
	} {
		t.Run(tc.name+" refuses an outsider", func(t *testing.T) {
			f := setup(t, "")
			if err := tc.run(f, f.who["outsider"]); !errors.Is(err, core.ErrForbidden) {
				t.Fatalf("an outsider got %v, want forbidden", err)
			}
		})
		t.Run(tc.name+" refuses a guest", func(t *testing.T) {
			f := setup(t, "")
			if err := tc.run(f, f.who["guest"]); !errors.Is(err, core.ErrForbidden) {
				t.Fatalf("a guest got %v, want forbidden", err)
			}
		})
		t.Run(tc.name+" refuses an archived proposition", func(t *testing.T) {
			f := setup(t, "")
			f.t = t
			if _, err := f.board.ArchiveProposition(ctx, f.who["owner"], f.prop); err != nil {
				t.Fatal(err)
			}
			if err := tc.run(f, f.who["editor"]); !errors.Is(err, board.ErrArchived) {
				t.Fatalf("an archived proposition got %v, want the archived refusal", err)
			}
		})
		t.Run(tc.name+" lets a member through", func(t *testing.T) {
			f := setup(t, "")
			if err := tc.run(f, f.who["editor"]); err != nil {
				t.Fatalf("a member got %v", err)
			}
		})
	}
}

// A block is a paragraph of a document, and taking one out is writing the
// document: a researcher may do it, and the empty block they just left is
// theirs to remove. The document itself is the other thing.
func TestDeletingABlockIsAnEditAndDeletingADocumentIsNot(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		run  func(f *fixture, a core.Actor) error
		want error
	}{
		{"a researcher may delete a block", func(f *fixture, a core.Actor) error {
			_, err := f.DeleteBlock(ctx, a, f.blocks(f.t)[0].ID)
			return err
		}, nil},
		{"a researcher may not delete a document", func(f *fixture, a core.Actor) error {
			_, err := f.DeleteDocument(ctx, a, f.doc)
			return err
		}, core.ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			f.t = t
			if err := tc.run(f, f.who["researcher"]); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSetBlockMergesOrConflicts(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		base, theirs string
		ours         string
		want         string
		wantConflict bool
		// seeded uses the first block the document was created with rather
		// than one this test inserted. A block written when the document was
		// made has to be as mergeable as any other, which it is not if the
		// text it started from left no trace to recover.
		seeded bool
	}{
		{name: "clean on the current version", base: "One two three.", theirs: "",
			ours: "One two four.", want: "One two four."},
		{name: "different words in one line merge",
			base:   "The tide is high and the moon is full.",
			theirs: "The tide is low and the moon is full.",
			ours:   "The tide is high and the moon is new.",
			want:   "The tide is low and the moon is new."},
		{name: "the same word changed two ways conflicts",
			base:         "The tide is high.",
			theirs:       "The tide is low.",
			ours:         "The tide is slack.",
			wantConflict: true},
		{name: "the same change from both sides is not a conflict",
			base: "One.", theirs: "Two.", ours: "Two.", want: "Two."},
		{name: "a block the document was created with merges like any other",
			seeded: true,
			base:   "# The sea is a battery.",
			theirs: "# The tide is a battery.",
			ours:   "# The sea is a flywheel.",
			want:   "# The tide is a flywheel."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			var id int64
			if tc.seeded {
				id = f.blocks(t)[0].ID
			} else {
				inserted, err := f.InsertBlock(ctx, f.who["editor"], f.doc, 0, "", tc.base, false)
				if err != nil {
					t.Fatal(err)
				}
				id = inserted.EntityID
			}
			started, err := GetBlock(ctx, f.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if started.Text != tc.base {
				t.Fatalf("the block starts at %q, want %q", started.Text, tc.base)
			}
			if tc.theirs != "" {
				if _, err := f.SetBlock(ctx, f.who["owner"], id, started.Version, tc.theirs, false); err != nil {
					t.Fatal(err)
				}
			}

			_, err = f.SetBlock(ctx, f.who["editor"], id, started.Version, tc.ours, false)
			var conflict *core.ConflictError
			if tc.wantConflict {
				if !errors.As(err, &conflict) {
					t.Fatalf("got %v, want a conflict", err)
				}
				if conflict.Current != tc.theirs {
					t.Fatalf("the conflict carries %q, want %q", conflict.Current, tc.theirs)
				}
				now, err := GetBlock(ctx, f.db, id)
				if err != nil {
					t.Fatal(err)
				}
				if now.Text != tc.theirs {
					t.Fatalf("a refused set changed the block to %q", now.Text)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			now, err := GetBlock(ctx, f.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if now.Text != tc.want {
				t.Fatalf("the block holds %q, want %q", now.Text, tc.want)
			}
		})
	}
}

// A save made while somebody is typing stores the text as it was sent. What
// the ordinary save does to it, trimming the edges and cutting it into
// paragraphs, is right for a finished edit and wrong under a caret.
func TestSetBlockWholeStoresTheTextAsItWasSent(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		whole   bool
		start   string
		theirs  string
		text    string
		want    string
		blocks  int
		wantErr error
	}{
		{name: "whole keeps the edges and the blank line", whole: true,
			text: "  One.\n\nTwo.\n", want: "  One.\n\nTwo.\n", blocks: 3},
		{name: "a plain set of the same text trims it and cuts it up",
			text: "  One.\n\nTwo.\n", want: "One.", blocks: 4},
		{name: "whole normalises the line endings and nothing else", whole: true,
			text: "One.\r\n Two. ", want: "One.\n Two. ", blocks: 3},
		{name: "a whole save from a stale version still merges", whole: true,
			start:  "The tide is high and the moon is full.",
			theirs: "The tide is low and the moon is full.",
			text:   "The tide is high and the moon is new.",
			want:   "The tide is low and the moon is new.", blocks: 3},
		{name: "a whole save longer than a block may be is refused", whole: true,
			text: strings.Repeat("a", board.MaxBody+1), wantErr: board.ErrTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			id := f.blocks(t)[0].ID
			if tc.start != "" {
				if _, err := f.SetBlock(ctx, f.who["owner"], id, f.blocks(t)[0].Version, tc.start, false); err != nil {
					t.Fatal(err)
				}
			}
			started, err := GetBlock(ctx, f.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.theirs != "" {
				if _, err := f.SetBlock(ctx, f.who["owner"], id, started.Version, tc.theirs, false); err != nil {
					t.Fatal(err)
				}
			}

			_, err = f.SetBlock(ctx, f.who["editor"], id, started.Version, tc.text, tc.whole)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			now, err := GetBlock(ctx, f.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if now.Text != tc.want {
				t.Fatalf("the block holds %q, want %q", now.Text, tc.want)
			}
			if n := len(f.blocks(t)); n != tc.blocks {
				t.Fatalf("the document has %d blocks, want %d", n, tc.blocks)
			}
		})
	}
}

// The insert the editor makes when Enter splits a block carries the same flag
// and for the same reason: the half paragraph somebody is in the middle of
// writing goes in as it was typed, in one block, rather than being trimmed and
// cut up under their caret.
func TestInsertBlockWholeStoresTheTextAsItWasSent(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		whole   bool
		text    string
		want    []string
		wantErr error
	}{
		{name: "whole keeps the edges and the blank line in one block", whole: true,
			text: "  One.\n\nTwo.\n", want: []string{"  One.\n\nTwo.\n"}},
		{name: "a plain insert of the same text trims it and cuts it up",
			text: "  One.\n\nTwo.\n", want: []string{"One.", "Two."}},
		{name: "whole normalises the line endings and nothing else", whole: true,
			text: "One.\r\n Two. ", want: []string{"One.\n Two. "}},
		{name: "a whole insert of nothing is one empty block", whole: true,
			text: "", want: []string{""}},
		{name: "a whole insert longer than a block may be is refused", whole: true,
			text: strings.Repeat("a", board.MaxBody+1), wantErr: board.ErrTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			was := f.blocks(t)

			_, err := f.InsertBlock(ctx, f.who["editor"], f.doc, was[0].ID, "", tc.text, tc.whole)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			now := f.blocks(t)
			if len(now) != len(was)+len(tc.want) {
				t.Fatalf("the document has %d blocks, want %d", len(now), len(was)+len(tc.want))
			}
			var got []string
			for _, b := range now[1 : 1+len(tc.want)] {
				got = append(got, b.Text)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("the blocks after the first read %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetBlockWithoutARecoverableBaseIsAConflict(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)

	// Version 9 was never written, so the text it held cannot be recovered and
	// there is nothing honest to merge against.
	_, err := f.SetBlock(ctx, f.who["editor"], b[0].ID, 9, "Something else.", false)
	var conflict *core.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	if conflict.Current != b[0].Text {
		t.Fatalf("the conflict carries %q, want %q", conflict.Current, b[0].Text)
	}
}

// The triggers migration 004 put on the blocks table are what fills
// block_texts, so a base is kept whichever path wrote the text.
func TestBlockTextsFollowEveryWriteAndKeepTwenty(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)[0]

	if got := blockTexts(t, f, b.ID); len(got) != 1 || got[1] != b.Text {
		t.Fatalf("after the insert block_texts holds %v, want version 1 as %q", got, b.Text)
	}

	// Twenty saves take the version to twenty one, which is the first that
	// drops a row: everything at or below version one goes.
	version := b.Version
	for i := 0; i < 20; i++ {
		if _, err := f.SetBlock(ctx, f.who["editor"], b.ID, version, "Save "+strconv.Itoa(i)+".", true); err != nil {
			t.Fatal(err)
		}
		now, err := GetBlock(ctx, f.db, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		version = now.Version
	}
	kept := blockTexts(t, f, b.ID)
	if len(kept) != 20 {
		t.Fatalf("block_texts holds %d versions, want 20", len(kept))
	}
	if _, ok := kept[1]; ok {
		t.Error("the twenty first version did not drop the first")
	}
	if kept[21] != "Save 19." {
		t.Errorf("version 21 holds %q, want %q", kept[21], "Save 19.")
	}

	// An undo writes the columns back itself rather than through SetBlock, and
	// it too leaves a base behind, at the version it moved the block on to.
	e, err := f.SetBlock(ctx, f.who["editor"], b.ID, version, "Undone shortly.", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], e.Seq); err != nil {
		t.Fatal(err)
	}
	if got := blockTexts(t, f, b.ID); got[23] != "Save 19." {
		t.Errorf("the undo left version 23 as %q, want %q", got[23], "Save 19.")
	}

	if _, err := f.DeleteDocument(ctx, f.who["owner"], f.doc); err != nil {
		t.Fatal(err)
	}
	if got := blockTexts(t, f, b.ID); len(got) != 0 {
		t.Errorf("the document's delete left %d rows in block_texts", len(got))
	}
}

func blockTexts(t *testing.T, f *fixture, block int64) map[int64]string {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT version, text FROM block_texts WHERE block_id = ?`, block)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var version int64
		var text string
		if err := rows.Scan(&version, &text); err != nil {
			t.Fatal(err)
		}
		out[version] = text
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// baseText has two places to look and a stale set merges from either: the
// block_texts row a trigger wrote, or, for a version written before that table
// existed, the after of the activity row that produced it.
func TestBaseTextComesFromEitherTheTableOrTheLog(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		clear string
	}{
		{name: "from block_texts", clear: `DELETE FROM activity WHERE entity = 'block'`},
		{name: "from the activity log", clear: `DELETE FROM block_texts`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			inserted, err := f.InsertBlock(ctx, f.who["editor"], f.doc, 0, "",
				"The tide is high and the moon is full.", false)
			if err != nil {
				t.Fatal(err)
			}
			id := inserted.EntityID
			started, err := GetBlock(ctx, f.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.SetBlock(ctx, f.who["owner"], id, started.Version,
				"The tide is low and the moon is full.", false); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, tc.clear); err != nil {
				t.Fatal(err)
			}

			if _, err := f.SetBlock(ctx, f.who["editor"], id, started.Version,
				"The tide is high and the moon is new.", false); err != nil {
				t.Fatal(err)
			}
			now, err := GetBlock(ctx, f.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if want := "The tide is low and the moon is new."; now.Text != want {
				t.Fatalf("the block holds %q, want %q", now.Text, want)
			}
		})
	}
}

func TestSetBlockSplitsAPasteIntoBlocks(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)

	if _, err := f.SetBlock(ctx, f.who["editor"], b[0].ID, b[0].Version, "# Title\n\nAnd a paragraph.", false); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	if len(after) != 4 {
		t.Fatalf("got %d blocks, want 4: %+v", len(after), after)
	}
	if after[0].Text != "# Title" || after[1].Text != "And a paragraph." {
		t.Fatalf("the paste landed as %q then %q", after[0].Text, after[1].Text)
	}
}

func TestDeleteTombstonesAndUndoRestores(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)

	deleted, err := f.DeleteBlock(ctx, f.who["editor"], b[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := GetBlock(ctx, f.db, b[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.DeletedAt == nil {
		t.Fatal("the block was not tombstoned")
	}
	if len(f.blocks(t)) != 2 {
		t.Fatalf("a tombstoned block is still in the list: %+v", f.blocks(t))
	}
	// A tombstoned block is not there as far as any command is concerned.
	if _, err := f.SetBlock(ctx, f.who["editor"], b[1].ID, gone.Version, "x", false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("setting a tombstoned block got %v", err)
	}

	if _, err := f.Undo(ctx, f.who["editor"], deleted.Seq); err != nil {
		t.Fatalf("undo of a tombstone: %v", err)
	}
	back, err := GetBlock(ctx, f.db, b[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.DeletedAt != nil || back.Text != b[1].Text {
		t.Fatalf("the block came back as %+v", back)
	}
	if back.Version <= gone.Version {
		t.Fatalf("undo left the version at %d, which a stale edit would still match", back.Version)
	}
	if len(f.blocks(t)) != 3 {
		t.Fatalf("the restored block is not in the list: %+v", f.blocks(t))
	}
}

func TestUndoOfASetPutsTheTextBack(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)

	changed, err := f.SetBlock(ctx, f.who["editor"], b[2].ID, b[2].Version, "Rewritten.", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Undo(ctx, f.who["editor"], changed.Seq); err != nil {
		t.Fatal(err)
	}
	back, err := GetBlock(ctx, f.db, b[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Text != b[2].Text {
		t.Fatalf("the block holds %q, want %q", back.Text, b[2].Text)
	}
}

// A folded run is still one undo. core.Compact keeps the newest row of the
// run, so undo's check that the block has not moved on since passes, and the
// before it puts back is the text the person started typing over.
func TestUndoOfACompactedRunPutsThePreRunTextBack(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)[0]

	// The saves are dated four days back so the run is old enough to fold; the
	// compaction itself runs on the real clock.
	clock := time.Now().Add(-4 * 24 * time.Hour)
	f.Now = func() time.Time { return clock }
	version := b.Version
	var last core.Event
	for i := 0; i < 5; i++ {
		clock = clock.Add(30 * time.Second)
		e, err := f.SetBlock(ctx, f.who["editor"], b.ID, version, "Draft "+strconv.Itoa(i)+".", true)
		if err != nil {
			t.Fatal(err)
		}
		last = e
		now, err := GetBlock(ctx, f.db, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		version = now.Version
	}
	f.Now = time.Now

	removed, err := f.Compact(ctx, core.CompactAfter)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 {
		t.Fatalf("Compact removed %d rows of the run of five, want 4", removed)
	}
	if _, err := f.Undo(ctx, f.who["editor"], last.Seq); err != nil {
		t.Fatal(err)
	}
	back, err := GetBlock(ctx, f.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Text != b.Text {
		t.Fatalf("the block holds %q, want the text before the run, %q", back.Text, b.Text)
	}
}

func TestMoveBlockReorders(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	b := f.blocks(t)

	if _, err := f.MoveBlock(ctx, f.who["editor"], b[0].ID, b[2].ID); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	if after[2].ID != b[0].ID {
		t.Fatalf("the moved block is at %d, want last: %+v", indexOf(after, b[0].ID), after)
	}
	// Back to the head.
	if _, err := f.MoveBlock(ctx, f.who["editor"], b[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if f.blocks(t)[0].ID != b[0].ID {
		t.Fatal("moving to the head did not")
	}
}

func indexOf(blocks []Block, id int64) int {
	for i, b := range blocks {
		if b.ID == id {
			return i
		}
	}
	return -1
}

func TestReadsAreThroughMembership(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")

	for _, handle := range []string{"owner", "editor", "guest"} {
		if _, err := f.Documents(ctx, f.user(t, handle), f.prop); err != nil {
			t.Fatalf("%s could not read the documents: %v", handle, err)
		}
	}
	// A non-member is told the proposition is not there rather than that they
	// may not read it.
	if _, err := f.Documents(ctx, f.user(t, "outsider"), f.prop); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("an outsider got %v, want not found", err)
	}
	if _, err := f.Document(ctx, f.user(t, "outsider"), f.doc); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("an outsider read a document: %v", err)
	}
	if _, err := f.History(ctx, f.user(t, "outsider"), f.doc); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("an outsider read the history: %v", err)
	}
}

func TestRevisionsRecordTheDocumentAndRefuseAnInventedReason(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")

	if _, err := f.CreateRevision(ctx, f.who["editor"], f.doc, "because"); !errors.Is(err, ErrReason) {
		t.Fatalf("an invented reason got %v", err)
	}
	if _, err := f.CreateRevision(ctx, f.who["editor"], f.doc, ReasonManual); err != nil {
		t.Fatal(err)
	}
	list, err := ListRevisions(ctx, f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Reason != ReasonManual {
		t.Fatalf("revisions are %+v", list)
	}
	if list[0].Markdown != Markdown(f.blocks(t)) {
		t.Fatalf("the revision holds %q", list[0].Markdown)
	}
}

// The periodic timer writes a revision for as long as the editing goes on and
// forgets the document when it stops, which is the whole of "while a document
// is being edited".
func TestPeriodicRevisionsStopWhenTheEditingDoes(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	f.Every = 20 * time.Millisecond
	t.Cleanup(f.Stop)

	b := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["editor"], b[0].ID, b[0].Version, "Edited once.", false); err != nil {
		t.Fatal(err)
	}
	if f.Editing() != 1 {
		t.Fatal("the edit did not arm a timer")
	}
	waitFor(t, "the first periodic revision", func() bool { return len(revisionsOf(t, f)) == 1 })
	if revisionsOf(t, f)[0].Reason != ReasonPeriodic {
		t.Fatalf("the revision is %+v", revisionsOf(t, f)[0])
	}

	// Nothing has been edited since, so the next tick forgets the document and
	// no further revisions are written.
	waitFor(t, "the timer to stop", func() bool { return f.Editing() == 0 })
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := len(revisionsOf(t, f)); n != 1 {
			t.Fatalf("a stopped timer wrote %d revisions", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Saving as somebody types means a tick can find the document reading exactly
// as the newest version does: a word written and taken back, or an edit merged
// into what was already there. Keeping that copy would push the versions worth
// restoring to off the end of the history list.
func TestPeriodicRevisionsSkipASnapshotNothingChanged(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		text func(was string) string
		want int
	}{
		{"a set that leaves the document as it was keeps no version",
			func(was string) string { return was }, 1},
		{"a set that changes it keeps one",
			func(string) string { return "Rewritten." }, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			f.Every = 20 * time.Millisecond
			t.Cleanup(f.Stop)
			if _, err := f.CreateRevision(ctx, f.who["editor"], f.doc, ReasonManual); err != nil {
				t.Fatal(err)
			}

			b := f.blocks(t)
			if _, err := f.SetBlock(ctx, f.who["editor"], b[0].ID, b[0].Version, tc.text(b[0].Text), false); err != nil {
				t.Fatal(err)
			}
			// Two ticks: the one that acts on the edit and the one that finds
			// nothing edited since and forgets the document. Nothing can write
			// another revision once the timers have stopped.
			waitFor(t, "the timers to stop with the history settled", func() bool {
				return f.Editing() == 0 && len(revisionsOf(t, f)) == tc.want
			})
			if n := len(revisionsOf(t, f)); n != tc.want {
				t.Fatalf("the history holds %d versions, want %d", n, tc.want)
			}
		})
	}
}

func revisionsOf(t *testing.T, f *fixture) []Revision {
	t.Helper()
	list, err := ListRevisions(context.Background(), f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// waitFor polls until the condition holds or the test fails, so a broken timer
// is a failure rather than a hang.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// after_key is how a block whose id nobody knows yet is named: the key the
// command that makes it was sent under. A browser with no connection draws the
// block it has just made and queues the insert, and a second block made below
// the first has only that key to point at.
func TestInsertAfterKey(t *testing.T) {
	ctx := context.Background()

	// keyed is one command's context, the way the socket and the API build one.
	keyed := func(t *testing.T, key string) context.Context {
		t.Helper()
		c, err := core.WithKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// texts is the document as it reads, so an assertion is about the order the
	// blocks are in rather than about their ids.
	texts := func(f *fixture) []string {
		var out []string
		for _, b := range f.blocks(t) {
			out = append(out, b.Text)
		}
		return out
	}
	reads := func(f *fixture) string { return strings.Join(texts(f), "|") }

	t.Run("a key names the block the command under it made", func(t *testing.T) {
		f := setup(t, "")
		was := f.blocks(t)
		if _, err := f.InsertBlock(keyed(t, "one"), f.who["editor"], f.doc,
			was[len(was)-1].ID, "", "First.", true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.InsertBlock(keyed(t, "two"), f.who["editor"], f.doc,
			0, "one", "Second.", true); err != nil {
			t.Fatal(err)
		}
		got := reads(f)
		if !strings.HasSuffix(got, "First.|Second.") {
			t.Fatalf("the document reads %q, want it to end First.|Second.", got)
		}
	})

	t.Run("a key names the last block a paste made", func(t *testing.T) {
		f := setup(t, "")
		was := f.blocks(t)
		if _, err := f.InsertBlock(keyed(t, "paste"), f.who["editor"], f.doc,
			was[len(was)-1].ID, "", "One.\n\nTwo.\n\nThree.", false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.InsertBlock(keyed(t, "after"), f.who["editor"], f.doc,
			0, "paste", "Four.", true); err != nil {
			t.Fatal(err)
		}
		got := reads(f)
		if !strings.HasSuffix(got, "One.|Two.|Three.|Four.") {
			t.Fatalf("the document reads %q, want it to end One.|Two.|Three.|Four.", got)
		}
	})

	t.Run("a key nobody spent is nothing to insert after", func(t *testing.T) {
		f := setup(t, "")
		_, err := f.InsertBlock(keyed(t, "mine"), f.who["editor"], f.doc, 0, "nobodys", "Text.", true)
		if !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("got %v, want %v", err, core.ErrNotFound)
		}
	})

	t.Run("one person cannot name another person's key", func(t *testing.T) {
		f := setup(t, "")
		was := f.blocks(t)
		if _, err := f.InsertBlock(keyed(t, "shared"), f.who["owner"], f.doc,
			was[len(was)-1].ID, "", "The owner's.", true); err != nil {
			t.Fatal(err)
		}
		_, err := f.InsertBlock(keyed(t, "editors"), f.who["editor"], f.doc, 0, "shared", "Text.", true)
		if !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("got %v, want %v", err, core.ErrNotFound)
		}
	})

	t.Run("a key whose command made no block is nothing to insert after", func(t *testing.T) {
		f := setup(t, "")
		if _, err := f.RenameDocument(keyed(t, "rename"), f.who["editor"], f.doc, "Notes"); err != nil {
			t.Fatal(err)
		}
		_, err := f.InsertBlock(keyed(t, "insert"), f.who["editor"], f.doc, 0, "rename", "Text.", true)
		if !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("got %v, want %v", err, core.ErrNotFound)
		}
	})

	t.Run("an insert names after or after_key, not both", func(t *testing.T) {
		f := setup(t, "")
		was := f.blocks(t)
		_, err := f.InsertBlock(ctx, f.who["editor"], f.doc, was[0].ID, "one", "Text.", true)
		if !errors.Is(err, ErrAfterBoth) {
			t.Fatalf("got %v, want %v", err, ErrAfterBoth)
		}
		if len(f.blocks(t)) != len(was) {
			t.Fatal("a refused insert left a block behind")
		}
	})

	// A key is forgotten after a day. The insert that was waiting on the key is
	// then applied again rather than answered with what it did, which records
	// the key afresh, so the insert queued behind it still finds its block.
	t.Run("a forgotten key is recorded again by the replay", func(t *testing.T) {
		f := setup(t, "")
		was := f.blocks(t)
		if _, err := f.InsertBlock(keyed(t, "old"), f.who["editor"], f.doc,
			was[len(was)-1].ID, "", "First.", true); err != nil {
			t.Fatal(err)
		}
		now := f.Now
		f.Now = func() time.Time { return now().Add(core.KeyLife + time.Minute) }
		if err := f.PruneKeys(ctx); err != nil {
			t.Fatal(err)
		}
		f.Now = now
		// The tab never saw the answer to the first, so it sends it again under
		// the same key. Nothing remembers it, so this is a second block.
		if _, err := f.InsertBlock(keyed(t, "old"), f.who["editor"], f.doc,
			was[len(was)-1].ID, "", "First.", true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.InsertBlock(keyed(t, "new"), f.who["editor"], f.doc,
			0, "old", "Second.", true); err != nil {
			t.Fatal(err)
		}
		// The replay made a second block, which is what a forgotten key costs
		// and why it is only forgotten after a day. What this is about is that
		// the insert queued behind it still found a block to go after, which is
		// the one the replay made.
		got := reads(f)
		if !strings.HasSuffix(got, "First.|Second.|First.") {
			t.Fatalf("the document reads %q, want it to end First.|Second.|First.", got)
		}
	})
}
