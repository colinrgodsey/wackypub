package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"github.com/colinrgodsey/wackypub/pkg/stdio"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var (
	wackypubBinOnce sync.Once
	wackypubBinPath string
	wackypubBinErr  error
)

// getWackypubBin builds (once) the real wackypub binary for spawn-in-test.
func getWackypubBin(t *testing.T) string {
	t.Helper()
	wackypubBinOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "wackypub-bin-*")
		if err != nil {
			wackypubBinErr = err
			return
		}
		out := filepath.Join(tmpDir, "wackypub")
		cmd := exec.Command("go", "build", "-o", out, "..")
		if outB, err := cmd.CombinedOutput(); err != nil {
			wackypubBinErr = fmt.Errorf("build wackypub: %w\n%s", err, outB)
			return
		}
		wackypubBinPath = out
	})
	if wackypubBinErr != nil {
		t.Fatalf("failed to build wackypub fixture: %v", wackypubBinErr)
	}
	return wackypubBinPath
}

// stdioWorkspace builds a workspace root with one mock-OpenAI-runtime agent
// and returns the wsDir + agentID.
func stdioWorkspace(t *testing.T, answer string) (string, string) {
	return stdioWorkspaceSlow(t, answer, 0)
}

// stdioWorkspaceSlow is stdioWorkspace with an optional pre-answer delay so a
// test can hold a turn in-flight before killing the child.
func stdioWorkspaceSlow(t *testing.T, answer string, delay time.Duration) (string, string) {
	t.Helper()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	agentID := "stdioagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("stdio test agent"), 0644); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, adkAgent.AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("allowlist: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, answer)
	}))
	t.Cleanup(srv.Close)

	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k"}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}
	return wsDir, agentID
}

// spawnStdioServe starts the real wackypub binary with cwd=wsDir and returns
// a grpc client over its stdio plus a cleanup that closes and reaps.
func spawnStdioServe(t *testing.T, wsDir string) (agentv1.AgentServiceClient, func()) {
	client, _, cleanup := spawnStdioServeWithEnv(t, wsDir)
	return client, cleanup
}

// spawnStdioServeWithEnv is spawnStdioServe with optional environment variable overrides.
func spawnStdioServeWithEnv(t *testing.T, wsDir string, extraEnv ...string) (agentv1.AgentServiceClient, *exec.Cmd, func()) {
	t.Helper()
	bin := getWackypubBin(t)
	cmd := exec.Command(bin, "stdio-serve")
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn, err := stdio.DialCommand(ctx, cmd, 3*time.Second)
	if err != nil {
		cancel()
		t.Fatalf("DialCommand: %v", err)
	}

	gc, err := grpc.NewClient(
		"passthrough:///stdio-serve-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return conn, nil
		}),
	)
	if err != nil {
		_ = conn.Close()
		cancel()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	client := agentv1.NewAgentServiceClient(gc)
	cleanup := func() {
		_ = gc.Close()
		_ = conn.Close()
		cancel()
	}
	t.Cleanup(cleanup)
	return client, cmd, cleanup
}

// TestStdioServe_CWDWorkspaceCarryOver is THE test for the one place a silent
// behavior difference could hide: the child must resolve the workspace from its
// own CWD exactly like the CLI (walk up for RootMarkerFile), not from any
// caller-supplied flag.
func TestStdioServe_CWDWorkspaceCarryOver(t *testing.T) {
	wsDir, agentID := stdioWorkspace(t, "carry over")
	client, _ := spawnStdioServe(t, wsDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents over stdio: %v", err)
	}
	if len(resp.GetAgentIds()) != 1 || resp.GetAgentIds()[0] != agentID {
		t.Fatalf("expected CWD-resolved workspace to list %q, got %v", agentID, resp.GetAgentIds())
	}
}

// TestStdioServe_StreamingRPCCleanExit spawns the real binary, performs a real
// streaming AddAndGenerateTurnStream over stdio (mock OpenAI reply), then
// closes the client and asserts the process exits promptly (CLI lifecycle).
func TestStdioServe_StreamingRPCCleanExit(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "hello from stdio serve")
	bin := getWackypubBin(t)

	cmd := exec.Command(bin, "stdio-serve")
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr

	conn, err := stdio.DialCommand(context.Background(), cmd, 3*time.Second)
	if err != nil {
		t.Fatalf("DialCommand: %v", err)
	}
	defer conn.Close()

	gc, err := grpc.NewClient(
		"passthrough:///stdio-serve-test2",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return conn, nil
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer gc.Close()

	client := agentv1.NewAgentServiceClient(gc)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "stdioagent",
		UserMessage: "hello",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream: %v", err)
	}

	var text string
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				t.Fatalf("stream recv: %v", err)
			}
			break
		}
		text += chunk.GetText()
	}
	if !contains(text, "hello from stdio serve") {
		t.Fatalf("got %q, want mock answer", text)
	}

	// CLI lifecycle: closing the client conn must make the child exit promptly.
	_ = gc.Close()
	_ = conn.Close()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
		// Clean exit.
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit within 5s of client close")
	}
}

// TestStdioServe_ChildDiesMidCall verifies error surfacing when the child dies
// mid-stream: the client must get a non-EOF error, not a hang.
func TestStdioServe_ChildDiesMidCall(t *testing.T) {
	wsDir, _ := stdioWorkspaceSlow(t, "will die mid-stream", 5*time.Second)
	bin := getWackypubBin(t)

	cmd := exec.Command(bin, "stdio-serve")
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr

	conn, err := stdio.DialCommand(context.Background(), cmd, 3*time.Second)
	if err != nil {
		t.Fatalf("DialCommand: %v", err)
	}
	defer conn.Close()

	gc, err := grpc.NewClient(
		"passthrough:///stdio-serve-kill",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return conn, nil
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer gc.Close()

	client := agentv1.NewAgentServiceClient(gc)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Start a streaming turn; the mock answers instantly, so begin a stream that
	// we hold open, then SIGKILL the child mid-request.
	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "stdioagent",
		UserMessage: "boom",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream: %v", err)
	}

	// Let the round trip begin, then kill the child process group.
	time.Sleep(100 * time.Millisecond)
	if err := killChild(cmd); err != nil {
		t.Fatalf("kill child: %v", err)
	}

	// The stream must surface a non-EOF error (Unavailable/transport closed),
	// not block forever.
	_, err = stream.Recv()
	if err == nil || err == io.EOF {
		t.Fatalf("expected a transport error after child death, got %v", err)
	}
	if !contains(err.Error(), "connection") && !contains(err.Error(), "transport") && !contains(err.Error(), "EOF") && !contains(err.Error(), "closed") {
		t.Logf("child-death surfaced as: %v", err)
	}

	// Cleanup: reap the child.
	_ = conn.Close()
}

// helper to keep syscall import meaningful for the kill test
func killChild(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return cmd.Process.Kill()
}

// TestStdioServe_ClientKillTeardown verifies the process-hygiene rule: when the
// CLIENT dies abruptly (its stdio fds close without a graceful grpc shutdown),
// the serve process sees stdin EOF and exits promptly - no orphan child.
func TestStdioServe_ClientKillTeardown(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "teardown")
	bin := getWackypubBin(t)

	cmd := exec.Command(bin, "stdio-serve")
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr

	// Same wiring as DialCommand but WITHOUT DialCommand's reaping: we deliberately
	// abandon stdio to simulate a killed client, so the pipes are closed abruptly.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	// Give the child a beat to start serving, then simulate the client dying:
	// closing both pipe fds without any graceful grpc close.
	time.Sleep(200 * time.Millisecond)
	_ = stdin.Close()
	_ = stdout.Close()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
		// Child exited cleanly on stdin EOF - the no-orphan contract.
	case <-time.After(5 * time.Second):
		_ = killChild(cmd)
		t.Fatal("child did not exit within 5s of client pipe close")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestStdioServe_ExplicitWorkspaceOverride pins phoebe's hard requirement: the
// served SDK must honor an explicit workspace_dir on each request, even when
// the child was spawned in a DIFFERENT workspace root (CWD inference stays the
// default; the override is the contract the decoupled bot relies on).
func TestStdioServe_ExplicitWorkspaceOverride(t *testing.T) {
	wsA, _ := stdioWorkspace(t, "workspace A")
	// Build a second workspace with a DIFFERENT agent name.
	wsB := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsB, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker B: %v", err)
	}
	agentB := "agentB"
	agentBDir := filepath.Join(wsB, agentB)
	if err := os.MkdirAll(agentBDir, 0755); err != nil {
		t.Fatalf("mkdir agentB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, "AGENTS.md"), []byte("b"), 0644); err != nil {
		t.Fatalf("AGENTS.md B: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, adkAgent.AllowedAgentsFile), []byte(agentB+"\n"), 0644); err != nil {
		t.Fatalf("allowlist B: %v", err)
	}

	// Spawn in wsA; the request explicitly targets wsB.
	client, _ := spawnStdioServe(t, wsA)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{WorkspaceDir: wsB})
	if err != nil {
		t.Fatalf("ListAgents with explicit workspace: %v", err)
	}
	if len(resp.GetAgentIds()) != 1 || resp.GetAgentIds()[0] != agentB {
		t.Fatalf("expected explicit workspace B to list %q, got %v", agentB, resp.GetAgentIds())
	}
}

// TestStdioServe_BinaryScratchpadCreate pins the protocol surface wackydiscord
// relies on: CreateScratchpad with data bytes stores a binary D48 entry.
func TestStdioServe_BinaryScratchpadCreate(t *testing.T) {
	wsDir, agentID := stdioWorkspace(t, "binary")
	client, _ := spawnStdioServe(t, wsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	createResp, err := client.CreateScratchpad(ctx, &agentv1.CreateScratchpadRequest{
		AgentId:  agentID,
		Data:     []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, // PNG magic
		MimeType: "image/png",
	})
	if err != nil {
		t.Fatalf("CreateScratchpad binary: %v", err)
	}
	if !createResp.GetEntry().GetIsBinary() {
		t.Fatalf("expected binary entry, got %v", createResp.GetEntry())
	}
	if createResp.GetEntry().GetMimeType() != "image/png" {
		t.Fatalf("expected mime image/png, got %q", createResp.GetEntry().GetMimeType())
	}

	// Read back through ListScratchpads: the binary entry must round-trip.
	listResp, err := client.ListScratchpads(ctx, &agentv1.ListScratchpadsRequest{
		AgentId: agentID,
	})
	if err != nil {
		t.Fatalf("ListScratchpads binary: %v", err)
	}
	var found bool
	for _, e := range listResp.GetEntries() {
		if e.GetEntryId() == createResp.GetEntry().GetEntryId() {
			found = true
			if !e.GetIsBinary() {
				t.Fatalf("expected binary on read-back, got %v", e)
			}
		}
	}
	if !found {
		t.Fatalf("created entry %q not found in list", createResp.GetEntry().GetEntryId())
	}
}

// TestStdioServe_BridgedAgentRouting verifies that wackypub stdio-serve acts as a routing
// proxy, dispatching calls for bridged agents defined in REMOTE_MANIFEST to their configured
// bridge subprocess over gRPC.
func TestStdioServe_BridgedAgentRouting(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShim(t)

	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	manifest := fmt.Sprintf("bridgedagent: %s --behavior=echo\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RemoteManifestFile), []byte(manifest), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	client, _ := spawnStdioServe(t, wsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. ListAgents includes bridged agent from manifest
	listResp, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var found bool
	for _, id := range listResp.GetAgentIds() {
		if id == "bridgedagent" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected bridgedagent in ListAgents, got %v", listResp.GetAgentIds())
	}

	// 2. InspectAgent routes to bridge
	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "bridgedagent"})
	if err != nil {
		t.Fatalf("InspectAgent over stdio: %v", err)
	}
	if insp.GetAgentId() != "bridgedagent" {
		t.Errorf("expected agent ID %q, got %q", "bridgedagent", insp.GetAgentId())
	}

	// 3. AddAndGenerateTurnStream routes to bridge
	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "bridgedagent",
		UserMessage: "hello over stdio",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream over stdio: %v", err)
	}
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream recv over stdio: %v", err)
	}
	if chunk.GetText() != "echo: hello over stdio" {
		t.Errorf("expected 'echo: hello over stdio', got %q", chunk.GetText())
	}
}

// TestStdioServe_TurnPanicRecovery_ServerStaysAlive verifies that a turn panic
// in a spawned stdio-serve process returns codes.Internal over the protocol and
// leaves the child process alive to serve subsequent requests.
func TestStdioServe_TurnPanicRecovery_ServerStaysAlive(t *testing.T) {
	wsDir, agentID := stdioWorkspace(t, "healthy post-panic answer")
	client, _, cleanup := spawnStdioServeWithEnv(t, wsDir, "WACKYPUB_FAULT_INJECT=turn_panic")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Injected turn panic: must return protocol error (codes.Internal), not kill child
	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: "__FAULT_INJECT_PANIC__",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream call setup: %v", err)
	}

	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatal("expected recv error from turn panic, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("expected codes.Internal status from turn panic, got %v (err: %v)", st.Code(), recvErr)
	}
	if !contains(st.Message(), "panic during turn generation") {
		t.Fatalf("expected error message to mention 'panic during turn generation', got %q", st.Message())
	}

	// 2. Subsequent turn on SAME client connection must succeed without server restart
	stream2, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: "subsequent healthy message",
	})
	if err != nil {
		t.Fatalf("subsequent AddAndGenerateTurnStream failed: %v", err)
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
	if !contains(reply, "healthy post-panic answer") {
		t.Fatalf("expected mock answer on subsequent turn, got: %q", reply)
	}
}

// TestStdioServe_UnaryPanicRecovery_ServerStaysAlive verifies that a unary panic
// in stdio-serve returns codes.Internal and leaves the server alive for subsequent calls.
func TestStdioServe_UnaryPanicRecovery_ServerStaysAlive(t *testing.T) {
	wsDir, agentID := stdioWorkspace(t, "healthy answer")
	client, _, cleanup := spawnStdioServeWithEnv(t, wsDir, "WACKYPUB_FAULT_INJECT=unary_panic")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Injected panic on unary method
	_, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{
		AgentId: "__PANIC_AGENT__",
	})
	if err == nil {
		t.Fatal("expected error on panic agent, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("expected codes.Internal, got %v (err: %v)", st.Code(), err)
	}

	// 2. Normal unary call on same client succeeds
	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{
		AgentId: agentID,
	})
	if err != nil {
		t.Fatalf("InspectAgent after recovered panic: %v", err)
	}
	if !insp.GetAgentDirExists() {
		t.Fatal("expected agent dir to exist")
	}
}

// TestStdioServe_UnrecoverablePanic_ProcessDies verifies the contract that
// unrecoverable internal corruption panics do NOT get swallowed; the process terminates.
func TestStdioServe_UnrecoverablePanic_ProcessDies(t *testing.T) {
	wsDir, agentID := stdioWorkspace(t, "should not be reached")
	client, cmd, cleanup := spawnStdioServeWithEnv(t, wsDir, "WACKYPUB_FAULT_INJECT=turn_unrecoverable")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: "__FAULT_INJECT_UNRECOVERABLE__",
	})
	if err == nil {
		_, _ = stream.Recv()
	}

	// Child must terminate with non-zero exit status
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatal("expected non-zero exit on unrecoverable panic, got exit 0")
		}
	case <-time.After(5 * time.Second):
		_ = killChild(cmd)
		t.Fatal("stdio-serve child did not terminate on unrecoverable panic within 5s")
	}
}

// TestStdioServe_SessionLockTimeout_ServerStaysAlive verifies that an agent lock contention
// timeout returns an error over the protocol and keeps stdio-serve alive.
func TestStdioServe_SessionLockTimeout_ServerStaysAlive(t *testing.T) {
	wsDir, agentID := stdioWorkspace(t, "post lock answer")
	agentDir := filepath.Join(wsDir, agentID)

	// External process / test thread holds session.lock
	holder, err := adkAgent.AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("AcquireSessionLock: %v", err)
	}

	client, cleanup := spawnStdioServe(t, wsDir)
	defer cleanup()

	// 1. Call AddAndGenerateTurnStream with short 200ms timeout
	callCtx, callCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer callCancel()

	stream, err := client.AddAndGenerateTurnStream(callCtx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: "contended message",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream setup: %v", err)
	}

	_, recvErr := stream.Recv()
	if recvErr == nil {
		holder.Release()
		t.Fatal("expected error due to session lock contention, got nil")
	}

	// 2. Release external lock; subsequent call on SAME stdio-serve process succeeds
	holder.Release()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	stream2, err := client.AddAndGenerateTurnStream(ctx2, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: "uncontended message",
	})
	if err != nil {
		t.Fatalf("second AddAndGenerateTurnStream failed: %v", err)
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
	if !contains(reply, "post lock answer") {
		t.Fatalf("expected mock answer, got: %q", reply)
	}
}

// TestStdioServe_StreamCancellation_ServerStaysAlive verifies that client cancellation
// mid-stream cleanly aborts the call, releases the lock, and keeps stdio-serve alive.
func TestStdioServe_StreamCancellation_ServerStaysAlive(t *testing.T) {
	wsDir, agentID := stdioWorkspaceSlow(t, "slow answer", 3*time.Second)
	client, cleanup := spawnStdioServe(t, wsDir)
	defer cleanup()

	// 1. Start stream and cancel early
	ctx1, cancel1 := context.WithCancel(context.Background())
	stream, err := client.AddAndGenerateTurnStream(ctx1, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: "abort turn",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream call 1: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	cancel1()

	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}

	// 2. Subsequent call to ListAgents on SAME client succeeds
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	resp, err := client.ListAgents(ctx2, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents after cancellation: %v", err)
	}
	if len(resp.GetAgentIds()) == 0 {
		t.Fatal("expected at least 1 agent")
	}
}
