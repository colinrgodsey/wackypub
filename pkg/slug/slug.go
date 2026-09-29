// Package slug generates 8-character pronounceable slug IDs (4 CVCV syllables,
// 24 bits of entropy) from crypto/rand. Colin 2026-09-29: replaces the old random
// 4-char alphanumeric scheme for new scratchpad entries and wackyproc records.
// IDs are opaque strings; nothing parses their format, so old-format IDs remain
// valid with no migration.
package slug

import (
	"crypto/rand"
	"strings"
	"time"
)

const (
	consonants = "bcdfghjklmnpqrstvz" // 16 consonants (4 bits)
	vowels     = "aeio"               // 4 vowels (2 bits)
	syllables  = 4
)

// New returns an 8-character pronounceable slug like "katoruvo": 4 CVCV
// syllables packed from 24 bits (3 crypto/rand bytes).
func New() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fallback()
	}
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

func fallback() string {
	var sb strings.Builder
	sb.Grow(syllables * 2)
	n := uint32(int64(0x9e3779b9) * (1 + timeNow()))
	for i := 0; i < syllables; i++ {
		slice := (n >> (6 * i)) & 0x3f
		sb.WriteByte(consonants[(slice>>2)&0x0f])
		sb.WriteByte(vowels[slice&0x03])
	}
	return sb.String()
}

// timeNow is separated so tests can fix the fallback scramble deterministically.
var timeNow = func() int64 { return time.Now().UnixNano() }
