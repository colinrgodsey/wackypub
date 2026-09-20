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
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

func TestSDKAddUserTurnAndReadSession(t *testing.T) {
	tempDir := t.TempDir()
	sdk := NewSDK(tempDir)

	agentID := "test_hero"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("test_hero\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	if _, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
		AgentId: agentID,
		Message: "What is your quest?",
	}); err != nil {
		t.Fatalf("failed to add user turn via SDK: %v", err)
	}

	resp, err := sdk.ReadSession(context.Background(), &agentv1.ReadSessionRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("failed to read session via SDK: %v", err)
	}
	turns := resp.GetTurns()
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}

	if turns[0].GetRole() != "user" || len(turns[0].GetParts()) == 0 || turns[0].GetParts()[0].GetText() != "What is your quest?" {
		t.Errorf("turn contents mismatch: %+v", turns[0])
	}
}

func TestSDKReadMemory(t *testing.T) {
	tempDir := t.TempDir()
	sdk := NewSDK(tempDir)

	agentID := "test_wizard"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("test_wizard\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	resp, err := sdk.ReadMemory(context.Background(), &agentv1.ReadMemoryRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("unexpected error reading non-existent memory: %v", err)
	}
	if resp.GetMemoryMd() != "" {
		t.Errorf("expected empty memory, got %s", resp.GetMemoryMd())
	}

	if err := WriteMemoryFile(agentDir, "Fact: Wizard knows fireball."); err != nil {
		t.Fatalf("failed writing memory: %v", err)
	}

	resp, err = sdk.ReadMemory(context.Background(), &agentv1.ReadMemoryRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("failed reading memory via SDK: %v", err)
	}
	if resp.GetMemoryMd() != "Fact: Wizard knows fireball." {
		t.Errorf("memory content mismatch: %s", resp.GetMemoryMd())
	}
}

func TestStreamingAndMultiPartTextPreservation(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Model outputs text narration ("Let me check that for you.") AND calls a tool (create_scratchpad)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Let me check that for you.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_scratchpad","arguments":"{\"text\":\"note\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			// Model gives final answer
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"The result is 42."},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	tempDir := t.TempDir()
	sdk := NewSDK(tempDir)

	agentID := "oracle"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are the Oracle."), 0644); err != nil {
		t.Fatalf("failed writing AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("oracle\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed writing runtime.json: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	ctx := context.Background()

	// 1. Test AddAndGenerateTurnStream yields both chunks in real time
	var chunks []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, agentID, "What is the answer?") {
		if err != nil {
			t.Fatalf("AddAndGenerateTurnStream failed: %v", err)
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}

	if len(chunks) != 2 {
		t.Fatalf("expected 2 yielded chunks (narration + final answer), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0] != "Let me check that for you." {
		t.Errorf("chunk 0 mismatch: %q", chunks[0])
	}
	if chunks[1] != "The result is 42." {
		t.Errorf("chunk 1 mismatch: %q", chunks[1])
	}

	// 2. Test AddAndGenerateTurn collects and joins both chunks with \n\n without dropping narration (D69 fix)
	callCount = 0 // Reset server calls for next turn
	fullResp, err := sdk.addAndGenerateTurnImpl(ctx, agentID, "Ask again")
	if err != nil {
		t.Fatalf("AddAndGenerateTurn failed: %v", err)
	}
	expectedFull := "Let me check that for you.\n\nThe result is 42."
	if fullResp.Text != expectedFull {
		t.Errorf("expected joined response %q, got %q (narration dropped!)", expectedFull, fullResp.Text)
	}
}

func TestStreamingEarlyBreakReleasesLock(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Chunk 1","tool_calls":[{"id":"c1","type":"function","function":{"name":"create_scratchpad","arguments":"{\"text\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Chunk 2"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	tempDir := t.TempDir()
	sdk := NewSDK(tempDir)

	agentID := "streamer"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Streamer agent"), 0644); err != nil {
		t.Fatalf("failed writing AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("streamer\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed writing runtime.json: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	ctx := context.Background()

	// Break early after first chunk
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, agentID, "Hello") {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if chunk == "Chunk 1" {
			break // Early break
		}
	}

	// Verify session lock was released cleanly: a subsequent call must acquire lock without blocking/failing
	callCount = 1 // Next call returns Chunk 2
	resp, err := sdk.addAndGenerateTurnImpl(ctx, agentID, "Follow up")
	if err != nil {
		t.Fatalf("subsequent call failed (lock held?): %v", err)
	}
	if resp.Text != "Chunk 2" {
		t.Errorf("expected 'Chunk 2', got %q", resp.Text)
	}
}

func TestSDK_CancelTurn(t *testing.T) {
	tempDir := t.TempDir()
	sdk := NewSDK(tempDir)

	// 1. CancelTurn with nothing in flight returns an error naming the agent
	if _, err := sdk.CancelTurn(context.Background(), &agentv1.CancelTurnRequest{AgentId: "nonexistent"}); err == nil || !strings.Contains(err.Error(), "no in-flight turn for agent \"nonexistent\"") {
		t.Fatalf("expected no in-flight turn error, got: %v", err)
	}

	agentID := "cancel_agent"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Cancel test agent"), 0644); err != nil {
		t.Fatalf("failed writing AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("cancel_agent\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}

	requestStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer srv.Close()

	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed writing runtime.json: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	streamDone := make(chan error, 1)
	go func() {
		var streamErr error
		for _, err := range sdk.addAndGenerateTurnStreamImpl(context.Background(), agentID, "Hello") {
			if err != nil {
				streamErr = err
				break
			}
		}
		streamDone <- streamErr
	}()

	// Wait until the mock model has received the request
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request to start")
	}

	// Cancel the in-flight turn
	if _, err := sdk.CancelTurn(context.Background(), &agentv1.CancelTurnRequest{AgentId: agentID}); err != nil {
		t.Fatalf("CancelTurn failed: %v", err)
	}

	// Assert stream finishes promptly with cancellation error
	select {
	case err := <-streamDone:
		if err == nil {
			t.Fatal("expected non-nil error when turn is cancelled, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancelled stream to terminate")
	}

	// After stream completion, CancelTurn must again report no in-flight turn
	if _, err := sdk.CancelTurn(context.Background(), &agentv1.CancelTurnRequest{AgentId: agentID}); err == nil || !strings.Contains(err.Error(), "no in-flight turn") {
		t.Fatalf("expected no in-flight turn after completion, got: %v", err)
	}
}

func TestSDK_CancelTurn_ConcurrentSafety(t *testing.T) {
	tempDir := t.TempDir()
	sdk := NewSDK(tempDir)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			agent := fmt.Sprintf("agent_%d", id%5)
			_, _ = sdk.CancelTurn(context.Background(), &agentv1.CancelTurnRequest{AgentId: agent})
		}(i)
	}
	wg.Wait()
}

func TestD93_InspectSessionContext(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	sdk := NewSDK(wsDir)
	agentDir := filepath.Join(wsDir, "testbot")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	runtimeJSON := `{"model":"deepseek-chat","contextWindow":100000}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are a helpful test agent."), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "MEMORY.md"), []byte("# Long term memory notes"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Initial report without session
	rep, err := sdk.InspectSessionContext(context.Background(), &agentv1.InspectSessionContextRequest{AgentId: "testbot"})
	if err != nil {
		t.Fatalf("InspectSessionContext failed: %v", err)
	}
	if rep.GetAgentId() != "testbot" || rep.GetModel() != "deepseek-chat" {
		t.Errorf("unexpected report meta: %+v", rep)
	}
	if rep.GetContextWindow() != 100000 || rep.GetCompactionThreshold() != 80000 {
		t.Errorf("unexpected window/threshold: %d / %d", rep.GetContextWindow(), rep.GetCompactionThreshold())
	}
	if rep.GetPromptTokensEstimate() == 0 || rep.GetMemoryTokensEstimate() == 0 {
		t.Errorf("expected nonzero prompt/memory tokens: prompt=%d mem=%d", rep.GetPromptTokensEstimate(), rep.GetMemoryTokensEstimate())
	}

	// 2. Add turns
	_ = AppendSessionTurn(agentDir, "user", "Hello world from user")
	_ = AppendSessionTurn(agentDir, "model", "Hello back from model")

	rep2, err := sdk.InspectSessionContext(context.Background(), &agentv1.InspectSessionContextRequest{AgentId: "testbot"})
	if err != nil {
		t.Fatalf("InspectSessionContext failed: %v", err)
	}
	if rep2.GetTurnCount() != 2 || rep2.GetSessionTurnsTokens() == 0 {
		t.Errorf("expected turns counted: turns=%d tokens=%d", rep2.GetTurnCount(), rep2.GetSessionTurnsTokens())
	}

	// 3. Write .last_usage.json sidecar
	lastUsage := &LastUsageRecord{
		PromptTokens:     1500,
		CandidatesTokens: 250,
		TotalTokens:      1750,
		Timestamp:        time.Now(),
	}
	if err := WriteLastUsage(agentDir, lastUsage); err != nil {
		t.Fatalf("WriteLastUsage failed: %v", err)
	}

	rep3, err := sdk.InspectSessionContext(context.Background(), &agentv1.InspectSessionContextRequest{AgentId: "testbot"})
	if err != nil {
		t.Fatalf("InspectSessionContext failed: %v", err)
	}
	if rep3.GetLastTotalTokens() != 1750 || rep3.GetLastPromptTokens() != 1500 {
		t.Errorf("expected last usage reflected: %+v", rep3)
	}

	// 4. Invalidate on compaction
	if err := InvalidateLastUsage(agentDir); err != nil {
		t.Fatalf("InvalidateLastUsage failed: %v", err)
	}
	rep4, err := sdk.InspectSessionContext(context.Background(), &agentv1.InspectSessionContextRequest{AgentId: "testbot"})
	if err != nil {
		t.Fatalf("InspectSessionContext failed: %v", err)
	}
	if !rep4.GetCompacted() || rep4.GetLastTotalTokens() != 0 {
		t.Errorf("expected compacted invalidation reflected: %+v", rep4)
	}
}
