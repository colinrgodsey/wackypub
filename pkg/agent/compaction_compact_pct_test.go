package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// The CLI's --compact-pct reaches compaction as a CompactConfig built from the wire override,
// so these tests pin what that does to a real pass: how much of the session it archives, that
// COMPACT.md is never written, that it stays orthogonal to --force-major, and that a one-off
// override cannot silently drop the agent's major-pass schedule (which the wire message has no
// field for).

// pctovStoredMD keeps every overridable field distinct from the zero value: pct 20, append-only
// on, a 20% overhead reserve, and a major schedule that does not line up at cycle 39.
const pctovStoredMD = "---\nappend-only: true\ncompact-pct: 20\ncompact-overhead-pct: 20\nmajor-compact-every: 5\nmajor-compact-memory-pct: 25\nmajor-compact-overhead-pct: 40\n---\n\nStored directive.\n"

// pctovNoScheduleMD is the control for schedule inheritance: a document that asks for no major
// passes at all, so a major appearing under it means something else supplied the schedule.
const pctovNoScheduleMD = "---\nappend-only: true\ncompact-pct: 20\ncompact-overhead-pct: 20\n---\n\nStored directive.\n"

// pctovMemory38 parks the cycle counter where the every-5 schedule never lands (39 % 5 = 4), so
// a major can only come from the flag - the same shape as the --force-major test.
const pctovMemory38 = "---\ncompact-cycle: 38\n---\n\nExisting notes.\n"

// pctovMemory34 is the opposite: the next cycle is 35, which is on the every-5 schedule.
const pctovMemory34 = "---\ncompact-cycle: 34\n---\n\nExisting notes.\n"

const pctovMajorAnswer = "# Task Overview\n\nSession consolidated into a full rewrite of the notes."

func pctovTurns() []*genai.Content {
	filler := "aaaa bbbb cccc dddd eeee ffff gggg hhhh iiii jjjj kkkk llll mmmm nnnn oooo pppp\n"
	turns := make([]*genai.Content, 0, 6)
	for i := 0; i < 3; i++ {
		turns = append(turns,
			genai.NewContentFromText(filler+"user turn", "user"),
			genai.NewContentFromText(filler+"model turn", "model"),
		)
	}
	return turns
}

// pctovWire builds the CompactConfig the SDK hands the engine for a one-off --compact-pct N
// invocation, through the same wire boundary production uses.
func pctovWire(agentDir string, pct float64) *CompactConfig {
	return compactConfigFromWire(&agentv1.CompactConfigOverride{
		AppendOnly:         true,
		CompactPct:         pct,
		CompactOverheadPct: 20,
		CompactionNotice:   defaultCompactionNotice,
		Prompt:             "Stored directive.",
	}, agentDir)
}

// pctovRun compacts a fresh six-turn session and reports the surviving turn count, the
// resulting MEMORY.md, and the COMPACT.md still on disk.
func pctovRun(t *testing.T, storedMD, memory string, build func(dir string) *CompactConfig, forceMajor bool) (int, string, string) {
	t.Helper()
	tempDir := t.TempDir()
	if storedMD != "" {
		if err := os.WriteFile(filepath.Join(tempDir, "COMPACT.md"), []byte(storedMD), 0644); err != nil {
			t.Fatalf("write COMPACT.md: %v", err)
		}
	}
	if err := WriteSessionTurns(tempDir, pctovTurns()); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, memory); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	cfg := &RuntimeConfig{ContextWindow: 10000, Endpoint: "http://pctov.test", Model: "pctov-model"}
	stub := &fallbackStubModel{name: "stub", answer: pctovMajorAnswer}
	var denials int64
	ca, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	loader := func(c *RuntimeConfig) (adkAgent.Agent, *int64, error) { return ca, &denials, nil }

	var override *CompactConfig
	if build != nil {
		override = build(tempDir)
	}
	compacted, err := CheckAndCompactSessionWithFallback(context.Background(), tempDir, cfg, loader, true, forceMajor, override)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !compacted {
		t.Fatal("force=true must compact")
	}
	left, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	mem, err := ReadMemoryFile(tempDir)
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(tempDir, "COMPACT.md"))
	if err == nil {
		return len(left), mem, string(onDisk)
	}
	return len(left), mem, ""
}

// TestCompactPctOverrideControlsHowMuchIsArchived pins the knob doing its job: a bigger
// percentage archives strictly more of the session, 100 archives all of it, and with no
// override at all the stored compact-pct is what governs.
func TestCompactPctOverrideControlsHowMuchIsArchived(t *testing.T) {
	_, _, diskAfter := pctovRun(t, pctovStoredMD, pctovMemory38, func(d string) *CompactConfig { return pctovWire(d, 100) }, false)
	if diskAfter != pctovStoredMD {
		t.Errorf("a one-off override must never be written back to COMPACT.md; stored document changed:\n%s", diskAfter)
	}

	full, _, _ := pctovRun(t, pctovStoredMD, pctovMemory38, func(d string) *CompactConfig { return pctovWire(d, 100) }, false)
	quarter, _, _ := pctovRun(t, pctovStoredMD, pctovMemory38, func(d string) *CompactConfig { return pctovWire(d, 25) }, false)
	stored, _, _ := pctovRun(t, pctovStoredMD, pctovMemory38, nil, false)

	if full >= quarter {
		t.Errorf("pct 100 must archive more than pct 25 (survivors %d vs %d)", full, quarter)
	}
	if quarter <= 1 {
		t.Errorf("pct 25 must leave most of the session in place, only %d turn(s) survived", quarter)
	}
	if stored < quarter {
		t.Errorf("with no override the stored pct 20 must archive no more than an explicit 25 (survivors %d vs %d)", stored, quarter)
	}
}

// TestCompactPctOverrideIsOrthogonalToForceMajor pins the documented pairing: the percentage
// decides how much history the pass consumes and --force-major decides the pass type, so the
// flag must still be the only thing making a pass major, at any percentage.
func TestCompactPctOverrideIsOrthogonalToForceMajor(t *testing.T) {
	survivors, memMinor, _ := pctovRun(t, pctovStoredMD, pctovMemory38, func(d string) *CompactConfig { return pctovWire(d, 100) }, false)
	if strings.Contains(memMinor, "last-major-cycle") {
		t.Errorf("cycle 39 is off the every-5 schedule, so without the flag the pass must stay minor: %s", memMinor)
	}
	if survivors > 1 {
		t.Errorf("pct 100 must archive the whole session in a minor pass too, %d turn(s) survived", survivors)
	}

	_, memMajor, _ := pctovRun(t, pctovStoredMD, pctovMemory38, func(d string) *CompactConfig { return pctovWire(d, 100) }, true)
	if !strings.Contains(memMajor, "last-major-cycle: 39") {
		t.Errorf("the flag must make it a major and book it as one, got: %s", memMajor)
	}
	if strings.Contains(memMajor, "Existing notes.") {
		t.Errorf("a major pass rewrites MEMORY.md wholesale, got: %s", memMajor)
	}

	smallMajor, memSmall, _ := pctovRun(t, pctovStoredMD, pctovMemory38, func(d string) *CompactConfig { return pctovWire(d, 25) }, true)
	if !strings.Contains(memSmall, "last-major-cycle: 39") {
		t.Errorf("a small percentage must not change the pass type, got: %s", memSmall)
	}
	if smallMajor <= 1 {
		t.Errorf("pct 25 must still leave most of the session in place, only %d turn(s) survived", smallMajor)
	}
}

// TestCompactConfigFromWireSeedsTheMajorSchedule pins the boundary rule where it is defined: the
// five fields the message carries pass through exactly as sent, including zero values, and the
// three schedule fields the message cannot carry come from the agent's own COMPACT.md.
func TestCompactConfigFromWireSeedsTheMajorSchedule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "COMPACT.md"), []byte(pctovStoredMD), 0644); err != nil {
		t.Fatalf("write COMPACT.md: %v", err)
	}

	cfg := compactConfigFromWire(&agentv1.CompactConfigOverride{AppendOnly: false, CompactPct: 100, CompactOverheadPct: 5}, dir)

	if cfg.AppendOnly {
		t.Error("append-only false from the wire must win over the stored true")
	}
	if cfg.CompactPct != 100 || cfg.CompactOverheadPct != 5 {
		t.Errorf("wire percentages must pass through, got pct=%v overhead=%v", cfg.CompactPct, cfg.CompactOverheadPct)
	}
	if cfg.MajorCompactEvery != 5 || cfg.MajorCompactMemoryPct != 25 || cfg.MajorCompactOverheadPct != 40 {
		t.Errorf("the schedule the wire cannot carry must come from disk, got every=%v memoryPct=%v overheadPct=%v",
			cfg.MajorCompactEvery, cfg.MajorCompactMemoryPct, cfg.MajorCompactOverheadPct)
	}

	// An agent with no COMPACT.md gets the embedded default's schedule, not a zeroed one.
	empty := t.TempDir()
	restore := DefaultCompactMDHybrid
	DefaultCompactMDHybrid = "---\nappend-only: true\ncompact-pct: 50\nmajor-compact-every: 7\n---\n\nBundled directive."
	t.Cleanup(func() { DefaultCompactMDHybrid = restore })
	if got := compactConfigFromWire(&agentv1.CompactConfigOverride{CompactPct: 10}, empty).MajorCompactEvery; got != 7 {
		t.Errorf("with no COMPACT.md the embedded default schedule must apply, got every=%v", got)
	}
}

// TestCompactWireOverrideKeepsTheScheduledMajor is the regression this card exists to prevent:
// a one-off --compact-pct invocation at a cycle that is on the agent's every-5 schedule must
// still run the scheduled major. The control case (a document with no schedule) proves the major
// above comes from the stored schedule rather than from the override path.
func TestCompactWireOverrideKeepsTheScheduledMajor(t *testing.T) {
	_, memInherit, _ := pctovRun(t, pctovStoredMD, pctovMemory34, func(d string) *CompactConfig { return pctovWire(d, 50) }, false)
	if !strings.Contains(memInherit, "last-major-cycle: 35") {
		t.Errorf("cycle 35 is on the stored every-5 schedule, so the override must inherit it and run a major, got: %s", memInherit)
	}

	_, memControl, _ := pctovRun(t, pctovNoScheduleMD, pctovMemory34, func(d string) *CompactConfig { return pctovWire(d, 50) }, false)
	if strings.Contains(memControl, "last-major-cycle") {
		t.Errorf("a document with no major schedule must keep the pass minor, so the major above came from the stored schedule, got: %s", memControl)
	}
}
