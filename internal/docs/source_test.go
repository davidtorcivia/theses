package docs

import (
	"context"
	"errors"
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
		e, err := f.InsertBlock(ctx, f.who["editor"], f.doc, after, text, true)
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

			conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if len(conflicts) != 0 {
				t.Fatalf("the save reported %+v, want nothing in conflict", conflicts)
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
	conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nOne, mine.\n\nTwo.")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("the save reported %+v, want nothing in conflict", conflicts)
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
			conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc, base,
				"# Tide\n\n"+tc.mine+"\n\nTwo.\n\nThree.")
			if err != nil {
				t.Fatal(err)
			}
			if tc.clash != (len(conflicts) == 1) {
				t.Fatalf("the save reported %+v, want a conflict: %v", conflicts, tc.clash)
			}
			if tc.clash {
				if conflicts[0].Block != was[1].ID || conflicts[0].Current != tc.theirs {
					t.Fatalf("the conflict is %+v, want block %d holding %q",
						conflicts[0], was[1].ID, tc.theirs)
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
			conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc, base,
				"# Tide\n\n"+tc.middle+"\n\nTwo, mine.")
			if err != nil {
				t.Fatal(err)
			}
			if len(conflicts) != 0 {
				t.Fatalf("the save reported %+v, want nothing in conflict", conflicts)
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

	added, err := f.InsertBlock(ctx, f.who["owner"], f.doc, was[1].ID, "Theirs.", false)
	if err != nil {
		t.Fatal(err)
	}
	conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nOne, mine.")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("the save reported %+v, want nothing in conflict", conflicts)
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
	conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, "# Tide\n\nTwo.")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Block != was[1].ID {
		t.Fatalf("the save reported %+v, want block %d in conflict", conflicts, was[1].ID)
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
	conflicts, err := f.WriteSource(first, f.who["editor"], f.doc, base, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("the save reported %+v, want one conflict", conflicts)
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

// Sending the same markdown twice must change nothing the second time, whoever
// else wrote in between. It is a request rather than a queued command, so
// pressing Save again, a request whose answer never arrived, and an agent
// retrying all land here.
func TestWriteSourceIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start []string
		text  string
		// theirs, when it names a block by its place in start, is the text
		// somebody else writes into it before the first save.
		at     int
		theirs string
	}{
		{name: "an edit and an addition", start: []string{"# Tide", "One.", "Two."},
			text: "# Tide\n\nOne, mine.\n\nTwo.\n\nThree new.", at: -1},
		{name: "a paragraph added at the head", start: []string{"# Tide", "One."},
			text: "Before.\n\n# Tide\n\nOne.", at: -1},
		{name: "two paragraphs that read alike", start: []string{"# Tide"},
			text: "# Tide\n\nSame.\n\nSame.", at: -1},
		{name: "everything replaced", start: []string{"# Tide", "One.", "Two."},
			text: "# Other\n\nA.\n\nB.\n\nC.", at: -1},
		{name: "a paragraph taken out", start: []string{"# Tide", "One.", "Two."},
			text: "# Tide\n\nTwo.", at: -1},
		{name: "with a block somebody else changed that this one changes too",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne, mine entirely.\n\nTwo.\n\nThree new.",
			at:    1, theirs: "One, theirs entirely."},
		{name: "with a block somebody else changed that this one leaves alone",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne.\n\nTwo, mine.\n\nThree new.",
			at:    1, theirs: "One, theirs."},
		{name: "with a block somebody else deleted",
			start: []string{"# Tide", "One.", "Two."},
			text:  "# Tide\n\nOne, mine.\n\nTwo.\n\nThree new.",
			at:    -2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := setup(t, "")
			base := f.seed(t, tc.start...)
			was := f.blocks(t)
			switch {
			case tc.theirs != "":
				if _, err := f.SetBlock(ctx, f.who["owner"], was[tc.at].ID, was[tc.at].Version,
					tc.theirs, false); err != nil {
					t.Fatal(err)
				}
			case tc.at == -2:
				if _, err := f.DeleteBlock(ctx, f.who["owner"], was[1].ID); err != nil {
					t.Fatal(err)
				}
			}

			first, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, tc.text)
			if err != nil {
				t.Fatal(err)
			}
			after := f.blocks(t)
			revisions := f.revisions(t)
			// With nobody else in the way the document is exactly the
			// paragraphs that were sent.
			if tc.at == -1 {
				if got := f.texts(t); !same(got, Paragraphs(tc.text)) {
					t.Fatalf("the document reads %q, want %q", got, Paragraphs(tc.text))
				}
			}

			second, err := f.WriteSource(ctx, f.who["editor"], f.doc, base, tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if len(second) != len(first) {
				t.Fatalf("the second save reported %+v, want the same as the first, %+v", second, first)
			}
			now := f.blocks(t)
			if len(now) != len(after) {
				t.Fatalf("the document has %d blocks after the second save, want %d", len(now), len(after))
			}
			for i, b := range now {
				if b.ID != after[i].ID || b.Version != after[i].Version || b.Text != after[i].Text {
					t.Fatalf("block %d is %d v%d %q after the second save, want %d v%d %q",
						i, b.ID, b.Version, b.Text, after[i].ID, after[i].Version, after[i].Text)
				}
			}
			if got := f.revisions(t); got != revisions {
				t.Fatalf("the document has %d revisions after the second save, want %d", got, revisions)
			}
		})
	}
}

// A base that names only some of the document's blocks says nothing about the
// rest, and the paragraphs standing for them take the blocks they already have
// rather than being written in a second time.
func TestWriteSourceWithAPartialBase(t *testing.T) {
	ctx := context.Background()
	f := setup(t, "")
	f.seed(t, "# Tide", "One.", "Two.")
	was := f.blocks(t)
	revisions := f.revisions(t)

	conflicts, err := f.WriteSource(ctx, f.who["editor"], f.doc,
		[]BlockRef{{ID: was[1].ID, Version: was[1].Version}}, "# Tide\n\nOne.\n\nTwo.")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("the save reported %+v, want nothing in conflict", conflicts)
	}
	now := f.blocks(t)
	if len(now) != len(was) {
		t.Fatalf("the document has %d blocks, want %d", len(now), len(was))
	}
	for i, b := range now {
		if b.ID != was[i].ID || b.Version != was[i].Version {
			t.Fatalf("block %d is %d v%d, want %d v%d", i, b.ID, b.Version, was[i].ID, was[i].Version)
		}
	}
	if got := f.revisions(t); got != revisions {
		t.Fatalf("the document has %d revisions, want %d", got, revisions)
	}
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
