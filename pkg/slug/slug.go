// Package slug generates 8-character pronounceable slug IDs (4 CVCV syllables,
// 24 bits of entropy) from crypto/rand. Colin 2026-09-29: replaces the old random
// 4-char alphanumeric scheme for new scratchpad entries and wackyproc records.
// IDs are opaque strings; nothing parses their format, so old-format IDs remain
// valid with no migration.
package slug

import (
	"crypto/rand"
	"strings"
)

const (
	consonants = "bcdfghjklmnpqrstvz" // 16 consonants (4 bits)
	vowels     = "aeio"               // 4 vowels (2 bits)
	syllables  = 4
)

// New returns an 8-character pronounceable slug like "katoruvo": 4 CVCV
// syllables packed from 24 bits (3 crypto/rand bytes). crypto/rand's Read never
// returns an error, so there is no fallback path (Colin 2026-09-29).
func New() string {
	var b [3]byte
	// crypto/rand.Read fills the buffer or panics; it has no error path to check.
	_, _ = rand.Read(b[:])
	return NewWith(b)
}

// NewWith renders an 8-character slug from exactly 3 entropy bytes (24 bits).
// It exists so tests can be fully deterministic while sharing the real packing
// logic.
func NewWith(b [3]byte) string {
	u := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	var sb strings.Builder
	sb.Grow(syllables * 2)
	for i := 0; i < syllables; i++ {
		slice := (u >> (6 * i)) & 0x3f
		sb.WriteByte(consonants[(slice>>2)&0x0f])
		sb.WriteByte(vowels[slice&0x03])
	}
	return sb.String()
}
