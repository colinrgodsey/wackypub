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

func TestProtoContract_SessionTurnContentJSON(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to create root marker: %v", err)
	}

	agentID := "contentjson-agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("failed to create allowed agents file: %v", err)
	}
	t.Chdir(agentDir)

	// Turn containing thought, text, and function call
	turn := &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "Thinking step 1", Thought: true},
			{Text: "Executing tool command."},
			{FunctionCall: &genai.FunctionCall{Name: "bash", ID: "call_abc"}},
		},
	}
	if _, _, err := AppendSessionContentGetSeq(agentDir, turn); err != nil {
		t.Fatalf("AppendSessionContentGetSeq failed: %v", err)
	}

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// 1. ReadSession carries content_json with full 1:1 fidelity
	readResp, err := sdk.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if len(readResp.GetTurns()) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(readResp.GetTurns()))
	}
	st := readResp.GetTurns()[0]
	if st.GetContentJson() == "" {
		t.Fatal("expected non-empty ContentJson in ReadSession response")
	}
	var unmarshaled genai.Content
	if err := json.Unmarshal([]byte(st.GetContentJson()), &unmarshaled); err != nil {
		t.Fatalf("failed to unmarshal ContentJson: %v", err)
	}
	if len(unmarshaled.Parts) != 3 {
		t.Fatalf("expected 3 parts in ContentJson, got %d", len(unmarshaled.Parts))
	}
	if !unmarshaled.Parts[0].Thought || unmarshaled.Parts[0].Text != "Thinking step 1" {
		t.Errorf("part 0 mismatch: %+v", unmarshaled.Parts[0])
	}
	if unmarshaled.Parts[1].Thought || unmarshaled.Parts[1].Text != "Executing tool command." {
		t.Errorf("part 1 mismatch: %+v", unmarshaled.Parts[1])
	}
	if unmarshaled.Parts[2].FunctionCall == nil || unmarshaled.Parts[2].FunctionCall.Name != "bash" {
		t.Errorf("part 2 function call mismatch: %+v", unmarshaled.Parts[2])
	}

	// 2. ReadSessionEvents carries content_json
	eventsResp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSessionEvents failed: %v", err)
	}
	var evTurn *agentv1.SessionTurn
	for _, ev := range eventsResp.GetEvents() {
		if t := ev.GetTurn(); t != nil {
			evTurn = t
			break
		}
	}
	if evTurn == nil || evTurn.GetContentJson() == "" {
		t.Fatalf("expected turn event with non-empty ContentJson")
	}
	var evContent genai.Content
	if err := json.Unmarshal([]byte(evTurn.GetContentJson()), &evContent); err != nil {
		t.Fatalf("failed to unmarshal ContentJson from event: %v", err)
	}
	if len(evContent.Parts) != 3 || !evContent.Parts[0].Thought || evContent.Parts[2].FunctionCall == nil {
		t.Fatalf("event ContentJson fidelity mismatch: %+v", evContent.Parts)
	}

	// 3. Trace roundtrip preserves full Content via content_json
	traceResult := &TraceResult{
		Steps: []TraceStep{
			{
				AgentID:      agentID,
				TurnContents: []*genai.Content{turn},
			},
		},
	}
	protoTrace := TraceResultToProto(traceResult)
	if len(protoTrace.GetSteps()) != 1 || len(protoTrace.GetSteps()[0].GetTurnContents()) != 1 {
		t.Fatalf("TraceResultToProto steps mismatch")
	}
	if protoTrace.GetSteps()[0].GetTurnContents()[0].GetContentJson() == "" {
		t.Fatalf("TraceResultToProto expected non-empty ContentJson")
	}
	roundtrip := TraceProtoToResult(protoTrace)
	if len(roundtrip.Steps) != 1 || len(roundtrip.Steps[0].TurnContents) != 1 {
		t.Fatalf("TraceProtoToResult steps mismatch")
	}
	rtParts := roundtrip.Steps[0].TurnContents[0].Parts
	if len(rtParts) != 3 || !rtParts[0].Thought || rtParts[2].FunctionCall == nil {
		t.Fatalf("Trace roundtrip fidelity mismatch: %+v", rtParts)
	}
}

func TestProtoContract_ReadSessionEvents_FromSeqAndLatestSeq(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to create root marker: %v", err)
	}

	agentID := "cursor-agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\ncompacted-cursor-agent\n"), 0644); err != nil {
		t.Fatalf("failed to create allowed agents file: %v", err)
	}
	t.Chdir(agentDir)

	sdk := NewSDK(wsDir)
	ctx := context.Background()

	// 1. Empty session: head_only and default polls report latest_seq = 0
	emptyHeadResp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: agentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_HeadOnly{HeadOnly: true},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents empty head_only: %v", err)
	}
	if emptyHeadResp.GetLatestSeq() != 0 {
		t.Errorf("empty session latest_seq = %d, want 0", emptyHeadResp.GetLatestSeq())
	}

	// Append 5 turns: sequence numbers 1, 2, 3, 4, 5
	for i := 1; i <= 5; i++ {
		if _, err := AppendSessionTurnGetSeq(agentDir, "user", fmt.Sprintf("Turn %d", i)); err != nil {
			t.Fatalf("AppendSessionTurnGetSeq %d failed: %v", i, err)
		}
	}

	// 2. Backward compatibility: absent from_seq / unspecified cursor returns all events and reports latest_seq
	defaultResp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSessionEvents default failed: %v", err)
	}
	if len(defaultResp.GetEvents()) != 5 {
		t.Fatalf("expected 5 events, got %d", len(defaultResp.GetEvents()))
	}
	if defaultResp.GetBaselineSeq() != 1 || defaultResp.GetLatestSeq() != 5 || defaultResp.GetLatestTurnSeq() != 5 {
		t.Errorf("unexpected watermarks in default response: %+v", defaultResp)
	}
	if defaultResp.GetRewound() || defaultResp.GetRolledBack() {
		t.Errorf("expected rewound=false, rolled_back=false")
	}

	// 3. Head-only mode reports baseline, latest, and latest_turn without events
	headResp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: agentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_HeadOnly{HeadOnly: true},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents head_only failed: %v", err)
	}
	if len(headResp.GetEvents()) != 0 {
		t.Fatalf("expected 0 events in head_only mode, got %d", len(headResp.GetEvents()))
	}
	if headResp.GetBaselineSeq() != 1 || headResp.GetLatestSeq() != 5 || headResp.GetLatestTurnSeq() != 5 {
		t.Errorf("unexpected watermarks in head_only response: %+v", headResp)
	}

	// 4. from_seq skips earlier events cleanly (from_seq = 3 returns events with seq >= 3)
	from3Resp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: agentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_FromSeq{FromSeq: 3},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents from_seq=3 failed: %v", err)
	}
	if len(from3Resp.GetEvents()) != 3 {
		t.Fatalf("expected 3 events from seq 3, got %d", len(from3Resp.GetEvents()))
	}
	if from3Resp.GetEvents()[0].GetSeq() != 3 || from3Resp.GetEvents()[1].GetSeq() != 4 || from3Resp.GetEvents()[2].GetSeq() != 5 {
		t.Fatalf("unexpected event seqs: %d, %d, %d", from3Resp.GetEvents()[0].GetSeq(), from3Resp.GetEvents()[1].GetSeq(), from3Resp.GetEvents()[2].GetSeq())
	}
	if from3Resp.GetLatestSeq() != 5 {
		t.Errorf("expected LatestSeq 5, got %d", from3Resp.GetLatestSeq())
	}
	if from3Resp.GetRewound() || from3Resp.GetRolledBack() {
		t.Errorf("expected rewound=false, rolled_back=false for valid from_seq")
	}

	// 5. from_seq at current head + 1 (from_seq = 6) returns 0 events but reports latest_seq = 5
	from6Resp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: agentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_FromSeq{FromSeq: 6},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents from_seq=6 failed: %v", err)
	}
	if len(from6Resp.GetEvents()) != 0 {
		t.Fatalf("expected 0 events from seq 6, got %d", len(from6Resp.GetEvents()))
	}
	if from6Resp.GetLatestSeq() != 5 {
		t.Errorf("expected LatestSeq 5, got %d", from6Resp.GetLatestSeq())
	}
	if from6Resp.GetRewound() || from6Resp.GetRolledBack() {
		t.Errorf("expected rewound=false, rolled_back=false when client is at head")
	}

	// 6. from_seq beyond head + 1 triggers rollback detection (from_seq = 10 on session with max seq 5)
	from10Resp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: agentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_FromSeq{FromSeq: 10},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents from_seq=10 failed: %v", err)
	}
	if !from10Resp.GetRolledBack() || !from10Resp.GetRewound() {
		t.Errorf("expected rolled_back=true, rewound=true for cursor beyond head")
	}
	if from10Resp.GetLatestSeq() != 5 {
		t.Errorf("expected LatestSeq 5 on rollback reset, got %d", from10Resp.GetLatestSeq())
	}
	if len(from10Resp.GetEvents()) != 5 {
		t.Errorf("expected all surviving events returned on rollback reset, got %d", len(from10Resp.GetEvents()))
	}

	// 7. from_seq < baseline_seq triggers rewound=true, rolled_back=false, and returns all surviving events (compaction recovery)
	compactedAgentID := "compacted-cursor-agent"
	compactedAgentDir := filepath.Join(wsDir, compactedAgentID)
	if err := os.MkdirAll(compactedAgentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(compactedAgentDir, AllowedAgentsFile), []byte(compactedAgentID+"\n"), 0644); err != nil {
		t.Fatalf("failed to create allowed agents file: %v", err)
	}
	// Seed session with turns starting at seq 10 (simulating earlier turns compacted away)
	for i := int64(10); i <= 14; i++ {
		content := &genai.Content{
			Role:  "user",
			Parts: []*genai.Part{{Text: fmt.Sprintf("Turn %d", i)}},
		}
		if _, err := AppendSessionContentWithSeq(compactedAgentDir, content, i); err != nil {
			t.Fatalf("AppendSessionContentWithSeq %d failed: %v", i, err)
		}
	}

	belowBaselineResp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: compactedAgentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_FromSeq{FromSeq: 5},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents from_seq=5 (< baseline 10) failed: %v", err)
	}
	if !belowBaselineResp.GetRewound() {
		t.Errorf("expected rewound=true when from_seq < baseline_seq")
	}
	if belowBaselineResp.GetRolledBack() {
		t.Errorf("expected rolled_back=false when from_seq < baseline_seq")
	}
	if belowBaselineResp.GetBaselineSeq() != 10 {
		t.Errorf("expected baseline_seq 10, got %d", belowBaselineResp.GetBaselineSeq())
	}
	if belowBaselineResp.GetLatestSeq() != 14 {
		t.Errorf("expected latest_seq 14, got %d", belowBaselineResp.GetLatestSeq())
	}
	if len(belowBaselineResp.GetEvents()) != 5 {
		t.Errorf("expected 5 surviving events, got %d", len(belowBaselineResp.GetEvents()))
	}
	if belowBaselineResp.GetEvents()[0].GetSeq() != 10 {
		t.Errorf("expected first event seq 10, got %d", belowBaselineResp.GetEvents()[0].GetSeq())
	}

	// At baseline (from_seq = 10), no rewind
	atBaselineResp, err := sdk.ReadSessionEvents(ctx, &agentv1.ReadSessionEventsRequest{
		AgentId: compactedAgentID,
		Cursor:  &agentv1.ReadSessionEventsRequest_FromSeq{FromSeq: 10},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents from_seq=10 (== baseline) failed: %v", err)
	}
	if atBaselineResp.GetRewound() || atBaselineResp.GetRolledBack() {
		t.Errorf("expected rewound=false, rolled_back=false when from_seq == baseline_seq")
	}
	if len(atBaselineResp.GetEvents()) != 5 {
		t.Errorf("expected 5 events at baseline, got %d", len(atBaselineResp.GetEvents()))
	}

	// 8. Verify bot-side cursor math compatibility:
	// A consumer tracking latest_seq can advance its cursor using from_seq without re-reading the entire session.
	// Comparing ReadSession turns vs ReadSessionEvents from_seq:
	readSess, err := sdk.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	var turnsAfterSeq2 []*agentv1.SessionTurn
	for _, t := range readSess.GetTurns() {
		if t.GetSeq() > 2 {
			turnsAfterSeq2 = append(turnsAfterSeq2, t)
		}
	}
	if len(turnsAfterSeq2) != len(from3Resp.GetEvents()) {
		t.Fatalf("cursor math divergence: ReadSession turns (%d) != ReadSessionEvents (%d)", len(turnsAfterSeq2), len(from3Resp.GetEvents()))
	}
	for idx, turn := range turnsAfterSeq2 {
		ev := from3Resp.GetEvents()[idx]
		if turn.GetSeq() != ev.GetSeq() {
			t.Errorf("turn seq %d != event seq %d at index %d", turn.GetSeq(), ev.GetSeq(), idx)
		}
	}
}
