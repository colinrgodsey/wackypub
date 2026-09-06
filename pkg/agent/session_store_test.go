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
		if got[0].Parts[0].Text != "system+memory turn" || got[0].Parts[1].Text != "first real message" {
			t.Errorf("merged parts out of order or wrong: %+v", got[0].Parts)
		}
		if got[1].Role != "model" || got[1].Parts[0].Text != "assistant reply" {
			t.Errorf("expected trailing model turn untouched: %+v", got[1])
		}
	})

	t.Run("merges multiple separate runs and skips nil", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "u1"),
			text("user", "u2"),
			text("model", "m1"),
			nil,
			text("user", "u3"),
			text("user", "u4"),
			text("user", "u5"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 3 {
			t.Fatalf("expected 3 turns (merged, model, merged), got %d", len(got))
		}
		if len(got[0].Parts) != 2 {
			t.Errorf("expected first merged run to have 2 parts, got %d", len(got[0].Parts))
		}
		if got[1].Role != "model" {
			t.Errorf("expected middle turn to be model, got %s", got[1].Role)
		}
		if len(got[2].Parts) != 3 {
			t.Errorf("expected second merged run to have 3 parts, got %d", len(got[2].Parts))
		}
	})

	t.Run("valid function call and matching response are preserved", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "run tool"),
			funcCall("run_command", "call_123"),
			funcResp("run_command", "call_123"),
			text("model", "tool finished"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 4 {
			t.Fatalf("expected 4 turns, got %d", len(got))
		}
		if got[2].Parts[0].FunctionResponse == nil || got[2].Parts[0].FunctionResponse.ID != "call_123" {
			t.Errorf("expected matching function response preserved, got %+v", got[2])
		}
	})

	t.Run("dangling function response at start of session is dropped", func(t *testing.T) {
		// Simulates compaction boundary where model turn with call was pruned
		in := []*genai.Content{
			funcResp("get_scratchpad", "call_orphan"),
			text("user", "What is the capital of France?"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 1 {
			t.Fatalf("expected 1 turn (dangling turn dropped), got %d", len(got))
		}
		if got[0].Role != "user" || got[0].Parts[0].Text != "What is the capital of France?" {
			t.Errorf("expected clean user turn, got %+v", got[0])
		}
	})

	t.Run("dangling function response after model text turn is dropped", func(t *testing.T) {
		in := []*genai.Content{
			text("user", "hi"),
			text("model", "hello (no tool called)"),
			funcResp("run_command", "call_fake"),
			text("user", "how are you?"),
		}
		got := CleanSessionTurns(in)
		if len(got) != 3 {
			t.Fatalf("expected 3 turns (user, model, user), got %d", len(got))
		}
		if got[1].Role != "model" || got[1].Parts[0].Text != "hello (no tool called)" {
			t.Errorf("expected model turn untouched, got %+v", got[1])
		}
		if got[2].Role != "user" || got[2].Parts[0].Text != "how are you?" {
			t.Errorf("expected clean trailing user turn, got %+v", got[2])
		}
	})

	t.Run("mixed turn with text and dangling function response has only dangling part stripped", func(t *testing.T) {
		in := []*genai.Content{
			text("model", "no tools here"),
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: "Important note: do not drop this"},
					{
						FunctionResponse: &genai.FunctionResponse{
							Name: "phantom_tool",
							ID:   "call_phantom",
						},
					},
				},
			},
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if len(got[1].Parts) != 1 || got[1].Parts[0].Text != "Important note: do not drop this" {
			t.Errorf("expected text part kept and dangling response stripped, got %+v", got[1])
		}
	})

	t.Run("parallel function calls: matches valid, strips excess/unmatched responses", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role: "model",
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "tool_a", ID: "id_a"}},
					{FunctionCall: &genai.FunctionCall{Name: "tool_b", ID: "id_b"}},
				},
			},
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "tool_a", ID: "id_a"}},
					{FunctionResponse: &genai.FunctionResponse{Name: "tool_b", ID: "id_b"}},
					{FunctionResponse: &genai.FunctionResponse{Name: "tool_c", ID: "id_c"}}, // dangling
				},
			},
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if len(got[1].Parts) != 2 {
			t.Fatalf("expected 2 valid responses (tool_c stripped), got %d", len(got[1].Parts))
		}
		if got[1].Parts[0].FunctionResponse.Name != "tool_a" || got[1].Parts[1].FunctionResponse.Name != "tool_b" {
			t.Errorf("unexpected responses in turn 1: %+v", got[1].Parts)
		}
	})

	t.Run("parallel function calls matched by Name when IDs are omitted", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role: "model",
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "echo", Args: map[string]any{"msg": "1"}}},
					{FunctionCall: &genai.FunctionCall{Name: "echo", Args: map[string]any{"msg": "2"}}},
				},
			},
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "echo", Response: map[string]any{"out": "1"}}},
					{FunctionResponse: &genai.FunctionResponse{Name: "echo", Response: map[string]any{"out": "2"}}},
					{FunctionResponse: &genai.FunctionResponse{Name: "echo", Response: map[string]any{"out": "3"}}}, // excess/dangling
				},
			},
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if len(got[1].Parts) != 2 {
			t.Fatalf("expected 2 matched responses, 1 excess stripped, got %d", len(got[1].Parts))
		}
	})

	t.Run("propagates call ID to response with empty ID", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role: "model",
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "ls", ID: "call_abc123"}},
				},
			},
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "ls", ID: ""}}, // empty ID (e.g. from Gemini or manual edit)
				},
			},
		}
		got := CleanSessionTurns(in)
		if len(got) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(got))
		}
		if got[1].Parts[0].FunctionResponse.ID != "call_abc123" {
			t.Errorf("expected response ID to be populated with call ID 'call_abc123', got %q", got[1].Parts[0].FunctionResponse.ID)
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
