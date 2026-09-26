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
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/genai"
)

// TestCompactionFallback_CLIPath_HealthyFallbackEngages drives the real SDK CompactSession
// surface (LoadFolderAgentWithA2A -> CheckAndCompactSessionWithFallback) against a primary
// whose dial fails instantly (connection refused - a transport error that is immediately
// qualifying and involves NO openai-go retry backoff, so it is deterministic) and a
// healthy fallback.
func TestCompactionFallback_CLIPath_HealthyFallbackEngages(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	agentID := "cfagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("cli fallback agent"), 0644); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
		genai.NewContentFromText("user two", "user"),
		genai.NewContentFromText("model two", "model"),
	}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(agentDir, "Initial Memory"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// Primary: grab a listener and close it immediately so its port refuses connections.
	// Port 1 is never bound on this machine; dialing it fails immediately with
	// connection refused (a qualifying transport error). A fixed port avoids the
	// listen-then-close race where another concurrent test's httptest could steal
	// the ephemeral port.
	deadPrimaryURL := "http://127.0.0.1:1"

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = ioReadAllBody(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"compacted by cli fallback"},"finish_reason":"stop"}]}`)
	}))
	defer fallback.Close()

	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"primary-model","apiKey":"k","fallback":{"provider":"openai","endpoint":%q,"model":"fallback-model","apiKey":"k"}}`, deadPrimaryURL, fallback.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}

	// Restore CWD semantics for ValidateAgentTarget.
	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	sdk := NewSDK(wsDir)
	sdk.CommandTimeoutSeconds = 30
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := sdk.CompactSession(ctx, &agentv1.CompactSessionRequest{
		AgentId: agentID,
		Force:   true,
	})
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if !resp.GetCompacted() {
		t.Fatal("expected compaction to succeed via fallback after primary dial failure")
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if !strings.Contains(mem, "compacted by cli fallback") {
		t.Fatalf("memory should contain fallback summary, got %q", mem)
	}
}

func ioReadAllBody(r *http.Request) ([]byte, error) {
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return b, nil
}
