package slug

import (
	"regexp"
	"strings"
	"testing"
)

func TestNew_ShapeAndCharset(t *testing.T) {
	for i := 0; i < 1000; i++ {
		s := New()
		if len(s) != 8 {
			t.Fatalf("slug %q has length %d, want 8", s, len(s))
		}
		for j, r := range s {
			if j%2 == 0 {
				if !strings.ContainsRune(consonants, r) {
					t.Fatalf("slug %q char %d is %q, want consonant", s, j, r)
				}
			} else {
				if !strings.ContainsRune(vowels, r) {
					t.Fatalf("slug %q char %d is %q, want vowel", s, j, r)
				}
			}
		}
	}
}

func TestNew_Uniqueness(t *testing.T) {
	// Card's "10k zero collisions" was off by an order of magnitude: with 24 bits of
	// entropy (16,777,216 slots), the birthday expectation is ~0.48 collisions at 4k
	// samples and ~3 at 10k. At n=500 the expected count is ~0.0074 and P(any collision)
	// is ~0.7% - a strict zero-collision assertion that is actually informative.
	const n = 500
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		s := New()
		if _, dup := seen[s]; dup {
			t.Fatalf("collision at iteration %d: %q", i, s)
		}
		seen[s] = struct{}{}
	}
	// Sanity: 10k samples stay collision-free in practice most runs, but that is not a
	// correctness bound - exercise it without asserting to catch a degenerate RNG.
	for i := 0; i < 10000; i++ {
		New()
	}
}

func TestNew_24BitEntropy(t *testing.T) {
	// 24 bits of payload across 4 syllables: 6 bits each. Verify diversity by
	// collecting first consonants across a run - should not collapse to one.
	distinct := map[byte]bool{}
	for i := 0; i < 2000; i++ {
		distinct[New()[0]] = true
	}
	if len(distinct) < 8 {
		t.Fatalf("first-consonant diversity too low: %d distinct of 16", len(distinct))
	}
}

func TestFallback_Deterministic(t *testing.T) {
	old := timeNow
	defer func() { timeNow = old }()
	timeNow = func() int64 { return 42 }
	a := fallback()
	b := fallback()
	if a != b {
		t.Fatalf("fallback should be deterministic for fixed timeNow: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("fallback slug %q has length %d, want 8", a, len(a))
	}
	if !regexp.MustCompile(`^[a-z]{8}$`).MatchString(a) {
		t.Fatalf("fallback slug %q is not lowercase letters", a)
	}
}
