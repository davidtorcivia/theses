package docs

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/davidtorcivia/theses/internal/board"
	"github.com/davidtorcivia/theses/internal/core"
)

// seed replaces the document's blocks with exactly these paragraphs, stored as
// they are written here, and answers with the base a source view opened on it
// would send.
func (f *fixture) seed(t *testing.T, texts ...string) []BlockRef {
	t.Helper()
	ctx := context.Background()
	for _, b := range f.blocks(t) {
		if _, err := f.DeleteBlock(ctx, f.who["editor"], b.ID); err != nil {
			t.Fatal(err)
		}
	}
	after := int64(0)
	for _, text := range texts {
		e, err := f.InsertBlock(ctx, f.who["editor"], f.doc, after, "", text, true)
		if err != nil {
			t.Fatal(err)
		}
		after = e.EntityID
	}
	return f.base(t)
}

func (f *fixture) base(t *testing.T) []BlockRef {
	t.Helper()
	out := []BlockRef{}
	for _, b := range f.blocks(t) {
		out = append(out, BlockRef{ID: b.ID, Version: b.Version})
	}
	return out
}

func (f *fixture) texts(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, b := range f.blocks(t) {
		out = append(out, b.Text)
	}
	return out
}

func (f *fixture) revisions(t *testing.T) int {
	t.Helper()
	list, err := ListRevisions(context.Background(), f.db, f.doc)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A source save is the whole document as markdown lined up against the blocks
// it was written from: what nobody touched keeps its block and is not written,
// what changed is a set on the block it came from, what is new is a block, and
// what is gone is deleted.
func TestWriteSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start []string
		text  string
		want  []string
		// kept says which of the starting blocks each paragraph of want must
		// still be, by index. A paragraph with no entry is a new block.
		kept map[int]int
	}{
		{
			name:  "nothing changed writes nothing",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne.\n\nTwo.",
			want:  []string{"# Tide", "One.", "Two."},
			kept:  map[int]int{0: 0, 1: 1, 2: 2},
		},
		{
			name:  "one paragraph edited keeps its block",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne and a half.\n\nTwo.",
			want:  []string{"# Tide", "One and a half.", "Two."},
			kept:  map[int]int{0: 0, 1: 1, 2: 2},
		},
		{
			name:  "a paragraph inserted in the middle",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne.\n\nOne and a half.\n\nTwo.",
			want:  []string{"# Tide", "One.", "One and a half.", "Two."},
			kept:  map[int]int{0: 0, 1: 1, 3: 2},
		},
		{
			name:  "a paragraph inserted at the head",
			start: []string{"# Tide", "One.", "Two."},
			text:  "Before.\n\n# Tide\n\nOne.\n\nTwo.",
			want:  []string{"Before.", "# Tide", "One.", "Two."},
			kept:  map[int]int{1: 0, 2: 1, 3: 2},
		},
		{
			name:  "a paragraph added at the end",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne.\n\nTwo.\n\nThree.",
			want:  []string{"# Tide", "One.", "Two.", "Three."},
			kept:  map[int]int{0: 0, 1: 1, 2: 2},
		},
		{
			name:  "a paragraph deleted",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nTwo.",
			want:  []string{"# Tide", "Two."},
			kept:  map[int]int{0: 0, 1: 2},
		},
		{
			// The longest common subsequence keeps one of the two where it
			// stands; the other is written over the block that was between
			// them, and the block it used to be is deleted.
			name:  "two paragraphs swapped keep one id",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nTwo.\n\nOne.",
			want:  []string{"# Tide", "Two.", "One."},
			kept:  map[int]int{0: 0, 1: 2},
		},
		{
			name:  "a paragraph split in two",
			start: []string{"# Tide", "One. Two."},
			text:  "# Tide\n\nOne.\n\nTwo.",
			want:  []string{"# Tide", "One.", "Two."},
			kept:  map[int]int{0: 0, 1: 1},
		},
		{
			name:  "two paragraphs joined",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne.\nTwo.",
			want:  []string{"# Tide", "One.\nTwo."},
			kept:  map[int]int{0: 0, 1: 1},
		},
		{
			name:  "a fenced code block with a blank line stays one block",
			start: []string{"# Tide", "```\nup\n\ndown\n```"},
			text:  "# Tide\n\n```\nup\n\ndown\n```",
			want:  []string{"# Tide", "```\nup\n\ndown\n```"},
			kept:  map[int]int{0: 0, 1: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := setup(t, "")
			base := f.seed(t, tc.start...)
			was := f.blocks(t)
			revisions := f.revisions(t)

			save, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if len(save.Conflicts) != 0 {
				t.Fatalf("the save reported %+v, want nothing in conflict", save.Conflicts)
			}
			if got := f.texts(t); !same(got, tc.want) {
				t.Fatalf("the document reads %q, want %q", got, tc.want)
			}
			now := f.blocks(t)
			for i, b := range now {
				at, ok := tc.kept[i]
				if !ok {
					for _, before := range was {
						if before.ID == b.ID {
							t.Fatalf("paragraph %d is block %d, which was already there", i, b.ID)
						}
					}
					continue
				}
				if b.ID != was[at].ID {
					t.Fatalf("paragraph %d is block %d, want the block %d it started as", i, b.ID, was[at].ID)
				}
				// A paragraph that did not change is not written, so its
				// version stands where it was.
				if b.Text == was[at].Text && b.Version != was[at].Version {
					t.Fatalf("block %d is at version %d and was not changed, want version %d",
						b.ID, b.Version, was[at].Version)
				}
			}
			// A save that changed nothing keeps no revision either: it is not
			// something to restore to.
			wantRevisions := revisions + 1
			if same(tc.want, tc.start) {
				wantRevisions = revisions
			}
			if got := f.revisions(t); got != wantRevisions {
				t.Fatalf("the document has %d revisions, want %d", got, wantRevisions)
			}
		})
	}
}

// Somebody else writing in another block while the markdown is being edited is
// not this save's business, and the save must not write over it.
func TestWriteSourceLeavesAnotherBlockAlone(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.", "Two.")
	was := f.blocks(t)

	if _, err := f.SetBlock(ctx, f.who["owner"], was[2].ID, was[2].Version, "Two, theirs.", false); err != nil {
		t.Fatal(err)
	}
	save, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nOne, mine.\n\nTwo.")
	if err != nil {
		t.Fatal(err)
	}
	if len(save.Conflicts) != 0 {
		t.Fatalf("the save reported %+v, want nothing in conflict", save.Conflicts)
	}
	// The third paragraph reads as theirs: this save did not touch it, because
	// it did not change it.
	if got := f.texts(t); !same(got, []string{"# Tide", "One, mine.", "Two, theirs."}) {
		t.Fatalf("the document reads %q", got)
	}
}

// Both sides writing in one block is the ordinary three way merge: two changes
// that can be put together are, and two that cannot leave the block alone and
// are reported, with the rest of the save applied.
func TestWriteSourceMergesAndReportsOneBlock(t *testing.T) {
	for _, tc := range []struct {
		name, theirs, mine, want string
		clash                    bool
	}{
		{
			name:   "different words in one line merge",
			theirs: "The tide comes in twice a day.",
			mine:   "The sea comes in twice a night.",
			want:   "The sea comes in twice a night.",
		},
		{
			name:   "the same words two ways conflict",
			theirs: "The tide comes in twice a night.",
			mine:   "The tide comes in twice a week.",
			want:   "The tide comes in twice a night.",
			clash:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := setup(t, "")
			base := f.seed(t, "# Tide", "The tide comes in twice a day.", "Two.")
			was := f.blocks(t)

			if _, err := f.SetBlock(ctx, f.who["owner"], was[1].ID, was[1].Version, tc.theirs, false); err != nil {
				t.Fatal(err)
			}
			save, err := f.WriteSource(ctx, f.who["editor"], f.doc, base,
				"# Tide\n\n"+tc.mine+"\n\nTwo.\n\nThree.")
			if err != nil {
				t.Fatal(err)
			}
			if tc.clash != (len(save.Conflicts) == 1) {
				t.Fatalf("the save reported %+v, want a conflict: %v", save.Conflicts, tc.clash)
			}
			if tc.clash {
				if save.Conflicts[0].Block != was[1].ID || save.Conflicts[0].Current != tc.theirs {
					t.Fatalf("the conflict is %+v, want block %d holding %q",
						save.Conflicts[0], was[1].ID, tc.theirs)
				}
			}
			// Either way the rest of the save went in: the paragraph added at
			// the end is there.
			if got := f.texts(t); !same(got, []string{"# Tide", tc.want, "Two.", "Three."}) {
				t.Fatalf("the document reads %q, want the middle to be %q and Three. at the end", got, tc.want)
			}
		})
	}
}

// A block somebody else deleted while the markdown was being edited: an
// unchanged paragraph is left out, so an edit made elsewhere does not put it
// back, and a changed one goes in as a new block where it stood, because those
// words are the person's own and the deletion is not theirs to answer for.
func TestWriteSourceAgainstADeletedBlock(t *testing.T) {
	for _, tc := range []struct {
		name, middle string
		want         []string
		fresh        bool
	}{
		{name: "unchanged stays deleted", middle: "One.", want: []string{"# Tide", "Two, mine."}},
		{name: "edited comes back", middle: "One, mine.",
			want: []string{"# Tide", "One, mine.", "Two, mine."}, fresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := setup(t, "")
			base := f.seed(t, "# Tide", "One.", "Two.")
			was := f.blocks(t)

			if _, err := f.DeleteBlock(ctx, f.who["owner"], was[1].ID); err != nil {
				t.Fatal(err)
			}
			save, err := f.WriteSource(ctx, f.who["editor"], f.doc, base,
				"# Tide\n\n"+tc.middle+"\n\nTwo, mine.")
			if err != nil {
				t.Fatal(err)
			}
			if len(save.Conflicts) != 0 {
				t.Fatalf("the save reported %+v, want nothing in conflict", save.Conflicts)
			}
			if got := f.texts(t); !same(got, tc.want) {
				t.Fatalf("the document reads %q, want %q", got, tc.want)
			}
			if tc.fresh && f.blocks(t)[1].ID == was[1].ID {
				t.Fatal("the deleted block was written to rather than made again")
			}
		})
	}
}

// A block somebody else added while the markdown was being edited is not in the
// base, so the save says nothing about it and it stays where it is.
func TestWriteSourceLeavesANewBlockInPlace(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.")
	was := f.blocks(t)

	added, err := f.InsertBlock(ctx, f.who["owner"], f.doc, was[1].ID, "", "Theirs.", false)
	if err != nil {
		t.Fatal(err)
	}
	save, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nOne, mine.")
	if err != nil {
		t.Fatal(err)
	}
	if len(save.Conflicts) != 0 {
		t.Fatalf("the save reported %+v, want nothing in conflict", save.Conflicts)
	}
	if got := f.texts(t); !same(got, []string{"# Tide", "One, mine.", "Theirs."}) {
		t.Fatalf("the document reads %q", got)
	}
	if f.blocks(t)[2].ID != added.EntityID {
		t.Fatal("the block somebody else added is not the one that is there")
	}
}

// Taking a paragraph out of a block somebody else has written in since is a
// conflict rather than a delete: their words are not thrown away without
// anybody being asked.
func TestWriteSourceWillNotDeleteAChangedBlock(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.", "Two.")
	was := f.blocks(t)

	if _, err := f.SetBlock(ctx, f.who["owner"], was[1].ID, was[1].Version, "One, theirs.", false); err != nil {
		t.Fatal(err)
	}
	save, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nTwo.")
	if err != nil {
		t.Fatal(err)
	}
	if len(save.Conflicts) != 1 || save.Conflicts[0].Block != was[1].ID {
		t.Fatalf("the save reported %+v, want block %d in conflict", save.Conflicts, was[1].ID)
	}
	if got := f.texts(t); !same(got, []string{"# Tide", "One, theirs.", "Two."}) {
		t.Fatalf("the document reads %q", got)
	}
}

// A base the database cannot produce the text for is a save with nothing to
// line its paragraphs up against, and lining them up wrongly would move
// paragraphs between blocks. The whole save is refused and nothing changes.
func TestWriteSourceRefusesAnUnrecoverableBase(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.")
	base[1].Version = 999
	was := f.texts(t)
	revisions := f.revisions(t)

	if _, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nOne, mine.\n\nTwo."); !errors.Is(err, ErrSourceBase) {
		t.Fatalf("the save answered %v, want ErrSourceBase", err)
	}
	if got := f.texts(t); !same(got, was) {
		t.Fatalf("the document reads %q, want %q", got, was)
	}
	if got := f.revisions(t); got != revisions {
		t.Fatalf("the document has %d revisions, want %d", got, revisions)
	}
}

// A base naming a block of another document is a request with no business here.
func TestWriteSourceRefusesAForeignBlock(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.")
	other, err := f.CreateDocument(ctx, f.who["editor"], f.prop, "Script")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := Blocks(ctx, f.db, other.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	base = append(base, BlockRef{ID: blocks[0].ID, Version: blocks[0].Version})

	if _, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nOne."); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("the save answered %v, want ErrNotFound", err)
	}
}

func TestWriteSourceAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, who string
		archive   bool
		want      error
	}{
		{name: "an editor writes", who: "editor"},
		{name: "a researcher writes", who: "researcher"},
		{name: "a guest may not", who: "guest", want: core.ErrForbidden},
		{name: "somebody who is not a member may not", who: "outsider", want: core.ErrForbidden},
		{name: "an archived proposition is read only", who: "editor", archive: true, want: board.ErrArchived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := setup(t, "")
			base := f.seed(t, "# Tide", "One.")
			if tc.archive {
				if _, err := f.board.ArchiveProposition(ctx, f.who["owner"], f.prop); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.WriteSource(ctx, f.who[tc.who], f.doc, base, "# Tide\n\nOne, mine.")
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("the save answered %v, want it to go through", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("the save answered %v, want %v", err, tc.want)
			}
		})
	}
}

// The whole save is one transaction of keyed commands, and the revision it
// opens with is the one the key is spent on, so a request sent twice because
// the answer never came back is answered rather than applied again.
func TestWriteSourceReplaysWithoutApplyingTwice(t *testing.T) {
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.")
	text := "# Tide\n\nOne, mine.\n\nTwo."

	first, err := core.WithKey(context.Background(), "a-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteSource(first, f.who["editor"], f.doc, base, text); err != nil {
		t.Fatal(err)
	}
	want := f.texts(t)
	revisions := f.revisions(t)

	again, err := core.WithKey(context.Background(), "a-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteSource(again, f.who["editor"], f.doc, base, text); err != nil {
		t.Fatal(err)
	}
	if got := f.texts(t); !same(got, want) {
		t.Fatalf("the document reads %q after the replay, want %q", got, want)
	}
	if got := f.revisions(t); got != revisions {
		t.Fatalf("the document has %d revisions after the replay, want %d", got, revisions)
	}
}

// The same, for the sequence that catches a replay out if the revision is not
// what answers it: one block written, one block in conflict, one paragraph
// added. The conflicted set leaves no key behind, so a replay that walked the
// whole save again would find its numbers one short of where they were and
// answer the insert with somebody else's event, putting the paragraph in twice.
func TestWriteSourceReplaysAConflictedSaveWithoutApplyingTwice(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	base := f.seed(t, "# Tide", "A.", "B.")
	was := f.blocks(t)
	if _, err := f.SetBlock(ctx, f.who["owner"], was[2].ID, was[2].Version, "B, theirs.", false); err != nil {
		t.Fatal(err)
	}
	text := "# Tide\n\nA, mine.\n\nB, mine.\n\nThree."

	first, err := core.WithKey(ctx, "a-key")
	if err != nil {
		t.Fatal(err)
	}
	save, err := f.WriteSource(first, f.who["editor"], f.doc, base, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(save.Conflicts) != 1 {
		t.Fatalf("the save reported %+v, want one conflict", save.Conflicts)
	}
	want := f.texts(t)

	again, err := core.WithKey(ctx, "a-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteSource(again, f.who["editor"], f.doc, base, text); err != nil {
		t.Fatal(err)
	}
	if got := f.texts(t); !same(got, want) {
		t.Fatalf("the document reads %q after the replay, want %q", got, want)
	}
}

// A save with no base at all is the document as it stands, which is what an
// agent replacing a document it has just read sends.
func TestWriteSourceWithNoBaseUsesTheDocument(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	f.seed(t, "# Tide", "One.")
	was := f.blocks(t)

	if _, err := f.WriteSource(ctx, f.who["editor"], f.doc, nil, "# Tide\n\nOne, mine."); err != nil {
		t.Fatal(err)
	}
	if got := f.texts(t); !same(got, []string{"# Tide", "One, mine."}) {
		t.Fatalf("the document reads %q", got)
	}
	if f.blocks(t)[1].ID != was[1].ID {
		t.Fatal("the paragraph that changed did not keep its block")
	}
}

// press is one Save: the text sent against the base the last answer gave, and
// the answer the next press goes from. It is the whole of the protocol a client
// follows, and every idempotence test below is written in it.
func (f *fixture) press(t *testing.T, base []BlockRef, text string) SourceSave {
	t.Helper()
	save, err := f.WriteSource(context.Background(), f.who["editor"], f.doc, base, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(save.Base) != len(Paragraphs(text)) {
		t.Fatalf("the answer names %d blocks for %d paragraphs", len(save.Base), len(Paragraphs(text)))
	}
	return save
}

// still asserts the document has not moved at all, block for block and version
// for version, and has kept no further revision.
func (f *fixture) still(t *testing.T, what string, was []Block, revisions int) {
	t.Helper()
	now := f.blocks(t)
	if len(now) != len(was) {
		t.Fatalf("%s: the document has %d blocks, want %d", what, len(now), len(was))
	}
	for i, b := range now {
		if b.ID != was[i].ID || b.Version != was[i].Version || b.Text != was[i].Text {
			t.Fatalf("%s: block %d is %d v%d %q, want %d v%d %q", what, i,
				b.ID, b.Version, b.Text, was[i].ID, was[i].Version, was[i].Text)
		}
	}
	if got := f.revisions(t); got != revisions {
		t.Fatalf("%s: the document has %d revisions, want %d", what, got, revisions)
	}
}

// theirs is somebody else at the same document, as each of the seven things
// they can do to it while somebody is writing its markdown.
func (f *fixture) set(t *testing.T, block, version int64, text string) {
	t.Helper()
	if _, err := f.SetBlock(context.Background(), f.who["owner"], block, version, text, false); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) insert(t *testing.T, after int64, text string) {
	t.Helper()
	if _, err := f.InsertBlock(context.Background(), f.who["owner"], f.doc, after, "", text, true); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) remove(t *testing.T, block int64) {
	t.Helper()
	if _, err := f.DeleteBlock(context.Background(), f.who["owner"], block); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) move(t *testing.T, block, after int64) {
	t.Helper()
	if _, err := f.MoveBlock(context.Background(), f.who["owner"], block, after); err != nil {
		t.Fatal(err)
	}
}

// A second press of the same markdown, against the base the first press
// answered with, writes nothing and keeps no revision, whatever somebody else
// did in between. The one thing not here is somebody writing in a block this
// text also changes, which is keep mine and has its own test below.
func TestWriteSourceSecondPressWritesNothing(t *testing.T) {
	start := []string{"# Tide", "One.", "Two.", "Three."}
	text := "# Tide\n\nOne, mine.\n\nTwo.\n\nThree.\n\nFour new."
	mine := map[string]bool{"# Tide": true, "One.": true, "Two.": true, "Three.": true}
	for _, tc := range []struct {
		name  string
		while func(t *testing.T, f *fixture, was []Block)
	}{
		{name: "with nobody else in the way"},
		{name: "somebody writes in a block this text leaves alone",
			while: func(t *testing.T, f *fixture, was []Block) { f.set(t, was[3].ID, was[3].Version, "Three, theirs.") }},
		{name: "somebody adds a block before the first",
			while: func(t *testing.T, f *fixture, was []Block) { f.insert(t, 0, "Theirs, at the head.") }},
		{name: "somebody adds a block in the middle",
			while: func(t *testing.T, f *fixture, was []Block) { f.insert(t, was[1].ID, "Theirs, in the middle.") }},
		{name: "somebody adds a block after the last",
			while: func(t *testing.T, f *fixture, was []Block) { f.insert(t, was[3].ID, "Theirs, at the end.") }},
		{name: "somebody deletes a block this text leaves alone",
			while: func(t *testing.T, f *fixture, was []Block) { f.remove(t, was[2].ID) }},
		{name: "somebody deletes a block this text changes",
			while: func(t *testing.T, f *fixture, was []Block) { f.remove(t, was[1].ID) }},
		{name: "somebody moves a block",
			while: func(t *testing.T, f *fixture, was []Block) { f.move(t, was[3].ID, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			base := f.seed(t, start...)
			was := f.blocks(t)
			if tc.while != nil {
				tc.while(t, f, was)
			}
			before := f.texts(t)

			first := f.press(t, base, text)
			if len(first.Conflicts) != 0 {
				t.Fatalf("the first press reported %+v, want nothing in conflict", first.Conflicts)
			}
			if tc.while == nil && !same(f.texts(t), Paragraphs(text)) {
				t.Fatalf("the document reads %q, want %q", f.texts(t), Paragraphs(text))
			}
			// Nothing of theirs went missing without being reported: every
			// paragraph they wrote is still in the document.
			for _, their := range before {
				if mine[their] {
					continue
				}
				if !slices.Contains(f.texts(t), their) {
					t.Fatalf("%q went missing and nothing was reported", their)
				}
			}
			after := f.blocks(t)
			revisions := f.revisions(t)

			second := f.press(t, first.Base, text)
			if len(second.Conflicts) != 0 {
				t.Fatalf("the second press reported %+v", second.Conflicts)
			}
			f.still(t, "after the second press", after, revisions)
			f.press(t, second.Base, text)
			f.still(t, "after the third press", after, revisions)
		})
	}
}

// Somebody writing in a block this text also changes is keep mine: the first
// press merges what can be merged and reports what cannot, and a press after
// that puts this person's paragraph over theirs, because the base they were
// answered with names the version theirs is at. A press after that writes
// nothing.
func TestWriteSourcePressingAgainKeepsMine(t *testing.T) {
	for _, tc := range []struct {
		name, theirs, mine, merged string
		clash                      bool
	}{
		{name: "a merge", theirs: "The tide comes in twice a night.",
			mine: "The sea comes in twice a day.", merged: "The sea comes in twice a night."},
		{name: "a conflict", theirs: "The tide comes in twice a night.",
			mine: "The tide comes in twice a week.", merged: "The tide comes in twice a night.", clash: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			base := f.seed(t, "# Tide", "The tide comes in twice a day.", "Two.")
			was := f.blocks(t)
			f.set(t, was[1].ID, was[1].Version, tc.theirs)

			text := "# Tide\n\n" + tc.mine + "\n\nTwo."
			first := f.press(t, base, text)
			if tc.clash != (len(first.Conflicts) == 1) {
				t.Fatalf("the first press reported %+v, want a conflict: %v", first.Conflicts, tc.clash)
			}
			if got := f.texts(t)[1]; got != tc.merged {
				t.Fatalf("the paragraph reads %q, want %q", got, tc.merged)
			}
			if first.Base[1].ID != was[1].ID {
				t.Fatalf("the answer names block %d, want %d", first.Base[1].ID, was[1].ID)
			}

			second := f.press(t, first.Base, text)
			if len(second.Conflicts) != 0 {
				t.Fatalf("the second press reported %+v, want it to go through", second.Conflicts)
			}
			if got := f.texts(t)[1]; got != tc.mine {
				t.Fatalf("the paragraph reads %q after pressing again, want %q", got, tc.mine)
			}
			after := f.blocks(t)
			revisions := f.revisions(t)
			f.press(t, second.Base, text)
			f.still(t, "after the third press", after, revisions)
		})
	}
}

// Somebody moving a block and somebody writing in one, together: three presses
// of the same markdown put the paragraph it adds in once.
func TestWriteSourceAfterAMoveAndAConflict(t *testing.T) {
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.", "Two.")
	was := f.blocks(t)
	f.move(t, was[2].ID, 0)
	f.set(t, was[1].ID, was[1].Version, "One, theirs entirely.")

	text := "# Tide\n\nOne, mine entirely.\n\nTwo.\n\nTail new."
	first := f.press(t, base, text)
	if len(first.Conflicts) != 1 || first.Conflicts[0].Block != was[1].ID {
		t.Fatalf("the first press reported %+v", first.Conflicts)
	}
	if n := count(f.texts(t), "Tail new."); n != 1 {
		t.Fatalf("the document holds %d copies of the added paragraph: %q", n, f.texts(t))
	}
	blocks := len(f.blocks(t))

	second := f.press(t, first.Base, text)
	third := f.press(t, second.Base, text)
	if len(third.Conflicts) != 0 {
		t.Fatalf("the third press reported %+v", third.Conflicts)
	}
	if n := count(f.texts(t), "Tail new."); n != 1 {
		t.Fatalf("three presses left %d copies of the added paragraph: %q", n, f.texts(t))
	}
	if got := len(f.blocks(t)); got != blocks {
		t.Fatalf("the document has %d blocks after three presses, want %d", got, blocks)
	}
}

func count(texts []string, want string) int {
	n := 0
	for _, x := range texts {
		if x == want {
			n++
		}
	}
	return n
}

// A paragraph is placed where the markdown puts it, never on a block somewhere
// else that happens to read the same.
func TestWriteSourcePlacesAParagraphWhereItIsWritten(t *testing.T) {
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.")
	was := f.blocks(t)
	f.insert(t, was[1].ID, "## Notes")

	save := f.press(t, base, "## Notes\n\n# Tide\n\nOne.")
	if len(save.Conflicts) != 0 {
		t.Fatalf("the save reported %+v", save.Conflicts)
	}
	if got := f.texts(t); !same(got, []string{"## Notes", "# Tide", "One.", "## Notes"}) {
		t.Fatalf("the document reads %q, want mine at the head and theirs still at the end", got)
	}
}

// A base naming some of the blocks is the scope of the text: those blocks are
// what it stands for, and every other block is left exactly where it is.
func TestWriteSourceWithAPartialBase(t *testing.T) {
	f := setup(t, "")
	f.seed(t, "# Tide", "Two.", "Three.")
	was := f.blocks(t)
	revisions := f.revisions(t)

	save := f.press(t, []BlockRef{{ID: was[1].ID, Version: was[1].Version}},
		"Two, mine.\n\nTwo and a half.")
	if len(save.Conflicts) != 0 {
		t.Fatalf("the save reported %+v", save.Conflicts)
	}
	now := f.blocks(t)
	if !same(f.texts(t), []string{"# Tide", "Two, mine.", "Two and a half.", "Three."}) {
		t.Fatalf("the document reads %q", f.texts(t))
	}
	if now[0].ID != was[0].ID || now[0].Version != was[0].Version {
		t.Fatal("the block before the scope was written to")
	}
	if now[3].ID != was[2].ID || now[3].Version != was[2].Version {
		t.Fatal("the block after the scope was written to")
	}
	if got := f.revisions(t); got != revisions+1 {
		t.Fatalf("the document has %d revisions, want %d", got, revisions+1)
	}
	after := f.blocks(t)
	f.press(t, save.Base, "Two, mine.\n\nTwo and a half.")
	f.still(t, "after the second press", after, revisions+1)
}

// One paragraph added to a long document is one insert wherever it goes. The
// runs that are the same above and below it are trimmed off before the
// paragraphs are lined up, so nothing else is touched and the budget, which one
// insert in eleven hundred paragraphs would otherwise be well past, is never
// reached.
func TestWriteSourceInALongDocument(t *testing.T) {
	for _, at := range []int{0, 550, 1100} {
		t.Run("a paragraph added at "+strconv.Itoa(at), func(t *testing.T) {
			ctx := context.Background()
			f := setup(t, "")
			start := make([]string, 0, 1100)
			for i := range 1100 {
				start = append(start, "Paragraph "+strconv.Itoa(i)+".")
			}
			base := f.seed(t, start...)
			was := f.blocks(t)

			text := append(append(append([]string{}, start[:at]...), "One more."), start[at:]...)
			if _, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, strings.Join(text, "\n\n")); err != nil {
				t.Fatal(err)
			}
			now := f.blocks(t)
			if len(now) != len(was)+1 || now[at].Text != "One more." {
				t.Fatalf("the document has %d blocks and %q at %d", len(now), now[at].Text, at)
			}
			// Every block that was there is the one it was, at the version it
			// was: one insert and not a single set.
			for i, b := range was {
				out := i
				if i >= at {
					out = i + 1
				}
				if now[out].ID != b.ID || now[out].Version != b.Version {
					t.Fatalf("block %d is %d v%d, want the %d v%d it was",
						out, now[out].ID, now[out].Version, b.ID, b.Version)
				}
			}
		})
	}
}

// Past the budget the save is refused rather than paired by position, which
// would write every block of a long document with its neighbor's text.
func TestWriteSourceRefusesTooLargeAChange(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	start := make([]string, 0, 1100)
	fresh := make([]string, 0, 1100)
	for i := range 1100 {
		start = append(start, "Was "+strconv.Itoa(i)+".")
		fresh = append(fresh, "Now "+strconv.Itoa(i)+".")
	}
	base := f.seed(t, start...)
	revisions := f.revisions(t)

	_, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, strings.Join(fresh, "\n\n"))
	if !errors.Is(err, ErrSourceSpread) {
		t.Fatalf("the save answered %v, want ErrSourceSpread", err)
	}
	if got := f.texts(t); !same(got, start) {
		t.Fatal("the refused save changed the document")
	}
	if got := f.revisions(t); got != revisions {
		t.Fatalf("the document has %d revisions, want %d", got, revisions)
	}
}

// A block at the version the base names is read off the row, so a document
// whose blocks were written before block_texts existed still saves.
func TestWriteSourceWithNoStoredBaseText(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	f.seed(t, "# Tide", "One.")
	if _, err := f.db.ExecContext(ctx, `DELETE FROM block_texts`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteSource(ctx, f.who["editor"], f.doc, nil, "# Tide\n\nOne, mine."); err != nil {
		t.Fatalf("the save answered %v, want it to go through", err)
	}
	if got := f.texts(t); !same(got, []string{"# Tide", "One, mine."}) {
		t.Fatalf("the document reads %q", got)
	}
}

// A block somebody is in the middle of typing holds whatever they typed, blank
// line and all. A save whose markdown leaves that paragraph alone must not cut
// it into two blocks under them.
func TestWriteSourceWillNotCutABlockSomebodyIsTypingIn(t *testing.T) {
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.", "Two.")
	was := f.blocks(t)
	// The save the editor makes while somebody types stores the text exactly.
	if _, err := f.SetBlock(context.Background(), f.who["owner"], was[1].ID, was[1].Version,
		"One, theirs.\n\nstill typing", true); err != nil {
		t.Fatal(err)
	}
	after := f.blocks(t)
	revisions := f.revisions(t)

	save := f.press(t, base, "# Tide\n\nOne.\n\nTwo.")
	if len(save.Conflicts) != 0 {
		t.Fatalf("the save reported %+v", save.Conflicts)
	}
	f.still(t, "after a save that says nothing about their block", after, revisions)
	// And the answer names it at the version the markdown was written from, so
	// that editing that paragraph later merges against what they wrote rather
	// than writing over it.
	if save.Base[1] != (BlockRef{ID: was[1].ID, Version: was[1].Version}) {
		t.Fatalf("the answer names %+v, want %d v%d", save.Base[1], was[1].ID, was[1].Version)
	}
}

// A save answered out of the key it was sent under says so and carries no base,
// because nothing remembers what the first answer said.
func TestWriteSourceReplayAnswersWithNoBase(t *testing.T) {
	f := setup(t, "")
	base := f.seed(t, "# Tide", "One.")
	text := "# Tide\n\nOne, mine."

	first, err := core.WithKey(context.Background(), "a-key")
	if err != nil {
		t.Fatal(err)
	}
	one, err := f.WriteSource(first, f.who["editor"], f.doc, base, text)
	if err != nil {
		t.Fatal(err)
	}
	if one.Replayed || len(one.Base) != 2 {
		t.Fatalf("the first answer is %+v", one)
	}
	again, err := core.WithKey(context.Background(), "a-key")
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.WriteSource(again, f.who["editor"], f.doc, base, text)
	if err != nil {
		t.Fatal(err)
	}
	if !two.Replayed || len(two.Base) != 0 || len(two.Conflicts) != 0 {
		t.Fatalf("the replayed answer is %+v, want replayed with nothing in it", two)
	}
}

// A clean merge pressed four times in a row, each press going from the base the
// last one answered with. The first merges, the second asserts this person's
// paragraph over the merge because that is what keep mine means, and the third
// and fourth write nothing at all: it settles rather than climbing a version a
// press.
func TestWriteSourceAMergePressedFourTimes(t *testing.T) {
	f := setup(t, "")
	base := f.seed(t, "# Tide", "The tide comes in twice a day.")
	was := f.blocks(t)
	f.set(t, was[1].ID, was[1].Version, "The tide comes in twice a night.")

	text := "# Tide\n\nThe sea comes in twice a day."
	first := f.press(t, base, text)
	if got := f.texts(t)[1]; got != "The sea comes in twice a night." {
		t.Fatalf("the first press left %q", got)
	}
	second := f.press(t, first.Base, text)
	if got := f.texts(t)[1]; got != "The sea comes in twice a day." {
		t.Fatalf("the second press left %q", got)
	}
	after := f.blocks(t)
	revisions := f.revisions(t)
	third := f.press(t, second.Base, text)
	f.press(t, third.Base, text)
	f.still(t, "after the fourth press", after, revisions)
	if after[1].Version != was[1].Version+3 {
		t.Fatalf("the block is at version %d, want %d", after[1].Version, was[1].Version+3)
	}
}

// A block that took somebody else's words in on the way is reported as merged,
// which is what tells the person their text is out of date for it. A block
// that took none of theirs is not in the list: one nobody else touched, one
// this text leaves alone, and one that conflicted and so went nowhere at all.
func TestWriteSourceReportsWhatMerged(t *testing.T) {
	for _, tc := range []struct {
		name, theirs, mine, reads string
		want                      bool
		clash                     bool
	}{
		{
			name:   "a merge is reported",
			theirs: "The tide comes in twice a night.",
			mine:   "The sea comes in twice a day.",
			reads:  "The sea comes in twice a night.",
			want:   true,
		},
		{
			name:   "a conflict is not a merge",
			theirs: "The tide comes in twice a night.",
			mine:   "The tide comes in twice a week.",
			reads:  "The tide comes in twice a night.",
			clash:  true,
		},
		{
			name:  "a paragraph nobody else touched is not a merge",
			mine:  "The sea comes in twice a day.",
			reads: "The sea comes in twice a day.",
		},
		{
			name:   "a paragraph this text leaves alone is not a merge",
			theirs: "The tide comes in twice a night.",
			mine:   "The tide comes in twice a day.",
			reads:  "The tide comes in twice a night.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, "")
			base := f.seed(t, "# Tide", "The tide comes in twice a day.", "Two.")
			was := f.blocks(t)
			if tc.theirs != "" {
				f.set(t, was[1].ID, was[1].Version, tc.theirs)
			}

			save := f.press(t, base, "# Tide\n\n"+tc.mine+"\n\nTwo.\n\nThree new.")
			if tc.clash != (len(save.Conflicts) == 1) {
				t.Fatalf("the save reported %+v, want a conflict: %v", save.Conflicts, tc.clash)
			}
			if tc.want != (len(save.Merged) == 1 && save.Merged[0] == was[1].ID) {
				t.Fatalf("the save reported merged %v, want %v for block %d",
					save.Merged, tc.want, was[1].ID)
			}
			// A merge is reported exactly when this save wrote the block and
			// left it holding something other than what it sent.
			if got := f.texts(t)[1]; got != tc.reads {
				t.Fatalf("the paragraph reads %q, want %q", got, tc.reads)
			}
		})
	}
}
