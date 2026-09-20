package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- Load-time: nested parsing + depth cap ---

func TestRuntimeFallback_NestedParsing(t *testing.T) {
	agentDir := t.TempDir()
	runtimeJSON := `{
		"provider": "openai",
		"endpoint": "https://primary.example",
		"model": "primary-model",
		"apiKey": "k1",
		"fallback": {
			"provider": "openai",
			"endpoint": "https://fallback1.example",
			"model": "fb1-model",
			"apiKey": "k2",
			"fallback": {
				"provider": "gemini",
				"model": "fb2-model",
				"apiKey": "k3"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	chain := cfg.FallbackChain()
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	if chain[0].Endpoint != "https://primary.example" || chain[1].Endpoint != "https://fallback1.example" || chain[2].Provider != "gemini" {
		t.Fatalf("chain order wrong: %+v", chain)
	}
	// Each level is fully-specified with its own provider normalization applied.
	if chain[2].Model != "fb2-model" || chain[2].APIKey != "k3" {
		t.Fatalf("fallback level not self-describing: %+v", chain[2])
	}
}

func TestRuntimeFallback_DepthCap(t *testing.T) {
	agentDir := t.TempDir()
	// Build a chain one deeper than MaxFallbackDepth.
	depth := MaxFallbackDepth + 2
	var b strings.Builder
	b.WriteString(`{"provider":"openai","endpoint":"e0","model":"m0","apiKey":"k"`)
	for i := 1; i <= depth; i++ {
		b.WriteString(fmt.Sprintf(`,"fallback":{"provider":"openai","endpoint":"e%d","model":"m%d","apiKey":"k"`, i, i))
	}
	for i := 0; i <= depth; i++ {
		b.WriteString("}")
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadRuntimeConfig(agentDir)
	if err == nil {
		t.Fatal("expected depth cap error, got nil")
	}
	if !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("error should mention depth cap, got: %v", err)
	}
}

// --- Qualification matrix ---

func TestRuntimeFallback_IsQualifyingFallbackError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Qualifying: transport
		{name: "connection refused", err: fmt.Errorf("connection refused"), want: true},
		{name: "connection reset", err: fmt.Errorf("connection reset by peer"), want: true},
		{name: "DNS no such host", err: fmt.Errorf("no such host"), want: true},
		{name: "timeout", err: fmt.Errorf("context deadline exceeded (Client.Timeout)"), want: true},
		{name: "dial lookup", err: fmt.Errorf("dial tcp: lookup api.example: no such host"), want: true},
		{name: "tls handshake", err: fmt.Errorf("tls handshake timeout"), want: true},
		// Qualifying: 429 after retries
		{name: "429", err: fmt.Errorf("StatusCode: 429, Rate limit exceeded"), want: true},
		{name: "rate limit", err: fmt.Errorf("rate limit exceeded"), want: true},
		{name: "too many requests", err: fmt.Errorf("too many requests"), want: true},
		// Qualifying: 5xx
		{name: "500", err: fmt.Errorf("StatusCode: 500"), want: true},
		{name: "502", err: fmt.Errorf("bad gateway 502"), want: true},
		{name: "503", err: fmt.Errorf("StatusCode: 503 Service Unavailable"), want: true},
		{name: "504", err: fmt.Errorf("upstream timeout 504"), want: true},
		{name: "generic internal error string without code", err: fmt.Errorf("internal server error"), want: false},
		// NOT qualifying: auth
		{name: "401", err: fmt.Errorf("StatusCode: 401 Unauthorized"), want: false},
		{name: "403", err: fmt.Errorf("StatusCode: 403 Forbidden"), want: false},
		{name: "invalid api key", err: fmt.Errorf("invalid api key"), want: false},
		// NOT qualifying: empty model output
		{name: "empty response", err: fmt.Errorf("received empty response from agent"), want: false},
		// NOT qualifying: other
		{name: "nil", err: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsQualifyingFallbackError(c.err); got != c.want {
				t.Fatalf("IsQualifyingFallbackError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// --- Integration: failover, fail-forward-per-turn, warning text, no-fallback regression ---

// failingServer returns 503 on every request (transport-class qualifying failure).
func failingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"service unavailable"}}`)
	}))
}

func okBackendServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, text))
	}))
}

// writeFallbackRuntime writes a runtime.json with primary -> fallback pointing at the two
// servers, and returns the written path.
func writeFallbackRuntime(t *testing.T, agentDir string, primaryURL, fallbackURL string) {
	t.Helper()
	runtimeJSON := fmt.Sprintf(`{
		"provider": "openai",
		"endpoint": %q,
		"model": "primary-model",
		"apiKey": "test-key",
		"fallback": {
			"provider": "openai",
			"endpoint": %q,
			"model": "fallback-model",
			"apiKey": "test-key"
		}
	}`, primaryURL, fallbackURL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatal(err)
	}
}

// setupFallbackAgent builds a workspace with one agent whose session ends on a user turn.
func setupFallbackAgent(t *testing.T, runtimeJSON string) (*AgentSDK, string) {
	t.Helper()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "fbagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("test agent"), 0644); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}
	if err := AppendSessionTurn(agentDir, "user", "hello"); err != nil {
		t.Fatalf("append session: %v", err)
	}
	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	return NewSDK(wsDir), agentDir
}

func TestRuntimeFallback_FailoverOnTransportAndWarning(t *testing.T) {
	primary := failingServer(t)
	defer primary.Close()
	fallback := okBackendServer(t, "fallback answered")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))

	var warnings []string
	onWarning := func(w string) { warnings = append(warnings, w) }
	ctx := context.Background()

	var chunks []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "question", onWarning) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		chunks = append(chunks, chunk)
	}

	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "fallback answered") {
		t.Fatalf("expected fallback text, got %q", joined)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 fallback warning, got %d: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if !strings.Contains(w, primary.URL) || !strings.Contains(w, fallback.URL) {
		t.Fatalf("warning should name both backends: %q", w)
	}
}

func TestRuntimeFallback_FailForwardPerTurn(t *testing.T) {
	// Primary fails for the whole first turn (openai-go retries 503 up to maxRetries, so
	// a by-call-count flip can succeed mid-retry); fail-forward-per-turn must retry primary
	// first on the next turn once we clear the failure flag.
	var mu sync.Mutex
	primaryBroken := true
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		broken := primaryBroken
		mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		if broken {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"primary answered now"},"finish_reason":"stop"}]}`)
	}))
	defer primary.Close()
	fallback := okBackendServer(t, "fallback answered")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))
	ctx := context.Background()

	// Turn 1: primary 503s -> fallback engages.
	var chunk1 []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "q1") {
		if err != nil {
			t.Fatalf("turn 1 error: %v", err)
		}
		chunk1 = append(chunk1, chunk)
	}
	if !strings.Contains(strings.Join(chunk1, ""), "fallback answered") {
		t.Fatalf("turn 1 should come from fallback, got %q", strings.Join(chunk1, ""))
	}

	mu.Lock()
	primaryBroken = false
	mu.Unlock()

	// Turn 2: primary healthy -> primary-first again (never sticky).
	var chunk2 []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "q2") {
		if err != nil {
			t.Fatalf("turn 2 error: %v", err)
		}
		chunk2 = append(chunk2, chunk)
	}
	if !strings.Contains(strings.Join(chunk2, ""), "primary answered now") {
		t.Fatalf("turn 2 should come from primary again, got %q", strings.Join(chunk2, ""))
	}
}

func TestRuntimeFallback_NoFallbackRegression(t *testing.T) {
	srv := okBackendServer(t, "single backend")
	defer srv.Close()
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k"}`, srv.URL)
	sdk, _ := setupFallbackAgent(t, runtimeJSON)

	ctx := context.Background()
	var chunks []string
	var gotErr error
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "question") {
		if err != nil {
			gotErr = err
			break
		}
		chunks = append(chunks, chunk)
	}
	if gotErr != nil {
		t.Fatalf("unexpected error: %v", gotErr)
	}
	if !strings.Contains(strings.Join(chunks, ""), "single backend") {
		t.Fatalf("expected single backend text, got %q", strings.Join(chunks, ""))
	}
}

func TestRuntimeFallback_NonQualifyingFailureDoesNotFallback(t *testing.T) {
	// 401 must fail loud, NOT flip to fallback.
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Unauthorized"}}`)
	}))
	defer authServer.Close()
	fallback := okBackendServer(t, "fallback answered")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, authServer.URL, fallback.URL))

	ctx := context.Background()
	var warnings []string
	var gotErr error
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "question", func(w string) { warnings = append(warnings, w) }) {
		if err != nil {
			gotErr = err
			break
		}
		_ = chunk
	}
	if gotErr == nil {
		t.Fatal("expected 401 to fail loud, got no error")
	}
	if len(warnings) != 0 {
		t.Fatalf("401 should not trigger fallback warning, got %v", warnings)
	}
}

func TestRuntimeFallback_GeneratePathFailsForwardToo(t *testing.T) {
	// The continue-only generation path (no user message) must also fail over.
	primary := failingServer(t)
	defer primary.Close()
	fallback := okBackendServer(t, "gen fallback answered")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))

	ctx := context.Background()
	var chunks []string
	for chunk, err := range sdk.generateTurnStreamImpl(ctx, "fbagent") {
		if err != nil {
			t.Fatalf("generate stream error: %v", err)
		}
		chunks = append(chunks, chunk)
	}
	if !strings.Contains(strings.Join(chunks, ""), "gen fallback answered") {
		t.Fatalf("generate path should fail over, got %q", strings.Join(chunks, ""))
	}
}

func mustFallbackRuntime(t *testing.T, primaryURL, fallbackURL string) string {
	t.Helper()
	return fmt.Sprintf(`{
		"provider": "openai",
		"endpoint": %q,
		"model": "primary-model",
		"apiKey": "test-key",
		"fallback": {
			"provider": "openai",
			"endpoint": %q,
			"model": "fallback-model",
			"apiKey": "test-key"
		}
	}`, primaryURL, fallbackURL)
}
