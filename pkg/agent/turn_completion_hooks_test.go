package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Turn-completion events are the post-everything stage: they fire once from the runtime-fallback
// walker, after the last session write, while the session lock is still held, and they honor no
// actions. These tests drive real turns against stub backends so that the fire point itself is
// under test rather than a unit-level stand-in for it.

// pinHookEnv keeps the ambient environment from silently disabling the chain or starving it: a
// runtime exporting WACKYPUB_HOOKS=0 would otherwise make every assertion here pass for the
// wrong reason.
func pinHookEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvHooks, "1")
	t.Setenv(EnvHookTimeoutSeconds, "20")
}

func singleBackendRuntime(t *testing.T, url string) string {
	t.Helper()
	return fmt.Sprintf(`{
		"provider": "openai",
		"endpoint": %q,
		"model": "primary-model",
		"apiKey": "k1"
	}`, url)
}

func installCompletionHook(t *testing.T, agentDir, event, name, script string) {
	t.Helper()
	dir := filepath.Join(agentDir, "hooks", event)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
}

// recordHookScript appends the payload it is handed, so a test can count fires and read the
// classification the server reported.
func recordHookScript(sink string) string {
	return "#!/bin/sh\ncat >> " + fmt.Sprintf("%q", sink) + "\n"
}

func readSink(t *testing.T, sink string) string {
	t.Helper()
	data, err := os.ReadFile(sink)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read sink %s: %v", sink, err)
	}
	return string(data)
}

func countFires(t *testing.T, sink string) int {
	t.Helper()
	return strings.Count(readSink(t, sink), "agent_id")
}

func sessionText(t *testing.T, agentDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentDir, "session.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read session: %v", err)
	}
	return string(data)
}

func sessionRows(t *testing.T, agentDir string) int {
	t.Helper()
	body := strings.TrimSpace(sessionText(t, agentDir))
	if body == "" {
		return 0
	}
	return len(strings.Split(body, "\n"))
}

func runTurn(t *testing.T, sdk *AgentSDK, agentID string, onWarning ...func(string)) (string, error) {
	t.Helper()
	var chunks []string
	var turnErr error
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(context.Background(), agentID, "question", onWarning...) {
		if err != nil {
			turnErr = err
			continue
		}
		chunks = append(chunks, chunk)
	}
	return strings.Join(chunks, ""), turnErr
}

func TestTurnCompletionHooks_CleanTurnFiresCompletedOnce(t *testing.T) {
	pinHookEnv(t)
	primary := okBackendServer(t, "the-answer")
	defer primary.Close()

	sdk, agentDir := setupFallbackAgent(t, singleBackendRuntime(t, primary.URL))
	dir := t.TempDir()
	completedSink := filepath.Join(dir, "completed.txt")
	failedSink := filepath.Join(dir, "failed.txt")
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-record", recordHookScript(completedSink))
	installCompletionHook(t, agentDir, EventTurnFailed, "00-record", recordHookScript(failedSink))

	text, err := runTurn(t, sdk, "fbagent")
	if err != nil {
		t.Fatalf("turn error: %v", err)
	}
	if !strings.Contains(text, "the-answer") {
		t.Fatalf("stub answer missing from the turn: %q", text)
	}

	if got := countFires(t, completedSink); got != 1 {
		t.Fatalf("turn-completed fired %d times, want exactly 1: %s", got, readSink(t, completedSink))
	}
	body := readSink(t, completedSink)
	for _, want := range []string{`"event":"turn-completed"`, `"reason":"completed"`, `"agent_id":"fbagent"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload missing %s: %s", want, body)
		}
	}
	if countFires(t, failedSink) != 0 {
		t.Fatalf("turn-failed must not fire on a clean turn: %s", readSink(t, failedSink))
	}
}

func TestTurnCompletionHooks_FailingTurnFiresFailedOnce(t *testing.T) {
	pinHookEnv(t)
	failing := failingServer(t)
	defer failing.Close()

	sdk, agentDir := setupFallbackAgent(t, singleBackendRuntime(t, failing.URL))
	dir := t.TempDir()
	failedSink := filepath.Join(dir, "failed.txt")
	completedSink := filepath.Join(dir, "completed.txt")
	installCompletionHook(t, agentDir, EventTurnFailed, "00-record", recordHookScript(failedSink))
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-record", recordHookScript(completedSink))

	if _, err := runTurn(t, sdk, "fbagent"); err == nil {
		t.Fatal("expected the failing backend to surface an error")
	}

	if got := countFires(t, failedSink); got != 1 {
		t.Fatalf("turn-failed fired %d times, want exactly 1: %s", got, readSink(t, failedSink))
	}
	body := readSink(t, failedSink)
	if !strings.Contains(body, `"reason":"stream_error"`) {
		t.Fatalf("a surfaced stream error should classify as stream_error: %s", body)
	}
	if !strings.Contains(body, `"error":`) || strings.Contains(body, `"error":""`) {
		t.Fatalf("the failed payload should name the error: %s", body)
	}
	if countFires(t, completedSink) != 0 {
		t.Fatalf("turn-completed must not fire on a failed turn")
	}
}

// A turn that descends the fallback chain is still one turn: the walker visits several levels but
// reports one terminal state, naming the level that actually served the turn.
func TestTurnCompletionHooks_FiresOnceAcrossFallbackDescent(t *testing.T) {
	pinHookEnv(t)
	primary := failingServer(t)
	defer primary.Close()
	fallback := okBackendServer(t, "fallback answered")
	defer fallback.Close()

	sdk, agentDir := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))
	sink := filepath.Join(t.TempDir(), "events.txt")
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-record", recordHookScript(sink))

	if _, err := runTurn(t, sdk, "fbagent"); err != nil {
		t.Fatalf("turn error: %v", err)
	}

	if got := countFires(t, sink); got != 1 {
		t.Fatalf("expected exactly 1 completion event across a 2-level turn, got %d: %s", got, readSink(t, sink))
	}
	body := readSink(t, sink)
	if !strings.Contains(body, `"reason":"completed"`) {
		t.Fatalf("the descent ended successfully, so the reason should be completed: %s", body)
	}
	if !strings.Contains(body, "fallback-model") {
		t.Fatalf("the backend field should name the level that served the turn: %s", body)
	}
}

// No stream was ever attempted when every backend is skipped as failed-until-reset, so there is
// no terminal turn state to report and nothing must reach a consumer.
func TestTurnCompletionHooks_SetupFailureFiresNothing(t *testing.T) {
	pinHookEnv(t)
	primary := okBackendServer(t, "unused")
	defer primary.Close()

	sdk, agentDir := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, ""))
	sink := filepath.Join(t.TempDir(), "events.txt")
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-record", recordHookScript(sink))
	installCompletionHook(t, agentDir, EventTurnFailed, "00-record", recordHookScript(sink))

	cfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	chain := cfg.FallbackChain()
	for _, level := range chain {
		sdk.markFailedUntil(level, time.Now().Add(time.Hour))
	}

	if _, err := runTurn(t, sdk, "fbagent"); err == nil {
		t.Fatal("expected the turn to report that no backend was available")
	}
	if got := countFires(t, sink); got != 0 {
		t.Fatalf("a turn that never started must fire nothing, got %d: %s", got, readSink(t, sink))
	}
}

// The events are observe-only, so a script that tries to steer the turn gets a warning and
// nothing else: no continuation, no substituted output.
func TestTurnCompletionHooks_ResolveNoActions(t *testing.T) {
	pinHookEnv(t)
	primary := okBackendServer(t, "the-answer")
	defer primary.Close()

	sdk, agentDir := setupFallbackAgent(t, singleBackendRuntime(t, primary.URL))
	steering := `#!/bin/sh
cat > /dev/null
printf '{"text":"SHOULD_NOT_BE_APPLIED","action":{"type":"continue","reason":"try to steer"}}'
`
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-steer", steering)

	before := sessionRows(t, agentDir)
	warn := captureStderr(func() {
		if _, err := runTurn(t, sdk, "fbagent"); err != nil {
			t.Fatalf("turn error: %v", err)
		}
	})
	if !strings.Contains(warn, "continue") {
		t.Fatalf("an unhonored action should be reported on stderr, got %q", warn)
	}

	body := sessionText(t, agentDir)
	if strings.Contains(body, "SHOULD_NOT_BE_APPLIED") {
		t.Fatalf("hook text must never enter the session for an observe-only event: %s", body)
	}
	if got := sessionRows(t, agentDir); got != before+2 {
		t.Fatalf("expected exactly one user turn plus one model turn (before=%d after=%d): %s", before, got, body)
	}
	if strings.Contains(body, "end-of-turn continuation") {
		t.Fatalf("a completion hook must not be able to request a continuation: %s", body)
	}
}

// Asides are excluded because they never reach the fallback walker at all. Driving one proves the
// exclusion instead of asserting it from a reading of the call graph.
func TestTurnCompletionHooks_AsideFiresNeitherEvent(t *testing.T) {
	pinHookEnv(t)
	primary := okBackendServer(t, "aside answer")
	defer primary.Close()

	sdk, agentDir := setupFallbackAgent(t, singleBackendRuntime(t, primary.URL))
	sink := filepath.Join(t.TempDir(), "events.txt")
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-record", recordHookScript(sink))
	installCompletionHook(t, agentDir, EventTurnFailed, "00-record", recordHookScript(sink))

	result := &asideTurnResult{}
	err := asideInternal(sdk, context.Background(), filepath.Dir(agentDir), "fbagent", "aside question", result, nil,
		func(chunk string, herr error) bool { return true })
	if err != nil {
		t.Fatalf("aside error: %v", err)
	}
	if got := countFires(t, sink); got != 0 {
		t.Fatalf("an aside must not emit turn-completion events, got %d: %s", got, readSink(t, sink))
	}
}

func TestTurnCompletionReason_Classification(t *testing.T) {
	cases := []struct {
		name       string
		turnErr    error
		ctxErr     error
		budgetSeen bool
		wantEvent  string
		wantReason string
	}{
		{"clean", nil, nil, false, EventTurnCompleted, TurnTerminalCompleted},
		{"budget spent", nil, nil, true, EventTurnCompleted, TurnTerminalBudgetExhausted},
		{"stream error", fmt.Errorf("provider exploded"), nil, false, EventTurnFailed, TurnTerminalStreamError},
		{"watchdog", fmt.Errorf("turn watchdog: deadline exceeded on backend x/y"), nil, false, EventTurnFailed, TurnTerminalWatchdog},
		{"consumer stopped", errTurnConsumerStopped, nil, true, EventTurnFailed, TurnTerminalConsumerStopped},
		{"cancel outranks error", fmt.Errorf("provider exploded"), context.Canceled, false, EventTurnFailed, TurnTerminalCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, reason := turnCompletionReason(tc.turnErr, tc.ctxErr, tc.budgetSeen)
			if event != tc.wantEvent || reason != tc.wantReason {
				t.Fatalf("turnCompletionReason = (%s, %s), want (%s, %s)", event, reason, tc.wantEvent, tc.wantReason)
			}
		})
	}
}

// The classifier keys on the same constant the notices are built from, so the two cannot drift
// apart silently.
func TestBudgetNoticeConstantMatchesEmittedText(t *testing.T) {
	notice := fmt.Sprintf("\n\n%s: 2 of 2 used - stopping with incomplete status.", autoContinuationBudgetExhaustedNotice)
	seen := strings.Contains(notice, autoContinuationBudgetExhaustedNotice)
	if !seen {
		t.Fatal("the classifier constant must be a prefix of the emitted notice")
	}
	event, reason := turnCompletionReason(nil, nil, seen)
	if event != EventTurnCompleted || reason != TurnTerminalBudgetExhausted {
		t.Fatalf("budget notice should classify as completed/budget_exhausted, got %s/%s", event, reason)
	}
}

// Budget exhaustion is a ratified semantic: a turn that stopped because the D88 auto-continuation
// budget ran out is delivered as turn-completed with reason budget_exhausted, because no error
// was surfaced and those long turns are the ones an always-fresh consumer most needs erased. This
// drives it end to end so the scan of the turn's own text is exercised, not just the classifier.
func TestTurnCompletionHooks_BudgetExhaustedTurnReportsReason(t *testing.T) {
	pinHookEnv(t)
	primary := okBackendServer(t, "keep going")
	defer primary.Close()

	runtimeJSON := `{
		"provider": "openai",
		"endpoint": "ENDPOINT",
		"model": "primary-model",
		"apiKey": "k1",
		"maxAutoContinuations": 1
	}`
	sdk, agentDir := setupFallbackAgent(t, strings.Replace(runtimeJSON, "ENDPOINT", primary.URL, 1))

	// An end-of-turn hook that always asks for another turn, so the budget is what stops it.
	alwaysContinue := `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"continue","reason":"always again"}}'
`
	installCompletionHook(t, agentDir, EventEndOfTurn, "00-continue", alwaysContinue)

	sink := filepath.Join(t.TempDir(), "events.txt")
	installCompletionHook(t, agentDir, EventTurnCompleted, "00-record", recordHookScript(sink))

	if _, err := runTurn(t, sdk, "fbagent"); err != nil {
		t.Fatalf("turn error: %v", err)
	}

	body := readSink(t, sink)
	if got := countFires(t, sink); got != 1 {
		t.Fatalf("expected exactly 1 completion event, got %d: %s", got, body)
	}
	if !strings.Contains(body, `"reason":"budget_exhausted"`) {
		t.Fatalf("a turn stopped by a spent D88 budget should report budget_exhausted: %s", body)
	}
}
