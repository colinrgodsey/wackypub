package agent

import (
"context"
"errors"
"fmt"
"io"
"net/http"
"net/http/httptest"
"strings"
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
recordConsecutiveToolFailure(tr, "files-rw", err)
if tr.FailureBreakerTripped {
t.Fatalf("tripped after a single non-schema failure")
}
recordConsecutiveToolFailure(tr, "files-rw", err)
if tr.ConsecutiveFailureCount != 2 {
t.Fatalf("expected count 2, got %d", tr.ConsecutiveFailureCount)
}
recordConsecutiveToolFailure(tr, "files-rw", err)
if !tr.FailureBreakerTripped {
t.Fatalf("expected trip at %d identical failures", failureBreakerThreshold)
}
if !strings.Contains(tr.FailureBreakerMessage, "3 consecutive times") || !strings.Contains(tr.FailureBreakerMessage, "files-rw") {
t.Errorf("unexpected breaker message: %q", tr.FailureBreakerMessage)
}
}

func TestRecordConsecutiveToolFailure_DifferentFailuresReset(t *testing.T) {
tr := &TurnUsageTracker{}
recordConsecutiveToolFailure(tr, "t", errors.New("err-A"))
recordConsecutiveToolFailure(tr, "t", errors.New("err-A"))
recordConsecutiveToolFailure(tr, "t", errors.New("err-B"))
recordConsecutiveToolFailure(tr, "t", errors.New("err-A"))
if tr.FailureBreakerTripped {
t.Fatalf("breaker tripped without 3 consecutive identical failures")
}
if tr.ConsecutiveFailureCount != 1 {
t.Errorf("expected counter reset to 1 on differing signature, got %d", tr.ConsecutiveFailureCount)
}
}

func TestRecordConsecutiveToolFailure_SchemaFastAbort(t *testing.T) {
for _, msg := range []string{
`invalid tool arguments: missing properties: "path"`,
`invalid arguments: "text" is required`,
} {
tr := &TurnUsageTracker{}
recordConsecutiveToolFailure(tr, "files-rw", errors.New(msg))
if !tr.FailureBreakerTripped {
t.Errorf("expected immediate trip for schema error %q", msg)
}
if !strings.Contains(tr.FailureBreakerMessage, "schema validation") {
t.Errorf("expected schema wording in message, got %q", tr.FailureBreakerMessage)
}
}
}

func TestRecordConsecutiveToolFailure_NilGuards(t *testing.T) {
recordConsecutiveToolFailure(nil, "t", errors.New("boom"))
tr := &TurnUsageTracker{}
recordConsecutiveToolFailure(tr, "t", nil)
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
recordConsecutiveToolFailure(tr, "files-rw", errors.New(`missing properties: "x"`))
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

requireAbortText(t, runBreakerTurn(t, ag, "breaker-identical"), "3 consecutive times", "aborting turn")
if n := atomic.LoadInt32(&calls); n != failureBreakerThreshold {
t.Errorf("expected exactly %d provider calls, got %d", failureBreakerThreshold, n)
}
if !tr.FailureBreakerTripped {
t.Errorf("tracker should record the trip")
}
}

// TestFailureBreaker_WireSchemaFastAbort: a schema-shaped tool error aborts on
// the first failure, without a second model call.
func TestFailureBreaker_WireSchemaFastAbort(t *testing.T) {
var calls int32
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
n := atomic.AddInt32(&calls, 1)
w.Header().Set("Content-Type", "application/json")
if n == 1 {
io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"flaky_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
} else {
t.Errorf("model called %d times - schema fast-abort should end the turn after one failure", n)
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
if n := atomic.LoadInt32(&calls); n != 1 {
t.Errorf("expected exactly 1 provider call, got %d", n)
}
if !tr.FailureBreakerTripped {
t.Errorf("tracker should record the trip")
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
