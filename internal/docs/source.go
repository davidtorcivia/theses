package docs

import (
	"context"
	"errors"

	"github.com/davidtorcivia/theses/internal/core"
	"github.com/davidtorcivia/theses/internal/merge"
)

// ErrSourceBase is a source save the server cannot line up: one of the blocks
// it names is at a version whose text is no longer on record, so there is no
// honest way to tell which paragraph used to be which. The whole save is
// refused rather than a guess applied, because a guess here would move
// paragraphs between blocks.
var ErrSourceBase = errors.New("this markdown was written from a version of the document that is no longer on record; read it again and edit that")

// errNothing is a save that writes nothing. It is raised so that the
// transaction rolls back and the revision the save opened with is not kept, and
// it never reaches a caller.
var errNothing = errors.New("nothing to write")

// A BlockRef is one block as the person editing the markdown saw it: its id and
// the version its text was at. A save carries the whole ordered list, which is
// what the paragraphs it sends are lined up against.
type BlockRef struct {
	ID      int64 `json:"id"`
	Version int64 `json:"version"`
}

// A SourceConflict is a paragraph a save did not write: somebody else changed
// that block while the markdown was being edited, and what they wrote and what
// this save wrote cannot be put together. The block is left exactly as the
// database holds it and the text it holds is reported, so the person editing
// can see what they are up against without reading the document again.
type SourceConflict struct {
	Block   int64  `json:"block"`
	Version int64  `json:"version"`
	Current string `json:"current"`
}

// maxPairs is how large a table lining the paragraphs up may build, counted in
// base blocks times paragraphs. A million is a few milliseconds and about eight
// megabytes, which is the same allowance one three way merge of a block gets.
//
// ponytail: past it the paragraphs are paired with the base blocks where they
// stand, which still keeps every unchanged paragraph's block and writes nothing
// for it, and only moves ids about when a paragraph was added or taken out of a
// document that large. The upgrade is the linear space form the merge package
// already wants.
const maxPairs = 1 << 20

// WriteSource writes a whole document from its markdown. base is the blocks the
// markdown was written from, in order, and text is the markdown as it now
// stands; the server cuts it into paragraphs with the same Paragraphs every
// other path uses, lines them up against the text those blocks held at those
// versions, and writes the difference: a paragraph nobody touched keeps its
// block and is not written at all, a paragraph that changed is a set on the
// block it came from and goes through the same three way merge a stale set from
// the editor does, a paragraph with no block left over is a new block where it
// stands, and a block with no paragraph left over is deleted.
//
// A base of nil means the document as it stands, which is the natural thing for
// an agent replacing a document it has just read. An empty but non-nil base is
// a document that had no blocks.
//
// Everything is one transaction, opened by the revision that makes a bad save
// one restore away, so a refusal leaves the document exactly as it was. What
// comes back is the blocks the save could not take, which are still in the
// document holding somebody else's words.
func (s *Service) WriteSource(ctx context.Context, a core.Actor, document int64,
	base []BlockRef, text string) ([]SourceConflict, error) {
	paragraphs := Paragraphs(text)
	var out []SourceConflict
	err := s.Together(ctx, func(ctx context.Context) error {
		// The revision is taken first for two reasons. It is the restore point,
		// and it is the one command of this save that a replay under the same
		// client key is answered by: everything after it is decided by what the
		// database held when the save first ran, which a replay no longer has,
		// so a replay that reached them would take numbers off the key counter
		// that its original spent on other commands. Answering here spends
		// exactly one number either way. It is also where the standing to write
		// this document is checked, so a save is refused before it reads
		// anything.
		//
		// The reason is the importer's, because this is the same thing arriving
		// by another road: markdown for the whole document, written somewhere
		// else and read back in.
		//
		// ponytail: a replay is answered without the conflict list the first run
		// built, because nothing remembers it. The client has that list already,
		// from the answer it is replaying because it never saw; the upgrade is a
		// table of answers by key, which nothing else here needs.
		rev, err := s.CreateRevision(ctx, a, document, ReasonPreImport)
		if err != nil {
			return err
		}
		if rev.Replayed {
			return nil
		}

		// The transaction is open and holds the write lock, so the document
		// cannot change under the rest of this: what is read here is what is
		// written over.
		blocks, err := Blocks(ctx, s.DB, document)
		if err != nil {
			return err
		}
		live := map[int64]Block{}
		for _, b := range blocks {
			live[b.ID] = b
		}
		if base == nil {
			base = make([]BlockRef, 0, len(blocks))
			for _, b := range blocks {
				base = append(base, BlockRef{ID: b.ID, Version: b.Version})
			}
		}
		items, err := s.plan(ctx, document, base, paragraphs, live)
		if err != nil {
			return err
		}
		if sameAs(items, blocks) {
			return errNothing
		}

		was := map[int64]int64{}
		for _, ref := range base {
			was[ref.ID] = ref.Version
		}
		conflicted, err := s.applyItems(ctx, a, document, items, blocks, func(b Block) missing {
			version, named := was[b.ID]
			switch {
			case !named:
				// A block somebody else added while the markdown was being
				// edited. It is not in the base, so the save says nothing about
				// it and it stays where it is.
				return keepMissing
			case version == b.Version:
				return dropMissing
			default:
				// A paragraph taken out of a block somebody else has written in
				// since. Deleting it would throw their words away without
				// anybody being asked, which is what the merge refuses to do on
				// a block both sides changed, so it is left and reported.
				return markMissing
			}
		})
		if err != nil {
			return err
		}
		for _, b := range blocks {
			// The text reported is the one read above: a block in conflict is
			// by definition one this save did not write.
			if conflicted[b.ID] {
				out = append(out, SourceConflict{Block: b.ID, Version: b.Version, Current: b.Text})
			}
		}
		return nil
	})
	if errors.Is(err, errNothing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// plan lines the paragraphs up against the blocks the markdown was written from
// and answers with the list applyItems writes. The pairing is by equality of
// whole paragraphs through the same longest common subsequence the line level
// merge uses: what both sides still have stays where it is, and a run neither
// side kept is paired off in order, the leftover paragraphs becoming new blocks
// and the leftover blocks falling out of the list to be deleted.
func (s *Service) plan(ctx context.Context, document int64, base []BlockRef,
	paragraphs []string, live map[int64]Block) ([]item, error) {
	texts := make([]string, 0, len(base))
	for _, ref := range base {
		if _, there := live[ref.ID]; !there {
			// A block the base names that the document no longer has: either
			// somebody deleted it, or the id was never this document's. The
			// second is a request that has no business here, and the two are
			// told apart before anything is read back under either.
			b, err := GetBlock(ctx, s.DB, ref.ID)
			if err != nil {
				return nil, err
			}
			if b.Document != document {
				return nil, core.ErrNotFound
			}
		}
		was, ok, err := baseText(ctx, s.DB, ref.ID, ref.Version)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrSourceBase
		}
		texts = append(texts, was)
	}

	keep := make([]int, len(texts))
	for i := range keep {
		keep[i] = -1
	}
	if len(texts)*len(paragraphs) <= maxPairs {
		keep = merge.Match(texts, paragraphs)
	}

	items := []item{}
	i, j := 0, 0
	for i < len(texts) || j < len(paragraphs) {
		if i < len(texts) && keep[i] == j {
			items = kept(items, base[i], texts[i], paragraphs[j], live)
			i, j = i+1, j+1
			continue
		}
		// A run neither side kept, which ends at the next paragraph that stayed
		// where it was or at the end of both.
		i2, j2 := len(texts), len(paragraphs)
		for k := i; k < len(texts); k++ {
			if keep[k] >= j {
				i2, j2 = k, keep[k]
				break
			}
		}
		for i < i2 && j < j2 {
			items = kept(items, base[i], texts[i], paragraphs[j], live)
			i, j = i+1, j+1
		}
		for j < j2 {
			items = append(items, item{Text: paragraphs[j]})
			j++
		}
		// Whatever is left of the run on the base side has no paragraph to go
		// in it. Leaving it out of the list is what deletes it.
		i = i2
	}
	return items, nil
}

// kept adds the paragraph one base block became.
//
// A block somebody else deleted while the markdown was being edited is the one
// case that needs deciding, and it is decided by whether this person changed
// the paragraph. An unchanged one is left out, so that saving an edit made
// somewhere else in the document does not put a paragraph somebody deleted back
// again. A changed one goes in with the id it had, which no longer names
// anything, so it lands as a new block where it stood: this person's words are
// not the deletion's to take away.
func kept(items []item, ref BlockRef, was, now string, live map[int64]Block) []item {
	if _, there := live[ref.ID]; !there && was == now {
		return items
	}
	return append(items, item{ID: ref.ID, Version: ref.Version, Text: now})
}
