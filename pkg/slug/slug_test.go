package slug

import (
	"regexp"
	"strings"
	"testing"
)

// pinned vectors: b -> slug, computed from the reference packing (4x 6-bit
// syllables: 4-bit consonant + 2-bit vowel). These are format pins - they lock
// the exact CVCV rendering so any future packing change is caught immediately.
var vectors = []struct {
	in   [3]byte
	want string
}{
	{[3]byte{0x00, 0x00, 0x00}, "babababa"},
	{[3]byte{0xff, 0xff, 0xff}, "totototo"},
	{[3]byte{0x01, 0x00, 0x00}, "babagaba"},
	{[3]byte{0x02, 0x00, 0x00}, "babalaba"},
	{[3]byte{0x04, 0x00, 0x00}, "babababe"},
	{[3]byte{0x2a, 0x70, 0xd2}, "gibomodi"},
}

func TestNewWith_DeterministicVectors(t *testing.T) {
	for _, v := range vectors {
		if got := NewWith(v.in); got != v.want {
			t.Errorf("NewWith(% x) = %q, want %q", v.in, got, v.want)
		}
	}
}

func TestNewWith_ShapeAndCharset(t *testing.T) {
	for i := 0; i < 256; i++ {
		s := NewWith([3]byte{byte(i), byte(i >> 1), byte(i >> 2)})
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

func TestNewWith_UniquenessDeterministic(t *testing.T) {
	// 8192 distinct inputs -> 8192 distinct outputs. Fully deterministic (no
	// birthday lottery) and fast; it bounds the packing's injectivity over the
	// tested half-space.
	seen := make(map[string]struct{}, 8192)
	for i := 0; i < 8192; i++ {
		var b [3]byte
		b[0] = byte(i)
		b[1] = byte(i >> 8)
		b[2] = byte(i >> 16)
		s := NewWith(b)
		if _, dup := seen[s]; dup {
			t.Fatalf("collision at input %d: %q", i, s)
		}
		seen[s] = struct{}{}
	}
	if len(seen) != 8192 {
		t.Fatalf("expected 8192 distinct slugs, got %d", len(seen))
	}
}

func TestNew_ShapeAndCharset(t *testing.T) {
	for i := 0; i < 100; i++ {
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
	if !regexp.MustCompile(`^[a-z]{8}$`).MatchString(New()) {
		t.Fatal("New() should produce 8 lowercase letters")
	}
}
