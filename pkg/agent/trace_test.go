package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

func TestTraceAgentCommitAndTraceID(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed writing root marker: %v", err)
	}

	// 1. Setup bob and jax agent repos
	if err := InitAgentGit(wsDir, "bob"); err != nil {
		t.Fatalf("failed initializing bob git: %v", err)
	}
	if err := InitAgentGit(wsDir, "jax"); err != nil {
		t.Fatalf("failed initializing jax git: %v", err)
	}

	bobDir := filepath.Join(wsDir, "bob")
	jaxDir := filepath.Join(wsDir, "jax")

	// Write WACKYPUB_ALLOWED_AGENTS for authorization
	_ = os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("jax\nuser\n"), 0644)
	_ = os.WriteFile(filepath.Join(jaxDir, AllowedAgentsFile), []byte("bob\nuser\n"), 0644)

	// Set A2A env for bob user turn
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)

	// Step 1: Bob user turn
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"user","call_chain":["user"],"trace_id":"trace-12345"}`)
	if err := AppendSessionTurn(bobDir, "user", "Bob start prompt"); err != nil {
		t.Fatalf("failed to append turn: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "bob", "user"); err != nil {
		t.Fatalf("failed committing bob event: %v", err)
	}
	bobHeadSHA, err := GetWorkspaceHeadCommit(bobDir)
	if err != nil || bobHeadSHA == "" {
		t.Fatalf("failed getting bob head SHA: %v", err)
	}

	// Step 2: Jax receiving call from bob (carrying bob's HEAD SHA as workspace_revision)
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"bob","call_chain":["user","bob"],"trace_id":"trace-12345","metadata":{"workspace_revision":"`+bobHeadSHA+`"}}`)
	if err := AppendSessionTurn(jaxDir, "user", "Jax received request from Bob"); err != nil {
		t.Fatalf("failed appending jax turn: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "jax", "user"); err != nil {
		t.Fatalf("failed committing jax event: %v", err)
	}
	jaxHeadSHA, err := GetWorkspaceHeadCommit(jaxDir)
	if err != nil || jaxHeadSHA == "" {
		t.Fatalf("failed getting jax head SHA: %v", err)
	}

	// 2. Trace targeted commit from jax
	sdk := NewSDK(wsDir)
	ctx := context.Background()
	opts := TraceOptions{MaxSteps: 10, Verbosity: 1}
	protoResp, err := sdk.Trace(ctx, &agentv1.TraceRequest{
		AgentId:   "jax",
		Target:    &agentv1.TraceRequest_CommitSpec{CommitSpec: jaxHeadSHA},
		MaxSteps:  int32(opts.MaxSteps),
		Verbosity: int32(opts.Verbosity),
	})
	if err != nil {
		t.Fatalf("Trace failed: %v", err)
	}
	res := TraceProtoToResult(protoResp)

	if len(res.Steps) != 2 {
		t.Fatalf("expected 2 trace steps, got %d", len(res.Steps))
	}

	if res.Steps[0].AgentID != "jax" || res.Steps[0].CommitSHA != jaxHeadSHA {
		t.Errorf("unexpected step 0: %+v", res.Steps[0])
	}
	if res.Steps[1].AgentID != "bob" || res.Steps[1].CommitSHA != bobHeadSHA {
		t.Errorf("unexpected step 1: %+v", res.Steps[1])
	}

	// 3. Trace by trace_id
	protoTraceResp, err := sdk.Trace(ctx, &agentv1.TraceRequest{
		Target:    &agentv1.TraceRequest_TraceId{TraceId: "trace-12345"},
		MaxSteps:  int32(opts.MaxSteps),
		Verbosity: int32(opts.Verbosity),
	})
	if err != nil {
		t.Fatalf("TraceByTraceID failed: %v", err)
	}
	traceRes := TraceProtoToResult(protoTraceResp)
	if len(traceRes.Steps) != 2 {
		t.Fatalf("expected 2 steps from trace_id search, got %d", len(traceRes.Steps))
	}

	// 4. Verify turn content text was populated from session.jsonl diffs
	if res.Steps[0].TurnContent == nil || !strings.Contains(ContentText(res.Steps[0].TurnContent), "Jax received request from Bob") {
		t.Errorf("expected step 0 turn content to contain 'Jax received request from Bob', got: %+v", res.Steps[0].TurnContent)
	}
	if res.Steps[1].TurnContent == nil || !strings.Contains(ContentText(res.Steps[1].TurnContent), "Bob start prompt") {
		t.Errorf("expected step 1 turn content to contain 'Bob start prompt', got: %+v", res.Steps[1].TurnContent)
	}

	// 5. Test formatting output across verbosity levels
	outputV1 := FormatTraceResult(wsDir, res, opts)
	if !strings.Contains(outputV1, "Bob start prompt") || !strings.Contains(outputV1, "Jax received request from Bob") {
		t.Errorf("expected -v 1 trace output to contain turn prompt text, got:\n%s", outputV1)
	}

	optsV3 := TraceOptions{MaxSteps: 10, Verbosity: 3}
	outputV3 := FormatTraceResult(wsDir, res, optsV3)
	if !strings.Contains(outputV3, "Bob start prompt") || !strings.Contains(outputV3, "Jax received request from Bob") {
		t.Errorf("expected -v 3 trace output to contain turn prompt text, got:\n%s", outputV3)
	}
}
