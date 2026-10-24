package agent

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// TestCheckAndCompact_MemoryPctIndependentTrigger pins the independent
// trigger: a memory file over major-compact-memory-pct compacts even when the
// session is far under its token budget, and the token gate still rejects a
// small session whose memory is small.
func TestCheckAndCompact_MemoryPctIndependentTrigger(t *testing.T) {
	turns := []*genai.Content{
		genai.NewContentFromText("hi", "user"),
		genai.NewContentFromText("hello", "model"),
	}
	cfg := &RuntimeConfig{ContextWindow: 1000, Endpoint: "http://trigger.test", Model: "trigger-model"}
	compactCfg := &CompactConfig{CompactOverheadPct: 20, MajorCompactOverheadPct: 40, MajorCompactMemoryPct: 10}

	tempDir := t.TempDir()
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	var mem strings.Builder
	for i := 0; i < 200; i++ {
		mem.WriteString("tokenword ")
	}
	if err := WriteMemoryFile(tempDir, mem.String()); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	// Long enough to clear the major-pass output-quality floor (minMajorOutputChars) - this
	// test exercises the memory-pct independent trigger, not output validation, so the canned
	// reply just needs to be realistic, not meaningful.
	stub := &fallbackStubModel{name: "stub", answer: "# Task Overview\n\nSession consolidated; nothing further to report."}
	var denials int64
	agent, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	compacted, err := CheckAndCompactSession(context.Background(), tempDir, cfg, agent, false, compactCfg, &denials)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !compacted {
		t.Fatal("memory over major-compact-memory-pct must compact a small session (independent trigger), got compacted=false")
	}

	// Control: same small session, small memory - the token gate rejects.
	tempDir2 := t.TempDir()
	if err := WriteSessionTurns(tempDir2, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir2, "small"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	stub2 := &fallbackStubModel{name: "stub2", answer: "- done"}
	var denials2 int64
	agent2, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub2, tempDir2, nil, &denials2)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	compacted2, err := CheckAndCompactSession(context.Background(), tempDir2, cfg, agent2, false, compactCfg, &denials2)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if compacted2 {
		t.Fatal("small session with small memory must not compact, got compacted=true")
	}
}

// TestCheckAndCompact_MajorCompactEveryIsNotATrigger pins the round-2
// regression fix: with major-compact-every satisfied (nextCycle % every == 0)
// but the session tiny and the memory small, no compaction fires - the
// every rule is a mode selector, not a session-independent trigger.
func TestCheckAndCompact_MajorCompactEveryIsNotATrigger(t *testing.T) {
	turns := []*genai.Content{
		genai.NewContentFromText("hi", "user"),
		genai.NewContentFromText("hello", "model"),
	}
	cfg := &RuntimeConfig{ContextWindow: 1000, Endpoint: "http://every.test", Model: "every-model"}
	compactCfg := &CompactConfig{CompactOverheadPct: 20, MajorCompactOverheadPct: 40, MajorCompactEvery: 4}

	tempDir := t.TempDir()
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	// Cycle 3 means the next pass is cycle 4, and 4 % 4 == 0: the every
	// rule is satisfied, which must not matter on a tiny session.
	if err := WriteMemoryFile(tempDir, "---\ncompact-cycle: 3\n---\n\nsmall memory\n"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	stub := &fallbackStubModel{name: "stub", answer: "- done"}
	var denials int64
	agent, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	compacted, err := CheckAndCompactSession(context.Background(), tempDir, cfg, agent, false, compactCfg, &denials)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if compacted {
		t.Fatal("major-compact-every must not fire a compaction on its own: the every rule is a mode selector, not a trigger")
	}
}

// TestMemoryExceedsPct_OnlySizeTriggers pins the trigger helper: the every-N
// rule never qualifies, the size rule does, and the cycling guard still
// blocks a major the cycle after a major.
func TestMemoryExceedsPct_OnlySizeTriggers(t *testing.T) {
	runtimeCfg := &RuntimeConfig{ContextWindow: 1000}
	cfg := &CompactConfig{MajorCompactEvery: 4, MajorCompactMemoryPct: 10}

	if memoryExceedsPct("---\ncompact-cycle: 3\n---\n\nsmall\n", cfg, runtimeCfg) {
		t.Error("every-N rule alone must not qualify as an independent trigger")
	}
	var mem strings.Builder
	for i := 0; i < 200; i++ {
		mem.WriteString("tokenword ")
	}
	if !memoryExceedsPct(mem.String(), cfg, runtimeCfg) {
		t.Error("memory over major-compact-memory-pct must qualify")
	}
	if memoryExceedsPct("---\ncompact-cycle: 3\nlast-major-cycle: 3\n---\n"+mem.String(), cfg, runtimeCfg) {
		t.Error("the cycling guard must block a major the cycle after a major")
	}
}
