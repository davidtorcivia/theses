package merge

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name         string
		base         string
		ours         string
		theirs       string
		want         string
		wantConflict bool
	}{
		{
			name: "identical",
			base: "One line.", ours: "One line.", theirs: "One line.",
			want: "One line.",
		},
		{
			name: "ours unchanged",
			base: "Alpha\nBeta", ours: "Alpha\nBeta", theirs: "Alpha\nBeta two",
			want: "Alpha\nBeta two",
		},
		{
			name: "theirs unchanged",
			base: "Alpha\nBeta", ours: "Alpha one\nBeta", theirs: "Alpha\nBeta",
			want: "Alpha one\nBeta",
		},
		{
			name:   "disjoint line edits",
			base:   "Alpha\nBeta\nGamma\nDelta\nEpsilon",
			ours:   "Alpha one\nBeta\nGamma\nDelta\nEpsilon",
			theirs: "Alpha\nBeta\nGamma\nDelta\nEpsilon five",
			want:   "Alpha one\nBeta\nGamma\nDelta\nEpsilon five",
		},
		{
			name: "adjacent line edits",
			base: "Alpha\nBeta\nGamma", ours: "Alpha one\nBeta\nGamma", theirs: "Alpha\nBeta two\nGamma",
			want: "Alpha one\nBeta two\nGamma",
		},
		{
			name: "same line different words",
			base: "the quick brown fox", ours: "the slow brown fox", theirs: "the quick brown dog",
			want: "the slow brown dog",
		},
		{
			name: "same word two ways",
			base: "the quick brown fox", ours: "the slow brown fox", theirs: "the fast brown fox",
			want: "the slow brown fox", wantConflict: true,
		},
		{
			name: "word inserted on each side",
			base: "one two", ours: "one and two", theirs: "one two three",
			want: "one and two three",
		},
		{
			name: "insertions at both ends",
			base: "Beta\nGamma", ours: "Alpha\nBeta\nGamma", theirs: "Beta\nGamma\nDelta",
			want: "Alpha\nBeta\nGamma\nDelta",
		},
		{
			name: "both insert different lines at the same place",
			base: "Alpha\nGamma", ours: "Alpha\nBeta ours\nGamma", theirs: "Alpha\nBeta theirs\nGamma",
			want: "Alpha\nBeta ours\nGamma", wantConflict: true,
		},
		{
			name: "deletion on one side, edit elsewhere on the other",
			base: "Alpha\nBeta\nGamma\nDelta", ours: "Alpha\nGamma\nDelta", theirs: "Alpha\nBeta\nGamma\nDelta four",
			want: "Alpha\nGamma\nDelta four",
		},
		{
			name: "deletion on one side, edit of the deleted line on the other",
			base: "Alpha\nBeta\nGamma", ours: "Alpha\nGamma", theirs: "Alpha\nBeta two\nGamma",
			want: "Alpha\nGamma", wantConflict: true,
		},
		{
			name: "unicode words",
			base: "Le café est ouvert", ours: "Le café est fermé", theirs: "Le thé est ouvert",
			want: "Le thé est fermé",
		},
		{
			name: "crlf normalized to lf",
			base: "Alpha\r\nBeta\r\n", ours: "Alpha one\r\nBeta\r\n", theirs: "Alpha\r\nBeta two\r\n",
			want: "Alpha one\nBeta two\n",
		},
		{
			name: "text added to an empty base",
			base: "", ours: "", theirs: "A first sentence.",
			want: "A first sentence.",
		},
		{
			name: "both sides make the same edit",
			base: "Alpha\nBeta", ours: "Alpha one\nBeta", theirs: "Alpha one\nBeta",
			want: "Alpha one\nBeta",
		},
		{
			name: "trailing blank line kept",
			base: "Alpha\n\n", ours: "Alpha one\n\n", theirs: "Alpha\n\n",
			want: "Alpha one\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, conflict := Merge(tt.base, tt.ours, tt.theirs)
			if conflict != tt.wantConflict {
				t.Fatalf("Merge conflict = %v, want %v (result %q)", conflict, tt.wantConflict, got)
			}
			if got != tt.want {
				t.Errorf("Merge = %q, want %q", got, tt.want)
			}
		})
	}
}

// A conflict hands ours back exactly as it arrived, line endings included, so
// the caller can put it straight back in the editor.
func TestMergeConflictReturnsOursVerbatim(t *testing.T) {
	ours := "the slow brown fox\r\n"
	got, conflict := Merge("the quick brown fox\r\n", ours, "the fast brown fox\r\n")
	if !conflict {
		t.Fatalf("Merge conflict = false, want true")
	}
	if got != ours {
		t.Errorf("Merge = %q, want ours unchanged %q", got, ours)
	}
}

func TestWords(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"one", []string{"one"}},
		{"one two", []string{"one", " ", "two"}},
		{"  one\t two ", []string{"  ", "one", "\t ", "two", " "}},
		{"café ouvert", []string{"café", " ", "ouvert"}},
	}
	for _, tt := range tests {
		got := words(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("words(%q) = %q, want %q", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("words(%q) = %q, want %q", tt.in, got, tt.want)
				break
			}
		}
	}
}

// A block that is one enormous line must not put a quadratic table in front of
// a request handler. It conflicts instead, and the caller offers keep mine or
// take theirs.
func TestMergeRefusesAnEnormousLine(t *testing.T) {
	parts := make([]string, 5000)
	for i := range parts {
		parts[i] = fmt.Sprintf("word%d", i)
	}
	base := strings.Join(parts, " ")
	ours := strings.Replace(base, "word10 ", "ours ", 1)
	theirs := strings.Replace(base, "word20 ", "theirs ", 1)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, conflict := Merge(base, ours, theirs)
	runtime.ReadMemStats(&after)

	if !conflict {
		t.Error("Merge conflict = false, want true")
	}
	if got != ours {
		t.Error("Merge did not return ours unchanged")
	}
	if used := after.TotalAlloc - before.TotalAlloc; used > 8<<20 {
		t.Errorf("merging a 5000 word line allocated %d bytes, want under 8 MB", used)
	}
}

// versions builds a document where both sides rewrote every line, one at a
// different word from the other, which is the shape that sends every line to
// the word level.
func versions(lines, words int) (base, ours, theirs string) {
	b := make([]string, lines)
	o := make([]string, lines)
	th := make([]string, lines)
	for i := range b {
		parts := make([]string, words)
		for j := range parts {
			parts[j] = fmt.Sprintf("w%dx%d", i, j)
		}
		b[i] = strings.Join(parts, " ")
		was := parts[1]
		parts[1] = "ours"
		o[i] = strings.Join(parts, " ")
		parts[1] = was
		parts[2] = "theirs"
		th[i] = strings.Join(parts, " ")
	}
	return strings.Join(b, "\n"), strings.Join(o, "\n"), strings.Join(th, "\n")
}

// The budget belongs to the whole merge, not to one table. Both shapes below
// stop: the first because its line table alone is too big, the second because
// three hundred small word tables add up, which a per table cap would let
// through at about ninety megabytes.
func TestMergeBudgetCoversTheWholeCall(t *testing.T) {
	tests := []struct {
		name  string
		lines int
		words int
	}{
		{"one huge table", 1024, 512},
		{"many small tables", 300, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, ours, theirs := versions(tt.lines, tt.words)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			got, conflict := Merge(base, ours, theirs)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)

			if !conflict {
				t.Error("Merge conflict = false, want true")
			}
			if got != ours {
				t.Error("Merge did not return ours unchanged")
			}
			if used := after.TotalAlloc - before.TotalAlloc; used > 16<<20 {
				t.Errorf("merging %d lines of %d words allocated %d bytes, want under 16 MB", tt.lines, tt.words, used)
			}
			if elapsed > time.Second {
				t.Errorf("merging %d lines of %d words took %v, want well under a second", tt.lines, tt.words, elapsed)
			}
		})
	}
}

// The budget is large enough that documents of the size people write still
// merge line by line and word by word.
func TestMergeStillMergesANormalDocument(t *testing.T) {
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = fmt.Sprintf("Line %d holds a short sentence about the thing.", i)
	}
	base := strings.Join(lines, "\n")
	ours := strings.Replace(base, "Line 3 holds", "Line 3 now holds", 1)
	theirs := strings.Replace(base, "Line 150 holds", "Line 150 also holds", 1)

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatal("Merge conflict = true, want a clean merge")
	}
	if !strings.Contains(got, "Line 3 now holds") || !strings.Contains(got, "Line 150 also holds") {
		t.Error("Merge lost one of the two edits")
	}
}
