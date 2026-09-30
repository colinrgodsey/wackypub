package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// Regression coverage for the Sept-29 audit finding F1 (notes/sept-29-wackypub-code-audit):
// tailMaxSeq used to back up at most 8KiB from EOF, so any legitimate final line larger
// than that (the persist layer allows MaxPersistTurnBytes = 512KiB) left a partial line,
// unmarshal failed, and the allocator fail-open'd to 0 - a fresh process then re-allocated
// seq 1 into a non-empty log.

// appendTurns appends smallCount small turns plus one final turn carrying bigText and
// returns the seq stamped on the final turn.
func appendTurns(t *testing.T, agentDir string, smallCount int, bigText string) int64 {
	t.Helper()
	for i := 1; i <= smallCount; i++ {
		if _, err := AppendSessionTurnGetSeq(agentDir, "user", fmt.Sprintf("fixture turn %d", i)); err != nil {
			t.Fatalf("append fixture turn %d: %v", i, err)
		}
	}
	seq, err := AppendSessionTurnGetSeq(agentDir, "user", bigText)
	if err != nil {
		t.Fatalf("append final turn: %v", err)
	}
	return seq
}

// TestTailMaxSeq_FinalLineSizes verifies the back-seek reaches the final line for every
// size between the old 8KiB window and the 512KiB persist bound.
func TestTailMaxSeq_FinalLineSizes(t *testing.T) {
	for _, size := range []int{8200, 100 * 1024, 500 * 1024} {
		agentDir := t.TempDir()
		finalSeq := appendTurns(t, agentDir, 3, strings.Repeat("y", size))
		if finalSeq != 4 {
			t.Fatalf("fixture: expected final seq 4, got %d", finalSeq)
		}
		got, err := tailMaxSeq(agentDir)
		if err != nil {
			t.Fatalf("tailMaxSeq with %dB final line: %v", size, err)
		}
		if got != finalSeq {
			t.Errorf("tailMaxSeq with %dB final line: want %d, got %d", size, finalSeq, got)
		}
	}
}

// TestTailMaxSeq_CorruptFinalLine_ScansLog: an unparseable final line must not reset the
// allocator to 0; the scan fallback recovers the max over the parseable lines - the same
// lines the read path keeps.
func TestTailMaxSeq_CorruptFinalLine_ScansLog(t *testing.T) {
	agentDir := t.TempDir()
	finalSeq := appendTurns(t, agentDir, 3, "normal small turn")
	if finalSeq != 4 {
		t.Fatalf("fixture: expected final seq 4, got %d", finalSeq)
	}
	f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append([]byte(strings.Repeat("x", 20*1024)), '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := tailMaxSeq(agentDir)
	if err != nil {
		t.Fatalf("tailMaxSeq over corrupt tail: %v", err)
	}
	if got != 4 {
		t.Errorf("tailMaxSeq over corrupt tail: want 4 (scan fallback), got %d", got)
	}
}

// TestTailMaxSeq_OverBoundFinalLine: a final line longer than the seek cap (and therefore
// never readable as a tail line) is decided by the scan - a parseable line keeps its
// stamped seq, an unparseable one is skipped. The production writer cannot create such a
// line (the persist cap is a hard invariant), so these simulate damage or pre-D101 writes.
func TestTailMaxSeq_OverBoundFinalLine(t *testing.T) {
	t.Run("parseable over-bound line keeps its stamped seq", func(t *testing.T) {
		agentDir := t.TempDir()
		if s := appendTurns(t, agentDir, 2, "small"); s != 3 {
			t.Fatalf("fixture: expected final seq 3, got %d", s)
		}
		big := genai.NewContentFromText(strings.Repeat("z", 540*1024), "user")
		line, err := json.Marshal(PersistedTurn{Content: *big, Seq: 3 + 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(line) <= tailMaxSeek {
			t.Fatalf("fixture line %dB does not exceed the seek cap %dB", len(line), tailMaxSeek)
		}
		f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(line, '\n'))
		f.Close()

		got, err := tailMaxSeq(agentDir)
		if err != nil {
			t.Fatalf("tailMaxSeq: %v", err)
		}
		if got != 4 {
			t.Errorf("want stamped seq 4 from scan, got %d", got)
		}
	})

	t.Run("garbage over-bound line is skipped by the scan", func(t *testing.T) {
		agentDir := t.TempDir()
		if s := appendTurns(t, agentDir, 2, "small"); s != 3 {
			t.Fatalf("fixture: expected final seq 3, got %d", s)
		}
		f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append([]byte(strings.Repeat("g", 540*1024)), '\n'))
		f.Close()

		got, err := tailMaxSeq(agentDir)
		if err != nil {
			t.Fatalf("tailMaxSeq: %v", err)
		}
		if got != 3 {
			t.Errorf("want 3 (garbage line skipped), got %d", got)
		}
	})
}

// TestSeqTailWorker is invoked as a subprocess by TestNextSeq_AfterLargeFinalLine_CrossProcess:
// a fresh process has an empty seqAllocBump, so the allocation must come entirely from the
// tail inference.
func TestSeqTailWorker(t *testing.T) {
	agentDir := os.Getenv("TEST_SEQ_TAIL_WORKER_AGENT_DIR")
	if agentDir == "" {
		return
	}
	seq, err := NextSeq(agentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("ALLOCATED_SEQ:%d\n", seq)
	os.Exit(0)
}

// TestNextSeq_AfterLargeFinalLine_CrossProcess is the F1 acceptance test: a 100KiB final
// line at seq N, then a FRESH process allocates N+1 (not 1). The old 8KiB window made
// this test fail with seq 1.
func TestNextSeq_AfterLargeFinalLine_CrossProcess(t *testing.T) {
	const turns = 20
	agentDir := t.TempDir()
	big := strings.Repeat("y", 100*1024)
	finalSeq := appendTurns(t, agentDir, turns-1, big)
	if finalSeq != int64(turns) {
		t.Fatalf("fixture: expected final seq %d, got %d", turns, finalSeq)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSeqTailWorker", "--")
	cmd.Env = append(os.Environ(), "TEST_SEQ_TAIL_WORKER_AGENT_DIR="+agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker failed: %v, output:\n%s", err, out)
	}
	var got int64
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "ALLOCATED_SEQ:") {
			got, err = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "ALLOCATED_SEQ:")), 10, 64)
			if err != nil {
				t.Fatalf("invalid worker output %q", line)
			}
		}
	}
	if got != int64(turns+1) {
		t.Fatalf("fresh process NextSeq after %dKiB final line at seq %d: want %d, got %d", 100, turns, turns+1, got)
	}
}
