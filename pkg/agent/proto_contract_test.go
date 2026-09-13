package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// Compile-time interface assertion: AgentSDK must satisfy AgentServiceServer (D112).
var _ agentv1.AgentServiceServer = (*AgentSDK)(nil)

func TestProtoContract_ListAgents(t *testing.T) {
	wsDir := t.TempDir()

	// Create two agent directories recognized by ListAgentIDs (containing AGENTS.md).
	agentA := filepath.Join(wsDir, "agent-alpha")
	agentB := filepath.Join(wsDir, "agent-beta")
	nonAgent := filepath.Join(wsDir, "not-an-agent")

	if err := os.MkdirAll(agentA, 0755); err != nil {
		t.Fatalf("mkdir agentA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentA, "AGENTS.md"), []byte("Alpha prompt"), 0644); err != nil {
		t.Fatalf("write agentA AGENTS.md: %v", err)
	}

	if err := os.MkdirAll(agentB, 0755); err != nil {
		t.Fatalf("mkdir agentB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentB, "AGENTS.md"), []byte("Beta prompt"), 0644); err != nil {
		t.Fatalf("write agentB AGENTS.md: %v", err)
	}

	// Plain directory without marker files should not be recognized as an agent.
	if err := os.MkdirAll(nonAgent, 0755); err != nil {
		t.Fatalf("mkdir nonAgent: %v", err)
	}

	sdk := NewSDK(wsDir)

	ctx := context.Background()

	// 1. Unparameterized / default workspace dir request.
	req := &agentv1.ListAgentsRequest{}
	resp, err := sdk.ListAgents(ctx, req)
	if err != nil {
		t.Fatalf("ListAgents proto call failed: %v", err)
	}
	if resp == nil {
		t.Fatal("ListAgents returned nil response")
	}

	got := resp.GetAgentIds()
	slices.Sort(got)
	want := []string{"agent-alpha", "agent-beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAgents proto response got %v, want %v", got, want)
	}

	// 2. Explicit WorkspaceDir override in request struct.
	emptyWs := t.TempDir()
	overrideReq := &agentv1.ListAgentsRequest{WorkspaceDir: emptyWs}
	overrideResp, err := sdk.ListAgents(ctx, overrideReq)
	if err != nil {
		t.Fatalf("ListAgents with override failed: %v", err)
	}
	if len(overrideResp.GetAgentIds()) != 0 {
		t.Fatalf("expected 0 agents in empty workspace, got %v", overrideResp.GetAgentIds())
	}
}

func TestProtoContract_Phase1Queries(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "testagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Setup agent files
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are testagent."), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "MEMORY.md"), []byte("Secret memory"), 0644); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}
	runtimeJSON := `{"model":"gemini-2.5-flash","contextWindow":128000}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// 1. InspectAgent
	inspResp, err := sdk.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("InspectAgent failed: %v", err)
	}
	if !inspResp.GetAgentDirExists() || inspResp.GetAgentId() != agentID {
		t.Fatalf("unexpected InspectAgent response: %+v", inspResp)
	}
	if !inspResp.GetAgentsMdExists() || !inspResp.GetMemoryMdExists() || !inspResp.GetRuntimeJsonExists() {
		t.Fatalf("expected files to be reported present: %+v", inspResp)
	}

	// 2. ReadMemory
	memResp, err := sdk.ReadMemory(ctx, &agentv1.ReadMemoryRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadMemory failed: %v", err)
	}
	if memResp.GetMemoryMd() != "Secret memory" {
		t.Fatalf("expected 'Secret memory', got %q", memResp.GetMemoryMd())
	}

	// 3. RenderSystemPrompt
	promptResp, err := sdk.RenderSystemPrompt(ctx, &agentv1.RenderSystemPromptRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("RenderSystemPrompt failed: %v", err)
	}
	if promptResp.GetRenderedPrompt() != "You are testagent." {
		t.Fatalf("expected rendered prompt, got %q", promptResp.GetRenderedPrompt())
	}

	// 4. ReadSession (empty)
	sessResp, err := sdk.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if len(sessResp.GetTurns()) != 0 {
		t.Fatalf("expected 0 turns, got %d", len(sessResp.GetTurns()))
	}

	// Add a turn and re-read
	_ = AppendSessionTurn(agentDir, "user", "Hello world")
	sessResp2, err := sdk.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSession turn 2 failed: %v", err)
	}
	if len(sessResp2.GetTurns()) != 1 || sessResp2.GetTurns()[0].GetRole() != "user" {
		t.Fatalf("expected 1 user turn, got %+v", sessResp2.GetTurns())
	}

	// 5. InspectSessionContext
	ctxResp, err := sdk.InspectSessionContext(ctx, &agentv1.InspectSessionContextRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("InspectSessionContext failed: %v", err)
	}
	if ctxResp.GetAgentId() != agentID || ctxResp.GetModel() != "gemini-2.5-flash" || ctxResp.GetContextWindow() != 128000 {
		t.Fatalf("unexpected session context response: %+v", ctxResp)
	}

	// 6. InspectAgentLocks
	locksResp, err := sdk.InspectAgentLocks(ctx, &agentv1.InspectAgentLocksRequest{})
	if err != nil {
		t.Fatalf("InspectAgentLocks failed: %v", err)
	}
	if len(locksResp.GetObservations()) != 1 || locksResp.GetObservations()[0].GetAgentId() != agentID {
		t.Fatalf("unexpected locks observations: %+v", locksResp.GetObservations())
	}
}
