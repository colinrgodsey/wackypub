package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

type d101NoopArgs struct{}

func TestRecordConsecutiveToolFailure_IdenticalTripsAtThreshold(t *testing.T) {
	tr := &TurnUsageTracker{}
	err := errors.New("permission denied for /root/secret")
	args := map[string]any{"path": "/root/secret"}
	// Loop up to failureBreakerThreshold-1, asserting no trip, then the K-th call trips.
	for i := 1; i < failureBreakerThreshold; i++ {
		recordConsecutiveToolFailure(tr, "files-rw", args, err)
		if tr.FailureBreakerTripped {
			t.Fatalf("tripped prematurely at iteration %d (D110 K=%d)", i, failureBreakerThreshold)
		}
		if tr.ConsecutiveFailureCount != i {
			t.Fatalf("expected count %d at iteration %d, got %d", i, i, tr.ConsecutiveFailureCount)
		}
	}
	recordConsecutiveToolFailure(tr, "files-rw", args, err)
	if !tr.FailureBreakerTripped {
		t.Fatalf("expected trip at %d identical failures (D110 raised K from 3 to 10)", failureBreakerThreshold)
	}
	if !strings.Contains(tr.FailureBreakerMessage, fmt.Sprintf("%d consecutive times", failureBreakerThreshold)) || !strings.Contains(tr.FailureBreakerMessage, "files-rw") {
		t.Errorf("unexpected breaker message: %q", tr.FailureBreakerMessage)
	}
}

func TestRecordConsecutiveToolFailure_DifferentFailuresReset(t *testing.T) {
	tr := &TurnUsageTracker{}
	args := map[string]any{"cmd": "echo"}
	recordConsecutiveToolFailure(tr, "t", args, errors.New("err-A"))
	recordConsecutiveToolFailure(tr, "t", args, errors.New("err-A"))
	recordConsecutiveToolFailure(tr, "t", args, errors.New("err-B"))
	recordConsecutiveToolFailure(tr, "t", args, errors.New("err-A"))
	if tr.FailureBreakerTripped {
		t.Fatalf("breaker tripped without 3 consecutive identical failures")
	}
	if tr.ConsecutiveFailureCount != 1 {
		t.Errorf("expected counter reset to 1 on differing signature, got %d", tr.ConsecutiveFailureCount)
	}
}

func TestRecordConsecutiveToolFailure_DistinctArgsNoTrip(t *testing.T) {
	// (MAJOR-2) Three different calls failing with identical error text must NOT trip the breaker.
	// E.g. files-rw "no such file or directory" on three different paths.
	tr := &TurnUsageTracker{}
	err := errors.New("no such file or directory: /some/file")
	paths := []string{"/a.txt", "/b.txt", "/c.txt"}
	for _, p := range paths {
		recordConsecutiveToolFailure(tr, "files-rw", map[string]any{"path": p}, err)
		if tr.FailureBreakerTripped {
			t.Fatalf("breaker tripped on distinct arguments: path=%s", p)
		}
		if tr.ConsecutiveFailureCount != 1 {
			t.Errorf("expected counter 1 on new args, got %d", tr.ConsecutiveFailureCount)
		}
	}
}

func TestRecordConsecutiveToolFailure_IdenticalArgsVaryingNumericErrorTrips(t *testing.T) {
	// (MAJOR-2) Errors containing varying numeric tokens (durations, timestamps, request IDs)
	// must normalize to the same signature and trip after 3 identical-args calls.
	tr := &TurnUsageTracker{}
	args := map[string]any{"endpoint": "https://api.example.com/v1/query"}
	errorsList := []string{
		"request failed with HTTP 503 after 120ms (request id req-1001)",
		"request failed with HTTP 503 after 245ms (request id req-1002)",
		"request failed with HTTP 503 after 390ms (request id req-1003)",
		"request failed with HTTP 503 after 512ms (request id req-1004)",
		"request failed with HTTP 503 after 75ms  (request id req-1005)",
		"request failed with HTTP 503 after 998ms (request id req-1006)",
		"request failed with HTTP 503 after 50ms  (request id req-1007)",
		"request failed with HTTP 503 after 305ms (request id req-1008)",
		"request failed with HTTP 503 after 421ms (request id req-1009)",
		"request failed with HTTP 503 after 188ms (request id req-1010)",
	}
	for i, errMsg := range errorsList {
		recordConsecutiveToolFailure(tr, "web_query", args, errors.New(errMsg))
		if i < failureBreakerThreshold-1 && tr.FailureBreakerTripped {
			t.Fatalf("breaker tripped prematurely at iteration %d", i)
		}
	}
	if !tr.FailureBreakerTripped {
		t.Fatalf("expected breaker to trip for identical args with normalized error, but it did not")
	}
	if tr.ConsecutiveFailureCount != failureBreakerThreshold {
		t.Errorf("expected count %d (D110 K), got %d", failureBreakerThreshold, tr.ConsecutiveFailureCount)
	}
}

func TestRecordConsecutiveToolFailure_InterleavedSuccessResets(t *testing.T) {
	// (MAJOR-3) Interleaved successful tool calls must reset consecutive failure tracking.
	tr := &TurnUsageTracker{}
	args := map[string]any{"path": "/tmp/test.txt"}
	err := errors.New("permission denied")

	// Fail 1
	recordConsecutiveToolFailure(tr, "files-rw", args, err)
	if tr.ConsecutiveFailureCount != 1 {
		t.Fatalf("expected count 1, got %d", tr.ConsecutiveFailureCount)
	}

	// Successful tool calls (e.g. 5 successful tool calls)
	for i := 0; i < 5; i++ {
		resetConsecutiveToolFailure(tr)
	}
	if tr.ConsecutiveFailureCount != 0 || tr.ConsecutiveFailureSignature != "" {
		t.Fatalf("expected state cleared after success, count=%d sig=%q", tr.ConsecutiveFailureCount, tr.ConsecutiveFailureSignature)
	}

	// Fail x 2 -> should reach count 2, NOT trip
	recordConsecutiveToolFailure(tr, "files-rw", args, err)
	recordConsecutiveToolFailure(tr, "files-rw", args, err)
	if tr.FailureBreakerTripped {
		t.Fatalf("breaker tripped after interleaved successes")
	}
	if tr.ConsecutiveFailureCount != 2 {
		t.Errorf("expected count 2 after reset and two failures, got %d", tr.ConsecutiveFailureCount)
	}
}

func TestRecordConsecutiveToolFailure_AlternatingABANeverTrips(t *testing.T) {
	// (MINOR-6) A/B/A alternating failures document and test the strictly-consecutive semantics.
	tr := &TurnUsageTracker{}
	argsA := map[string]any{"cmd": "git status"}
	argsB := map[string]any{"cmd": "git log"}
	err := errors.New("exit status 128")

	for i := 0; i < 10; i++ {
		recordConsecutiveToolFailure(tr, "bash", argsA, err)
		if tr.FailureBreakerTripped || tr.ConsecutiveFailureCount != 1 {
			t.Fatalf("cycle %d A: unexpected trip or count=%d", i, tr.ConsecutiveFailureCount)
		}
		recordConsecutiveToolFailure(tr, "bash", argsB, err)
		if tr.FailureBreakerTripped || tr.ConsecutiveFailureCount != 1 {
			t.Fatalf("cycle %d B: unexpected trip or count=%d", i, tr.ConsecutiveFailureCount)
		}
	}
}

func TestRecordConsecutiveToolFailure_ConcurrentRace(t *testing.T) {
	// (MAJOR-1) Driving N concurrent failures through recordConsecutiveToolFailure must not race under -race.
	tr := &TurnUsageTracker{}
	args := map[string]any{"path": "/concurrent/test"}
	err := errors.New("concurrent I/O failure")
	const N = 32

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			recordConsecutiveToolFailure(tr, "files-rw", args, err)
		}()
	}
	wg.Wait()

	tripped, msg, count, _ := tr.BreakerState()
	if !tripped {
		t.Fatalf("expected breaker to trip under %d concurrent failures", N)
	}
	if count < failureBreakerThreshold {
		t.Errorf("expected count >= %d, got %d", failureBreakerThreshold, count)
	}
	if !strings.Contains(msg, "consecutive times") {
		t.Errorf("expected consecutive times message, got %q", msg)
	}
}

// TestRecordConsecutiveToolFailure_SchemaDoesNotFastAbort (D110): a single schema-shaped
// error no longer trips the breaker. The model is given K=failureBreakerThreshold chances
// to recover. The K=1 fast-abort was removed because it killed turns on planning-phase
// skeleton tool calls (just the tool name, no args).
func TestRecordConsecutiveToolFailure_SchemaDoesNotFastAbort(t *testing.T) {
	for _, msg := range []string{
		`invalid tool arguments: missing properties: "path"`,
		`invalid arguments: "text" is required`,
	} {
		tr := &TurnUsageTracker{}
		recordConsecutiveToolFailure(tr, "files-rw", map[string]any{"dummy": 1}, errors.New(msg))
		if tr.FailureBreakerTripped {
			t.Errorf("D110: schema error %q should NOT trip breaker on first occurrence (was K=1, now K=%d)", msg, failureBreakerThreshold)
		}
		if tr.ConsecutiveFailureCount != 1 {
			t.Errorf("expected ConsecutiveFailureCount=1 after first schema error, got %d", tr.ConsecutiveFailureCount)
		}
	}

	// Unscoped "is required" must not be confused with schema validation. With D110 the
	// distinction matters less (both go through the K=10 counter now), but verify the
	// false-positive detector still excludes them.
	tr := &TurnUsageTracker{}
	recordConsecutiveToolFailure(tr, "agent_tool", map[string]any{"id": "a"}, errors.New("root agent is required"))
	if tr.FailureBreakerTripped {
		t.Errorf("unscoped 'is required' must not trigger immediate schema fast abort")
	}
	if tr.ConsecutiveFailureCount != 1 {
		t.Errorf("expected count 1, got %d", tr.ConsecutiveFailureCount)
	}
}

func TestRecordConsecutiveToolFailure_NilGuards(t *testing.T) {
	recordConsecutiveToolFailure(nil, "t", nil, errors.New("boom"))
	tr := &TurnUsageTracker{}
	recordConsecutiveToolFailure(tr, "t", nil, nil)
	if tr.FailureBreakerTripped || tr.ConsecutiveFailureCount != 0 {
		t.Errorf("nil inputs must be no-ops")
	}
}

func TestFailureBreakerMessage_SnippetIsRuneSafeAndCollapsed(t *testing.T) {
	long := "line1\n\nline2  " + strings.Repeat("汉字あ🐴", 100)
	s := failureSnippet(errors.New(long))
	if strings.ContainsAny(s, "\n") {
		t.Errorf("snippet should collapse newlines, got %q", s[:60])
	}
	if !strings.HasSuffix(s, "...") || len([]rune(s)) > 204 {
		t.Errorf("expected rune-safe 200-rune truncation with ellipsis, got %d runes", len([]rune(s)))
	}
	if strings.ContainsRune(s, '\uFFFD') {
		t.Errorf("snippet cut a multi-byte rune")
	}
}

func TestTurnUsageTrackerReset_ClearsBreaker(t *testing.T) {
	tr := &TurnUsageTracker{}
	err := errors.New(`invalid tool arguments: missing properties: "x"`)
	args := map[string]any{"x": 1}
	// D110: a single schema failure no longer trips. Loop to failureBreakerThreshold to trip.
	for i := 0; i < failureBreakerThreshold; i++ {
		recordConsecutiveToolFailure(tr, "files-rw", args, err)
	}
	if !tr.FailureBreakerTripped {
		t.Fatal("expected trip")
	}
	tr.Reset()
	if tr.FailureBreakerTripped || tr.FailureBreakerMessage != "" || tr.ConsecutiveFailureCount != 0 || tr.ConsecutiveFailureSignature != "" {
		t.Errorf("Reset() must clear all breaker state")
	}
}

// TestFailureBreaker_WireAbortsAfterThreeIdenticalFailures drives the full
// callback loop: the fake provider keeps emitting the same failing tool call,
// the tool deterministically fails identically, and the breaker must end the
// turn without issuing a fourth model call.
func TestFailureBreaker_WireAbortsAfterThreeIdenticalFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n <= failureBreakerThreshold {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			t.Errorf("model called %d times - breaker should have aborted the turn", n)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"SHOULD NOT REACH MODEL"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	failingTool, err := functiontool.New(functiontool.Config{Name: "flaky_tool"}, func(ctx agent.Context, args d101NoopArgs) (map[string]any, error) {
		return nil, errors.New("permission denied for /root/secret")
	})
	if err != nil {
		t.Fatalf("failed to create failing tool: %v", err)
	}

	runtimeCfg := &RuntimeConfig{Model: "test-model", Endpoint: srv.URL}
	ag, tr := mustBuildBreakerAgent(t, runtimeCfg, failingTool)

	requireAbortText(t, runBreakerTurn(t, ag, "breaker-identical"), "10 consecutive times", "aborting turn")
	if n := atomic.LoadInt32(&calls); n != failureBreakerThreshold {
		t.Errorf("expected exactly %d provider calls, got %d", failureBreakerThreshold, n)
	}
	if !tr.FailureBreakerTripped {
		t.Errorf("tracker should record the trip")
	}
}

// TestFailureBreaker_WireSchemaToleratesPlanningCalls (D110): a schema-shaped tool error
// does NOT abort the turn on the first occurrence. The model is given K=failureBreakerThreshold
// chances to recover, which handles planning-phase skeleton tool calls (just the name, no args).
// A model genuinely stuck on the same broken call still trips the breaker.
func TestFailureBreaker_WireSchemaToleratesPlanningCalls(t *testing.T) {
	var calls int32
	const expectedCalls = failureBreakerThreshold
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if int(n) <= expectedCalls {
			// Model keeps emitting the same broken tool call
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			t.Errorf("model called %d times - schema breaker should end the turn at %d failures", n, expectedCalls)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"SHOULD NOT REACH MODEL"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	failingTool, err := functiontool.New(functiontool.Config{Name: "flaky_tool"}, func(ctx agent.Context, args d101NoopArgs) (map[string]any, error) {
		return nil, fmt.Errorf(`invalid tool arguments: missing properties: "path"`)
	})
	if err != nil {
		t.Fatalf("failed to create failing tool: %v", err)
	}

	runtimeCfg := &RuntimeConfig{Model: "test-model", Endpoint: srv.URL}
	ag, tr := mustBuildBreakerAgent(t, runtimeCfg, failingTool)

	requireAbortText(t, runBreakerTurn(t, ag, "breaker-schema"), "schema validation", "aborting turn")
	if n := atomic.LoadInt32(&calls); int(n) != expectedCalls {
		t.Errorf("expected exactly %d provider calls (D110 K=%d), got %d", expectedCalls, failureBreakerThreshold, n)
	}
	if !tr.FailureBreakerTripped {
		t.Errorf("tracker should record the trip after %d schema failures", failureBreakerThreshold)
	}
}

// TestRecordConsecutiveToolFailure_SchemaValidationDoesNotTripImmediately (D110):
// unit-level test: a single schema-validation failure does NOT trip the breaker.
// Nine more identical schema failures should still leave it un-tripped. The tenth trips it.
func TestRecordConsecutiveToolFailure_SchemaValidationDoesNotTripImmediately(t *testing.T) {
	tr := &TurnUsageTracker{}
	err := errors.New(`invalid tool arguments: missing properties: "path" - jsonschema validation failed`)
	args := map[string]any{"foo": "bar"}

	for i := 1; i < failureBreakerThreshold; i++ {
		recordConsecutiveToolFailure(tr, "flaky_tool", args, err)
		if tr.FailureBreakerTripped {
			t.Fatalf("breaker tripped after %d schema failures (D110: should require %d)", i, failureBreakerThreshold)
		}
		if tr.ConsecutiveFailureCount != i {
			t.Errorf("after %d calls, expected count %d, got %d", i, i, tr.ConsecutiveFailureCount)
		}
	}
	recordConsecutiveToolFailure(tr, "flaky_tool", args, err)
	if !tr.FailureBreakerTripped {
		t.Fatalf("breaker should trip at the %d-th identical schema failure (D110 K=%d)", failureBreakerThreshold, failureBreakerThreshold)
	}
	if !strings.Contains(tr.FailureBreakerMessage, "schema validation") || !strings.Contains(tr.FailureBreakerMessage, fmt.Sprintf("%d consecutive times", failureBreakerThreshold)) {
		t.Errorf("unexpected breaker message: %q", tr.FailureBreakerMessage)
	}
}

func mustBuildBreakerAgent(t *testing.T, runtimeCfg *RuntimeConfig, tools ...tool.Tool) (agent.Agent, *TurnUsageTracker) {
	t.Helper()
	tr := &TurnUsageTracker{}
	ag, err := BuildADKAgentWithConfigAndTracker("breaker-bot", "System prompt", DefaultMaxToolTurns, runtimeCfg, NewOpenAIModel(runtimeCfg), "", tr, tools...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfigAndTracker failed: %v", err)
	}
	return ag, tr
}

func runBreakerTurn(t *testing.T, ag agent.Agent, sessionID string) string {
	t.Helper()
	sessionSvc := session.InMemoryService()
	_, err := sessionSvc.Create(context.Background(), &session.CreateRequest{
		AppName: "wackypub", UserID: "user", SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("sessionSvc.Create failed: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "wackypub", Agent: ag, SessionService: sessionSvc})
	if err != nil {
		t.Fatalf("runner.New failed: %v", err)
	}
	var out string
	for event, err := range r.Run(context.Background(), "user", sessionID, genai.NewContentFromText("go", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run failed: %v", err)
		}
		if event != nil && event.Content != nil {
			out += ExtractTextFromEvent(event)
		}
	}
	return out
}

func requireAbortText(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("expected abort output to contain %q, got: %q", w, out)
		}
	}
}

// TestFailureBreaker_WireInterleavedSuccessResetsBreaker verifies that AfterToolCallbacks
// resets the consecutive failure counter upon a successful tool execution (MAJOR-3).
// If a tool fails once, succeeds once, and fails twice, the breaker must not trip.
func TestFailureBreaker_WireInterleavedSuccessResetsBreaker(t *testing.T) {
	var step int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := atomic.AddInt32(&step, 1)
		w.Header().Set("Content-Type", "application/json")
		switch s {
		case 1: // Call failing tool (failure #1)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2: // Call good tool (success -> resets breaker)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"good_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 3: // Call failing tool (failure #1 after reset)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c3","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 4: // Call failing tool (failure #2 after reset)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c4","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 5: // Model terminates turn
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"all done without breaker trip"},"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected extra model call step %d", s)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"overflow"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	flakyTool, err := functiontool.New(functiontool.Config{Name: "flaky_tool"}, func(ctx agent.Context, args d101NoopArgs) (map[string]any, error) {
		return nil, errors.New("transient network timeout")
	})
	if err != nil {
		t.Fatalf("failed to create flaky tool: %v", err)
	}

	goodTool, err := functiontool.New(functiontool.Config{Name: "good_tool"}, func(ctx agent.Context, args d101NoopArgs) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("failed to create good tool: %v", err)
	}

	runtimeCfg := &RuntimeConfig{Model: "test-model", Endpoint: srv.URL}
	ag, tr := mustBuildBreakerAgent(t, runtimeCfg, flakyTool, goodTool)

	out := runBreakerTurn(t, ag, "breaker-interleaved")
	if !strings.Contains(out, "all done without breaker trip") {
		t.Errorf("expected clean turn completion, got output: %q", out)
	}
	if tr.FailureBreakerTripped {
		t.Errorf("breaker should NOT have tripped when successful tool call intervened")
	}
	if tr.ConsecutiveFailureCount != 2 {
		t.Errorf("expected consecutive failure count 2, got %d", tr.ConsecutiveFailureCount)
	}
}

// TestFailureBreaker_TakesPrecedenceOverBudgetBail verifies that when the failure breaker
// is tripped, its synthetic response takes precedence over the D88 mid-turn token budget bail,
// and StoppedEarlyForCompaction is not set.
func TestFailureBreaker_TakesPrecedenceOverBudgetBail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Emit failing tool call
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	flakyTool, err := functiontool.New(functiontool.Config{Name: "flaky_tool"}, func(ctx agent.Context, args d101NoopArgs) (map[string]any, error) {
		return nil, errors.New(`invalid tool arguments: missing properties: "path"`)
	})
	if err != nil {
		t.Fatalf("failed to create failing tool: %v", err)
	}

	// Tiny context window to ensure mid-turn budget threshold is also exceeded
	runtimeCfg := &RuntimeConfig{ContextWindow: 10, Model: "test-model", Endpoint: srv.URL}
	ag, tr := mustBuildBreakerAgent(t, runtimeCfg, flakyTool)
	// D110: pre-trip the breaker. With K=10, a single schema failure no longer trips; the
	// test exercises the precedence relationship (breaker vs compaction bail) so we trip
	// the breaker directly to keep the precedence assertion under test.
	tr.FailureBreakerTripped = true
	tr.FailureBreakerMessage = `Tool "flaky_tool" failed schema validation 10 consecutive times - aborting turn.`

	out := runBreakerTurn(t, ag, "breaker-precedence")
	requireAbortText(t, out, "schema validation", "aborting turn")
	if strings.Contains(out, "stopping turn early to allow session compaction") {
		t.Errorf("breaker should take precedence over compaction bail, but compaction wording was found: %q", out)
	}
	if tr.StoppedEarlyForCompaction {
		t.Errorf("StoppedEarlyForCompaction should remain false when breaker aborts")
	}
}
