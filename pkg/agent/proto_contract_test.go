package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"google.golang.org/genai"

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

// TestPhase1MethodParity acts as an automated parity test harness (D112)
// asserting field-by-field parity between the protobuf service interface and
// the unexported legacy methods for ALL 7 Phase 1 operations.
func TestPhase1MethodParity(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	agentAlpha := "agent-alpha"
	alphaDir := filepath.Join(wsDir, agentAlpha)
	if err := os.MkdirAll(alphaDir, 0755); err != nil {
		t.Fatalf("mkdir alpha: %v", err)
	}

	agentBeta := "agent-beta"
	betaDir := filepath.Join(wsDir, agentBeta)
	if err := os.MkdirAll(betaDir, 0755); err != nil {
		t.Fatalf("mkdir beta: %v", err)
	}

	// Setup agent-alpha with comprehensive files
	if err := os.WriteFile(filepath.Join(alphaDir, "AGENTS.md"), []byte("Alpha system prompt with macro context."), 0644); err != nil {
		t.Fatalf("write alpha AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alphaDir, "MEMORY.md"), []byte("Alpha long-term memory notes."), 0644); err != nil {
		t.Fatalf("write alpha MEMORY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alphaDir, ".env"), []byte("FOO=BAR\n"), 0644); err != nil {
		t.Fatalf("write alpha .env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alphaDir, AllowedAgentsFile), []byte("agent-beta\n"), 0644); err != nil {
		t.Fatalf("write alpha allowed_agents: %v", err)
	}

	runtimeJSON := `{
		"provider": "anthropic",
		"endpoint": "https://api.anthropic.com",
		"model": "claude-3-5-sonnet",
		"apiKey": "test-key-alpha",
		"contextWindow": 200000,
		"timeoutSeconds": 180,
		"anthropicThinkingBudgetTokens": 2048,
		"anthropicThinkingEffort": "high",
		"anthropicThinkingMode": "adaptive"
	}`
	if err := os.WriteFile(filepath.Join(alphaDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write alpha runtime.json: %v", err)
	}

	compactJSON := `{"compact_overhead_pct": 15.0}`
	if err := os.WriteFile(filepath.Join(alphaDir, "compact.json"), []byte(compactJSON), 0644); err != nil {
		t.Fatalf("write alpha compact.json: %v", err)
	}

	// Tools and skills directories
	toolsDir := filepath.Join(alphaDir, ToolsDirName)
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "tool1.sh"), []byte("#!/bin/sh\necho ok"), 0755); err != nil {
		t.Fatalf("write tool1.sh: %v", err)
	}

	skillsDir := filepath.Join(alphaDir, SkillsDirName, "skill1")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatalf("mkdir skill1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte("---\nname: skill1\ndescription: test skill\n---\nBody"), 0644); err != nil {
		t.Fatalf("write skill1 SKILL.md: %v", err)
	}

	// Session turns
	turns := []*genai.Content{
		genai.NewContentFromText("Hello from user", genai.Role("user")),
		{
			Role: "model",
			Parts: []*genai.Part{
				{Text: "Model response text"},
				{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("fake-png-bytes")}},
			},
		},
	}
	if err := WriteSessionTurns(alphaDir, turns); err != nil {
		t.Fatalf("write alpha session: %v", err)
	}

	// Last usage
	if err := WriteLastUsage(alphaDir, &LastUsageRecord{
		PromptTokens:     150,
		CandidatesTokens: 75,
		TotalTokens:      225,
		Compacted:        false,
	}); err != nil {
		t.Fatalf("write alpha last usage: %v", err)
	}

	// Acquire session lock on alpha so lock fields are populated
	alphaLock, err := AcquireSessionLock(alphaDir)
	if err != nil {
		t.Fatalf("acquire alpha lock: %v", err)
	}
	defer alphaLock.Release()

	// Setup agent-beta with minimal files
	if err := os.WriteFile(filepath.Join(betaDir, "AGENTS.md"), []byte("Beta prompt."), 0644); err != nil {
		t.Fatalf("write beta AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(betaDir, "MEMORY.md"), []byte("Beta memory."), 0644); err != nil {
		t.Fatalf("write beta MEMORY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(betaDir, "runtime.json"), []byte(`{"model":"gemini-2.5-flash","contextWindow":128000}`), 0644); err != nil {
		t.Fatalf("write beta runtime.json: %v", err)
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// ==========================================
	// Parity 1: ListAgents vs listAgentsLegacy
	// ==========================================
	protoList, err := sdk.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents proto failed: %v", err)
	}
	legacyList, err := sdk.listAgentsLegacy()
	if err != nil {
		t.Fatalf("listAgentsLegacy failed: %v", err)
	}
	protoIDs := append([]string(nil), protoList.GetAgentIds()...)
	legacyIDs := append([]string(nil), legacyList...)
	slices.Sort(protoIDs)
	slices.Sort(legacyIDs)
	if !reflect.DeepEqual(protoIDs, legacyIDs) {
		t.Fatalf("ListAgents parity mismatch: proto=%v, legacy=%v", protoIDs, legacyIDs)
	}

	// ==========================================
	// Parity 2: InspectAgent vs inspectAgentLegacy
	// ==========================================
	for _, targetID := range []string{agentAlpha, agentBeta, "non-existent-agent"} {
		protoInsp, err := sdk.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: targetID})
		if err != nil {
			t.Fatalf("[%s] InspectAgent proto failed: %v", targetID, err)
		}
		legacyInsp, err := sdk.inspectAgentLegacy(targetID)
		if err != nil {
			t.Fatalf("[%s] inspectAgentLegacy failed: %v", targetID, err)
		}

		if protoInsp.GetAgentId() != legacyInsp.AgentID {
			t.Errorf("[%s] AgentId mismatch: %q vs %q", targetID, protoInsp.GetAgentId(), legacyInsp.AgentID)
		}
		if protoInsp.GetAgentDir() != legacyInsp.AgentDir {
			t.Errorf("[%s] AgentDir mismatch: %q vs %q", targetID, protoInsp.GetAgentDir(), legacyInsp.AgentDir)
		}
		if protoInsp.GetAgentDirExists() != legacyInsp.AgentDirExists {
			t.Errorf("[%s] AgentDirExists mismatch: %v vs %v", targetID, protoInsp.GetAgentDirExists(), legacyInsp.AgentDirExists)
		}
		if protoInsp.GetAgentsMdExists() != legacyInsp.AgentsMDExists {
			t.Errorf("[%s] AgentsMdExists mismatch: %v vs %v", targetID, protoInsp.GetAgentsMdExists(), legacyInsp.AgentsMDExists)
		}
		if protoInsp.GetMemoryMdExists() != legacyInsp.MemoryMDExists {
			t.Errorf("[%s] MemoryMdExists mismatch: %v vs %v", targetID, protoInsp.GetMemoryMdExists(), legacyInsp.MemoryMDExists)
		}
		if protoInsp.GetDotEnvExists() != legacyInsp.DotEnvExists {
			t.Errorf("[%s] DotEnvExists mismatch: %v vs %v", targetID, protoInsp.GetDotEnvExists(), legacyInsp.DotEnvExists)
		}
		if protoInsp.GetRuntimeJsonExists() != legacyInsp.RuntimeJSONExists {
			t.Errorf("[%s] RuntimeJsonExists mismatch: %v vs %v", targetID, protoInsp.GetRuntimeJsonExists(), legacyInsp.RuntimeJSONExists)
		}
		if protoInsp.GetRuntimeJsonIsSymlink() != legacyInsp.RuntimeJSONIsSymlink {
			t.Errorf("[%s] RuntimeJsonIsSymlink mismatch: %v vs %v", targetID, protoInsp.GetRuntimeJsonIsSymlink(), legacyInsp.RuntimeJSONIsSymlink)
		}
		if protoInsp.GetRuntimeJsonResolved() != legacyInsp.RuntimeJSONResolved {
			t.Errorf("[%s] RuntimeJsonResolved mismatch: %q vs %q", targetID, protoInsp.GetRuntimeJsonResolved(), legacyInsp.RuntimeJSONResolved)
		}
		if protoInsp.GetRuntimeJsonValid() != legacyInsp.RuntimeJSONValid {
			t.Errorf("[%s] RuntimeJsonValid mismatch: %v vs %v", targetID, protoInsp.GetRuntimeJsonValid(), legacyInsp.RuntimeJSONValid)
		}
		if protoInsp.GetRuntimeJsonError() != legacyInsp.RuntimeJSONError {
			t.Errorf("[%s] RuntimeJsonError mismatch: %q vs %q", targetID, protoInsp.GetRuntimeJsonError(), legacyInsp.RuntimeJSONError)
		}
		if protoInsp.GetSessionJsonlExists() != legacyInsp.SessionJSONLExists {
			t.Errorf("[%s] SessionJsonlExists mismatch: %v vs %v", targetID, protoInsp.GetSessionJsonlExists(), legacyInsp.SessionJSONLExists)
		}
		if protoInsp.GetSessionTurnCount() != int32(legacyInsp.SessionTurnCount) {
			t.Errorf("[%s] SessionTurnCount mismatch: %d vs %d", targetID, protoInsp.GetSessionTurnCount(), legacyInsp.SessionTurnCount)
		}
		if protoInsp.GetSessionCorruptLines() != int32(legacyInsp.SessionCorruptLines) {
			t.Errorf("[%s] SessionCorruptLines mismatch: %d vs %d", targetID, protoInsp.GetSessionCorruptLines(), legacyInsp.SessionCorruptLines)
		}
		if protoInsp.GetAllowedAgentsExists() != legacyInsp.AllowedAgentsExists {
			t.Errorf("[%s] AllowedAgentsExists mismatch: %v vs %v", targetID, protoInsp.GetAllowedAgentsExists(), legacyInsp.AllowedAgentsExists)
		}
		if !reflect.DeepEqual(protoInsp.GetAllowedAgents(), legacyInsp.AllowedAgents) && (len(protoInsp.GetAllowedAgents()) != 0 || len(legacyInsp.AllowedAgents) != 0) {
			t.Errorf("[%s] AllowedAgents mismatch: %v vs %v", targetID, protoInsp.GetAllowedAgents(), legacyInsp.AllowedAgents)
		}
		if protoInsp.GetToolsDirExists() != legacyInsp.ToolsDirExists {
			t.Errorf("[%s] ToolsDirExists mismatch: %v vs %v", targetID, protoInsp.GetToolsDirExists(), legacyInsp.ToolsDirExists)
		}
		if !reflect.DeepEqual(protoInsp.GetDiscoveredTools(), legacyInsp.DiscoveredTools) && (len(protoInsp.GetDiscoveredTools()) != 0 || len(legacyInsp.DiscoveredTools) != 0) {
			t.Errorf("[%s] DiscoveredTools mismatch: %v vs %v", targetID, protoInsp.GetDiscoveredTools(), legacyInsp.DiscoveredTools)
		}
		if !reflect.DeepEqual(protoInsp.GetShadowedTools(), legacyInsp.ShadowedTools) && (len(protoInsp.GetShadowedTools()) != 0 || len(legacyInsp.ShadowedTools) != 0) {
			t.Errorf("[%s] ShadowedTools mismatch: %v vs %v", targetID, protoInsp.GetShadowedTools(), legacyInsp.ShadowedTools)
		}
		if protoInsp.GetSkillsDirExists() != legacyInsp.SkillsDirExists {
			t.Errorf("[%s] SkillsDirExists mismatch: %v vs %v", targetID, protoInsp.GetSkillsDirExists(), legacyInsp.SkillsDirExists)
		}
		if !reflect.DeepEqual(protoInsp.GetDiscoveredSkills(), legacyInsp.DiscoveredSkills) && (len(protoInsp.GetDiscoveredSkills()) != 0 || len(legacyInsp.DiscoveredSkills) != 0) {
			t.Errorf("[%s] DiscoveredSkills mismatch: %v vs %v", targetID, protoInsp.GetDiscoveredSkills(), legacyInsp.DiscoveredSkills)
		}
		if !reflect.DeepEqual(protoInsp.GetShadowedSkills(), legacyInsp.ShadowedSkills) && (len(protoInsp.GetShadowedSkills()) != 0 || len(legacyInsp.ShadowedSkills) != 0) {
			t.Errorf("[%s] ShadowedSkills mismatch: %v vs %v", targetID, protoInsp.GetShadowedSkills(), legacyInsp.ShadowedSkills)
		}

	}

	// ==========================================
	// Parity 3: ReadSession vs readSessionLegacy
	// ==========================================
	protoSess, err := sdk.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: agentAlpha})
	if err != nil {
		t.Fatalf("ReadSession proto failed: %v", err)
	}
	legacySess, err := sdk.readSessionLegacy(agentAlpha)
	if err != nil {
		t.Fatalf("readSessionLegacy failed: %v", err)
	}
	if len(protoSess.GetTurns()) != len(legacySess) {
		t.Fatalf("ReadSession turn count mismatch: proto=%d, legacy=%d", len(protoSess.GetTurns()), len(legacySess))
	}
	for i, lTurn := range legacySess {
		pTurn := protoSess.GetTurns()[i]
		if pTurn.GetRole() != lTurn.Role {
			t.Errorf("Turn %d role mismatch: %q vs %q", i, pTurn.GetRole(), lTurn.Role)
		}
		if len(pTurn.GetParts()) != len(lTurn.Parts) {
			t.Fatalf("Turn %d parts count mismatch: proto=%d, legacy=%d", i, len(pTurn.GetParts()), len(lTurn.Parts))
		}
		for j, lPart := range lTurn.Parts {
			pPart := pTurn.GetParts()[j]
			if pPart.GetText() != lPart.Text {
				t.Errorf("Turn %d Part %d text mismatch: %q vs %q", i, j, pPart.GetText(), lPart.Text)
			}
			if lPart.InlineData != nil {
				if !bytes.Equal(pPart.GetInlineData(), lPart.InlineData.Data) {
					t.Errorf("Turn %d Part %d inline data mismatch", i, j)
				}
				if pPart.GetMimeType() != lPart.InlineData.MIMEType {
					t.Errorf("Turn %d Part %d MIME type mismatch: %q vs %q", i, j, pPart.GetMimeType(), lPart.InlineData.MIMEType)
				}
			} else {
				if len(pPart.GetInlineData()) != 0 {
					t.Errorf("Turn %d Part %d expected empty inline data, got %v", i, j, pPart.GetInlineData())
				}
				if pPart.GetMimeType() != "" {
					t.Errorf("Turn %d Part %d expected empty MIME type, got %q", i, j, pPart.GetMimeType())
				}
			}
		}
	}

	// ==========================================
	// Parity 4: ReadMemory vs readMemoryLegacy
	// ==========================================
	protoMem, err := sdk.ReadMemory(ctx, &agentv1.ReadMemoryRequest{AgentId: agentAlpha})
	if err != nil {
		t.Fatalf("ReadMemory proto failed: %v", err)
	}
	legacyMem, err := sdk.readMemoryLegacy(agentAlpha)
	if err != nil {
		t.Fatalf("readMemoryLegacy failed: %v", err)
	}
	if protoMem.GetMemoryMd() != legacyMem {
		t.Errorf("ReadMemory parity mismatch: proto=%q, legacy=%q", protoMem.GetMemoryMd(), legacyMem)
	}

	// ==========================================
	// Parity 5: RenderSystemPrompt vs renderSystemPromptLegacy
	// ==========================================
	protoPrompt, err := sdk.RenderSystemPrompt(ctx, &agentv1.RenderSystemPromptRequest{AgentId: agentAlpha})
	if err != nil {
		t.Fatalf("RenderSystemPrompt proto failed: %v", err)
	}
	legacyPrompt, err := sdk.renderSystemPromptLegacy(agentAlpha)
	if err != nil {
		t.Fatalf("renderSystemPromptLegacy failed: %v", err)
	}
	if protoPrompt.GetRenderedPrompt() != legacyPrompt {
		t.Errorf("RenderSystemPrompt parity mismatch: proto=%q, legacy=%q", protoPrompt.GetRenderedPrompt(), legacyPrompt)
	}

	// ==========================================
	// Parity 6: InspectSessionContext vs inspectSessionContextLegacy
	// ==========================================
	protoCtx, err := sdk.InspectSessionContext(ctx, &agentv1.InspectSessionContextRequest{AgentId: agentAlpha})
	if err != nil {
		t.Fatalf("InspectSessionContext proto failed: %v", err)
	}
	legacyCtx, err := sdk.inspectSessionContextLegacy(agentAlpha)
	if err != nil {
		t.Fatalf("inspectSessionContextLegacy failed: %v", err)
	}

	if protoCtx.GetAgentId() != legacyCtx.AgentID {
		t.Errorf("AgentID mismatch: %q vs %q", protoCtx.GetAgentId(), legacyCtx.AgentID)
	}
	if protoCtx.GetModel() != legacyCtx.Model {
		t.Errorf("Model mismatch: %q vs %q", protoCtx.GetModel(), legacyCtx.Model)
	}
	if protoCtx.GetContextWindow() != int32(legacyCtx.ContextWindow) {
		t.Errorf("ContextWindow mismatch: %d vs %d", protoCtx.GetContextWindow(), legacyCtx.ContextWindow)
	}
	if protoCtx.GetCompactionThreshold() != int32(legacyCtx.CompactionThreshold) {
		t.Errorf("CompactionThreshold mismatch: %d vs %d", protoCtx.GetCompactionThreshold(), legacyCtx.CompactionThreshold)
	}
	if protoCtx.GetCompactionOverheadPct() != legacyCtx.CompactionOverheadPct {
		t.Errorf("CompactionOverheadPct mismatch: %v vs %v", protoCtx.GetCompactionOverheadPct(), legacyCtx.CompactionOverheadPct)
	}
	if protoCtx.GetEstimatedTotalTokens() != int32(legacyCtx.EstimatedTotalTokens) {
		t.Errorf("EstimatedTotalTokens mismatch: %d vs %d", protoCtx.GetEstimatedTotalTokens(), legacyCtx.EstimatedTotalTokens)
	}
	if protoCtx.GetSessionTurnsTokens() != int32(legacyCtx.SessionTurnsTokens) {
		t.Errorf("SessionTurnsTokens mismatch: %d vs %d", protoCtx.GetSessionTurnsTokens(), legacyCtx.SessionTurnsTokens)
	}
	if protoCtx.GetPromptTokensEstimate() != int32(legacyCtx.PromptTokensEstimate) {
		t.Errorf("PromptTokensEstimate mismatch: %d vs %d", protoCtx.GetPromptTokensEstimate(), legacyCtx.PromptTokensEstimate)
	}
	if protoCtx.GetMemoryTokensEstimate() != int32(legacyCtx.MemoryTokensEstimate) {
		t.Errorf("MemoryTokensEstimate mismatch: %d vs %d", protoCtx.GetMemoryTokensEstimate(), legacyCtx.MemoryTokensEstimate)
	}
	if protoCtx.GetPercentToThreshold() != legacyCtx.PercentToThreshold {
		t.Errorf("PercentToThreshold mismatch: %v vs %v", protoCtx.GetPercentToThreshold(), legacyCtx.PercentToThreshold)
	}
	if protoCtx.GetPercentToWindow() != legacyCtx.PercentToWindow {
		t.Errorf("PercentToWindow mismatch: %v vs %v", protoCtx.GetPercentToWindow(), legacyCtx.PercentToWindow)
	}
	if protoCtx.GetTurnCount() != int32(legacyCtx.TurnCount) {
		t.Errorf("TurnCount mismatch: %d vs %d", protoCtx.GetTurnCount(), legacyCtx.TurnCount)
	}
	if protoCtx.GetCompacted() != legacyCtx.Compacted {
		t.Errorf("Compacted mismatch: %v vs %v", protoCtx.GetCompacted(), legacyCtx.Compacted)
	}
	if protoCtx.GetLastPromptTokens() != legacyCtx.LastPromptTokens {
		t.Errorf("LastPromptTokens mismatch: %d vs %d", protoCtx.GetLastPromptTokens(), legacyCtx.LastPromptTokens)
	}
	if protoCtx.GetLastCandidatesTokens() != legacyCtx.LastCandidatesTokens {
		t.Errorf("LastCandidatesTokens mismatch: %d vs %d", protoCtx.GetLastCandidatesTokens(), legacyCtx.LastCandidatesTokens)
	}
	if protoCtx.GetLastTotalTokens() != legacyCtx.LastTotalTokens {
		t.Errorf("LastTotalTokens mismatch: %d vs %d", protoCtx.GetLastTotalTokens(), legacyCtx.LastTotalTokens)
	}

	// ==========================================
	// Parity 7: InspectAgentLocks vs inspectAgentLocksLegacy
	// ==========================================
	protoLocks, err := sdk.InspectAgentLocks(ctx, &agentv1.InspectAgentLocksRequest{})
	if err != nil {
		t.Fatalf("InspectAgentLocks proto failed: %v", err)
	}
	legacyLocks, err := sdk.inspectAgentLocksLegacy()
	if err != nil {
		t.Fatalf("inspectAgentLocksLegacy failed: %v", err)
	}
	if len(protoLocks.GetObservations()) != len(legacyLocks) {
		t.Fatalf("Observations count mismatch: proto=%d, legacy=%d", len(protoLocks.GetObservations()), len(legacyLocks))
	}
	for i, lObs := range legacyLocks {
		pObs := protoLocks.GetObservations()[i]
		if pObs.GetAgentId() != lObs.AgentID {
			t.Errorf("Obs %d AgentId mismatch: %q vs %q", i, pObs.GetAgentId(), lObs.AgentID)
		}
		if pObs.GetAgentDir() != lObs.AgentDir {
			t.Errorf("Obs %d AgentDir mismatch: %q vs %q", i, pObs.GetAgentDir(), lObs.AgentDir)
		}
		if pObs.GetLockExists() != lObs.LockExists {
			t.Errorf("Obs %d LockExists mismatch: %v vs %v", i, pObs.GetLockExists(), lObs.LockExists)
		}
		if pObs.GetHolderPid() != int32(lObs.HolderPID) {
			t.Errorf("Obs %d HolderPid mismatch: %d vs %d", i, pObs.GetHolderPid(), lObs.HolderPID)
		}
		if pObs.GetHolderPidValid() != lObs.HolderPIDValid {
			t.Errorf("Obs %d HolderPidValid mismatch: %v vs %v", i, pObs.GetHolderPidValid(), lObs.HolderPIDValid)
		}
		if pObs.GetHolderAlive() != lObs.HolderAlive {
			t.Errorf("Obs %d HolderAlive mismatch: %v vs %v", i, pObs.GetHolderAlive(), lObs.HolderAlive)
		}
		if pObs.GetHolderCommand() != lObs.HolderCommand {
			t.Errorf("Obs %d HolderCommand mismatch: %q vs %q", i, pObs.GetHolderCommand(), lObs.HolderCommand)
		}
		if pObs.GetSessionExists() != lObs.SessionExists {
			t.Errorf("Obs %d SessionExists mismatch: %v vs %v", i, pObs.GetSessionExists(), lObs.SessionExists)
		}
		if lObs.LockHeldSince.IsZero() {
			if pObs.GetLockHeldSince() != nil {
				t.Errorf("Obs %d expected nil LockHeldSince, got %v", i, pObs.GetLockHeldSince())
			}
		} else {
			if pObs.GetLockHeldSince() == nil || !pObs.GetLockHeldSince().AsTime().Equal(lObs.LockHeldSince) {
				t.Errorf("Obs %d LockHeldSince mismatch: %v vs %v", i, pObs.GetLockHeldSince(), lObs.LockHeldSince)
			}
		}
		if lObs.LastWrite.IsZero() {
			if pObs.GetLastWrite() != nil {
				t.Errorf("Obs %d expected nil LastWrite, got %v", i, pObs.GetLastWrite())
			}
		} else {
			if pObs.GetLastWrite() == nil || !pObs.GetLastWrite().AsTime().Equal(lObs.LastWrite) {
				t.Errorf("Obs %d LastWrite mismatch: %v vs %v", i, pObs.GetLastWrite(), lObs.LastWrite)
			}
		}
	}
}

// TestReadSession_AcceptedLossyConversion explicitly tests the accepted-lossy
// conversion documented for v1: SessionTurn preserves text and inline data parts,
// while tool invocations (FunctionCall and FunctionResponse) are omitted in the
// v1 protobuf schema while retained in raw session.jsonl / legacy readSession.
func TestReadSession_AcceptedLossyConversion(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "lossy-agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	// Write a session turn containing a FunctionCall to session.jsonl
	contentWithToolCall := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "Calling tool"},
			{FunctionCall: &genai.FunctionCall{Name: "bash", ID: "call_123"}},
		},
	}
	raw, err := json.Marshal(contentWithToolCall)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "session.jsonl"), append(raw, '\n'), 0644); err != nil {
		t.Fatalf("write session.jsonl: %v", err)
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// Legacy method preserves raw genai.Content including FunctionCall
	legacyTurns, err := sdk.readSessionLegacy(agentID)
	if err != nil {
		t.Fatalf("readSessionLegacy failed: %v", err)
	}
	if len(legacyTurns) != 1 || len(legacyTurns[0].Parts) != 2 || legacyTurns[0].Parts[1].FunctionCall == nil {
		t.Fatalf("expected legacy turns to retain FunctionCall, got %+v", legacyTurns)
	}

	// Proto method converts to SessionTurn; text is preserved, FunctionCall is omitted (lossy v1 accepted)
	protoResp, err := sdk.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSession proto failed: %v", err)
	}
	turns := protoResp.GetTurns()
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if len(turns[0].GetParts()) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(turns[0].GetParts()))
	}
	// Part 0 is text "Calling tool"
	if turns[0].GetParts()[0].GetText() != "Calling tool" {
		t.Errorf("expected text 'Calling tool', got %q", turns[0].GetParts()[0].GetText())
	}
	// Part 1 is the converted part where FunctionCall was omitted, leaving empty text/inline data
	if turns[0].GetParts()[1].GetText() != "" || len(turns[0].GetParts()[1].GetInlineData()) != 0 {
		t.Errorf("expected empty part for omitted tool call, got %+v", turns[0].GetParts()[1])
	}
}

func TestProtoContract_InspectAgentResponse_NoSensitiveRuntimeConfig(t *testing.T) {
	// D112: InspectAgentResponse must not expose raw structured runtime config
	// containing sensitive API keys. Verify that the descriptor has reserved field 12
	// and does not declare runtime_config.
	resp := &agentv1.InspectAgentResponse{}
	desc := resp.ProtoReflect().Descriptor()
	if desc.Fields().ByName("runtime_config") != nil {
		t.Fatal("InspectAgentResponse must not expose sensitive runtime_config field")
	}
	if desc.Fields().ByNumber(12) != nil {
		t.Fatal("field number 12 must remain reserved and unassigned in InspectAgentResponse")
	}
}
