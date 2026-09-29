package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNextSeqPerfLargeSession measures the inference path (seek-based tail read) on a large
// session.jsonl and reports the per-append allocation cost. The tail-read must seek, not
// scan: with a 100k-turn log the tail read is O(last line), not O(file).
func TestNextSeqPerfLargeSession(t *testing.T) {
	tempDir := t.TempDir()
	agentDir := filepath.Join(tempDir, "perf-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := buildLargeSession(agentDir, 100_000); err != nil {
		t.Fatalf("build large session: %v", err)
	}

	// tailMaxSeq alone on the 100k-turn log (must be O(1), not O(n)).
	const iters = 1000
	start := time.Now()
	for i := 0; i < iters; i++ {
		if _, err := tailMaxSeq(agentDir); err != nil {
			t.Fatalf("tail: %v", err)
		}
	}
	el := time.Since(start)
	t.Logf("tailMaxSeq alone (100k turns): %.2f us/op", float64(el.Microseconds())/float64(iters))

	// alloc+append path (the production unit).
	start = time.Now()
	for i := 0; i < iters; i++ {
		if _, err := AppendSessionTurnGetSeq(agentDir, "user", fmt.Sprintf("perf %d", i)); err != nil {
			t.Fatalf("alloc+append: %v", err)
		}
	}
	el = time.Since(start)
	t.Logf("alloc+append (100k turns): %.2f us/op", float64(el.Microseconds())/float64(iters))

	cur, err := CurrentSeq(agentDir)
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}
	if want := int64(100_000 + iters); cur != want {
		t.Fatalf("CurrentSeq = %d, want %d", cur, want)
	}
}

func buildLargeSession(agentDir string, n int) error {
	f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	for i := 1; i <= n; i++ {
		if _, err := fmt.Fprintf(f, "{\"role\":\"user\",\"seq\":%d,\"parts\":[{\"text\":\"turn %d\"}]}\n", i, i); err != nil {
			return err
		}
	}
	return nil
}
