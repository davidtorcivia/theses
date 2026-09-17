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
		{"after last", "V", "", "l"},
		{"wide gap", "V", "l", "d"},
		{"adjacent digits", "1", "2", "1V"},
		{"adjacent digits again", "1V", "2", "1l"},
		{"shared prefix", "Vd", "Vl", "Vh"},
		{"upper extends lower", "V", "V1", "V0V"},
		{"lower is prefix of upper", "V", "VV", "VG"},
		{"top of the range", "z", "", "zV"},
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

func TestFirstAfterBefore(t *testing.T) {
	first := First()
	if first != Between("", "") {
		t.Errorf("First() = %q, want %q", first, Between("", ""))
	}
	if after := After(first); after <= first {
		t.Errorf("After(%q) = %q, not above it", first, after)
	}
	if before := Before(first); before >= first {
		t.Errorf("Before(%q) = %q, not below it", first, before)
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

func TestRandomInsertions(t *testing.T) {
	const n = 10000
	r := rand.New(rand.NewPCG(1, 2))
	keys := []string{First()}
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
