// Package frac makes short ordering keys for blocks and cards. A key is read
// as the base-62 fraction 0.key, so byte-wise string comparison is numeric
// comparison and an item can be inserted between two neighbours without
// renumbering anything else.
package frac

import "strings"

// digits are in ASCII order, which is what makes byte-wise comparison of keys
// agree with the value of the fraction they spell.
const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Valid reports whether key is a well formed ordering key. A key is non-empty,
// made only of base-62 digits, and never ends in '0': a trailing zero would
// give one fraction two spellings and leave no room directly below it.
func Valid(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if strings.IndexByte(digits, key[i]) < 0 {
			return false
		}
	}
	return key[len(key)-1] != '0'
}

// Between returns a key that sorts strictly after a and strictly before b. An
// empty a means the start of the list and an empty b means the end. It panics
// if a or b is a non-empty invalid key or if a is not below b, both of which
// are caller bugs that would otherwise corrupt the ordering silently.
func Between(a, b string) string {
	if a != "" && !Valid(a) {
		panic("frac: invalid lower key " + a)
	}
	if b != "" && !Valid(b) {
		panic("frac: invalid upper key " + b)
	}
	if a != "" && b != "" && a >= b {
		panic("frac: keys out of order: " + a + " >= " + b)
	}
	return midpoint(a, b)
}

// First returns the key for the first item in an empty list.
func First() string { return Between("", "") }

// After returns a key that sorts after a and after every key below it.
func After(a string) string { return Between(a, "") }

// Before returns a key that sorts before b and before every key above it.
func Before(b string) string { return Between("", b) }

// midpoint returns a fraction strictly between the fractions a and b, where an
// empty a is 0 and an empty b is 1. It walks past the digits the two bounds
// share and then either picks a digit in the gap or, when the bounds are
// adjacent, extends the lower bound so the next digit has room.
func midpoint(a, b string) string {
	if b != "" {
		n := 0
		for n < len(b) {
			d := byte('0')
			if n < len(a) {
				d = a[n]
			}
			if d != b[n] {
				break
			}
			n++
		}
		if n > 0 {
			return b[:n] + midpoint(tail(a, n), b[n:])
		}
	}
	lo, hi := 0, len(digits)
	if a != "" {
		lo = strings.IndexByte(digits, a[0])
	}
	if b != "" {
		hi = strings.IndexByte(digits, b[0])
	}
	if hi-lo > 1 {
		return string(digits[(lo+hi+1)/2])
	}
	if len(b) > 1 {
		return b[:1]
	}
	return string(digits[lo]) + midpoint(tail(a, 1), "")
}

func tail(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}
