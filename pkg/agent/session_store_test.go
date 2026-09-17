package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestReadWriteAppendSessionTurns(t *testing.T) {
	tempDir := t.TempDir()

	turns, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("unexpected error reading non-existent session file: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("expected 0 turns, got %d", len(turns))
	}

	if err := AppendSessionTurn(tempDir, "user", "Hello agent"); err != nil {
		t.Fatalf("failed to append user turn: %v", err)
	}
	if err := AppendSessionTurn(tempDir, "assistant", "Hello user"); err != nil {
		t.Fatalf("failed to append assistant turn: %v", err)
	}

	turns, err = ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("failed to read appended session turns: %v", err)
	}

	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}

	if turns[0].Role != "user" || ContentText(turns[0]) != "Hello agent" {
		t.Errorf("turn 0 mismatch: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || ContentText(turns[1]) != "Hello user" {
		t.Errorf("turn 1 mismatch: %+v", turns[1])
	}
}

func TestCleanSessionTurns(t *testing.T) {
	text := func(role, s string) *genai.Content {
		return genai.NewContentFromText(s, genai.Role(role))
	}

	funcCall := func(name, id string) *genai.Content {
		return &genai.Content{
			Role: "model",
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						Name: name,
						ID:   id,
						Args: map[string]any{"arg": "val"},
					},
				},
			},
		}
	}

	callWithText := func(name, id, s string) *genai.Content {
		return &genai.Content{
			Role: "model",
			Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{Name: name, ID: id, Args: map[string]any{"arg": "val"}}},
				{Text: s},
			},
		}
	}

	funcResp := func(name, id string) *genai.Content {
		return &genai.Content{
			Role: "user",
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						Name:     name,
						ID:       id,
						Response: map[string]any{"output": "ok"},
					},
				},
			},
		}
	}

	t.Run("empty input", func(t *testing.T) {
		got := CleanSessionTurns(nil)
		if len(got) != 0 {
			t.Errorf("expected 0 turns, got %d", len(got))
		}
	})

	t.Run("no merge needed (already alternating)", func(t *testing.T) {
		in := []*genai.Content{text("user", "a"), text("model", "b"), text("user", "c")}
		got := CleanSessionTurns(in)
		if len(got) != 3 {
			t.Fatalf("expected 3 turns, got %d", len(got))
		}
		if got[0].Role != "user" || got[0].Parts[0].Text != "a" {
			t.Errorf("turn 0 mismatch: %+v", got[0])
		}
		if got[1].Role != "model" || got[1].Parts[0].Text != "b" {
			t.Errorf("turn 1 mismatch: %+v", got[1])
		}
		if got[2].Role != "user" || got[2].Parts[0].Text != "c" {
			t.Errorf("turn 2 mismatch: %+v", got[2])
		}
	})

	t.Run("merges a run of consecutive user turns", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "system+memory turn"),
			text("user", "first real message"),
			text("model", "assistant reply"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if got[0].Role != "user" || len(got[0].Parts) != 2 {
			t.Fatalf("expected merged user turn with 2 parts, got %+v", got[0])
		}
	})

	t.Run("drops dangling function response (response without preceding call)", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "hello"),
			funcResp("create_scratchpad", "call_x"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(got))
		}
		if len(got[0].Parts) != 1 || got[0].Parts[0].Text != "hello" {
			t.Errorf("expected only the text part to survive, got %+v", got[0].Parts)
		}
	})

	t.Run("keeps call+response pair intact", func(t *testing.T) {
		in := []*genai.Content{
			funcCall("run_command", "call_1"),
			funcResp("run_command", "call_1"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if got[0].Role != "model" || got[0].Parts[0].FunctionCall == nil {
			t.Errorf("expected model call to survive, got %+v", got[0])
		}
		if got[1].Role != "user" || got[1].Parts[0].FunctionResponse == nil {
			t.Errorf("expected user response to survive, got %+v", got[1])
		}
	})

	t.Run("drops dangling function call (call with no following response)", func(t *testing.T) {
		in := []*genai.Content{
			funcCall("run_command", "call_1"),
			text("user", "never mind"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 1 {
			t.Fatalf("expected 1 turn (model turn pruned entirely), got %d", len(got))
		}
		if got[0].Role != "user" || got[0].Parts[0].Text != "never mind" {
			t.Errorf("expected only the user text turn to survive, got %+v", got)
		}
	})

	t.Run("strips unanswered calls but keeps text in same model turn", func(t *testing.T) {
		in := []*genai.Content{
			callWithText("run_command", "call_1", "let me check"),
			text("user", "ok"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if len(got[0].Parts) != 1 || got[0].Parts[0].Text != "let me check" {
			t.Errorf("expected only text to survive in model turn, got %+v", got[0].Parts)
		}
	})

	t.Run("multiple calls: only answered call survives", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role: "model",
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "good_tool", ID: "c1", Args: map[string]any{}}},
					{FunctionCall: &genai.FunctionCall{Name: "flaky_tool", ID: "c2", Args: map[string]any{}}},
				},
			},
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "good_tool", ID: "c1", Response: map[string]any{}}},
				},
			},
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if len(got[0].Parts) != 1 || got[0].Parts[0].FunctionCall == nil || got[0].Parts[0].FunctionCall.ID != "c1" {
			t.Errorf("expected only answered call c1 to survive, got %+v", got[0].Parts)
		}
		if len(got[1].Parts) != 1 || got[1].Parts[0].FunctionResponse == nil {
			t.Errorf("expected response turn to survive, got %+v", got[1])
		}
	})

	t.Run("call at end of history (compaction cut dropped its response)", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "do something"),
			funcCall("run_command", "call_cut"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(got))
		}
		if got[0].Role != "user" {
			t.Errorf("expected only user turn to survive, got %+v", got)
		}
	})

	t.Run("dangling response dropped from mixed user turn", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "hello"),
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "ghost_tool", ID: "ghost", Response: map[string]any{}}},
					{Text: "actual message"},
				},
			},
		}
		got := CleanSessionTurns(in)
		if len(got) != 1 {
			t.Fatalf("expected 1 merged turn, got %d", len(got))
		}
		if len(got[0].Parts) != 2 {
			t.Fatalf("expected both text parts, got %+v", got[0].Parts)
		}
		for _, p := range got[0].Parts {
			if p.FunctionResponse != nil {
				t.Errorf("dangling response must be dropped, got %+v", p)
			}
		}
	})

	t.Run("MergeConsecutiveUserTurns wrapper maintains identical behavior", func(t *testing.T) {
		in := []*genai.Content{text("user", "1"), text("user", "2")}
		gotClean := CleanSessionTurns(in)
		gotMerge := MergeConsecutiveUserTurns(in)
		if len(gotClean) != len(gotMerge) {
			t.Errorf("mismatch between CleanSessionTurns and MergeConsecutiveUserTurns length")
		}
		if len(gotMerge[0].Parts) != 2 {
			t.Errorf("expected 2 merged parts, got %d", len(gotMerge[0].Parts))
		}
	})
}

// TestAppendSessionContentHealsTrailingNewline reproduces the corruption mode documented
// in AGENTS.md's Gotchas section: if session.jsonl's last line has no trailing newline
// (e.g. a hand-edit that dropped it) and AppendSessionContent then appends a new turn,
// the two JSON objects land on one line and ReadSessionTurns silently drops both. D75.
func TestAppendSessionContentHealsTrailingNewline(t *testing.T) {
	tempDir := t.TempDir()
	sessionPath := filepath.Join(tempDir, SessionFileName)

	// Write a valid turn directly to the file, deliberately omitting the trailing '\n'
	// to simulate the hand-edit corruption mode.
	firstTurn := genai.NewContentFromText("turn before hand-edit", "user")
	firstData, err := json.Marshal(firstTurn)
	if err != nil {
		t.Fatalf("failed to marshal first turn: %v", err)
	}
	if err := os.WriteFile(sessionPath, firstData, 0644); err != nil {
		t.Fatalf("failed to write session file without trailing newline: %v", err)
	}

	// Appending should heal the missing newline rather than merging with the prior turn.
	secondTurn := genai.NewContentFromText("turn appended after hand-edit", "model")
	if err := AppendSessionContent(tempDir, secondTurn); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	turns, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns after healing append, got %d (corruption not healed)", len(turns))
	}
	if ContentText(turns[0]) != "turn before hand-edit" {
		t.Errorf("turn[0] text mismatch: got %q", ContentText(turns[0]))
	}
	if ContentText(turns[1]) != "turn appended after hand-edit" {
		t.Errorf("turn[1] text mismatch: got %q", ContentText(turns[1]))
	}
}

// TestAppendSessionContentNormalCase verifies that the newline check does not interfere
// with ordinary appends where each prior write correctly left a trailing newline.
func TestAppendSessionContentNormalCase(t *testing.T) {
	tempDir := t.TempDir()

	for i, tc := range []struct{ role, text string }{
		{"user", "first"},
		{"model", "second"},
		{"user", "third"},
	} {
		if err := AppendSessionTurn(tempDir, tc.role, tc.text); err != nil {
			t.Fatalf("AppendSessionTurn[%d] failed: %v", i, err)
		}
	}

	turns, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got := ContentText(turns[i]); got != want {
			t.Errorf("turns[%d]: got %q, want %q", i, got, want)
		}
	}
}

func TestReadSessionTurns_LargeLineOverOldCap(t *testing.T) {
	agentDir := t.TempDir()
	// 1.5MB single-turn line: exceeds the historical 1MB cap, well under 16MB.
	big := strings.Repeat("x", 1500*1024)
	line, err := json.Marshal(genai.NewContentFromText(big, "model"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "session.jsonl"), append(line, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed on a >1MB line: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if got := len(turns[0].Parts[0].Text); got != 1500*1024 {
		t.Errorf("expected %d chars, got %d", 1500*1024, got)
	}
}

func TestAppendSessionContent_TruncatesOversizedTextPart(t *testing.T) {
	agentDir := t.TempDir()

	origSize := 1500 * 1024 // 1.5MB
	headPrefix := "HEAD_START_CONTENT_"
	tailSuffix := "_TAIL_END_CONTENT"

	var sb strings.Builder
	sb.WriteString(headPrefix)
	padding := strings.Repeat("M", origSize-len(headPrefix)-len(tailSuffix))
	sb.WriteString(padding)
	sb.WriteString(tailSuffix)
	largeText := sb.String()

	content := genai.NewContentFromText(largeText, "model")
	if err := AppendSessionContent(agentDir, content); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if len(turns[0].Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(turns[0].Parts))
	}

	gotText := turns[0].Parts[0].Text

	// Text length is bounded (~16KB + banner)
	expectedBanner := fmt.Sprintf("\n[...truncated - original part was %d chars...]\n", origSize)
	maxExpectedLen := PersistTruncationHead + PersistTruncationTail + len(expectedBanner) + 100
	if len(gotText) > maxExpectedLen {
		t.Errorf("text length %d exceeds expected bound %d", len(gotText), maxExpectedLen)
	}

	// Banner contains original size
	if !strings.Contains(gotText, fmt.Sprintf("original part was %d chars", origSize)) {
		t.Errorf("expected text to contain banner with original size %d, got: %q", origSize, gotText)
	}

	// Head and tail content present
	if !strings.HasPrefix(gotText, headPrefix) {
		t.Errorf("expected text to start with head prefix %q", headPrefix)
	}
	if !strings.HasSuffix(gotText, tailSuffix) {
		t.Errorf("expected text to end with tail suffix %q", tailSuffix)
	}
}

func TestAppendSessionContent_TruncationPreservesSmallParts(t *testing.T) {
	agentDir := t.TempDir()

	smallText := "This is a normal user turn with reasonable length."
	content := genai.NewContentFromText(smallText, "user")

	expectedData, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	expectedData = append(expectedData, '\n')

	if err := AppendSessionContent(agentDir, content); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	rawFile, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}

	if string(rawFile) != string(expectedData) {
		t.Errorf("expected byte-identical output:\nwant: %s\ngot:  %s", string(expectedData), string(rawFile))
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) != 1 || turns[0].Parts[0].Text != smallText {
		t.Errorf("unexpected read turn: %+v", turns)
	}
}

func TestAppendSessionContent_WholeContentFallback(t *testing.T) {
	agentDir := t.TempDir()

	// 3 text parts of 200KB each (total 600KB > 512KB MaxPersistTurnBytes).
	// Each part is <= 256KB, so part-level capping does not trigger.
	// Whole-content fallback must trigger and clamp total size to <= 512KB.
	part1 := "PART1_" + strings.Repeat("a", 200*1024-6)
	part2 := "PART2_" + strings.Repeat("b", 200*1024-6)
	part3 := "PART3_" + strings.Repeat("c", 200*1024-6)

	content := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: part1},
			{Text: part2},
			{Text: part3},
		},
	}

	if err := AppendSessionContent(agentDir, content); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	rawFile, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}

	// Marshaled turn on disk must not exceed MaxPersistTurnBytes + newline
	if len(rawFile) > MaxPersistTurnBytes+1 {
		t.Errorf("persisted turn size %d exceeds MaxPersistTurnBytes %d", len(rawFile), MaxPersistTurnBytes)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	// Fallback should keep only the first text part plus the drop banner
	if len(turns[0].Parts) != 2 {
		t.Fatalf("expected 2 parts (first text part + banner), got %d", len(turns[0].Parts))
	}
	if !strings.HasPrefix(turns[0].Parts[0].Text, "PART1_") {
		t.Errorf("expected first part to be preserved, got: %q", turns[0].Parts[0].Text[:50])
	}
	if !strings.Contains(turns[0].Parts[1].Text, "remaining content dropped") {
		t.Errorf("expected second part to be drop banner, got: %q", turns[0].Parts[1].Text)
	}
}
