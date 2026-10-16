package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuntimeFallback_MidRunAfterToolCall_Failover pins the D1(b) sub-case: the primary
// succeeds its first model call (a tool call the runner executes and persists), then 429s on
// the second model call. The session is now mid-run. The zero-text failover must not be
// rejected by the ends-on-user-turn guard, and the fallback must serve the turn.
func TestRuntimeFallback_MidRunAfterToolCall_Failover(t *testing.T) {
	calls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"bash\",\"args\":[\"-c\",\"echo TOOLRAN\"]}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2099-01-02 03:04:05"}}`)
	}))
	defer primary.Close()
	fallback := okBackendServer(t, "fallback answered after tool")
	defer fallback.Close()

	sdk, agentDir := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))

	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	if err := os.Symlink("/bin/bash", filepath.Join(toolsDir, "bash")); err != nil {
		t.Fatalf("symlink bash: %v", err)
	}

	var warnings []string
	ctx := context.Background()
	var chunks []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "run the tool", func(w string) { warnings = append(warnings, w) }) {
		if err != nil {
			t.Fatalf("stream error (mid-run failover should have switched to the fallback): %v", err)
		}
		chunks = append(chunks, chunk)
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "fallback answered after tool") {
		t.Fatalf("expected the fallback to serve the turn after a mid-run tool call, got %q", joined)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, primary.URL) && strings.Contains(w, fallback.URL) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the failover warning naming both backends, got %v", warnings)
	}
}
