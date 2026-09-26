package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	adkAgent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

// TestCompactionFallback_NilRuntimeConfig_NoPanicAndNoDeadlock pins Finding 2 from agy's
// review: CheckAndCompactSession accepts a nil RuntimeConfig (pre-existing contract), which
// made backendFailedUntilReset lock the package mutex and THEN dereference cfg.Endpoint -
// a panic with no deferred unlock, permanently deadlocking backendFailedUntilMu for the
// process. The nil path must return cleanly AND leave the failed-until machinery usable.
func TestCompactionFallback_NilRuntimeConfig_NoPanicAndNoDeadlock(t *testing.T) {
	tempDir := t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "Initial Memory"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// A healthy stub model so the nil-cfg path reaches the runner (exercising
	// backendIdentity(nil) via the skip check) rather than failing earlier for other reasons.
	stub := &fallbackStubModel{name: "nilcfg", answer: "nil cfg summary"}
	var denials int64
	ag, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, nil, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build agent: %v", err)
	}
	loader := func(cfg *RuntimeConfig) (adkAgent.Agent, *int64, error) { return ag, &denials, nil }

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = CheckAndCompactSessionWithFallback(context.Background(), tempDir, nil, loader, true, nil)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CheckAndCompactSessionWithFallback with nil runtimeCfg hung (mutex deadlock?)")
	}

	// The mutex must still be usable: mark + reset on a real cfg prove no permanent deadlock.
	cfg := &RuntimeConfig{Endpoint: "http://post-nil.test", Model: "m"}
	markSeen := make(chan struct{})
	go func() {
		defer close(markSeen)
		markBackendFailedUntil(cfg, time.Now().Add(time.Hour))
		if !backendFailedUntilReset(cfg) {
			t.Error("backendFailedUntilReset should observe the mark just placed")
		}
	}()
	select {
	case <-markSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("failed-until machinery deadlocked after nil-cfg compaction")
	}

	mem, _ := ReadMemoryFile(tempDir)
	if !strings.Contains(mem, "nil cfg summary") {
		t.Fatalf("nil-cfg compaction should still succeed via the wrapper's single level, got %q", mem)
	}
}

// resetBackendFailedUntilForTest clears the package-level failed-until map so tests do not
// leak skip state into one another (agy Finding 4 hardening).
func resetBackendFailedUntilForTest() {
	backendFailedUntilMu.Lock()
	defer backendFailedUntilMu.Unlock()
	backendFailedUntil = make(map[string]time.Time)
}
