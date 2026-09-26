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

	"google.golang.org/genai"
)

// TestCompactionFallback_InTurnContinuationSucceeds drives the mid-turn bail surface
// (FolderAgent.compactForContinuation) against a workspace whose primary backend refuses
// connections (immediately qualifying, no retry timing) and a healthy fallback. The
// continuation compaction must SUCCEED and produce a summary from the fallback instead of
// aborting with "session compaction error".
func TestCompactionFallback_InTurnContinuationSucceeds(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	agentID := "cfinturn"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("in-turn fallback agent"), 0644); err != nil {
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

	// Port 1 is never bound on this machine; dialing it fails immediately with
	// connection refused (a qualifying transport error). A fixed port avoids the
	// listen-then-close race where another concurrent test's httptest could steal
	// the ephemeral port.
	deadPrimary := "http://127.0.0.1:1"

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = ioReadAllBody(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"in-turn fallback summary"},"finish_reason":"stop"}]}`)
	}))
	defer fallback.Close()

	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"primary-model","apiKey":"k","fallback":{"provider":"openai","endpoint":%q,"model":"fallback-model","apiKey":"k"}}`, deadPrimary, fallback.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	fa, err := LoadFolderAgentWithHookEnv(wsDir, agentID, nil, nil, DefaultMaxToolTurns, 30)
	if err != nil {
		t.Fatalf("load folder agent: %v", err)
	}

	var yielded string
	success := fa.compactForContinuation(context.Background(), func(chunk string, err error) bool {
		yielded += chunk
		return true
	})
	if !success {
		t.Fatalf("compactForContinuation failed; yielded so far: %q", yielded)
	}
	if strings.Contains(yielded, "session compaction error") {
		t.Fatalf("continuation aborted with compaction error: %q", yielded)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if !strings.Contains(mem, "in-turn fallback summary") {
		t.Fatalf("memory should contain the in-turn fallback summary, got %q", mem)
	}
}
