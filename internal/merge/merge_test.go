package merge

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
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
			name: "crlf normalised to lf",
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
