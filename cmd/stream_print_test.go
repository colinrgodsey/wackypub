package cmd

import (
	"bytes"
	"testing"
)

// TestPrintStreamChunksContiguously pins the streamed-replies-double-newlines fix:
// chunks must be printed contiguously (no intervening blank line) with exactly one
// trailing newline after the whole stream. Before the fix, each chunk boundary got a
// fmt.Println() + fmt.Println(text), inserting \n\n between fragments - bridged harnesses
// (wackyagy/claude-acp) stream tiny deltas so words were chopped with double newlines.
func TestPrintStreamChunksContiguously(t *testing.T) {
	// Simulate a bridged harness: the model text arrives as byte-sized fragments that
	// span word boundaries. This is not a realistic native chunk size - it is the worst
	// case that exposed the bug and must stay contiguous.
	fragments := []string{"LI). Dranb", "o has been rout", "ing me\n\n PRs and design", "decisions via `["}

	var buf bytes.Buffer

	for _, frag := range fragments {
		printStreamChunk(&buf, frag)
	}
	finishStream(&buf)

	got := buf.String()
	want := "LI). Dranbo has been routing me\n\n PRs and designdecisions via `[\n"
	if got != want {
		t.Fatalf("streamed output = %q, want %q", got, want)
	}
}
