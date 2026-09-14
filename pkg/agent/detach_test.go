package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDetach_UpstreamCallChainPropagationAndCycleRejection verifies D103 Item 7:
// A prompts B, then B dispatches --detach C, then C prompting A or B must be
// rejected on the strength of both being preserved in the call chain.
func TestDetach_UpstreamCallChainPropagationAndCycleRejection(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	agentADir := filepath.Join(wsDir, "agentA")
	agentBDir := filepath.Join(wsDir, "agentB")
	agentCDir := filepath.Join(wsDir, "agentC")

	for _, dir := range []string{agentADir, agentBDir, agentCDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create agent dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agent"), 0644); err != nil {
			t.Fatalf("failed to write AGENTS.md: %v", err)
		}
	}

	// Allowlist setup: A allows B; B allows C; C allows A and B.
	if err := os.WriteFile(filepath.Join(agentADir, AllowedAgentsFile), []byte("agentB\n"), 0644); err != nil {
		t.Fatalf("failed writing A allowlist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, AllowedAgentsFile), []byte("agentC\n"), 0644); err != nil {
		t.Fatalf("failed writing B allowlist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentCDir, AllowedAgentsFile), []byte("agentA\nagentB\n"), 0644); err != nil {
		t.Fatalf("failed writing C allowlist: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	defer os.Chdir(origCwd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	origChain := os.Getenv(CallChainEnvVar)
	defer func() {
		os.Setenv(Agent2AgentEnvVar, origA2A)
		os.Setenv(CallChainEnvVar, origChain)
	}()

	// 1. A prompts B: in B's execution context, the incoming chain is [A, B].
	traceID := GenerateTraceID()
	bIncomingMeta := &A2AMetadata{
		CallerID:  "agentA",
		CallChain: []string{"agentA", "agentB"},
		TraceID:   traceID,
		Metadata:  map[string]string{"flow": "test-multihop"},
	}
	bDense, err := bIncomingMeta.Encode()
	if err != nil {
		t.Fatalf("failed to encode B incoming meta: %v", err)
	}
	os.Setenv(Agent2AgentEnvVar, bDense)
	os.Setenv(CallChainEnvVar, "agentA,agentB")

	// 2. B dispatches --detach C from B's directory.
	if err := os.Chdir(agentBDir); err != nil {
		t.Fatalf("failed to chdir to agentB: %v", err)
	}

	// Parent B performs explicit ValidateAgentTarget("agentC") before spawn (Item 1).
	parentMeta, err := ValidateAgentTarget("agentC")
	if err != nil {
		t.Fatalf("B validating target C should succeed, got: %v", err)
	}
	if parentMeta == nil {
		t.Fatalf("expected non-nil parentMeta")
	}

	// Parent mints a correlation ID distinct from the trace ID.
	corrID := GenerateTraceID()
	if corrID == traceID {
		t.Fatalf("per-request correlation ID must be distinct from trace ID")
	}

	// Prepare the child's environment inheriting the caller's upstream chain.
	childIncomingMeta := &A2AMetadata{
		CallerID:  bIncomingMeta.CallerID,
		CallChain: append([]string{}, bIncomingMeta.CallChain...),
		TraceID:   parentMeta.TraceID,
		Metadata: map[string]string{
			CorrelationIDMetadataKey: corrID,
			"request_id":             corrID,
		},
	}
	childDense, err := childIncomingMeta.Encode()
	if err != nil {
		t.Fatalf("failed to encode child meta: %v", err)
	}

	// 3. Child C receives childIncomingMeta in its environment; child re-checks
	// authorization from the caller's CWD (identity anchor) - defense-in-depth.
	os.Setenv(Agent2AgentEnvVar, childDense)
	os.Setenv(CallChainEnvVar, strings.Join(childIncomingMeta.CallChain, ","))

	childMeta, err := ValidateAgentTarget("agentC")
	if err != nil {
		t.Fatalf("child C validating target C against upstream chain should succeed, got: %v", err)
	}

	// Child's updated call chain must be [agentA, agentB, agentC].
	expectedChain := []string{"agentA", "agentB", "agentC"}
	if len(childMeta.CallChain) != len(expectedChain) {
		t.Fatalf("expected child call chain %v, got %v", expectedChain, childMeta.CallChain)
	}
	for i, v := range expectedChain {
		if childMeta.CallChain[i] != v {
			t.Errorf("call chain mismatch at index %d: expected %s, got %s", i, v, childMeta.CallChain[i])
		}
	}

	// 4. Now inside C's turn (CWD = agentCDir), C's tool environment carries childMeta.
	if err := os.Chdir(agentCDir); err != nil {
		t.Fatalf("failed to chdir to agentC: %v", err)
	}
	cToolDense, err := childMeta.Encode()
	if err != nil {
		t.Fatalf("failed to encode C tool meta: %v", err)
	}
	os.Setenv(Agent2AgentEnvVar, cToolDense)
	os.Setenv(CallChainEnvVar, strings.Join(childMeta.CallChain, ","))

	// C prompting A must be REJECTED: agentA is in the call chain.
	_, err = ValidateAgentTarget("agentA")
	if err == nil {
		t.Fatalf("expected C prompting A to be REJECTED because agentA is in call chain, but it succeeded")
	}
	if !strings.Contains(err.Error(), "already in call chain") {
		t.Errorf("expected deadlock cycle error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "agentA") {
		t.Errorf("expected error to name agentA, got: %v", err)
	}

	// C prompting B must ALSO be rejected: agentB is in the call chain.
	_, err = ValidateAgentTarget("agentB")
	if err == nil {
		t.Fatalf("expected C prompting B to be REJECTED because agentB is in call chain, but it succeeded")
	}
	if !strings.Contains(err.Error(), "already in call chain") {
		t.Errorf("expected deadlock cycle error message for agentB, got: %v", err)
	}
}

// TestDetach_FailFastBusyTarget verifies D103 Item 4: when the target session
// lock is held, dispatch fails fast instead of queuing in an untimed flock wait.
func TestDetach_FailFastBusyTarget(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "busy_target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	holderLock, err := AcquireSessionLock(targetDir)
	if err != nil {
		t.Fatalf("failed to acquire initial lock: %v", err)
	}
	defer holderLock.Release()

	testLock, err := TryAcquireSessionLock(targetDir)
	if err == nil {
		testLock.Release()
		t.Fatalf("expected TryAcquireSessionLock to fail fast when busy, got nil")
	}

	if !errors.Is(err, ErrSessionBusy) && !strings.Contains(err.Error(), "agent session is busy") {
		t.Errorf("expected ErrSessionBusy, got: %v", err)
	}

	holderPID := strconv.Itoa(os.Getpid())
	if !strings.Contains(err.Error(), holderPID) {
		t.Errorf("expected error message to contain holder PID %s, got: %v", holderPID, err)
	}

	holderLock.Release()

	freeLock, err := TryAcquireSessionLock(targetDir)
	if err != nil {
		t.Fatalf("expected TryAcquireSessionLock to succeed after release, got: %v", err)
	}
	freeLock.Release()
}

// TestDetach_TimeoutProducesValidPartialSessionRecord verifies D103 Item 3:
// when a detached turn times out, the child-side context.WithTimeout unwinds
// cleanly, appends an explicit model notice turn, and commits to git so
// session.jsonl keeps role alternation valid.
func TestDetach_TimeoutProducesValidPartialSessionRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"Should not arrive"}}]}`)
	}))
	defer srv.Close()

	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	agentID := "sleeper"
	sdk := NewSDK(wsDir)
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are Sleeper."), 0644); err != nil {
		t.Fatalf("failed writing AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("sleeper\n"), 0644); err != nil {
		t.Fatalf("failed writing allowlist: %v", err)
	}

	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed writing runtime.json: %v", err)
	}

	if err := InitAgentGit(wsDir, agentID); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}

	origCwd, _ := os.Getwd()
	defer os.Chdir(origCwd)
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var yieldedChunks []string
	var finalErr error
	for chunk, err := range sdk.AddAndGenerateTurnStream(ctx, agentID, "Hello sleeper, please complete this task") {
		if chunk != "" {
			yieldedChunks = append(yieldedChunks, chunk)
		}
		if err != nil {
			finalErr = err
		}
	}

	if !errors.Is(finalErr, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", finalErr)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed after timeout: %v", err)
	}

	if len(turns) != 2 {
		t.Fatalf("expected exactly 2 turns in session.jsonl (user + model notice), got %d: %+v", len(turns), turns)
	}

	if turns[0].Role != "user" || !strings.Contains(ContentText(turns[0]), "Hello sleeper") {
		t.Errorf("unexpected turn 0: role=%s text=%s", turns[0].Role, ContentText(turns[0]))
	}

	if turns[1].Role != "model" {
		t.Errorf("expected turn 1 role to be 'model', got: %s", turns[1].Role)
	}
	if !strings.Contains(ContentText(turns[1]), "Generation timed out") {
		t.Errorf("expected turn 1 to contain timeout notice, got: %s", ContentText(turns[1]))
	}

	headSHA, err := GetWorkspaceHeadCommit(agentDir)
	if err != nil || headSHA == "" {
		t.Fatalf("expected git commit after timeout, got SHA: %q, err: %v", headSHA, err)
	}
}

// TestDetach_WorkspaceCommitSerialization verifies D103 Item 5: concurrent
// commits on a shared repository are serialized behind .git.lock.
func TestDetach_WorkspaceCommitSerialization(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	agentID := "worker"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := InitAgentGit(wsDir, agentID); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}

	var wg sync.WaitGroup
	errCount := 0
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = AppendSessionTurn(agentDir, "user", fmt.Sprintf("Turn %d", idx))
			if err := CommitWorkspaceEvent(wsDir, agentID, fmt.Sprintf("event-%d", idx)); err != nil {
				mu.Lock()
				errCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	if errCount > 0 {
		t.Errorf("expected all serialized commits to succeed, had %d errors", errCount)
	}

	lock, err := AcquireGitCommitLock(agentDir)
	if err != nil {
		t.Fatalf("failed to acquire git commit lock after concurrent operations: %v", err)
	}
	lock.Release()
}
