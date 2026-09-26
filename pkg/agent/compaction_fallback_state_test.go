package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkAgent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

// TestCompactionFallback_FailedUntilResetSkipped pins that a backend a prior turn's 429
// marked failed-until-reset is SKIPPED for compaction: the primary is never called again
// and the fallback produces the summary.
func TestCompactionFallback_FailedUntilResetSkipped(t *testing.T) {
	primary := &fallbackStubModel{name: "primary", fail: true}
	fallback := &fallbackStubModel{name: "fallback", answer: "compacted while primary skipped"}

	// Build the same fixture but mark the primary failed-until BEFORE running.
	primaryCfg := &RuntimeConfig{
		ContextWindow: 100000,
		Endpoint:      "http://primary.test",
		Model:         "primary-model",
		Fallback: &RuntimeConfig{
			ContextWindow: 100000,
			Endpoint:      "http://fallback.test",
			Model:         "fallback-model",
		},
	}
	markBackendFailedUntil(primaryCfg, time.Now().Add(time.Hour))

	mem, compacted, primaryCalls, fallbackCalls, err := compactionFallbackFixture(t, primary, fallback)
	if err != nil {
		t.Fatalf("compaction with skipped primary errored: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to succeed via fallback when primary is skipped")
	}
	if primaryCalls != 0 {
		t.Fatalf("failed-until-reset primary should NOT be called, got %d calls", primaryCalls)
	}
	if fallbackCalls == 0 {
		t.Fatal("fallback should have served the compaction")
	}
	if !strings.Contains(mem, "compacted while primary skipped") {
		t.Fatalf("memory should contain fallback summary, got %q", mem)
	}

	// Global state cleanup: this test shares the package-level failed-until map with the
	// other fallback tests, so unmark before returning.
	backendFailedUntilMu.Lock()
	delete(backendFailedUntil, backendIdentity(primaryCfg))
	backendFailedUntilMu.Unlock()
}

// TestCompactionFallback_HookNamesServingLevel pins acceptance item: the post-compact hook
// payload's compaction_model names the level that actually served (the fallback model),
// not the primary.
func TestCompactionFallback_HookNamesServingLevel(t *testing.T) {
	prev := postCompactHookDone
	defer func() {
		postCompactHookDoneMu.Lock()
		postCompactHookDone = prev
		postCompactHookDoneMu.Unlock()
	}()

	tempDir := t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
		genai.NewContentFromText("user two", "user"),
		genai.NewContentFromText("model two", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "Initial Memory"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// Capture the hook script output like compaction_hooks_test does.
	capture := filepath.Join(tempDir, "captured-post.json")
	writeCompactionHookScript(t, tempDir, EventPostCompact, "00-capture", captureHookScript(capture))

	done := make(chan struct{})
	postCompactHookDoneMu.Lock()
	postCompactHookDone = func(dir string) {
		if dir == tempDir {
			close(done)
		}
	}
	postCompactHookDoneMu.Unlock()

	primary := &fallbackStubModel{name: "primary", fail: true}
	fallback := &fallbackStubModel{name: "fallback", answer: "compacted by fallback"}

	primaryCfg := &RuntimeConfig{
		ContextWindow: 100000,
		Endpoint:      "http://primary.test",
		Model:         "primary-model",
		Fallback: &RuntimeConfig{
			ContextWindow: 100000,
			Endpoint:      "http://fallback.test",
			Model:         "fallback-model",
		},
	}
	var pd, fd int64
	primaryAgent, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, primaryCfg, primary, tempDir, nil, &pd)
	if err != nil {
		t.Fatalf("build primary: %v", err)
	}
	loader := func(cfg *RuntimeConfig) (adkAgent.Agent, *int64, error) {
		if cfg == primaryCfg {
			return primaryAgent, &pd, nil
		}
		var d int64
		ag, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, fallback, tempDir, nil, &d)
		if err != nil {
			return nil, nil, err
		}
		return ag, &d, nil
	}

	compacted, err := CheckAndCompactSessionWithFallback(t.Context(), tempDir, primaryCfg, loader, true, nil)
	if err != nil {
		t.Fatalf("compaction errored: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to succeed")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-compact hook did not finish")
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var post PostCompactPayload
	if err := json.Unmarshal(data, &post); err != nil {
		t.Fatalf("payload parse: %v\nraw: %s", err, data)
	}
	if post.CompactionModel != "fallback-model" {
		t.Fatalf("compaction_model = %q, want %q (the serving fallback level)", post.CompactionModel, "fallback-model")
	}
	_ = atomic.LoadInt64(&fd)
}
