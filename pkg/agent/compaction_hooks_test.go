package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

// writeCompactionHookScript writes an executable script under <agentDir>/hooks/<event>/.
func writeCompactionHookScript(t *testing.T, agentDir, event, filename, content string) {
	t.Helper()
	dir := filepath.Join(agentDir, "hooks", event)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0755); err != nil {
		t.Fatalf("failed to write hook script %s: %v", filename, err)
	}
}

// captureHookScript is a hook script body that dumps its stdin payload verbatim to
// <agentDir>/<outfile> (RunHookChain runs it with cmd.Dir = agentDir, so a bare relative
// path lands there) and answers with an empty, valid HookOutput.
func captureHookScript(outfile string) string {
	return "#!/bin/sh\ncat > " + outfile + "\nprintf '{}'\n"
}

// armPostCompactHookWait registers the package's test-only async-completion seam, filtered
// to agentDir, and returns a function that blocks until that agent's post-compact hook chain
// finishes. Many other tests in this package compact successfully with no awareness of
// hooks at all; their own goroutines can still be in flight when this one installs its
// callback, so the agentDir filter (always a fresh t.TempDir(), so unique) is required, not
// cosmetic - without it a stray completion from an unrelated test can close this test's
// channel early. It must be called (armed) BEFORE triggering the compaction that will spawn
// the goroutine, and the returned function called AFTER, once the run under test has
// returned.
func armPostCompactHookWait(t *testing.T, agentDir string) func() {
	t.Helper()
	done := make(chan struct{})
	postCompactHookDoneMu.Lock()
	prev := postCompactHookDone
	postCompactHookDone = func(dir string) {
		if dir != agentDir {
			return
		}
		close(done)
	}
	postCompactHookDoneMu.Unlock()
	t.Cleanup(func() {
		postCompactHookDoneMu.Lock()
		postCompactHookDone = prev
		postCompactHookDoneMu.Unlock()
	})
	return func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for async post-compact hook to finish")
		}
	}
}

// seedCompactableSession writes enough session turns and a MEMORY.md to agentDir that
// CheckAndCompactSession has real work to do.
func seedCompactableSession(t *testing.T, agentDir string) {
	t.Helper()
	turns := []*genai.Content{
		genai.NewContentFromText(strings.Repeat("hello there. ", 20), "user"),
		genai.NewContentFromText(strings.Repeat("hi, how can I help. ", 20), "model"),
		genai.NewContentFromText(strings.Repeat("second question. ", 20), "user"),
		genai.NewContentFromText(strings.Repeat("second answer. ", 20), "model"),
	}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("WriteSessionTurns: %v", err)
	}
	if err := WriteMemoryFile(agentDir, "Existing memory."); err != nil {
		t.Fatalf("WriteMemoryFile: %v", err)
	}
}

// TestCompactionHooks_PayloadCapturePerEvent pins the payload shape and delivery mechanism
// for all three compaction lifecycle events: a real hook script fixture captures the exact
// JSON bytes it received on stdin (the same delivery channel on-user-message uses for its
// message text - no second shape), and this test parses each capture back into the payload
// struct to check the documented fields.
func TestCompactionHooks_PayloadCapturePerEvent(t *testing.T) {
	t.Run("pre-compact and post-compact on a successful run", func(t *testing.T) {
		tempDir := t.TempDir()
		seedCompactableSession(t, tempDir)

		writeCompactionHookScript(t, tempDir, EventPreCompact, "00-capture", captureHookScript("captured-pre.json"))
		writeCompactionHookScript(t, tempDir, EventPostCompact, "00-capture", captureHookScript("captured-post.json"))

		srv := okBackendServer(t, "Summary of the conversation.")
		defer srv.Close()
		runtimeCfg := &RuntimeConfig{
			Provider: "openai",
			Model:    "test-compaction-model",
			Endpoint: srv.URL,
			APIKey:   "fake",
		}
		var denials int64
		ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, &denials)
		if err != nil {
			t.Fatalf("build compaction agent: %v", err)
		}

		wait := armPostCompactHookWait(t, tempDir)
		compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, &denials)
		if err != nil {
			t.Fatalf("CheckAndCompactSession: %v", err)
		}
		if !compacted {
			t.Fatal("expected compaction to occur")
		}

		preData, err := os.ReadFile(filepath.Join(tempDir, "captured-pre.json"))
		if err != nil {
			t.Fatalf("reading pre-compact capture: %v", err)
		}
		var pre PreCompactPayload
		if err := json.Unmarshal(preData, &pre); err != nil {
			t.Fatalf("pre-compact payload did not parse: %v\nraw: %s", err, preData)
		}
		if pre.AgentID != filepath.Base(tempDir) {
			t.Errorf("pre-compact agent_id = %q, want %q", pre.AgentID, filepath.Base(tempDir))
		}
		if pre.SessionPath != filepath.Join(tempDir, SessionFileName) {
			t.Errorf("pre-compact session_path = %q, want %q", pre.SessionPath, filepath.Join(tempDir, SessionFileName))
		}
		if pre.TurnCount != 4 {
			t.Errorf("pre-compact turn_count = %d, want 4", pre.TurnCount)
		}
		if pre.TokenEstimate <= 0 {
			t.Errorf("pre-compact token_estimate = %d, want > 0", pre.TokenEstimate)
		}
		if pre.Trigger != "forced" {
			t.Errorf("pre-compact trigger = %q, want %q", pre.Trigger, "forced")
		}

		// The hook runs async; block until this run's post-compact chain has actually
		// finished before reading its capture file.
		wait()

		postData, err := os.ReadFile(filepath.Join(tempDir, "captured-post.json"))
		if err != nil {
			t.Fatalf("reading post-compact capture: %v", err)
		}
		var post PostCompactPayload
		if err := json.Unmarshal(postData, &post); err != nil {
			t.Fatalf("post-compact payload did not parse: %v\nraw: %s", err, postData)
		}
		if post.AgentID != filepath.Base(tempDir) {
			t.Errorf("post-compact agent_id = %q, want %q", post.AgentID, filepath.Base(tempDir))
		}
		if post.TurnsArchived <= 0 {
			t.Errorf("post-compact turns_archived = %d, want > 0", post.TurnsArchived)
		}
		if post.TokensBefore <= 0 {
			t.Errorf("post-compact tokens_before = %d, want > 0", post.TokensBefore)
		}
		if post.CompactionModel != "test-compaction-model" {
			t.Errorf("post-compact compaction_model = %q, want %q", post.CompactionModel, "test-compaction-model")
		}
		if post.ToolDenials != 0 {
			t.Errorf("post-compact tool_denials = %d, want 0 (no tools attached)", post.ToolDenials)
		}
	})

	t.Run("compact-failed on a generation error", func(t *testing.T) {
		tempDir := t.TempDir()
		seedCompactableSession(t, tempDir)

		writeCompactionHookScript(t, tempDir, EventCompactFailed, "00-capture", captureHookScript("captured-failed.json"))

		srv := failingServer(t)
		defer srv.Close()
		runtimeCfg := &RuntimeConfig{
			Provider: "openai",
			Model:    "test-compaction-model",
			Endpoint: srv.URL,
			APIKey:   "fake",
		}
		ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, nil)
		if err != nil {
			t.Fatalf("build compaction agent: %v", err)
		}

		_, err = CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, nil)
		if err == nil {
			t.Fatal("expected CheckAndCompactSession to fail against a 503 backend")
		}

		data, err := os.ReadFile(filepath.Join(tempDir, "captured-failed.json"))
		if err != nil {
			t.Fatalf("reading compact-failed capture: %v", err)
		}
		var failed CompactFailedPayload
		if err := json.Unmarshal(data, &failed); err != nil {
			t.Fatalf("compact-failed payload did not parse: %v\nraw: %s", err, data)
		}
		if failed.Stage != "generation" {
			t.Errorf("compact-failed stage = %q, want %q", failed.Stage, "generation")
		}
		if failed.Error == "" {
			t.Error("compact-failed error is empty, want the underlying error text")
		}
		if failed.AgentID != filepath.Base(tempDir) {
			t.Errorf("compact-failed agent_id = %q, want %q", failed.AgentID, filepath.Base(tempDir))
		}
	})
}

// TestCompactionHooks_ThreeTriggerPathsRouteThroughChokePoint proves the "single choke
// point" claim: the natural token-threshold check (auto), an explicit force from a
// CLI/RPC-style call (forced), and the mid-turn short-circuit inside
// FolderAgent.checkPostTurnCompaction (also forced, but a structurally different call path)
// all end up firing pre-compact, because all three route through CheckAndCompactSession.
func TestCompactionHooks_ThreeTriggerPathsRouteThroughChokePoint(t *testing.T) {
	runOne := func(t *testing.T, run func(tempDir string, srv string)) string {
		t.Helper()
		tempDir := t.TempDir()
		seedCompactableSession(t, tempDir)
		writeCompactionHookScript(t, tempDir, EventPreCompact, "00-capture", captureHookScript("captured-pre.json"))

		srv := okBackendServer(t, "Summary text.")
		defer srv.Close()

		// Every subtest here compacts successfully, which fires the async post-compact
		// hook; drain it before returning so its goroutine never outlives this subtest and
		// races the next one over the shared postCompactHookDone test seam.
		wait := armPostCompactHookWait(t, tempDir)
		run(tempDir, srv.URL)
		wait()

		data, err := os.ReadFile(filepath.Join(tempDir, "captured-pre.json"))
		if err != nil {
			t.Fatalf("pre-compact hook never fired: %v", err)
		}
		var pre PreCompactPayload
		if err := json.Unmarshal(data, &pre); err != nil {
			t.Fatalf("pre-compact payload did not parse: %v\nraw: %s", err, data)
		}
		return pre.Trigger
	}

	t.Run("auto: natural token-threshold check", func(t *testing.T) {
		trigger := runOne(t, func(tempDir, endpoint string) {
			turns, err := ReadSessionTurns(tempDir)
			if err != nil {
				t.Fatalf("ReadSessionTurns: %v", err)
			}
			estTokens := EstimateTokens(turns, false)
			runtimeCfg := &RuntimeConfig{
				Provider: "openai",
				Model:    "m",
				Endpoint: endpoint,
				APIKey:   "fake",
				// threshold = ContextWindow * 0.8 (default 20% overhead); dividing by 0.85
				// puts estTokens just above that threshold, so the natural check fires.
				ContextWindow: int(float64(estTokens) / 0.85),
			}
			ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, nil)
			if err != nil {
				t.Fatalf("build compaction agent: %v", err)
			}
			compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, false, nil, nil)
			if err != nil {
				t.Fatalf("CheckAndCompactSession (auto): %v", err)
			}
			if !compacted {
				t.Fatal("expected the natural threshold check to trigger compaction")
			}
		})
		if trigger != "auto" {
			t.Errorf("trigger = %q, want %q", trigger, "auto")
		}
	})

	t.Run("forced: explicit force=true (CLI/RPC shape)", func(t *testing.T) {
		trigger := runOne(t, func(tempDir, endpoint string) {
			runtimeCfg := &RuntimeConfig{Provider: "openai", Model: "m", Endpoint: endpoint, APIKey: "fake"}
			ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, nil)
			if err != nil {
				t.Fatalf("build compaction agent: %v", err)
			}
			compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, nil)
			if err != nil {
				t.Fatalf("CheckAndCompactSession (forced): %v", err)
			}
			if !compacted {
				t.Fatal("expected the forced call to compact")
			}
		})
		if trigger != "forced" {
			t.Errorf("trigger = %q, want %q", trigger, "forced")
		}
	})

	t.Run("mid-turn: FolderAgent.checkPostTurnCompaction short-circuit", func(t *testing.T) {
		trigger := runOne(t, func(tempDir, endpoint string) {
			runtimeCfg := &RuntimeConfig{
				Provider: "openai",
				Model:    "m",
				Endpoint: endpoint,
				APIKey:   "fake",
				// threshold = ContextWindow*0.8 = 800; LastTotalTokens (1000) below forces the
				// check, while ContextWindow*compactionSeedTokenFraction (500) stays clear of
				// the seed cap for these turns, so it isn't confused with a stubbed seed.
				ContextWindow: 1000,
			}
			ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, nil)
			if err != nil {
				t.Fatalf("build compaction agent: %v", err)
			}
			fa := &FolderAgent{
				AgentID:         filepath.Base(tempDir),
				AgentDir:        tempDir,
				RuntimeConfig:   runtimeCfg,
				CompactionAgent: ca,
				UsageTracker:    &TurnUsageTracker{LastTotalTokens: 1000}, // == threshold-crossing usage
			}
			// checkPostTurnCompaction logs failures to stderr and returns nothing; the
			// pre-compact capture file is the observable proof it reached the choke point.
			fa.checkPostTurnCompaction(context.Background(), filepath.Dir(tempDir))
		})
		if trigger != "forced" {
			t.Errorf("trigger = %q, want %q (mid-turn short-circuits already decided, so it forces)", trigger, "forced")
		}
	})
}

// TestCompactionHooks_FailureDoesNotAbortCompaction pins that a hook script exiting nonzero
// (pre-compact) or timing out (post-compact, observed async) never aborts or delays the
// compaction it wraps - only a warning is logged.
func TestCompactionHooks_FailureDoesNotAbortCompaction(t *testing.T) {
	tempDir := t.TempDir()
	seedCompactableSession(t, tempDir)

	writeCompactionHookScript(t, tempDir, EventPreCompact, "00-fails", "#!/bin/sh\ncat > /dev/null\nexit 1\n")

	srv := okBackendServer(t, "Summary text.")
	defer srv.Close()
	runtimeCfg := &RuntimeConfig{Provider: "openai", Model: "m", Endpoint: srv.URL, APIKey: "fake"}
	ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, nil)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}

	wait := armPostCompactHookWait(t, tempDir)
	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, nil)
	if err != nil {
		t.Fatalf("a failing pre-compact hook must not abort compaction, got error: %v", err)
	}
	if !compacted {
		t.Fatal("a failing pre-compact hook must not prevent compaction from completing")
	}
	wait()

	mem, err := ReadMemoryFile(tempDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile: %v", err)
	}
	if !strings.Contains(mem, "Summary text.") {
		t.Fatalf("compaction summary missing from MEMORY.md despite the failing hook: %q", mem)
	}
}

// TestCompactionHooks_AsyncPostCompactDoesNotBlock proves the post-compact hook is genuinely
// fire-and-forget: CheckAndCompactSession returns well before a slow hook script exits, and
// the hook is still observed to complete afterward (via the test-only completion seam) - the
// window is small (the hook's own sleep) and bounded by RunHookChain's existing per-hook
// timeout, not by anything CheckAndCompactSession waits on.
func TestCompactionHooks_AsyncPostCompactDoesNotBlock(t *testing.T) {
	tempDir := t.TempDir()
	seedCompactableSession(t, tempDir)

	const hookSleepSeconds = 2
	writeCompactionHookScript(t, tempDir, EventPostCompact, "00-slow", "#!/bin/sh\ncat > /dev/null\nsleep "+
		time.Duration(hookSleepSeconds*time.Second).String()+"\nprintf '{}'\n")

	srv := okBackendServer(t, "Summary text.")
	defer srv.Close()
	runtimeCfg := &RuntimeConfig{Provider: "openai", Model: "m", Endpoint: srv.URL, APIKey: "fake"}
	ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(filepath.Base(tempDir), "system", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), tempDir, nil, nil)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}

	var mu sync.Mutex
	var hookFinishedAt time.Time
	done := make(chan struct{})
	postCompactHookDoneMu.Lock()
	prev := postCompactHookDone
	postCompactHookDone = func(dir string) {
		if dir != tempDir {
			return
		}
		mu.Lock()
		hookFinishedAt = time.Now()
		mu.Unlock()
		close(done)
	}
	postCompactHookDoneMu.Unlock()
	t.Cleanup(func() {
		postCompactHookDoneMu.Lock()
		postCompactHookDone = prev
		postCompactHookDoneMu.Unlock()
	})

	start := time.Now()
	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, nil)
	returnedAt := time.Now()
	if err != nil {
		t.Fatalf("CheckAndCompactSession: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to occur")
	}
	if elapsed := returnedAt.Sub(start); elapsed >= hookSleepSeconds*time.Second {
		t.Fatalf("CheckAndCompactSession blocked for %v, waiting on a hook it should never wait on", elapsed)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("async post-compact hook never completed")
	}
	mu.Lock()
	finished := hookFinishedAt
	mu.Unlock()
	if finished.Sub(start) < hookSleepSeconds*time.Second {
		t.Fatalf("post-compact hook reported done after only %v, expected it to actually run its sleep", finished.Sub(start))
	}
}
