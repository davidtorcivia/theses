package frac

import (
	"math/rand/v2"
	"sort"
	"testing"
)

func TestBetween(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want string
	}{
		{"empty list", "", "", "V"},
		{"before first", "", "V", "G"},
		{"after last", "V", "", "W"},
		{"wide gap", "V", "l", "d"},
		{"adjacent digits", "1", "2", "1V"},
		{"adjacent digits again", "1V", "2", "1l"},
		{"shared prefix", "Vd", "Vl", "Vh"},
		{"upper extends lower", "V", "V1", "V0V"},
		{"lower is prefix of upper", "V", "VV", "VG"},
		{"top of the range", "z", "", "z1"},
		{"bottom of the range", "", "01", "00V"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Between(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("Between(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
			if !Valid(got) {
				t.Errorf("Between(%q, %q) = %q, which is not a valid key", tt.a, tt.b, got)
			}
			if tt.a != "" && got <= tt.a {
				t.Errorf("Between(%q, %q) = %q, not above the lower bound", tt.a, tt.b, got)
			}
			if tt.b != "" && got >= tt.b {
				t.Errorf("Between(%q, %q) = %q, not below the upper bound", tt.a, tt.b, got)
			}
		})
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"V", true},
		{"0V", true},
		{"zzzzz1", true},
		{"", false},
		{"V0", false},
		{"0", false},
		{"V-", false},
		{"V!", false},
		{"Vé", false},
		{"V V", false},
	}
	for _, tt := range tests {
		if got := Valid(tt.key); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestBetweenPanics(t *testing.T) {
	tests := []struct {
		name string
		a, b string
	}{
		{"equal keys", "V", "V"},
		{"out of order", "l", "V"},
		{"invalid lower", "V0", "l"},
		{"invalid upper", "V", "l!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Between(%q, %q) did not panic", tt.a, tt.b)
				}
			}()
			Between(tt.a, tt.b)
		})
	}
}

// Inserting over and over into the same gap is the worst case for key length:
// each insertion can only halve the remaining interval.
func TestRepeatedInsertInSameGap(t *testing.T) {
	lo, hi := "1", "2"
	for i := 0; i < 200; i++ {
		key := Between(lo, hi)
		if key <= lo || key >= hi {
			t.Fatalf("insertion %d: %q is not between %q and %q", i, key, lo, hi)
		}
		hi = key
	}
	if len(hi) > 40 {
		t.Errorf("200 insertions in one gap grew the key to %d characters: %q", len(hi), hi)
	}
}

// Appending is the other common case: blocks added at the end of a document,
// cards added to the bottom of a column.
func TestRepeatedAppend(t *testing.T) {
	key := Between("", "")
	for i := 0; i < 1000; i++ {
		got := Between(key, "")
		if !Valid(got) {
			t.Fatalf("append %d produced invalid key %q", i, got)
		}
		if got <= key {
			t.Fatalf("append %d: %q is not above %q", i, got, key)
		}
		key = got
	}
	if len(key) > 20 {
		t.Errorf("1000 appends grew the key to %d characters: %q", len(key), key)
	}
}

func TestRandomInsertions(t *testing.T) {
	const n = 10000
	r := rand.New(rand.NewPCG(1, 2))
	keys := []string{Between("", "")}
	for i := 0; i < n; i++ {
		at := r.IntN(len(keys) + 1)
		lo, hi := "", ""
		if at > 0 {
			lo = keys[at-1]
		}
		if at < len(keys) {
			hi = keys[at]
		}
		key := Between(lo, hi)
		if !Valid(key) {
			t.Fatalf("insertion %d produced invalid key %q", i, key)
		}
		if (lo != "" && key <= lo) || (hi != "" && key >= hi) {
			t.Fatalf("insertion %d: %q is not between %q and %q", i, key, lo, hi)
		}
		keys = append(keys, "")
		copy(keys[at+1:], keys[at:])
		keys[at] = key
	}
	if !sort.SliceIsSorted(keys, func(i, j int) bool { return keys[i] < keys[j] }) {
		t.Fatal("keys are not in strictly increasing order after 10000 insertions")
	}
	total := 0
	for _, k := range keys {
		total += len(k)
	}
	if avg := float64(total) / float64(len(keys)); avg >= 12 {
		t.Errorf("average key length %.2f, want under 12", avg)
	}
}

// The browser guesses where a row it has just made goes, before the server has
// allocated a key for it, by putting a zero on the end of the key of the row it
// was made under. That guess has to hold whatever the server does next: the row
// is drawn below the one it was made under and above everything that was
// already there, and it stays there when the real key arrives. What makes it
// hold is that a trailing zero is not a key anybody else can be given.
func TestAKeyWithAZeroOnTheEndSitsDirectlyAfterIt(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	keys := []string{""}
	for i := 0; i < 2000; i++ {
		at := r.IntN(len(keys) + 1)
		lo, hi := "", ""
		if at > 0 {
			lo = keys[at-1]
		}
		if at < len(keys) {
			hi = keys[at]
		}
		keys = append(keys, "")
		copy(keys[at+1:], keys[at:])
		keys[at] = Between(lo, hi)
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		guess := key + "0"
		if Valid(guess) {
			t.Fatalf("%q is a key the server could allocate", guess)
		}
		if guess <= key {
			t.Fatalf("%q does not sort after %q", guess, key)
		}
		// Every valid key above the one the row was made under is above the
		// guess as well, so nothing the server has already given out, and
		// nothing it can give out later, lands between the two.
		for _, other := range keys {
			if other > key && guess >= other {
				t.Fatalf("%q is not below %q, which is above %q", guess, other, key)
			}
		}
	}
}
