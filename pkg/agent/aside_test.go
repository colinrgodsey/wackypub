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
	"testing"
)

// asideMockModelServer returns a mock OpenAI endpoint whose response text embeds
// the answer marker and (when enabled) the tool-call flow.
func asideMockServer(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, answer))
	}))
}

// asideWorkspace builds a workspace with one agent whose session is populated with
// history (so the aside has context to draw on) and returns the SDK + agentDir.
func asideWorkspace(t *testing.T, answer string) (*AgentSDK, string) {
	t.Helper()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "asideagent"
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
	srv := asideMockServer(t, answer)
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k"}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}
	// Populated session: user + model + user, so the aside has context.
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
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	return NewSDK(wsDir), agentDir
}

// TestAside_ContextualAnswerAndNothingPersisted is the acceptance core: an aside on a
// populated session returns a contextual answer, and the main session is byte-identical
// after - session.jsonl, MEMORY.md, and the agent directory tree all unchanged.
func TestAside_ContextualAnswerAndNothingPersisted(t *testing.T) {
	sdk, agentDir := asideWorkspace(t, "project X state is known")

	// Snapshot every writeable surface before the aside.
	snapshot := func() map[string][]byte {
		m := make(map[string][]byte)
		for _, name := range []string{"session.jsonl", "MEMORY.md", "runtime.json", "AGENTS.md", RootMarkerFile} {
			p := filepath.Join(agentDir, name)
			if b, err := os.ReadFile(p); err == nil {
				m[name] = b
			}
		}
		return m
	}
	before := snapshot()

	result, err := sdk.asideTurn(context.Background(), "asideagent", "what is the project state?")
	if err != nil {
		t.Fatalf("asideTurn: %v", err)
	}
	if result.Text == "" {
		t.Fatal("aside returned empty text")
	}
	if !strings.Contains(result.Text, "project X state is known") {
		t.Fatalf("aside answer should be contextual, got %q", result.Text)
	}

	after := snapshot()
	for name, beforeBytes := range before {
		afterBytes, ok := after[name]
		if !ok {
			t.Fatalf("file %s missing after aside", name)
		}
		if string(beforeBytes) != string(afterBytes) {
			t.Fatalf("%s changed after aside:\nbefore: %q\nafter:  %q", name, beforeBytes, afterBytes)
		}
	}
	// No new files appeared in the agent dir (e.g. no .last_usage.json, no trace artifacts).
	entries, _ := os.ReadDir(agentDir)
	expected := map[string]bool{"session.jsonl": true, "MEMORY.md": true, "runtime.json": true, "AGENTS.md": true, AllowedAgentsFile: true, RootMarkerFile: true, SeqFileName: true, sessionLockFileName: true}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !expected[e.Name()] {
			t.Fatalf("unexpected file appeared in agent dir after aside: %s", e.Name())
		}
	}
	if result.Usage.TotalTokens == 0 {
		t.Log("note: usage metadata empty (mock provider may not report usage)")
	}
}

// TestAside_ToolInvocationDenied pins acceptance 3: an aside whose model emits a
// functionCall receives the denial and completes; the tool is never executed and the
// denial count is surfaced on the result.
func TestAside_ToolInvocationDenied(t *testing.T) {
	// Mock: first call returns a functionCall to create_scratchpad, second call returns the final
	// text once the denied functionResponse is fed back.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_scratchpad","arguments":"{\"text\":\"hello\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		io.WriteString(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Done after denial"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "denyagent"
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
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k"}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}
	if err := AppendSessionTurn(agentDir, "user", "hello"); err != nil {
		t.Fatalf("append: %v", err)
	}
	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	sdk := NewSDK(wsDir)
	result, err := sdk.asideTurn(context.Background(), agentID, "echo hi")
	if err != nil {
		t.Fatalf("asideTurn: %v", err)
	}
	if !strings.Contains(result.Text, "Done after denial") {
		t.Fatalf("aside should complete after denial, got %q", result.Text)
	}
	if result.ToolDenials != 1 {
		t.Fatalf("expected 1 tool denial, got %d", result.ToolDenials)
	}
}

// TestAside_DoesNotContendWithLiveTurnLock pins the archon lock discipline: an aside runs
// while another process holds the exclusive session lock - it must NOT block on it (no
// AcquireSessionLock), while a normal generate turn would.
func TestAside_DoesNotContendWithLiveTurnLock(t *testing.T) {
	sdk, agentDir := asideWorkspace(t, "lock-safe answer")

	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	result, err := sdk.asideTurn(context.Background(), "asideagent", "are you there?")
	if err != nil {
		t.Fatalf("aside should succeed while session lock is held: %v", err)
	}
	if !strings.Contains(result.Text, "lock-safe answer") {
		t.Fatalf("unexpected answer: %q", result.Text)
	}
}
