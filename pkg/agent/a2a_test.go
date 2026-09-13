package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseA2AMetadata_FallbackAndParsing(t *testing.T) {
	origA2A := os.Getenv(Agent2AgentEnvVar)
	origChain := os.Getenv(CallChainEnvVar)
	defer func() {
		os.Setenv(Agent2AgentEnvVar, origA2A)
		os.Setenv(CallChainEnvVar, origChain)
	}()

	t.Run("empty environment fallback to legacy WACKYPUB_CALL_CHAIN", func(t *testing.T) {
		os.Setenv(Agent2AgentEnvVar, "")
		os.Setenv(CallChainEnvVar, "bob,jax")

		meta, err := ParseA2AMetadata()
		if err != nil {
			t.Fatalf("ParseA2AMetadata failed: %v", err)
		}

		if meta.CallerID != "jax" {
			t.Errorf("expected CallerID 'jax', got %q", meta.CallerID)
		}
		if len(meta.CallChain) != 2 || meta.CallChain[0] != "bob" || meta.CallChain[1] != "jax" {
			t.Errorf("unexpected call chain: %v", meta.CallChain)
		}
		if !strings.HasPrefix(meta.TraceID, "a2a-") {
			t.Errorf("expected traceID starting with 'a2a-', got %q", meta.TraceID)
		}
	})

	t.Run("parsing AGENT2AGENT dense JSON", func(t *testing.T) {
		payload := `{"caller_id":"alice","call_chain":["bob","alice"],"trace_id":"a2a-test1234"}`
		os.Setenv(Agent2AgentEnvVar, payload)

		meta, err := ParseA2AMetadata()
		if err != nil {
			t.Fatalf("ParseA2AMetadata failed: %v", err)
		}

		if meta.CallerID != "alice" {
			t.Errorf("expected CallerID 'alice', got %q", meta.CallerID)
		}
		if len(meta.CallChain) != 2 || meta.CallChain[1] != "alice" {
			t.Errorf("unexpected CallChain: %v", meta.CallChain)
		}
		if meta.TraceID != "a2a-test1234" {
			t.Errorf("expected TraceID 'a2a-test1234', got %q", meta.TraceID)
		}
	})
}

func TestValidateAgentTarget_A2AMetadataPropagation(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("jax\n"), 0644); err != nil {
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

	origA2A := os.Getenv(Agent2AgentEnvVar)
	origChain := os.Getenv(CallChainEnvVar)
	defer func() {
		os.Setenv(Agent2AgentEnvVar, origA2A)
		os.Setenv(CallChainEnvVar, origChain)
	}()

	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"bob","call_chain":["bob"],"trace_id":"a2a-flow1"}`)

	// Target 'jax' should succeed and return updated A2AMetadata without process-global env mutation
	activeMeta, err := ValidateAgentTarget("jax")
	if err != nil {
		t.Fatalf("ValidateAgentTarget failed: %v", err)
	}

	if activeMeta == nil {
		t.Fatalf("expected non-nil activeMeta")
	}
	if activeMeta.CallerID != "bob" {
		t.Errorf("expected CallerID 'bob', got %q", activeMeta.CallerID)
	}
	if len(activeMeta.CallChain) != 2 || activeMeta.CallChain[0] != "bob" || activeMeta.CallChain[1] != "jax" {
		t.Errorf("unexpected active CallChain: %v", activeMeta.CallChain)
	}
	if activeMeta.TraceID != "a2a-flow1" {
		t.Errorf("expected TraceID 'a2a-flow1', got %q", activeMeta.TraceID)
	}
}

func TestValidateAgentTarget_MultiHopTraceIDPreservation(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	bobDir := filepath.Join(tmpDir, "bob")
	jaxDir := filepath.Join(tmpDir, "jax")
	clerkDir := filepath.Join(tmpDir, "clerk")

	os.MkdirAll(bobDir, 0755)
	os.MkdirAll(jaxDir, 0755)
	os.MkdirAll(clerkDir, 0755)

	os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("jax\n"), 0644)
	os.WriteFile(filepath.Join(jaxDir, AllowedAgentsFile), []byte("clerk\n"), 0644)

	origCwd, _ := os.Getwd()
	defer os.Chdir(origCwd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)

	// Originating call initializes trace_id "a2a-multihop-999"
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"bob","call_chain":["bob"],"trace_id":"a2a-multihop-999"}`)

	// Hop 1: Bob calls Jax
	os.Chdir(bobDir)
	metaHop1, err := ValidateAgentTarget("jax")
	if err != nil || metaHop1.TraceID != "a2a-multihop-999" {
		t.Fatalf("Hop 1 failed to preserve trace_id: got %q, err: %v", metaHop1.TraceID, err)
	}

	// Hop 2: Jax calls Clerk (simulating Jax subprocess receiving metaHop1 in its environment)
	os.Chdir(jaxDir)
	denseHop1, _ := metaHop1.Encode()
	os.Setenv(Agent2AgentEnvVar, denseHop1)
	metaHop2, err := ValidateAgentTarget("clerk")
	if err != nil || metaHop2.TraceID != "a2a-multihop-999" {
		t.Fatalf("Hop 2 failed to preserve trace_id: got %q, err: %v", metaHop2.TraceID, err)
	}

	if len(metaHop2.CallChain) != 3 || metaHop2.CallChain[0] != "bob" || metaHop2.CallChain[1] != "jax" || metaHop2.CallChain[2] != "clerk" {
		t.Errorf("unexpected multi-hop call chain: %v", metaHop2.CallChain)
	}
}

func TestValidateAgentTarget_A2ACycleRejection(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "jax")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("bob\n"), 0644); err != nil {
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

	origA2A := os.Getenv(Agent2AgentEnvVar)
	origChain := os.Getenv(CallChainEnvVar)
	defer func() {
		os.Setenv(Agent2AgentEnvVar, origA2A)
		os.Setenv(CallChainEnvVar, origChain)
	}()

	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"jax","call_chain":["bob","jax"],"trace_id":"a2a-cycle"}`)

	// Target 'bob' should fail because 'bob' is already in call_chain
	_, err = ValidateAgentTarget("bob")
	if err == nil {
		t.Fatalf("expected error for cycle targeting 'bob', got nil")
	}
	if !strings.Contains(err.Error(), "already in call chain") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestD59_StatelessA2APropagationAndReadOnlyExemption(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	bobDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(bobDir, 0755); err != nil {
		t.Fatalf("failed to create bob dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("You are Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "MEMORY.md"), []byte("Bob's memory"), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}
	if err := AppendSessionTurn(bobDir, "user", "Hello Bob"); err != nil {
		t.Fatalf("failed to append session turn: %v", err)
	}

	origA2A := os.Getenv(Agent2AgentEnvVar)
	origChain := os.Getenv(CallChainEnvVar)
	defer func() {
		os.Setenv(Agent2AgentEnvVar, origA2A)
		os.Setenv(CallChainEnvVar, origChain)
	}()

	// Simulate bob being active in call chain
	const testInitialA2A = `{"caller_id":"bob","call_chain":["bob"],"trace_id":"a2a-d59-test"}`
	os.Setenv(Agent2AgentEnvVar, testInitialA2A)
	os.Setenv(CallChainEnvVar, "bob")
	origCwd, _ := os.Getwd()
	os.Chdir(wsDir)
	defer os.Chdir(origCwd)

	sdk := NewSDK(wsDir)

	// 1. Read-only methods must NOT fail on deadlock cycle even when target agent is in active call chain
	turns, err := sdk.ReadSessionLegacy("bob")
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn from ReadSession, got %d", len(turns))
	}

	mem, err := sdk.ReadMemoryLegacy("bob")
	if err != nil {
		t.Fatalf("ReadMemory failed: %v", err)
	}
	if !strings.Contains(mem, "Bob's memory") {
		t.Errorf("unexpected ReadMemory output: %q", mem)
	}

	prompt, err := sdk.RenderSystemPromptLegacy("bob")
	if err != nil {
		t.Fatalf("RenderSystemPrompt failed: %v", err)
	}
	if !strings.Contains(prompt, "You are Bob") {
		t.Errorf("unexpected RenderSystemPrompt output: %q", prompt)
	}

	items, count, _, err := sdk.ListScratchpads("bob")
	if err != nil {
		t.Fatalf("ListScratchpads failed: %v", err)
	}
	if count != 0 || len(items) != 0 {
		t.Errorf("expected 0 scratchpads, got %d", count)
	}

	// 2. ValidateAgentTarget must return updated A2AMetadata WITHOUT mutating host process environment
	meta, err := ValidateAgentTarget("alice")
	if err != nil {
		t.Fatalf("ValidateAgentTarget alice failed: %v", err)
	}
	if len(meta.CallChain) != 2 || meta.CallChain[0] != "bob" || meta.CallChain[1] != "alice" {
		t.Fatalf("unexpected updated call chain: %v", meta.CallChain)
	}

	// Verify host process environment was NOT mutated
	if os.Getenv(Agent2AgentEnvVar) != testInitialA2A {
		t.Errorf("expected host process AGENT2AGENT to remain unchanged (%s), got: %s", testInitialA2A, os.Getenv(Agent2AgentEnvVar))
	}
	if os.Getenv(CallChainEnvVar) != "bob" {
		t.Errorf("expected host process WACKYPUB_CALL_CHAIN to remain 'bob', got: %s", os.Getenv(CallChainEnvVar))
	}
}

func TestD60_ReadOnlyCrossAgentAuthorizationGating(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	bobDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(bobDir, 0755); err != nil {
		t.Fatalf("failed to create bob dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("You are Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	aliceDir := filepath.Join(wsDir, "alice")
	if err := os.MkdirAll(aliceDir, 0755); err != nil {
		t.Fatalf("failed to create alice dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aliceDir, "AGENTS.md"), []byte("You are Alice"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aliceDir, "MEMORY.md"), []byte("Alice private memory"), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}
	if err := AppendSessionTurn(aliceDir, "user", "Secret from Alice"); err != nil {
		t.Fatalf("failed to append session turn: %v", err)
	}

	sdk := NewSDK(wsDir)

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	defer os.Chdir(origCwd)

	// Switch CWD to bob's directory without an allowlist
	if err := os.Chdir(bobDir); err != nil {
		t.Fatalf("failed to chdir to bobDir: %v", err)
	}

	// 1. All read-only content operations for Alice must FAIL due to missing allowlist in Bob's dir
	if _, err := sdk.ReadSessionLegacy("alice"); err == nil || !strings.Contains(err.Error(), "has no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected ReadSession to fail with missing allowlist, got: %v", err)
	}
	if _, err := sdk.ReadMemoryLegacy("alice"); err == nil || !strings.Contains(err.Error(), "has no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected ReadMemory to fail with missing allowlist, got: %v", err)
	}
	if _, err := sdk.RenderSystemPromptLegacy("alice"); err == nil || !strings.Contains(err.Error(), "has no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected RenderSystemPrompt to fail with missing allowlist, got: %v", err)
	}
	if _, _, _, err := sdk.ListScratchpads("alice"); err == nil || !strings.Contains(err.Error(), "has no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected ListScratchpads to fail with missing allowlist, got: %v", err)
	}
	if _, err := sdk.GetScratchpad("alice", "1234", nil, nil); err == nil || !strings.Contains(err.Error(), "has no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected GetScratchpad to fail with missing allowlist, got: %v", err)
	}
	if _, err := sdk.SearchScratchpad("alice", "1234", "secret", nil, false, 10); err == nil || !strings.Contains(err.Error(), "has no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected SearchScratchpad to fail with missing allowlist, got: %v", err)
	}

	// 2. InspectAgent MUST SUCCEED (diagnostic exemption per D16)
	info, err := sdk.InspectAgentLegacy("alice")
	if err != nil {
		t.Fatalf("InspectAgent should be exempt from authorization, got err: %v", err)
	}
	if info.AgentID != "alice" {
		t.Errorf("unexpected inspect info: %+v", info)
	}

	// 3. Grant Alice in Bob's allowlist
	if err := os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("alice\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}

	// Now all read operations must SUCCEED
	turns, err := sdk.ReadSessionLegacy("alice")
	if err != nil || len(turns) != 1 {
		t.Fatalf("ReadSession failed after authorization: err=%v, turns=%d", err, len(turns))
	}
	mem, err := sdk.ReadMemoryLegacy("alice")
	if err != nil || !strings.Contains(mem, "Alice private memory") {
		t.Fatalf("ReadMemory failed after authorization: err=%v, mem=%q", err, mem)
	}
	prompt, err := sdk.RenderSystemPromptLegacy("alice")
	if err != nil || !strings.Contains(prompt, "You are Alice") {
		t.Fatalf("RenderSystemPrompt failed after authorization: err=%v, prompt=%q", err, prompt)
	}
}
