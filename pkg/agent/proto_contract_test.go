package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

// TestPhase3MethodParity acts as an automated parity test harness (D112)
// asserting field-by-field parity between the protobuf service interface and
// the unexported legacy methods for ALL 11 Phase 3 operations.
func TestPhase3MethodParity(t *testing.T) {
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

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* summary"},"finish_reason":"stop"}]}`)
	}))
	defer mockServer.Close()

	initAgent := func(id string) string {
		dir := filepath.Join(wsDir, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Parity prompt for "+id), 0644); err != nil {
			t.Fatalf("write AGENTS.md: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("Parity memory for "+id), 0644); err != nil {
			t.Fatalf("write MEMORY.md: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, AllowedAgentsFile), []byte(id+"\n"), 0644); err != nil {
			t.Fatalf("write allowed_agents: %v", err)
		}
		rtJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q,"contextWindow":128000,"maxImageDimension":400}`, mockServer.URL)
		if err := os.WriteFile(filepath.Join(dir, "runtime.json"), []byte(rtJSON), 0644); err != nil {
			t.Fatalf("write runtime.json: %v", err)
		}
		return dir
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// ==========================================
	// Parity 1: AddUserTurn vs addUserTurnLegacy
	// ==========================================
	initAgent("agent-turn-leg")
	initAgent("agent-turn-proto")
	turnLeg, err := sdk.addUserTurnLegacy("agent-turn-leg", "Parity turn message")
	if err != nil {
		t.Fatalf("addUserTurnLegacy failed: %v", err)
	}
	turnProto, err := sdk.AddUserTurn(ctx, &agentv1.AddUserTurnRequest{
		AgentId: "agent-turn-proto",
		Message: "Parity turn message",
	})
	if err != nil {
		t.Fatalf("AddUserTurn proto failed: %v", err)
	}
	if turnProto.GetText() != turnLeg.Text {
		t.Errorf("AddUserTurn Text mismatch: %q vs %q", turnProto.GetText(), turnLeg.Text)
	}
	if !slices.Equal(turnProto.GetWarnings(), turnLeg.Warnings) {
		t.Errorf("AddUserTurn Warnings mismatch: %v vs %v", turnProto.GetWarnings(), turnLeg.Warnings)
	}
	if turnProto.GetTurn().GetRole() != turnLeg.Content.Role {
		t.Errorf("AddUserTurn Role mismatch: %q vs %q", turnProto.GetTurn().GetRole(), turnLeg.Content.Role)
	}
	if len(turnProto.GetTurn().GetParts()) != len(turnLeg.Content.Parts) {
		t.Fatalf("AddUserTurn Parts count mismatch: %d vs %d", len(turnProto.GetTurn().GetParts()), len(turnLeg.Content.Parts))
	}
	if turnProto.GetTurn().GetParts()[0].GetText() != turnLeg.Content.Parts[0].Text {
		t.Errorf("AddUserTurn part text mismatch: %q vs %q", turnProto.GetTurn().GetParts()[0].GetText(), turnLeg.Content.Parts[0].Text)
	}

	// ==========================================
	// Parity 2: AddMedia vs addMediaLegacy
	// ==========================================
	initAgent("agent-media-leg")
	initAgent("agent-media-proto")
	testImg := createTestImage(100, 100, false)
	mediaLeg, err := sdk.addMediaLegacy("agent-media-leg", bytes.NewReader(testImg))
	if err != nil {
		t.Fatalf("addMediaLegacy failed: %v", err)
	}
	mediaProto, err := sdk.AddMedia(ctx, &agentv1.AddMediaRequest{
		AgentId:   "agent-media-proto",
		MediaData: testImg,
	})
	if err != nil {
		t.Fatalf("AddMedia proto failed: %v", err)
	}
	if mediaProto.GetMimeType() != mediaLeg.Parts[0].InlineData.MIMEType {
		t.Errorf("AddMedia MIMEType mismatch: %q vs %q", mediaProto.GetMimeType(), mediaLeg.Parts[0].InlineData.MIMEType)
	}
	if mediaProto.GetRawSize() != int64(len(mediaLeg.Parts[0].InlineData.Data)) {
		t.Errorf("AddMedia RawSize mismatch: %d vs %d", mediaProto.GetRawSize(), len(mediaLeg.Parts[0].InlineData.Data))
	}
	if mediaProto.GetTurn().GetRole() != mediaLeg.Role {
		t.Errorf("AddMedia Role mismatch: %q vs %q", mediaProto.GetTurn().GetRole(), mediaLeg.Role)
	}
	if !bytes.Equal(mediaProto.GetTurn().GetParts()[0].GetInlineData(), mediaLeg.Parts[0].InlineData.Data) {
		t.Error("AddMedia InlineData payload mismatch")
	}

	// ==========================================
	// Parity 3: CancelTurn vs cancelTurnLegacy
	// ==========================================
	errLeg := sdk.cancelTurnLegacy("idle-agent")
	_, errProto := sdk.CancelTurn(ctx, &agentv1.CancelTurnRequest{AgentId: "idle-agent"})
	if errLeg == nil || errProto == nil {
		t.Fatal("expected CancelTurn against idle agent to return error on both paths")
	}
	if errLeg.Error() != errProto.Error() {
		t.Errorf("CancelTurn error message mismatch: %q vs %q", errProto.Error(), errLeg.Error())
	}
	// Test cancellation of in-flight turns
	var legCancelled, protoCancelled bool
	cleanupLeg := registerInFlightTurn("leg-running", func() { legCancelled = true })
	defer cleanupLeg()
	if err := sdk.cancelTurnLegacy("leg-running"); err != nil {
		t.Fatalf("cancelTurnLegacy in-flight failed: %v", err)
	}
	if !legCancelled {
		t.Error("cancelTurnLegacy did not trigger cancel func")
	}
	cleanupProto := registerInFlightTurn("proto-running", func() { protoCancelled = true })
	defer cleanupProto()
	if _, err := sdk.CancelTurn(ctx, &agentv1.CancelTurnRequest{AgentId: "proto-running"}); err != nil {
		t.Fatalf("CancelTurn proto in-flight failed: %v", err)
	}
	if !protoCancelled {
		t.Error("CancelTurn proto did not trigger cancel func")
	}

	// ==========================================
	// Parity 4: StripSignatures vs stripSignaturesLegacy
	// ==========================================
	dirStripLeg := initAgent("agent-strip-leg")
	dirStripProto := initAgent("agent-strip-proto")
	signedTurn := `{"role":"model","parts":[{"text":"Thinking...","thoughtSignature":"sig123"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dirStripLeg, "session.jsonl"), []byte(signedTurn), 0644); err != nil {
		t.Fatalf("write signed session leg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirStripProto, "session.jsonl"), []byte(signedTurn), 0644); err != nil {
		t.Fatalf("write signed session proto: %v", err)
	}
	modLeg, err := sdk.stripSignaturesLegacy("agent-strip-leg")
	if err != nil {
		t.Fatalf("stripSignaturesLegacy failed: %v", err)
	}
	modProto, err := sdk.StripSignatures(ctx, &agentv1.StripSignaturesRequest{AgentId: "agent-strip-proto"})
	if err != nil {
		t.Fatalf("StripSignatures proto failed: %v", err)
	}
	if int(modProto.GetModifiedTurns()) != modLeg {
		t.Errorf("StripSignatures ModifiedTurns mismatch: %d vs %d", modProto.GetModifiedTurns(), modLeg)
	}

	// ==========================================
	// Parity 5: CompactSession vs compactSessionLegacy
	// ==========================================
	initAgent("agent-compact-leg")
	initAgent("agent-compact-proto")
	compLeg, err := sdk.compactSessionLegacy(ctx, "agent-compact-leg", false)
	if err != nil {
		t.Fatalf("compactSessionLegacy failed: %v", err)
	}
	compProto, err := sdk.CompactSession(ctx, &agentv1.CompactSessionRequest{
		AgentId: "agent-compact-proto",
		Force:   false,
	})
	if err != nil {
		t.Fatalf("CompactSession proto failed: %v", err)
	}
	if compProto.GetCompacted() != compLeg {
		t.Errorf("CompactSession Compacted mismatch: %v vs %v", compProto.GetCompacted(), compLeg)
	}

	// ==========================================
	// Parity 6: CreateScratchpad vs createScratchpadLegacy
	// ==========================================
	initAgent("agent-sp-leg")
	initAgent("agent-sp-proto")
	spLeg, err := sdk.createScratchpadLegacy("agent-sp-leg", "Line 1: Parity entry\nLine 2: Target\nLine 3: End\n", "tester")
	if err != nil {
		t.Fatalf("createScratchpadLegacy failed: %v", err)
	}
	spProto, err := sdk.CreateScratchpad(ctx, &agentv1.CreateScratchpadRequest{
		AgentId:   "agent-sp-proto",
		Text:      "Line 1: Parity entry\nLine 2: Target\nLine 3: End\n",
		CreatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("CreateScratchpad proto failed: %v", err)
	}
	pe := spProto.GetEntry()
	if len(pe.GetEntryId()) != len(spLeg.ID) || len(pe.GetEntryId()) != 4 {
		t.Errorf("CreateScratchpad ID length mismatch: %d vs %d", len(pe.GetEntryId()), len(spLeg.ID))
	}
	if pe.GetSize() != int64(spLeg.Size) {
		t.Errorf("CreateScratchpad Size mismatch: %d vs %d", pe.GetSize(), spLeg.Size)
	}
	if pe.GetLines() != int32(spLeg.Lines) {
		t.Errorf("CreateScratchpad Lines mismatch: %d vs %d", pe.GetLines(), spLeg.Lines)
	}
	if pe.GetCreatedBy() != spLeg.CreatedBy {
		t.Errorf("CreateScratchpad CreatedBy mismatch: %q vs %q", pe.GetCreatedBy(), spLeg.CreatedBy)
	}
	if pe.GetIsBinary() != spLeg.IsBinary {
		t.Errorf("CreateScratchpad IsBinary mismatch: %v vs %v", pe.GetIsBinary(), spLeg.IsBinary)
	}

	// ==========================================
	// Parity 7: GetScratchpad vs getScratchpadLegacy
	// ==========================================
	getLeg, err := sdk.getScratchpadLegacy("agent-sp-leg", spLeg.ID, nil, nil)
	if err != nil {
		t.Fatalf("getScratchpadLegacy failed: %v", err)
	}
	getProto, err := sdk.GetScratchpad(ctx, &agentv1.GetScratchpadRequest{
		AgentId: "agent-sp-proto",
		EntryId: pe.GetEntryId(),
	})
	if err != nil {
		t.Fatalf("GetScratchpad proto failed: %v", err)
	}
	if getProto.GetText() != getLeg {
		t.Errorf("GetScratchpad full text mismatch: %q vs %q", getProto.GetText(), getLeg)
	}
	// With pagination: skip 1, num 1
	skip := 1
	num := 1
	getLegPaged, err := sdk.getScratchpadLegacy("agent-sp-leg", spLeg.ID, &skip, &num)
	if err != nil {
		t.Fatalf("getScratchpadLegacy paged failed: %v", err)
	}
	skip32 := int32(1)
	num32 := int32(1)
	getProtoPaged, err := sdk.GetScratchpad(ctx, &agentv1.GetScratchpadRequest{
		AgentId:   "agent-sp-proto",
		EntryId:   pe.GetEntryId(),
		SkipLines: &skip32,
		NumLines:  &num32,
	})
	if err != nil {
		t.Fatalf("GetScratchpad proto paged failed: %v", err)
	}
	if getProtoPaged.GetText() != getLegPaged {
		t.Errorf("GetScratchpad paged text mismatch: %q vs %q", getProtoPaged.GetText(), getLegPaged)
	}

	// ==========================================
	// Parity 8: ListScratchpads vs listScratchpadsLegacy
	// ==========================================
	listLegItems, listLegCount, listLegCap, err := sdk.listScratchpadsLegacy("agent-sp-leg")
	if err != nil {
		t.Fatalf("listScratchpadsLegacy failed: %v", err)
	}
	listProto, err := sdk.ListScratchpads(ctx, &agentv1.ListScratchpadsRequest{
		AgentId: "agent-sp-proto",
	})
	if err != nil {
		t.Fatalf("ListScratchpads proto failed: %v", err)
	}
	if int(listProto.GetTotalEntries()) != listLegCount {
		t.Errorf("ListScratchpads TotalEntries mismatch: %d vs %d", listProto.GetTotalEntries(), listLegCount)
	}
	if int(listProto.GetMaxCapacity()) != listLegCap {
		t.Errorf("ListScratchpads MaxCapacity mismatch: %d vs %d", listProto.GetMaxCapacity(), listLegCap)
	}
	if len(listProto.GetEntries()) != len(listLegItems) {
		t.Fatalf("ListScratchpads entries count mismatch: %d vs %d", len(listProto.GetEntries()), len(listLegItems))
	}
	if listProto.GetEntries()[0].GetSize() != int64(listLegItems[0].Size) {
		t.Errorf("ListScratchpads entry size mismatch: %d vs %d", listProto.GetEntries()[0].GetSize(), listLegItems[0].Size)
	}

	// ==========================================
	// Parity 9: SearchScratchpad vs searchScratchpadLegacy
	// ==========================================
	searchLeg, err := sdk.searchScratchpadLegacy("agent-sp-leg", spLeg.ID, "Target", nil, false, 10)
	if err != nil {
		t.Fatalf("searchScratchpadLegacy failed: %v", err)
	}
	searchProto, err := sdk.SearchScratchpad(ctx, &agentv1.SearchScratchpadRequest{
		AgentId:    "agent-sp-proto",
		EntryId:    pe.GetEntryId(),
		Query:      "Target",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("SearchScratchpad proto failed: %v", err)
	}
	if int(searchProto.GetTotalMatches()) != searchLeg.TotalMatches {
		t.Errorf("SearchScratchpad TotalMatches mismatch: %d vs %d", searchProto.GetTotalMatches(), searchLeg.TotalMatches)
	}
	if int(searchProto.GetMaxResults()) != searchLeg.MaxResults {
		t.Errorf("SearchScratchpad MaxResults mismatch: %d vs %d", searchProto.GetMaxResults(), searchLeg.MaxResults)
	}
	if len(searchProto.GetMatches()) != len(searchLeg.Matches) {
		t.Fatalf("SearchScratchpad Matches count mismatch: %d vs %d", len(searchProto.GetMatches()), len(searchLeg.Matches))
	}
	if searchProto.GetMatches()[0].GetLine() != int32(searchLeg.Matches[0].Line) {
		t.Errorf("SearchScratchpad Line mismatch: %d vs %d", searchProto.GetMatches()[0].GetLine(), searchLeg.Matches[0].Line)
	}
	if searchProto.GetMatches()[0].GetText() != searchLeg.Matches[0].Text {
		t.Errorf("SearchScratchpad Text mismatch: %q vs %q", searchProto.GetMatches()[0].GetText(), searchLeg.Matches[0].Text)
	}

	// ==========================================
	// Parity 10: DiffScratchpadEntries vs diffScratchpadEntriesLegacy
	// ==========================================
	spLeg2, err := sdk.createScratchpadLegacy("agent-sp-leg", "Line 1: Parity entry\nLine 2: Modified Target\nLine 3: End\n", "tester")
	if err != nil {
		t.Fatalf("createScratchpadLegacy 2 failed: %v", err)
	}
	spProto2, err := sdk.CreateScratchpad(ctx, &agentv1.CreateScratchpadRequest{
		AgentId:   "agent-sp-proto",
		Text:      "Line 1: Parity entry\nLine 2: Modified Target\nLine 3: End\n",
		CreatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("CreateScratchpad proto 2 failed: %v", err)
	}
	diffLeg, err := sdk.diffScratchpadEntriesLegacy("agent-sp-leg", spLeg.ID, spLeg2.ID)
	if err != nil {
		t.Fatalf("diffScratchpadEntriesLegacy failed: %v", err)
	}
	diffProto, err := sdk.DiffScratchpadEntries(ctx, &agentv1.DiffScratchpadEntriesRequest{
		AgentId:       "agent-sp-proto",
		BeforeEntryId: pe.GetEntryId(),
		AfterEntryId:  spProto2.GetEntry().GetEntryId(),
	})
	if err != nil {
		t.Fatalf("DiffScratchpadEntries proto failed: %v", err)
	}
	if diffLeg == "" || diffProto.GetDiff() == "" {
		t.Error("expected non-empty diff output on both paths")
	}
	if !strings.Contains(diffProto.GetDiff(), "Modified Target") || !strings.Contains(diffLeg, "Modified Target") {
		t.Errorf("diff output missing expected change line: %q", diffProto.GetDiff())
	}

	// ==========================================
	// Parity 11: DeleteScratchpad vs deleteScratchpadLegacy
	// ==========================================
	if err := sdk.deleteScratchpadLegacy("agent-sp-leg", spLeg.ID); err != nil {
		t.Fatalf("deleteScratchpadLegacy failed: %v", err)
	}
	if _, err := sdk.DeleteScratchpad(ctx, &agentv1.DeleteScratchpadRequest{
		AgentId: "agent-sp-proto",
		EntryId: pe.GetEntryId(),
	}); err != nil {
		t.Fatalf("DeleteScratchpad proto failed: %v", err)
	}
	// Verify deleted on both
	if _, err := sdk.getScratchpadLegacy("agent-sp-leg", spLeg.ID, nil, nil); err == nil {
		t.Error("expected legacy get to fail on deleted scratchpad")
	}
	if _, err := sdk.GetScratchpad(ctx, &agentv1.GetScratchpadRequest{
		AgentId: "agent-sp-proto",
		EntryId: pe.GetEntryId(),
	}); err == nil {
		t.Error("expected proto get to fail on deleted scratchpad")
	}
}

// TestAddMedia_CeilingCheck tests that AddMedia enforces the 10MB ceiling (MaxMediaPayloadBytes)
// before processing, returning an error when exceeded.
func TestAddMedia_CeilingCheck(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "ceiling-agent"
	agentDir := filepath.Join(wsDir, agentID)
	_ = os.MkdirAll(agentDir, 0755)
	_ = os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644)
	_ = os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("ceiling-agent\n"), 0644)
	_ = os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"m","maxImageDimension":400}`), 0644)

	origCwd, _ := os.Getwd()
	_ = os.Chdir(wsDir)
	defer os.Chdir(origCwd)

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// 10MB + 1 byte payload
	oversized := make([]byte, MaxMediaPayloadBytes+1)
	_, err := sdk.AddMedia(ctx, &agentv1.AddMediaRequest{
		AgentId:   agentID,
		MediaData: oversized,
	})
	if err == nil {
		t.Fatal("expected error for oversized media payload (>10MB), got nil")
	}
	if !strings.Contains(err.Error(), "exceeds 10MB limit") {
		t.Fatalf("expected error message mentioning 10MB limit, got: %v", err)
	}
}

// TestProtoContract_CompactSession_CrossAgentAuthorization verifies D60 cross-agent
// authorization gating on CompactSession: when invoked from a caller agent directory,
// cross-agent compaction of a target agent is rejected unless the target is explicitly
// permitted in the caller's allowed_agents allowlist. Internal/self-compaction paths
// and authorized cross-agent invocations remain functional.
func TestProtoContract_CompactSession_CrossAgentAuthorization(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer os.Chdir(origCwd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, "")

	origChain := os.Getenv(CallChainEnvVar)
	defer os.Setenv(CallChainEnvVar, origChain)
	os.Setenv(CallChainEnvVar, "")

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* compacted summary"},"finish_reason":"stop"}]}`)
	}))
	defer mockServer.Close()

	// Caller agent: bob
	bobDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(bobDir, 0755); err != nil {
		t.Fatalf("mkdir bob: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("You are Bob"), 0644); err != nil {
		t.Fatalf("write bob AGENTS.md: %v", err)
	}

	// Target agent: alice
	aliceDir := filepath.Join(wsDir, "alice")
	if err := os.MkdirAll(aliceDir, 0755); err != nil {
		t.Fatalf("mkdir alice: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aliceDir, "AGENTS.md"), []byte("You are Alice"), 0644); err != nil {
		t.Fatalf("write alice AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aliceDir, AllowedAgentsFile), []byte("alice\n"), 0644); err != nil {
		t.Fatalf("write alice allowed_agents: %v", err)
	}
	rtJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q,"contextWindow":128000,"maxImageDimension":400}`, mockServer.URL)
	if err := os.WriteFile(filepath.Join(aliceDir, "runtime.json"), []byte(rtJSON), 0644); err != nil {
		t.Fatalf("write alice runtime.json: %v", err)
	}
	if err := AppendSessionTurn(aliceDir, "user", "Alice turn 1"); err != nil {
		t.Fatalf("write alice session turn: %v", err)
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// Switch CWD to bob's directory
	if err := os.Chdir(bobDir); err != nil {
		t.Fatalf("chdir bobDir: %v", err)
	}

	// 1. Without allowed_agents in bob's directory, CompactSession targeting alice must fail
	_, err = sdk.CompactSession(ctx, &agentv1.CompactSessionRequest{
		AgentId: "alice",
		Force:   false,
	})
	if err == nil || !strings.Contains(err.Error(), "has no "+AllowedAgentsFile+" allowlist") {
		t.Fatalf("expected CompactSession to fail with missing allowlist, got err: %v", err)
	}

	// 2. With an allowed_agents file that does NOT include alice (e.g. only charlie), it must fail
	if err := os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("charlie\n"), 0644); err != nil {
		t.Fatalf("write bob allowed_agents: %v", err)
	}
	_, err = sdk.CompactSession(ctx, &agentv1.CompactSessionRequest{
		AgentId: "alice",
		Force:   false,
	})
	if err == nil || !strings.Contains(err.Error(), "not in "+AllowedAgentsFile+" allowlist") {
		t.Fatalf("expected CompactSession to fail when target not in allowlist, got err: %v", err)
	}

	// 3. Grant alice in bob's allowlist -> CompactSession targeting alice must now SUCCEED
	if err := os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("alice\n"), 0644); err != nil {
		t.Fatalf("write bob allowed_agents: %v", err)
	}
	resp, err := sdk.CompactSession(ctx, &agentv1.CompactSessionRequest{
		AgentId: "alice",
		Force:   false,
	})
	if err != nil {
		t.Fatalf("expected CompactSession to succeed once authorized in allowlist, got err: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil CompactSessionResponse")
	}

	// 4. Self-compaction from alice's own directory succeeds
	if err := os.Chdir(aliceDir); err != nil {
		t.Fatalf("chdir aliceDir: %v", err)
	}
	selfResp, err := sdk.CompactSession(ctx, &agentv1.CompactSessionRequest{
		AgentId: "alice",
		Force:   false,
	})
	if err != nil {
		t.Fatalf("expected self-compaction from target agent directory to succeed, got err: %v", err)
	}
	if selfResp == nil {
		t.Fatal("expected non-nil response for self-compaction")
	}
}

// TestTraceMethodParity verifies field-by-field parity between proto methods and legacy methods
// for the 2 Phase 4 operations: GetAgent and Trace.
func TestTraceMethodParity(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed writing root marker: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer os.Chdir(origCwd)

	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir wsDir: %v", err)
	}

	// 1. Setup git repos for bob and jax
	if err := InitAgentGit(wsDir, "bob"); err != nil {
		t.Fatalf("failed initializing bob git: %v", err)
	}
	if err := InitAgentGit(wsDir, "jax"); err != nil {
		t.Fatalf("failed initializing jax git: %v", err)
	}

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* summary"},"finish_reason":"stop"}]}`)
	}))
	defer mockServer.Close()

	bobDir := filepath.Join(wsDir, "bob")
	jaxDir := filepath.Join(wsDir, "jax")

	if err := os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("You are Bob"), 0644); err != nil {
		t.Fatalf("write bob AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "MEMORY.md"), []byte("Bob memory notes"), 0644); err != nil {
		t.Fatalf("write bob MEMORY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("bob\njax\n"), 0644); err != nil {
		t.Fatalf("write bob allowed_agents: %v", err)
	}
	bobRtJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q,"contextWindow":128000}`, mockServer.URL)
	if err := os.WriteFile(filepath.Join(bobDir, "runtime.json"), []byte(bobRtJSON), 0644); err != nil {
		t.Fatalf("write bob runtime.json: %v", err)
	}

	if err := os.WriteFile(filepath.Join(jaxDir, "AGENTS.md"), []byte("You are Jax"), 0644); err != nil {
		t.Fatalf("write jax AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jaxDir, "MEMORY.md"), []byte("Jax memory notes"), 0644); err != nil {
		t.Fatalf("write jax MEMORY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jaxDir, AllowedAgentsFile), []byte("bob\njax\n"), 0644); err != nil {
		t.Fatalf("write jax allowed_agents: %v", err)
	}
	jaxRtJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q,"contextWindow":64000}`, mockServer.URL)
	if err := os.WriteFile(filepath.Join(jaxDir, "runtime.json"), []byte(jaxRtJSON), 0644); err != nil {
		t.Fatalf("write jax runtime.json: %v", err)
	}

	// Commit turns for causal tracing
	if err := AppendSessionTurn(bobDir, "user", "Bob start prompt"); err != nil {
		t.Fatalf("appending bob turn: %v", err)
	}
	// Inject a trace ID into A2A metadata for bob's commit
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	metaJSON := `{"trace_id":"trace-parity-phase4","caller_id":"user"}`
	os.Setenv(Agent2AgentEnvVar, metaJSON)

	if err := CommitWorkspaceEvent(wsDir, "bob", "user"); err != nil {
		t.Fatalf("committing bob event: %v", err)
	}
	bobHeadSHA, err := GetWorkspaceHeadCommit(bobDir)
	if err != nil || bobHeadSHA == "" {
		t.Fatalf("bob head SHA: %v", err)
	}

	// Jax commit referencing bob
	metaJax := fmt.Sprintf(`{"trace_id":"trace-parity-phase4","caller_id":"bob","metadata":{"workspace_revision":%q}}`, bobHeadSHA)
	os.Setenv(Agent2AgentEnvVar, metaJax)

	if err := AppendSessionTurn(jaxDir, "user", "Jax received request from Bob"); err != nil {
		t.Fatalf("appending jax turn: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "jax", "user"); err != nil {
		t.Fatalf("committing jax event: %v", err)
	}
	jaxHeadSHA, err := GetWorkspaceHeadCommit(jaxDir)
	if err != nil || jaxHeadSHA == "" {
		t.Fatalf("jax head SHA: %v", err)
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// ==========================================

	// ==========================================
	// Parity 2: Trace vs traceLegacy (by commit)
	// ==========================================
	opts := TraceOptions{MaxSteps: 10, Verbosity: 1}
	legTrace, err := sdk.traceLegacy("jax", jaxHeadSHA, "", opts)
	if err != nil {
		t.Fatalf("traceLegacy failed: %v", err)
	}
	protoTrace, err := sdk.Trace(ctx, &agentv1.TraceRequest{
		AgentId:   "jax",
		Target:    &agentv1.TraceRequest_CommitSpec{CommitSpec: jaxHeadSHA},
		MaxSteps:  int32(opts.MaxSteps),
		Verbosity: int32(opts.Verbosity),
	})
	if err != nil {
		t.Fatalf("Trace proto failed: %v", err)
	}

	if protoTrace.GetTargetAgentId() != legTrace.TargetAgentID {
		t.Errorf("Trace TargetAgentId mismatch: %q vs %q", protoTrace.GetTargetAgentId(), legTrace.TargetAgentID)
	}
	if protoTrace.GetTargetCommit() != legTrace.TargetCommit {
		t.Errorf("Trace TargetCommit mismatch: %q vs %q", protoTrace.GetTargetCommit(), legTrace.TargetCommit)
	}
	if len(protoTrace.GetSteps()) != len(legTrace.Steps) {
		t.Fatalf("Trace steps count mismatch: %d vs %d", len(protoTrace.GetSteps()), len(legTrace.Steps))
	}
	for i := range legTrace.Steps {
		pStep := protoTrace.GetSteps()[i]
		lStep := legTrace.Steps[i]
		if pStep.GetStepIndex() != int32(lStep.StepIndex) {
			t.Errorf("step %d index mismatch: %d vs %d", i, pStep.GetStepIndex(), lStep.StepIndex)
		}
		if pStep.GetAgentId() != lStep.AgentID {
			t.Errorf("step %d agentId mismatch: %q vs %q", i, pStep.GetAgentId(), lStep.AgentID)
		}
		if pStep.GetCommitSha() != lStep.CommitSHA {
			t.Errorf("step %d commitSha mismatch: %q vs %q", i, pStep.GetCommitSha(), lStep.CommitSHA)
		}
		if pStep.GetShortSha() != lStep.ShortSHA {
			t.Errorf("step %d shortSha mismatch: %q vs %q", i, pStep.GetShortSha(), lStep.ShortSHA)
		}
		if pStep.GetEventType() != lStep.EventType {
			t.Errorf("step %d eventType mismatch: %q vs %q", i, pStep.GetEventType(), lStep.EventType)
		}
		if pStep.GetRawCommitMessage() != lStep.RawCommitMessage {
			t.Errorf("step %d rawCommitMessage mismatch: %q vs %q", i, pStep.GetRawCommitMessage(), lStep.RawCommitMessage)
		}
		if lStep.A2AMetadata != nil {
			if pStep.GetA2AMetadata() == nil {
				t.Errorf("step %d expected A2AMetadata, got nil", i)
			} else if pStep.GetA2AMetadata().GetTraceId() != lStep.A2AMetadata.TraceID {
				t.Errorf("step %d A2AMetadata traceId mismatch: %q vs %q", i, pStep.GetA2AMetadata().GetTraceId(), lStep.A2AMetadata.TraceID)
			}
		}
	}

	// ==========================================
	// Parity 3: Trace vs traceLegacy (by trace_id)
	// ==========================================
	legTraceID, err := sdk.traceLegacy("", "", "trace-parity-phase4", opts)
	if err != nil {
		t.Fatalf("traceLegacy by trace_id failed: %v", err)
	}
	protoTraceID, err := sdk.Trace(ctx, &agentv1.TraceRequest{
		Target:    &agentv1.TraceRequest_TraceId{TraceId: "trace-parity-phase4"},
		MaxSteps:  int32(opts.MaxSteps),
		Verbosity: int32(opts.Verbosity),
	})
	if err != nil {
		t.Fatalf("Trace proto by trace_id failed: %v", err)
	}

	if protoTraceID.GetTraceId() != legTraceID.TraceID {
		t.Errorf("Trace trace_id mismatch: %q vs %q", protoTraceID.GetTraceId(), legTraceID.TraceID)
	}
	if len(protoTraceID.GetSteps()) != len(legTraceID.Steps) {
		t.Fatalf("Trace by trace_id steps count mismatch: %d vs %d", len(protoTraceID.GetSteps()), len(legTraceID.Steps))
	}
}

// TestTrace_OneofTarget verifies that the oneof target in TraceRequest enforces
// that exactly one of commit_spec or trace_id is specified.
func TestTrace_OneofTarget(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed writing root marker: %v", err)
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// 1. Neither target set: should fail with error
	_, err := sdk.Trace(ctx, &agentv1.TraceRequest{
		AgentId: "testagent",
	})
	if err == nil || !strings.Contains(err.Error(), "must specify either commit_spec or trace_id") {
		t.Fatalf("expected error for neither target specified, got: %v", err)
	}

	// 2. Commit spec set but agent_id empty: should fail requiring agent_id
	_, err = sdk.Trace(ctx, &agentv1.TraceRequest{
		Target: &agentv1.TraceRequest_CommitSpec{CommitSpec: "HEAD"},
	})
	if err == nil || !strings.Contains(err.Error(), "agent_id is required") {
		t.Fatalf("expected error for commit_spec without agent_id, got: %v", err)
	}

	// 3. Commit spec set with empty string: should fail with empty commit_spec error
	_, err = sdk.Trace(ctx, &agentv1.TraceRequest{
		AgentId: "testagent",
		Target:  &agentv1.TraceRequest_CommitSpec{CommitSpec: ""},
	})
	if err == nil || !strings.Contains(err.Error(), "commit_spec cannot be empty") {
		t.Fatalf("expected error for empty commit_spec, got: %v", err)
	}

	// 4. Trace ID set with empty string: should fail with empty trace_id error
	_, err = sdk.Trace(ctx, &agentv1.TraceRequest{
		Target: &agentv1.TraceRequest_TraceId{TraceId: ""},
	})
	if err == nil || !strings.Contains(err.Error(), "trace_id cannot be empty") {
		t.Fatalf("expected error for empty trace_id, got: %v", err)
	}
}
