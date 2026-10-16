package agent

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/genai"
)

// --- D1(b) mechanism pin: hard tool failures persist as function responses ---

// TestRuntimeFallback_HardToolFailure_Failover pins the D1(b) mechanism finding: even a
// hard tool failure (non-zero exit) is persisted as a function response (role user), the
// agentic run continues, and a subsequent 429 still fails over zero-text to the fallback.
func TestRuntimeFallback_HardToolFailure_Failover(t *testing.T) {
	calls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"bash\",\"args\":[\"-c\",\"exit 3\"]}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"1308","message":"Usage limit reached"}}`)
	}))
	defer primary.Close()
	fallback := okBackendServer(t, "fallback ok after hard tool fail")
	defer fallback.Close()

	sdk, agentDir := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	if err := os.Symlink("/bin/bash", filepath.Join(toolsDir, "bash")); err != nil {
		t.Fatalf("symlink bash: %v", err)
	}

	var chunks []string
	ctx := context.Background()
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "run the tool", nil) {
		if err != nil {
			t.Fatalf("stream error (hard tool failure must not kill the turn): %v", err)
		}
		chunks = append(chunks, chunk)
	}
	if !strings.Contains(strings.Join(chunks, ""), "fallback ok after hard tool fail") {
		t.Fatalf("expected the fallback to serve after a hard tool failure + 429, got %q", chunks)
	}
	// The failed tool's outcome must have been persisted as a function response.
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("read turns: %v", err)
	}
	sawFR := false
	for _, c := range turns {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				sawFR = true
			}
		}
	}
	if !sawFR {
		t.Fatalf("expected the failed tool call to be persisted as a function response")
	}
}

// --- Guard semantics (strict vs resumable) ---

func TestGenerateGuardError(t *testing.T) {
	part := func(text string) *genai.Content { return &genai.Content{Parts: []*genai.Part{{Text: text}}} }
	c := func(role string) *genai.Content { c := part("x"); c.Role = role; return c }
	cases := []struct {
		name      string
		turns     []*genai.Content
		resumable bool
		wantErr   bool
		wantSub   string
	}{
		{"strict empty", nil, false, true, "does not end on a user turn"},
		{"resumable empty", nil, true, true, "nothing to resume"},
		{"strict user-ending", []*genai.Content{c("user")}, false, false, ""},
		{"strict model-ending", []*genai.Content{c("user"), c("model")}, false, true, "does not end on a user turn"},
		{"strict tool-ending", []*genai.Content{c("user"), c("tool")}, false, true, "does not end on a user turn"},
		{"resumable user-ending", []*genai.Content{c("user")}, true, false, ""},
		{"resumable model-ending", []*genai.Content{c("user"), c("model")}, true, false, ""},
		{"resumable tool-ending", []*genai.Content{c("user"), c("tool")}, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := generateGuardError("a", tc.turns, tc.resumable)
			if tc.wantErr != (err != nil) {
				t.Fatalf("generateGuardError = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q missing %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// --- Turn watchdog ---

// blockingStream yields nothing and returns nothing, ignoring its context: the pure
// wedge the watchdog exists for.
func blockingStream(fa *FolderAgent, level int, ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		select {}
	}
}

// TestTurnWatchdog_WedgeNamedTerminal: a context-deaf wedged stream on a single-level
// chain must free the walk at the deadline with a named, backend-attributed terminal
// error (the D5 "named terminal state, not a silent wedge").
func TestTurnWatchdog_WedgeNamedTerminal(t *testing.T) {
	sdk, _ := setupFallbackAgent(t, `{"provider":"openai","endpoint":"http://unused","model":"m","apiKey":"k"}`)
	cfg := &RuntimeConfig{Endpoint: "http://wedge-host/primary-model", Model: "wedge-model", TurnDeadlineSeconds: 1}

	gotErr := ""
	start := time.Now()
	sdk.runTurnWithRuntimeFallback(context.Background(), "w", &FolderAgent{AgentID: "w"},
		[]*RuntimeConfig{cfg},
		func(cfg *RuntimeConfig) (*FolderAgent, error) { return &FolderAgent{AgentID: "w"}, nil },
		blockingStream,
		nil,
		func(chunk string, err error) bool {
			if err != nil {
				gotErr = err.Error()
			}
			return true
		})
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("walk took %v - the watchdog did not free it", elapsed)
	}
	if !strings.Contains(gotErr, "turn watchdog") {
		t.Fatalf("expected the named watchdog error, got %q", gotErr)
	}
	if !strings.Contains(gotErr, "wedge-host/primary-model") {
		t.Fatalf("expected the backend named in the watchdog error, got %q", gotErr)
	}
}

// TestTurnWatchdog_ZeroTextFailover: a zero-text wedge on level 0 must descend to the
// fallback (watchdog errors are qualifying) and the fallback must serve the turn.
func TestTurnWatchdog_ZeroTextFailover(t *testing.T) {
	sdk, _ := setupFallbackAgent(t, `{"provider":"openai","endpoint":"http://unused","model":"m","apiKey":"k"}`)
	cwedge := &RuntimeConfig{Endpoint: "http://wedge-host/primary-model", Model: "wedge-model", TurnDeadlineSeconds: 1}
	cok := &RuntimeConfig{Endpoint: "http://ok-host/fallback-model", Model: "ok-model", TurnDeadlineSeconds: 30}

	serving := func(fa *FolderAgent, level int, ctx context.Context) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			yield("wedge fallback served", nil)
		}
	}
	yieldStream := func(fa *FolderAgent, level int, ctx context.Context) iter.Seq2[string, error] {
		if level == 0 {
			return blockingStream(fa, level, ctx)
		}
		return serving(fa, level, ctx)
	}

	var chunks []string
	var warnings []string
	sdk.runTurnWithRuntimeFallback(context.Background(), "w", &FolderAgent{AgentID: "w"},
		[]*RuntimeConfig{cwedge, cok},
		func(cfg *RuntimeConfig) (*FolderAgent, error) { return &FolderAgent{AgentID: "w"}, nil },
		yieldStream,
		[]func(string){func(w string) { warnings = append(warnings, w) }},
		func(chunk string, err error) bool {
			if err != nil {
				t.Fatalf("unexpected stream error: %v", err)
			}
			chunks = append(chunks, chunk)
			return true
		})
	if strings.Join(chunks, "") != "wedge fallback served" {
		t.Fatalf("expected the fallback to serve the wedged turn, got %q", chunks)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "wedge-host") && strings.Contains(w, "ok-host") && strings.Contains(w, "turn watchdog") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a failover warning naming both backends and the watchdog, got %v", warnings)
	}
}

// TestRuntimeFallback_TurnWatchdog_HttpWedge: end-to-end through the real stack - a model
// backend that accepts the request and never responds. The watchdog must terminate the
// turn well before the HTTP client timeout, with the backend named in the stream error.
func TestRuntimeFallback_TurnWatchdog_HttpWedge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(60 * time.Second):
		}
	}))
	defer srv.Close()
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"wedge-model","apiKey":"test-key","timeoutSeconds":300,"turnDeadlineSeconds":2}`, srv.URL)
	sdk, _ := setupFallbackAgent(t, runtimeJSON)

	start := time.Now()
	var streamErr error
	for _, err := range sdk.generateTurnStreamImpl(context.Background(), "fbagent") {
		if err != nil {
			streamErr = err
			break
		}
		_ = err
	}
	elapsed := time.Since(start)
	if elapsed > 20*time.Second {
		t.Fatalf("turn took %v - the watchdog did not terminate it", elapsed)
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "turn watchdog") {
		t.Fatalf("expected the named watchdog error, got %v", streamErr)
	}
	if !strings.Contains(streamErr.Error(), srv.URL) {
		t.Fatalf("expected the backend named in the watchdog error, got %v", streamErr)
	}
}

// --- Item 3: the fallback switch is visible on the GenerateTurnStream stream ---

// TestGenerateTurnStream_WarningOnStream: the continue-only serving path must carry the
// fallback-engage warning as a warning response (proto field 5), the same shape as the
// add-and-generate path.
func TestGenerateTurnStream_WarningOnStream(t *testing.T) {
	primary := failingServer(t)
	defer primary.Close()
	fallback := okBackendServer(t, "switch visible")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := NewInProcessStream[agentv1.GenerateTurnStreamResponse](ctx, 8)
	var gotText, gotWarning string
	rpcErr := make(chan error, 1)
	go func() {
		defer stream.Close()
		rpcErr <- sdk.GenerateTurnStream(&agentv1.GenerateTurnStreamRequest{AgentId: "fbagent"}, stream)
	}()

	for resp := range stream.Chunks() {
		if resp.GetWarning() != "" {
			gotWarning = resp.GetWarning()
		}
		if resp.GetText() != "" {
			gotText += resp.GetText()
		}
	}
	select {
	case err := <-rpcErr:
		if err != nil {
			t.Fatalf("RPC error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the streaming RPC to return")
	}
	if gotText != "switch visible" {
		t.Fatalf("expected the fallback text, got %q", gotText)
	}
	if !strings.Contains(gotWarning, primary.URL) || !strings.Contains(gotWarning, fallback.URL) {
		t.Fatalf("expected a warning naming both backends on the stream, got %q", gotWarning)
	}
}
