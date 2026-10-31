package agent

import (
	"context"
	"strings"
	"testing"

	adkAgent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

// TestCheckAndCompact_ForceMajorBypassesModulo pins the CLI's --force-major
// escape hatch (card compact-cli-add-force-major-flag-to-bypass-cycle-l): with
// the cycle counter parked where the major-compact-every schedule does not line
// up, the flag off runs a minor pass and the flag on runs a major, and the
// forced major books itself exactly like a scheduled one.
func TestCheckAndCompact_ForceMajorBypassesModulo(t *testing.T) {
	turns := []*genai.Content{
		genai.NewContentFromText("hi", "user"),
		genai.NewContentFromText("hello", "model"),
	}
	cfg := &RuntimeConfig{ContextWindow: 1000, Endpoint: "http://forcemajor.test", Model: "forcemajor-model"}
	// AppendOnly makes the minor pass observable as an append onto the old body.
	compactCfg := &CompactConfig{AppendOnly: true, CompactOverheadPct: 20, MajorCompactOverheadPct: 40, MajorCompactEvery: 5}

	// Cycle 38 means the next pass is cycle 39 and 39 % 5 = 4: the schedule does not
	// line up, and the memory is far under any size trigger - only the flag can make
	// this pass major. (The jess incident, on record in the card.)
	memory38 := `---
compact-cycle: 38
---

Existing notes.
`

	// Long enough to clear minMajorOutputChars and refusal-free: a valid major output.
	answer := `# Task Overview

Session consolidated; nothing further to report.`

	run := func(t *testing.T, memory string, forceMajor bool) (bool, string) {
		t.Helper()
		tempDir := t.TempDir()
		if err := WriteSessionTurns(tempDir, turns); err != nil {
			t.Fatalf("write session: %v", err)
		}
		if err := WriteMemoryFile(tempDir, memory); err != nil {
			t.Fatalf("write memory: %v", err)
		}
		stub := &fallbackStubModel{name: "stub", answer: answer}
		var denials int64
		ca, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
		if err != nil {
			t.Fatalf("build compaction agent: %v", err)
		}
		loader := func(c *RuntimeConfig) (adkAgent.Agent, *int64, error) {
			return ca, &denials, nil
		}
		compacted, err := CheckAndCompactSessionWithFallback(context.Background(), tempDir, cfg, loader, true, forceMajor, compactCfg)
		if err != nil {
			t.Fatalf("compact: %v", err)
		}
		mem, err := ReadMemoryFile(tempDir)
		if err != nil {
			t.Fatalf("read memory: %v", err)
		}
		return compacted, mem
	}

	t.Run("flag off: minor pass when the modulo does not line up", func(t *testing.T) {
		compacted, mem := run(t, memory38, false)
		if !compacted {
			t.Fatal("force=true must compact regardless of the schedule")
		}
		if !strings.Contains(mem, "compact-cycle: 39") {
			t.Errorf("cycle must advance to 39, got: %s", mem)
		}
		if strings.Contains(mem, "last-major-cycle") {
			t.Errorf("a minor pass must not stamp last-major-cycle, got: %s", mem)
		}
		if !strings.Contains(mem, "Existing notes.") {
			t.Errorf("a minor append must preserve the existing body, got: %s", mem)
		}
	})

	t.Run("flag on: major pass bypasses the modulo", func(t *testing.T) {
		compacted, mem := run(t, memory38, true)
		if !compacted {
			t.Fatal("force=true must compact regardless of the schedule")
		}
		if !strings.Contains(mem, "compact-cycle: 39") || !strings.Contains(mem, "last-major-cycle: 39") {
			t.Errorf("the forced major must stamp cycle 39 as both compact-cycle and last-major-cycle, got: %s", mem)
		}
		if strings.Contains(mem, "Existing notes.") {
			t.Errorf("a major pass rewrites the body wholesale, the old notes must be gone, got: %s", mem)
		}
		if !strings.Contains(mem, "Session consolidated; nothing further to report.") {
			t.Errorf("the major body must be the model consolidation, got: %s", mem)
		}
	})

	// The post-major guard keeps a scheduled pass minor the cycle after a major;
	// an explicit --force-major is an operator request and beats it.
	memoryAfterMajor := `---
compact-cycle: 40
last-major-cycle: 40
---

Body notes.
`
	t.Run("flag on: beats the post-major minor guard", func(t *testing.T) {
		compacted, mem := run(t, memoryAfterMajor, true)
		if !compacted {
			t.Fatal("force=true must compact regardless of the schedule")
		}
		if !strings.Contains(mem, "last-major-cycle: 41") {
			t.Errorf("a forced major right after a major must still run and stamp cycle 41, got: %s", mem)
		}
	})
}
