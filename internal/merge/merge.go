// Package merge does the three-way merge behind a stale block.set: base is the
// text the editor started from, ours is what it sends, theirs is what the
// block holds now.
package merge

import (
	"slices"
	"strings"
	"unicode"
)

// Merge combines the changes ours and theirs each made to base. Line endings
// in all three are normalized to LF. When both sides changed the same words
// differently, or one side deleted lines the other edited, there is nothing
// honest to return, so Merge reports a conflict and returns ours exactly as it
// was passed in and the caller decides.
func Merge(base, ours, theirs string) (result string, conflict bool) {
	b, o, t := lf(base), lf(ours), lf(theirs)
	switch {
	case o == t:
		return o, false
	case b == o:
		return t, false
	case b == t:
		return o, false
	}
	budget := maxCells
	lines, ok := merge3(&budget, strings.Split(b, "\n"), strings.Split(o, "\n"), strings.Split(t, "\n"), mergeLines)
	if !ok {
		return ours, true
	}
	return strings.Join(lines, "\n"), false
}

func lf(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// maxCells is the budget for one whole Merge, counted in cells of the LCS
// tables, the only thing here that grows with the product of the input sizes.
// A million cells is about eight megabytes of tables and a few milliseconds of
// work, and the line level pass and every word level pass under it draw on that
// one allowance, so no single block.set can cost more than that whatever shape
// it has. A merge that runs out of budget reports a conflict, and the person
// chooses keep mine or take theirs.
//
// ponytail: a linear space LCS would merge the blocks that now conflict, at
// maybe eighty more lines; worth it only if blocks that large turn out to be
// common.
const maxCells = 1 << 20

// merge3 walks base, ours and theirs together. Runs that all three agree on
// pass through; everything between them is a chunk that at least one side
// changed, settled by whichever side left it alone or, when both changed it,
// by refine.
func merge3(budget *int, base, ours, theirs []string, refine refiner) ([]string, bool) {
	cells := len(base)*len(ours) + len(base)*len(theirs)
	if cells > *budget {
		return nil, false
	}
	*budget -= cells
	mo := Match(base, ours)
	mt := Match(base, theirs)
	var out []string
	i, o, t := 0, 0, 0
	for i < len(base) || o < len(ours) || t < len(theirs) {
		if i < len(base) && mo[i] == o && mt[i] == t {
			out = append(out, base[i])
			i, o, t = i+1, o+1, t+1
			continue
		}
		// The chunk ends at the next line both sides still have in place, or
		// at the end of all three.
		i2, o2, t2 := len(base), len(ours), len(theirs)
		for j := i; j < len(base); j++ {
			if mo[j] >= o && mt[j] >= t {
				i2, o2, t2 = j, mo[j], mt[j]
				break
			}
		}
		chunk, ok := resolve(budget, base[i:i2], ours[o:o2], theirs[t:t2], refine)
		if !ok {
			return nil, false
		}
		out = append(out, chunk...)
		i, o, t = i2, o2, t2
	}
	return out, true
}

// A refiner settles a chunk both sides rewrote, spending the same budget.
type refiner func(budget *int, base, ours, theirs []string) ([]string, bool)

func resolve(budget *int, base, ours, theirs []string, refine refiner) ([]string, bool) {
	switch {
	case slices.Equal(ours, theirs):
		return ours, true
	case slices.Equal(base, ours):
		return theirs, true
	case slices.Equal(base, theirs):
		return ours, true
	}
	return refine(budget, base, ours, theirs)
}

// mergeLines refines a chunk both sides rewrote by pairing the lines up, which
// only means anything when neither side added or removed any.
func mergeLines(budget *int, base, ours, theirs []string) ([]string, bool) {
	if len(base) != len(ours) || len(base) != len(theirs) {
		return nil, false
	}
	out := make([]string, len(base))
	for i := range base {
		line, ok := mergeWords(budget, base[i], ours[i], theirs[i])
		if !ok {
			return nil, false
		}
		out[i] = line
	}
	return out, true
}

// mergeWords merges one line both sides changed. Two sides touching different
// words in a sentence is the common case and merges; the same word changed two
// ways conflicts.
func mergeWords(budget *int, base, ours, theirs string) (string, bool) {
	switch {
	case ours == theirs:
		return ours, true
	case base == ours:
		return theirs, true
	case base == theirs:
		return ours, true
	}
	tokens, ok := merge3(budget, words(base), words(ours), words(theirs), conflict)
	if !ok {
		return "", false
	}
	return strings.Join(tokens, ""), true
}

func conflict(*int, []string, []string, []string) ([]string, bool) { return nil, false }

// words splits at every boundary between whitespace and non-whitespace, so the
// tokens joined back together are the original string.
func words(s string) []string {
	var out []string
	start := 0
	space := false
	for i, r := range s {
		isSpace := unicode.IsSpace(r)
		if i == 0 {
			space = isSpace
			continue
		}
		if isSpace != space {
			out = append(out, s[start:i])
			start, space = i, isSpace
		}
	}
	if s != "" {
		out = append(out, s[start:])
	}
	return out
}

// Match pairs each element of a with the element of b it keeps in a longest
// common subsequence, or -1 if b no longer has it. Callers take the cells out
// of the budget first.
//
// It is exported because lining a document's paragraphs up against the blocks
// they were written from is the same question asked of whole paragraphs rather
// than of lines, and a second longest common subsequence would be a second set
// of answers to it.
func Match(a, b []string) []int {
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	out := make([]int, len(a))
	for i := range out {
		out[i] = -1
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out[i] = j
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}
