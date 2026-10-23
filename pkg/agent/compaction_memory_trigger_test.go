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
