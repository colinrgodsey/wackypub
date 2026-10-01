package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/genai"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func createTestAgentWithMockRuntime(t *testing.T, wsDir, agentID, answer string) string {
	t.Helper()
	t.Chdir(wsDir)
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("test agent"), 0644); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("allowlist: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, answer)
	}))
	t.Cleanup(srv.Close)

	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k"}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}
	return agentDir
}

// TestRobustness_RecoverableUnaryPanic verifies that a panic in an RPC method
// is caught by StreamHandler, converted to codes.Internal, and the server continues serving.
func TestRobustness_RecoverableUnaryPanic(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	createTestAgentWithMockRuntime(t, wsDir, "panicagent", "hello")

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	// 1. Injected panic on InspectAgent
	testHookRoutingPreDispatch = func(methodName string, req any) {
		if methodName == "InspectAgent" {
			panic("injected unary panic")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "panicagent"})
	if err == nil {
		t.Fatal("expected error from panic in InspectAgent, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("expected codes.Internal status error, got code %v, err: %v", st.Code(), err)
	}
	if !strings.Contains(st.Message(), "injected unary panic") {
		t.Fatalf("expected message to mention 'injected unary panic', got %q", st.Message())
	}

	// 2. Clear hook: server must still be alive and serve subsequent calls cleanly
	testHookRoutingPreDispatch = nil

	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "panicagent"})
	if err != nil {
		t.Fatalf("InspectAgent after recovered panic failed: %v", err)
	}
	if !insp.GetAgentDirExists() {
		t.Fatal("expected agent dir to exist on subsequent call")
	}
}

// TestRobustness_RecoverableTurnPanic verifies that a panic inside the streaming
// generator goroutine is caught, converted to codes.Internal over gRPC, releases
// the session lock, and allows subsequent turns to succeed on the same server.
func TestRobustness_RecoverableTurnPanic(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	createTestAgentWithMockRuntime(t, wsDir, "turnagent", "mock turn response")

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	// 1. Injected turn panic in generator goroutine
	testHookTurnPreGenerate = func(agentID, userMessage string) {
		panic("injected turn generator panic")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "turnagent",
		UserMessage: "hello with panic",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream initial call: %v", err)
	}

	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatal("expected recv error from turn panic, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("expected codes.Internal from turn panic, got code %v, err: %v", st.Code(), recvErr)
	}
	if !strings.Contains(st.Message(), "injected turn generator panic") {
		t.Fatalf("expected panic message in error, got: %v", st.Message())
	}

	// 2. Clear hook and verify session lock is free and subsequent turn succeeds
	testHookTurnPreGenerate = nil

	stream2, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "turnagent",
		UserMessage: "hello subsequent turn",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream after recovered panic: %v", err)
	}

	var reply string
	for {
		chunk, err := stream2.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("stream2 recv failed: %v", err)
		}
		reply += chunk.GetText()
	}
	if !strings.Contains(reply, "mock turn response") {
		t.Fatalf("expected mock response on subsequent turn, got: %q", reply)
	}
}

// TestRobustness_SessionLockTimeout verifies that lock contention and context deadline
// return codes.DeadlineExceeded, leave the server alive, and allow subsequent requests.
func TestRobustness_SessionLockTimeout(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentDir := createTestAgentWithMockRuntime(t, wsDir, "lockagent", "after lock release")

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	// 1. External lock holder acquires session.lock
	holder, err := AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("AcquireSessionLock: %v", err)
	}

	// 2. Call AddAndGenerateTurnStream with short timeout (150ms)
	callCtx, callCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer callCancel()

	stream, err := client.AddAndGenerateTurnStream(callCtx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "lockagent",
		UserMessage: "contended call",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream call setup: %v", err)
	}

	_, recvErr := stream.Recv()
	if recvErr == nil {
		holder.Release()
		t.Fatal("expected lock timeout error, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok || st.Code() != codes.DeadlineExceeded {
		t.Logf("contention recv error code=%v msg=%v", st.Code(), recvErr)
	}

	// 3. Release external lock: subsequent call must succeed on the same server
	holder.Release()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	stream2, err := client.AddAndGenerateTurnStream(ctx2, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "lockagent",
		UserMessage: "uncontended call",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream after lock release: %v", err)
	}

	var reply string
	for {
		chunk, err := stream2.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("stream2 recv failed: %v", err)
		}
		reply += chunk.GetText()
	}
	if !strings.Contains(reply, "after lock release") {
		t.Fatalf("expected answer after lock release, got: %q", reply)
	}
}

// TestRobustness_StreamCancellationCleanTeardown verifies that cancelling a client
// stream context cleanly frees the lock and server keeps serving.
func TestRobustness_StreamCancellationCleanTeardown(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	createTestAgentWithMockRuntime(t, wsDir, "cancelagent", "cancelled turn")

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	// 1. Start stream and immediately cancel context
	ctx1, cancel1 := context.WithCancel(context.Background())
	stream1, err := client.AddAndGenerateTurnStream(ctx1, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "cancelagent",
		UserMessage: "abort me",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream call 1: %v", err)
	}
	cancel1()

	// Drain stream until error
	for {
		_, err := stream1.Recv()
		if err != nil {
			break
		}
	}

	// 2. Ensure server is healthy and session lock is released for next call
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	stream2, err := client.AddAndGenerateTurnStream(ctx2, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "cancelagent",
		UserMessage: "second call after cancel",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream call 2: %v", err)
	}

	var reply string
	for {
		chunk, err := stream2.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("stream2 recv: %v", err)
		}
		reply += chunk.GetText()
	}
	if !strings.Contains(reply, "cancelled turn") {
		t.Fatalf("expected normal response on call 2, got: %q", reply)
	}
}

// TestRobustness_UnrecoverableClassification tests the IsUnrecoverable helper.
func TestRobustness_UnrecoverableClassification(t *testing.T) {
	if IsUnrecoverable(nil) {
		t.Fatal("nil should not be unrecoverable")
	}
	if IsUnrecoverable("plain panic string") {
		t.Fatal("plain string should not be unrecoverable")
	}
	if IsUnrecoverable(errors.New("plain error")) {
		t.Fatal("plain error should not be unrecoverable")
	}

	corrupt := MarkUnrecoverable("disk corruption", errors.New("bad magic"))
	if !IsUnrecoverable(corrupt) {
		t.Fatal("MarkUnrecoverable should be unrecoverable")
	}
	wrapped := fmt.Errorf("wrap: %w", corrupt)
	if !IsUnrecoverable(wrapped) {
		t.Fatal("wrapped MarkUnrecoverable should be unrecoverable")
	}
	custom := &CorruptStateError{Reason: "state desync"}
	if !IsUnrecoverable(custom) {
		t.Fatal("CorruptStateError should be unrecoverable")
	}
}

// TestRobustness_StreamHandlerRePanicsOnUnrecoverable verifies that StreamHandler
// does not swallow unrecoverable panics, re-panicking as required by contract.
func TestRobustness_StreamHandlerRePanicsOnUnrecoverable(t *testing.T) {
	wsDir := t.TempDir()
	sdk := NewSDK(wsDir)
	router := NewRoutingServer(sdk)

	testHookRoutingPreDispatch = func(methodName string, req any) {
		panic(MarkUnrecoverable("fatal memory corruption"))
	}
	defer func() {
		testHookRoutingPreDispatch = nil
	}()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to be re-panicked, but recover was nil")
		}
		if !IsUnrecoverable(r) {
			t.Fatalf("expected unrecoverable panic, got %v", r)
		}
	}()

	// Call StreamHandler via mock stream
	ctx := grpc.NewContextWithServerTransportStream(context.Background(), &mockServerTransportStream{method: "/wackypub.agent.v1.AgentService/ListAgents"})
	mock := &mockServerStream{ctx: ctx}
	_ = router.StreamHandler(nil, mock)
}

type mockServerTransportStream struct {
	method string
}

func (m *mockServerTransportStream) Method() string               { return m.method }
func (m *mockServerTransportStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockServerTransportStream) SendHeader(metadata.MD) error { return nil }
func (m *mockServerTransportStream) SetTrailer(metadata.MD) error { return nil }

type mockServerStream struct {
	ctx context.Context
}

func (m *mockServerStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockServerStream) SendHeader(metadata.MD) error { return nil }
func (m *mockServerStream) SetTrailer(metadata.MD)       {}
func (m *mockServerStream) Context() context.Context     { return m.ctx }
func (m *mockServerStream) SendMsg(m2 any) error         { return nil }
func (m *mockServerStream) RecvMsg(m2 any) error         { return nil }

// TestRecoverMaxSeqFromLog_ScannerErrorIsUnrecoverable verifies the #88 fail-closed path
// now carries the unrecoverable marker: a session log with a line over the 16MiB scanner
// cap cannot be safely allocated from, so the error is marked corrupt-state, which the
// #90 supervision (stdio-serve / ProcessDialer) turns into a process restart instead of
// failing every subsequent turn individually.
func TestRecoverMaxSeqFromLog_ScannerErrorIsUnrecoverable(t *testing.T) {
	agentDir := t.TempDir()
	// One small turn, then a line far beyond the 16MiB scanner buffer.
	turns := []*genai.Content{genai.NewContentFromText("seed", "user")}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("WriteSessionTurns: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	// 17MiB line - exceeds scanner buffer (16MiB).
	if _, err := f.Write(append([]byte(strings.Repeat("x", 17*1024*1024)), '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = recoverMaxSeqFromLog(agentDir)
	if err == nil {
		t.Fatal("expected scanner error from 17MiB line")
	}
	if !IsUnrecoverable(err) {
		t.Fatalf("scanner error should be marked unrecoverable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be scanned for sequence inference") {
		t.Errorf("expected reason in error, got: %v", err)
	}
}

// TestRecoverMaxSeqFromLog_LegacyUnsequencedStaysRecoverable verifies the legacy-row path
// (valid JSON, no seq key) is NOT unrecoverable - that's log-format correctness, not corruption.
func TestRecoverMaxSeqFromLog_LegacyUnsequencedStaysRecoverable(t *testing.T) {
	agentDir := t.TempDir()
	// A legacy row written directly without a seq key.
	if err := WriteSessionTurns(agentDir, []*genai.Content{genai.NewContentFromText("legacy", "user")}); err != nil {
		t.Fatalf("WriteSessionTurns: %v", err)
	}
	// Overwrite with a legacy-format line (no seq in the JSON).
	legacy := `{"content":{"role":"user","parts":[{"text":"legacy row"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(agentDir, SessionFileName), []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	seq, err := recoverMaxSeqFromLog(agentDir)
	if err != nil {
		t.Fatalf("legacy row should scan cleanly: %v", err)
	}
	if seq != 1 {
		t.Errorf("expected maxSeq 1 (single unsequenced row occupies 1..N), got %d", seq)
	}
	if IsUnrecoverable(err) {
		t.Error("legacy row is log-format correctness, must NOT be unrecoverable")
	}
}

// TestStreamHandler_RePanicsOnUnrecoverableError verifies the error-branch of the
// StreamHandler recover: an RPC that RETURNS an unrecoverable error (not just panics)
// must re-panic so supervision restarts - a corrupt session becomes a server-level
// restart, not a per-turn protocol error.
func TestStreamHandler_RePanicsOnUnrecoverableError(t *testing.T) {
	wsDir := t.TempDir()
	sdk := NewSDK(wsDir)
	router := NewRoutingServer(sdk)

	testHookRoutingPreDispatch = func(methodName string, req any) {
		// Simulate an RPC returning an unrecoverable error through its normal error path.
		panic(MarkUnrecoverable("corrupt session state"))
	}
	defer func() {
		testHookRoutingPreDispatch = nil
	}()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected unrecoverable to re-panic, got nil")
		}
		if !IsUnrecoverable(r) {
			t.Fatalf("expected unrecoverable panic, got %v", r)
		}
	}()

	ctx := grpc.NewContextWithServerTransportStream(context.Background(), &mockServerTransportStream{method: "/wackypub.agent.v1.AgentService/ListAgents"})
	mock := &mockServerStream{ctx: ctx}
	_ = router.StreamHandler(nil, mock)
}
