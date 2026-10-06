package agent

import (
	"bytes"
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

	expectedData, err := json.Marshal(&PersistedTurn{Content: genai.Content{Role: "user", Parts: content.Parts}, Seq: 1})
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

// TestPersistedTurnLoadsLegacySessionJSONL pins the reason genai.Content is embedded
// rather than mirrored: a session file written before sequence numbers existed must
// load unchanged, in both directions.
func TestPersistedTurnLoadsLegacySessionJSONL(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"role":"user","parts":[{"text":"written before seq existed"}]}
`
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(legacy), 0644); err != nil {
		t.Fatalf("write legacy session: %v", err)
	}

	turns, err := ReadPersistedTurns(dir)
	if err != nil {
		t.Fatalf("ReadPersistedTurns on legacy file: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 legacy turn, got %d", len(turns))
	}
	if turns[0].Role != "user" || ContentText(&turns[0].Content) != "written before seq existed" {
		t.Fatalf("legacy turn lost data: role=%q text=%q", turns[0].Role, ContentText(&turns[0].Content))
	}
	if turns[0].Seq != 0 {
		t.Fatalf("legacy turn must carry no seq, got %d", turns[0].Seq)
	}

	// The embedded half must be a genai.Content, not merely shaped like one.
	var asContent genai.Content
	if err := json.Unmarshal([]byte(legacy), &asContent); err != nil {
		t.Fatalf("legacy line into genai.Content: %v", err)
	}
	if asContent.Role != turns[0].Role || ContentText(&asContent) != ContentText(&turns[0].Content) {
		t.Fatal("genai.Content and PersistedTurn disagree on the same bytes")
	}

	// And a seq-bearing turn must still decode as a plain genai.Content, so readers
	// that know nothing about seq keep working against a seq-bearing file.
	withSeq, err := json.Marshal(PersistedTurn{Content: genai.Content{Role: "model", Parts: []*genai.Part{{Text: "after"}}}, Seq: 42})
	if err != nil {
		t.Fatalf("marshal seq turn: %v", err)
	}
	var round genai.Content
	if err := json.Unmarshal(withSeq, &round); err != nil {
		t.Fatalf("seq turn into genai.Content: %v", err)
	}
	if round.Role != "model" || ContentText(&round) != "after" {
		t.Fatalf("seq turn lost data when read as genai.Content: %+v", round)
	}
}

// TestWritePersistedTurns_AtomicAbortOnMarshalFailure is F4 from
// notes/sept-29-wackypub-code-audit: a turn that fails to marshal must ABORT the
// rewrite (error) and leave the original session.jsonl byte-intact - not skip the
// turn silently (old behavior) and not truncate history in place.
func TestWritePersistedTurns_AtomicAbortOnMarshalFailure(t *testing.T) {
	agentDir := t.TempDir()
	orig := []PersistedTurn{
		{Content: genai.Content{Role: "user", Parts: []*genai.Part{{Text: "keep me"}}}, Seq: 1},
		{Content: genai.Content{Role: "model", Parts: []*genai.Part{{Text: "original answer"}}}, Seq: 2},
	}
	if err := WritePersistedTurns(agentDir, orig); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	// A Content whose Part carries only an unsupported function field still marshals as
	// JSON (genai.Part is a struct), so to force a genuine marshal error we pass a turn
	// whose Content contains a non-marshalable value via InlineData with invalid bytes.
	// InlineData.Data is []byte, which always marshals; instead use a direct marshal
	// failure by inserting a turn that is fine but rely on the error path being tested
	// via a nil-invalid content: a Content with a Part that has DataBytes nil and
	// FunctionCall non-nil still marshals. The reliable way to force json.Marshal to
	// error is a cycle or a chan - neither sits in PersistedTurn. So this test instead
	// verifies the ABORT property by pointing the temp writer at a path that fails:
	// covered by TestWritePersistedTurns_TempFileFailure below. Here we assert the
	// marshal-error branch returns an error when given a turn whose Part has a
	// FunctionResponse with an invalid (non-JSON-serializable) Response value.
	badTurn := []PersistedTurn{
		{Content: genai.Content{Role: "user", Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     "x",
				Response: map[string]interface{}{"bad": make(chan int)},
			},
		}}}, Seq: 3},
	}
	err = WritePersistedTurns(agentDir, badTurn)
	if err == nil {
		t.Fatal("expected marshal error from pathological turn")
	}
	if !strings.Contains(err.Error(), "failed to marshal turn") {
		t.Errorf("expected marshal-abort error, got: %v", err)
	}

	// Original history must be byte-identical (no truncation, no partial write).
	after, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read after abort: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("session.jsonl changed after aborted rewrite")
		t.Logf("before=%q after=%q", before, after)
	}
	// No temp file may remain.
	leftovers, _ := filepath.Glob(filepath.Join(agentDir, ".session.jsonl.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left after abort: %v", leftovers)
	}
}

// TestWritePersistedTurns_TempFileFailure verifies that when the temp file cannot be
// created the rewrite fails and the original session.jsonl is untouched (crash/disk-full
// simulation: no temp slot available, or the rename target is protected).
func TestWritePersistedTurns_TempFileFailure(t *testing.T) {
	agentDir := t.TempDir()
	orig := []PersistedTurn{
		{Content: genai.Content{Role: "user", Parts: []*genai.Part{{Text: "still here"}}}, Seq: 1},
	}
	if err := WritePersistedTurns(agentDir, orig); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(agentDir, SessionFileName))

	// Make the tmp creation fail: agent dir becomes read-only.
	if err := os.Chmod(agentDir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(agentDir, 0755)
	err := WritePersistedTurns(agentDir, orig)
	if err == nil {
		t.Fatal("expected error when tmp cannot be created")
	}
	os.Chmod(agentDir, 0755)
	after, _ := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if !bytes.Equal(before, after) {
		t.Errorf("session.jsonl changed when tmp creation failed")
	}
}

// TestWritePersistedTurns_RenameFailure keeps the original intact when os.Rename fails
// (e.g. target is a non-empty directory or FS error) - the old in-place os.Create would
// have zeroed the file already.
func TestWritePersistedTurns_RenameFailure(t *testing.T) {
	agentDir := t.TempDir()
	orig := []PersistedTurn{{Content: genai.Content{Role: "user", Parts: []*genai.Part{{Text: "keep"}}}, Seq: 1}}
	if err := WritePersistedTurns(agentDir, orig); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	// Replace session.jsonl with a DIRECTORY so rename over it fails (ENOTDIR/EEXIST).
	os.Remove(filepath.Join(agentDir, SessionFileName))
	if err := os.Mkdir(filepath.Join(agentDir, SessionFileName), 0755); err != nil {
		t.Fatalf("mkdir over session.jsonl: %v", err)
	}
	defer os.RemoveAll(filepath.Join(agentDir, SessionFileName))
	err := WritePersistedTurns(agentDir, orig)
	if err == nil {
		t.Fatal("expected rename error over a directory")
	}
	// The ORIGINAL data is gone (directory replaced it before the test) - this asserts
	// the failure surfaces rather than silently corrupting; the atomicity guarantee is
	// that a FAILED rewrite never truncates history, which the other two tests cover.
	if _, statErr := os.Stat(filepath.Join(agentDir, SessionFileName)); statErr == nil {
		fi, _ := os.Stat(filepath.Join(agentDir, SessionFileName))
		if fi.IsDir() {
			t.Log("rename failed; original not truncated by the writer (dir remains)")
		}
	}
}
