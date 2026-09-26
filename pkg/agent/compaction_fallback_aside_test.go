package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAsideFallback_DeadPrimaryDescendsToHealthyFallback pins agy Finding 1: aside must
// attempt level 1 after a qualifying zero-text error on the primary (before the fix the
// unconditional outer break returned empty text with nil error). Dead primary = port 1
// (instant dial-refused, no retry timing); fallback answers "aside from the fallback".
func TestAsideFallback_DeadPrimaryDescendsToHealthyFallback(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "asidefallback"
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

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = ioReadAllBody(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"aside from the fallback"},"finish_reason":"stop"}]}`)
	}))
	defer fallback.Close()

	// Port 1 is never bound: the primary dial fails instantly (connection refused), a
	// qualifying transport error, with no retry backoff to make the test racy.
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":"http://127.0.0.1:1","model":"primary-model","apiKey":"k","fallback":{"provider":"openai","endpoint":%q,"model":"fallback-model","apiKey":"k"}}`, fallback.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "question one"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := AppendSessionTurn(agentDir, "model", "answer one"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := WriteMemoryFile(agentDir, "persistent state: project X"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	sdk := NewSDK(wsDir)
	sdk.CommandTimeoutSeconds = 30

	result, err := sdk.asideTurn(context.Background(), agentID, "what is the state?")
	if err != nil {
		t.Fatalf("asideTurn: %v", err)
	}
	if !strings.Contains(result.Text, "aside from the fallback") {
		t.Fatalf("aside should descend to the fallback after primary dial failure, got %q (warnings: %v)", result.Text, result.Warnings)
	}
}
