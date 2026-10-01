package agent

import (
	"fmt"
	"testing"
)

// Regression coverage for the Sept-29 audit finding F8 (notes/sept-29-wackypub-code-audit):
// the package-level seqAllocBump map grew one entry per agent dir, never pruned. The
// bound is two-part: consume-time eviction (an entry the log has caught up to is
// redundant) plus a hard cap with reset (backstop for unbounded distinct dirs).

// TestSeqAllocBumpEvictedWhenLogCatchesUp pins the consume-time eviction: after the
// log catches up to a recorded bump, CurrentSeq must return the file value AND drop
// the redundant entry, without losing in-process monotonicity.
func TestSeqAllocBumpEvictedWhenLogCatchesUp(t *testing.T) {
	agentDir := t.TempDir()
	key := cleanLockDir(agentDir)
	if _, ok := seqAllocBump[key]; ok {
		t.Fatalf("fixture: fresh dir must not have a bump entry")
	}

	n1, err := NextSeq(agentDir)
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("first allocation = %d, want 1", n1)
	}
	if got := seqAllocBump[key]; got != 1 {
		t.Fatalf("fixture: bump after first allocation = %d, want 1", got)
	}

	// Append a turn: the log now carries seq 2 and catches up to the bump.
	seq2, err := AppendSessionTurnGetSeq(agentDir, "user", "catch-up turn")
	if err != nil {
		t.Fatalf("AppendSessionTurnGetSeq: %v", err)
	}
	if seq2 != 2 {
		t.Fatalf("append seq = %d, want 2", seq2)
	}

	cur, err := CurrentSeq(agentDir)
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}
	if cur != 2 {
		t.Errorf("CurrentSeq = %d, want 2 (the log value)", cur)
	}
	if _, ok := seqAllocBump[key]; ok {
		t.Errorf("bump entry not evicted after the log caught up")
	}

	// Eviction must not lose the bridge: the next allocation still comes after the log.
	n3, err := NextSeq(agentDir)
	if err != nil {
		t.Fatalf("NextSeq after eviction: %v", err)
	}
	if n3 != 3 {
		t.Errorf("allocation after eviction = %d, want 3", n3)
	}
}

// TestSeqAllocBumpCapResets pins the F8 backstop: at the cap, a new allocation resets
// the map to just the allocating dir. Cleared dirs re-derive from their logs like
// fresh processes - the documented tradeoff.
func TestSeqAllocBumpCapResets(t *testing.T) {
	prev := seqAllocBump
	t.Cleanup(func() { seqAllocBump = prev })
	seqAllocBump = make(map[string]int64, seqAllocBumpMax)
	for i := 0; i < seqAllocBumpMax; i++ {
		seqAllocBump[fmt.Sprintf("dummy-%04d", i)] = 1
	}

	agentDir := t.TempDir()
	n, err := NextSeq(agentDir)
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if n != 1 {
		t.Fatalf("first allocation = %d, want 1", n)
	}
	if got := len(seqAllocBump); got != 1 {
		t.Fatalf("map not reset to the allocating dir, len = %d, want 1", got)
	}
	if got := seqAllocBump[cleanLockDir(agentDir)]; got != n {
		t.Errorf("allocating dir bump = %d, want %d", got, n)
	}
}
