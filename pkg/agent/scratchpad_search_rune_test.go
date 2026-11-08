package agent

import (
	"testing"
	"unicode/utf8"
)

// The [:200] byte truncation may split a multi-byte rune, leaving invalid
// UTF-8 in ScratchpadMatch.Text (the card's "encoding" candidate).
func TestScratchpad_SearchRuneTruncation(t *testing.T) {
	agentDir := t.TempDir()

	// A line with >200 bytes of multi-byte runes plus a needle at the START.
	// The needle is within the first 200 bytes so matching still works; the
	// truncation boundary lands mid-rune.
	long := ""
	for i := 0; i < 120; i++ {
		long += "界" // 3 bytes each
	}
	text := "NEEDLE " + long + "\n"
	entry, err := CreateScratchpad(agentDir, text, "test")
	if err != nil {
		t.Fatal(err)
	}
	res, err := SearchScratchpad(agentDir, entry.ID, "NEEDLE", nil, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 1 {
		t.Fatalf("want 1 match, got %d", res.TotalMatches)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("want 1 match entry, got %d", len(res.Matches))
	}
	t.Logf("match text bytes=%d validUTF8=%v", len(res.Matches[0].Text), utf8.ValidString(res.Matches[0].Text))
	// If the truncation split a rune, Text contains invalid UTF-8.
	if !utf8.ValidString(res.Matches[0].Text) {
		t.Fatalf("match text has invalid UTF-8 (byte truncation split a rune): %q", res.Matches[0].Text)
	}
}
