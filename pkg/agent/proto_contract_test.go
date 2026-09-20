package agent

import (
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

	// Direct ReadSessionTurns preserves raw genai.Content including FunctionCall
	rawTurns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(rawTurns) != 1 || len(rawTurns[0].Parts) != 2 || rawTurns[0].Parts[1].FunctionCall == nil {
		t.Fatalf("expected raw turns to retain FunctionCall, got %+v", rawTurns)
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
