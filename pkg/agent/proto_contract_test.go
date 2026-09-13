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

	// 2. Parity check: legacy signature returns identical slice.
	legacyIDs, err := sdk.ListAgentsLegacy()
	if err != nil {
		t.Fatalf("ListAgentsLegacy call failed: %v", err)
	}
	slices.Sort(legacyIDs)
	if !reflect.DeepEqual(got, legacyIDs) {
		t.Fatalf("proto IDs %v != legacy IDs %v", got, legacyIDs)
	}

	// 3. Explicit WorkspaceDir override in request struct.
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
