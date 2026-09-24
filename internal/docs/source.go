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

// ErrSourceSpread is a save with more changed at once than the paragraphs can
// be placed against. What is the same at the top and the bottom costs nothing
// to line up, so this is a stretch of changed text long enough that lining it
// up would be a table of a million cells.
var ErrSourceSpread = errors.New("too much of the document changed at once to line the paragraphs up; save it in smaller pieces")

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

// A SourceSave is what writing a document from its markdown did.
//
// Base is the answer to the question the next save asks: which block each
// paragraph of the text it just sent stands in, and at which version. One per
// paragraph, in the order of the text, whatever happened to it: a block written
// to at its new version, a block made for it at the version it was made with,
// and a block in conflict at the version somebody else left it at, so that
// sending the same text again writes this text's paragraph over theirs. A
// paragraph this save had nothing to say about is the exception and is named at
// the version the text was written from, which kept says why. Sending the same
// text again under this base writes nothing. Blocks the text does not stand for
// are not in it.
//
// Merged is the blocks that took somebody else's words in on the way: what is
// stored there is neither what the text sent nor what they wrote but both, so
// the text the caller still holds is out of date for those paragraphs, and
// sending it again would write their wording back over the merge. It is the
// other half of Conflicts: those are the paragraphs that did not go in at all.
//
// Replayed is a save answered out of the client key it was sent under, having
// applied nothing because the first one did. It carries no base, because
// nothing remembers what the first answer said: read the document again.
type SourceSave struct {
	Base      []BlockRef       `json:"base"`
	Conflicts []SourceConflict `json:"conflicts"`
	Merged    []int64          `json:"merged"`
	Replayed  bool             `json:"replayed,omitempty"`
}

// MaxPairs is how large a table lining the paragraphs up may build, counted in
// changed base blocks times changed paragraphs. A million is a few
// milliseconds and about eight megabytes, which is the same allowance one three
// way merge of a block gets. It counts only the stretch that changed, because
// the runs that are the same at the top and the bottom are trimmed off first,
// so an ordinary edit in a long document costs almost nothing here.
//
// Past it the save is refused. Pairing by position instead would rewrite every
// block of a long document with its neighbor's text the moment one paragraph
// was added at the top.
//
// It is a variable so that a test of the refusal need not build a document of
// a thousand paragraphs to reach it.
var MaxPairs = 1 << 20

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
	base []BlockRef, text string) (SourceSave, error) {
	paragraphs := Paragraphs(text)
	out := SourceSave{Base: []BlockRef{}, Conflicts: []SourceConflict{}, Merged: []int64{}}
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
			out.Replayed = true
			return nil
		}

		// The transaction is open and holds the write lock, so the document
		// cannot change under the rest of this: what is read here is what is
		// written over.
		blocks, err := Blocks(ctx, s.Querier(ctx), document)
		if err != nil {
			return err
		}
		if base == nil {
			base = make([]BlockRef, 0, len(blocks))
			for _, b := range blocks {
				base = append(base, BlockRef{ID: b.ID, Version: b.Version})
			}
		}
		placed, err := s.plan(ctx, document, base, paragraphs, blocks)
		if err != nil {
			return err
		}
		items := make([]item, 0, len(placed))
		for _, p := range placed {
			if !p.skip {
				items = append(items, p.item)
			}
		}

		was := map[int64]int64{}
		for _, ref := range base {
			was[ref.ID] = ref.Version
		}
		conflicted, wrote, done, err := s.applyItems(ctx, a, document, items, blocks, func(b Block) missing {
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
		// Every paragraph has exactly one item unless the save had nothing to
		// do for it, and every item of a source save is one paragraph, so the
		// refs come back in the same order the paragraphs went out.
		at := 0
		for _, p := range placed {
			switch {
			case p.skip:
				out.Base = append(out.Base, p.answer)
			case p.answer.ID != 0:
				out.Base = append(out.Base, p.answer)
				at++
			default:
				out.Base = append(out.Base, done[at].Ref)
				if done[at].Merged {
					out.Merged = append(out.Merged, done[at].Ref.ID)
				}
				at++
			}
		}
		for _, b := range blocks {
			// The text reported is the one read above: a block in conflict is
			// by definition one this save did not write.
			if conflicted[b.ID] {
				out.Conflicts = append(out.Conflicts,
					SourceConflict{Block: b.ID, Version: b.Version, Current: b.Text})
			}
		}
		if !wrote {
			// Nothing went in, so the revision this opened with records no
			// change and is not a version anybody would restore to. That
			// covers the same markdown sent twice, which lands here because
			// the second time every paragraph already has its block. A
			// conflict reported with nothing written is one of these too: the
			// caller is told about it either way, and a snapshot of a document
			// that did not move is noise in a list of fifty.
			return errNothing
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothing) {
		return SourceSave{}, err
	}
	return out, nil
}

// A placed is one paragraph of the saved text and what the save does for it.
//
// item is what applyItems is given, unless skip is set, in which case there is
// nothing to send at all.
//
// answer, when it names a block, is what the save tells the caller this
// paragraph stands on, in place of whatever was applied. It is the version the
// markdown was written from, and it is the answer for a paragraph the save has
// nothing to say about: naming the version somebody else has since moved the
// block to would turn "I did not touch this" into "mine wins" the next time the
// same text was sent, and the whole point of answering with a base is that the
// next save asserts exactly what this one did.
type placed struct {
	item   item
	skip   bool
	answer BlockRef
}

// plan lines the paragraphs up against the blocks the markdown was written from
// and answers with what to do for each of them, in the order of the text. The
// pairing is by equality of whole paragraphs through the same longest common
// subsequence the line level merge uses: what both sides still have stays where
// it is, and a run neither side kept is paired off in order, the leftover
// paragraphs becoming new blocks and the leftover blocks falling out of the
// list to be deleted.
//
// base is the scope as well as the starting point. A block it does not name is
// left exactly where it is, whatever the text says, so a caller who means to
// rewrite one section names that section's blocks and sends that section. The
// cost of that is the plain one: a paragraph of the text that belongs to a
// block outside the base is a paragraph with no block, and it is added.
func (s *Service) plan(ctx context.Context, document int64, base []BlockRef,
	paragraphs []string, blocks []Block) ([]placed, error) {
	live := map[int64]Block{}
	for _, b := range blocks {
		live[b.ID] = b
	}
	texts := make([]string, 0, len(base))
	for _, ref := range base {
		b, there := live[ref.ID]
		switch {
		case there && b.Version == ref.Version:
			// The block still holds what the markdown was written from, so
			// there is nothing to look up. This is almost every block of almost
			// every save, and it is also what lets a document whose blocks were
			// written before block_texts existed be saved at all when the base
			// is the document as it stands.
			texts = append(texts, b.Text)
			continue
		case !there:
			// A block the base names that the document no longer has: either
			// somebody deleted it, or the id was never this document's. The
			// second is a request that has no business here, and the two are
			// told apart before anything is read back under either.
			gone, err := GetBlock(ctx, s.Querier(ctx), ref.ID)
			if err != nil {
				return nil, err
			}
			if gone.Document != document {
				return nil, core.ErrNotFound
			}
		}
		was, ok, err := baseText(ctx, s.Querier(ctx), ref.ID, ref.Version)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrSourceBase
		}
		texts = append(texts, was)
	}

	keep, err := pairs(texts, paragraphs)
	if err != nil {
		return nil, err
	}

	out := make([]placed, 0, len(paragraphs))
	i, j := 0, 0
	for i < len(texts) || j < len(paragraphs) {
		if i < len(texts) && keep[i] == j {
			out = append(out, kept(base[i], texts[i], paragraphs[j], live))
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
			out = append(out, kept(base[i], texts[i], paragraphs[j], live))
			i, j = i+1, j+1
		}
		for j < j2 {
			out = append(out, placed{item: item{Text: paragraphs[j]}})
			j++
		}
		// Whatever is left of the run on the base side has no paragraph to go
		// in it. Leaving it out of the list is what deletes it.
		i = i2
	}

	return out, nil
}

// pairs is the longest common subsequence of the base texts and the paragraphs,
// as the index in the paragraphs each base text kept or minus one for the ones
// it did not.
//
// The runs that are the same at the top and at the bottom are trimmed off and
// paired straight across: they are what a long document is almost entirely made
// of, and leaving them in would put an edit in a thousand paragraph document
// over the budget for a change of one line.
func pairs(texts, paragraphs []string) ([]int, error) {
	head := 0
	for head < len(texts) && head < len(paragraphs) && texts[head] == paragraphs[head] {
		head++
	}
	tail := 0
	for tail < len(texts)-head && tail < len(paragraphs)-head &&
		texts[len(texts)-1-tail] == paragraphs[len(paragraphs)-1-tail] {
		tail++
	}
	if (len(texts)-head-tail)*(len(paragraphs)-head-tail) > MaxPairs {
		return nil, ErrSourceSpread
	}
	middle := merge.Match(texts[head:len(texts)-tail], paragraphs[head:len(paragraphs)-tail])
	keep := make([]int, len(texts))
	for i := range keep {
		switch {
		case i < head:
			keep[i] = i
		case i >= len(texts)-tail:
			keep[i] = i - len(texts) + len(paragraphs)
		case middle[i-head] < 0:
			keep[i] = -1
		default:
			keep[i] = middle[i-head] + head
		}
	}
	return keep, nil
}

// kept is what the save has to do for the paragraph one base block became. Two
// cases turn on whether this person changed that paragraph at all, which is
// what was and now say.
//
// A block somebody else deleted while the markdown was being edited: an
// unchanged paragraph is nothing to do, so that saving an edit made somewhere
// else in the document does not put a paragraph somebody deleted back again,
// and the answer names the block as it stands, tombstoned at the version it
// was left at, so that the same text sent again is nothing to do again. A
// changed one goes in with the id it had, which no longer names anything, so it
// lands as a new block where it stood. This person's words are not the
// deletion's to take away.
//
// A block somebody else wrote in, holding a paragraph this person did not
// touch: the save has nothing to say about it, and it is carried as the block
// reads now so that nothing is written for it. Carrying the paragraph as it was
// written instead would send a set whose merge can only answer with what they
// wrote, which is already there, at the cost of a version, a row in the log,
// their paragraph recorded as this person's, and a save that is not the same
// the second time it is sent.
func kept(ref BlockRef, was, now string, live map[int64]Block) placed {
	b, there := live[ref.ID]
	switch {
	case !there && was == now:
		return placed{item: item{ID: ref.ID, Version: ref.Version, Text: now}, skip: true, answer: ref}
	case there && was == now && b.Text != was:
		return placed{item: item{ID: ref.ID, Version: b.Version, Text: b.Text}, answer: ref}
	}
	return placed{item: item{ID: ref.ID, Version: ref.Version, Text: now}}
}
