package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// spawnTCPServe starts the real wackypub binary in tcp-serve mode on an
// ephemeral port and returns a grpc client dialing it directly. The dial
// target is the ONLY thing that differs from the stdio path - everything the
// client does afterwards is transport-agnostic. This is the card's acceptance:
// the same client works against both serve modes with only the dial changed.
func spawnTCPServe(t *testing.T, wsDir string, extraEnv ...string) (agentv1.AgentServiceClient, net.Addr, func()) {
	t.Helper()
	bin := getWackypubBin(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	cmd := exec.Command(bin, "tcp-serve", "--listen", addr)
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), extraEnv...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tcp-serve: %v", err)
	}

	// Wait for the listener to accept (the process prints its listen line, but
	// a real connect is the reliable readiness check).
	token := ""
	for _, e := range extraEnv {
		if v, ok := strings.CutPrefix(e, serveTokenEnv+"="); ok {
			token = v
		}
	}
	gc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	client := agentv1.NewAgentServiceClient(gc)

	// Readiness: block until a cheap call succeeds. With a token configured the
	// probe must carry it, else the server (correctly) refuses and readiness
	// never flips.
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		_, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
		cancel()
		if err == nil {
			ready = true
			break
		}
	}
	if !ready {
		_ = gc.Close()
		_ = cmd.Process.Kill()
		t.Fatalf("tcp-serve did not become ready on %s", addr)
	}

	cleanup := func() {
		_ = gc.Close()
		_ = cmd.Process.Kill()
	}
	t.Cleanup(cleanup)
	return client, lis.Addr(), cleanup
}

// TestTCPServe_ListAgentsAndInspect proves the served surface over TCP: the
// same ListAgents + InspectAgent calls that stdio-serve answers, over a real
// network dial, with the workspace resolved from the child's CWD.
func TestTCPServe_ListAgentsAndInspect(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "tcp reply")
	client, _, _ := spawnTCPServe(t, wsDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agents, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents.GetAgentIds()) != 1 || agents.GetAgentIds()[0] != "stdioagent" {
		t.Fatalf("ListAgents over tcp returned %+v", agents.GetAgentIds())
	}

	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("InspectAgent: %v", err)
	}
	if !insp.GetAgentDirExists() {
		t.Fatalf("InspectAgent said agent dir does not exist")
	}
}

// TestServeSurface_IdenticalAcrossStdioAndTCP is the card's acceptance: the same
// grpc test client operations run against both serve modes with only the dial
// target changed. The operation set is deliberately shared: ListAgents and an
// InspectAgent that must produce the identical answer on both transports.
func TestServeSurface_IdenticalAcrossStdioAndTCP(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "surface reply")

	// stdio side
	stdioClient, _ := spawnStdioServe(t, wsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdioAgents, err := stdioClient.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("stdio ListAgents: %v", err)
	}
	stdioInsp, err := stdioClient.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("stdio InspectAgent: %v", err)
	}

	// tcp side, same workspace, same calls
	tcpClient, _, _ := spawnTCPServe(t, wsDir)
	tcpAgents, err := tcpClient.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("tcp ListAgents: %v", err)
	}
	tcpInsp, err := tcpClient.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("tcp InspectAgent: %v", err)
	}

	if fmt.Sprint(stdioAgents.GetAgentIds()) != fmt.Sprint(tcpAgents.GetAgentIds()) {
		t.Errorf("ListAgents differs between transports:\nstdio: %v\ntcp:   %v", stdioAgents.GetAgentIds(), tcpAgents.GetAgentIds())
	}
	if fmt.Sprint(stdioInsp) != fmt.Sprint(tcpInsp) {
		t.Errorf("InspectAgent differs between transports:\nstdio: %v\ntcp:   %v", stdioInsp, tcpInsp)
	}
}

// TestTCPServe_NonLoopbackRequiresToken: binding a non-loopback address without
// WACKYPUB_SERVE_TOKEN must be refused at startup.
func TestTCPServe_NonLoopbackRequiresToken(t *testing.T) {
	bin := getWackypubBin(t)
	wsDir, _ := stdioWorkspace(t, "x")

	// 0.0.0.0 is non-loopback semantically even on a loopback-only host.
	cmd := exec.Command(bin, "tcp-serve", "--listen", "0.0.0.0:0")
	cmd.Dir = wsDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("non-loopback bind without token should fail, got success: %s", out)
	}
	if !strings.Contains(string(out), serveTokenEnv) {
		t.Errorf("error should name %s, got: %s", serveTokenEnv, out)
	}
}

// TestTCPServe_TokenRequiredWhenSet: when a token is configured, calls without
// it are refused, and calls with it succeed.
func TestTCPServe_TokenRequiredWhenSet(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "token reply")
	client, _, _ := spawnTCPServe(t, wsDir, serveTokenEnv+"=sekret")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Without the token: Unauthenticated.
	_, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token should be Unauthenticated, got %v", err)
	}
}

// TestTCPServe_TokenAccepted sets the metadata and verifies the call succeeds.
func TestTCPServe_TokenAccepted(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "token ok")
	client, _, _ := spawnTCPServe(t, wsDir, serveTokenEnv+"=sekret")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = withBearer(ctx, "sekret")

	agents, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("with-token ListAgents: %v", err)
	}
	if len(agents.GetAgentIds()) != 1 {
		t.Fatalf("with-token ListAgents returned %+v", agents.GetAgentIds())
	}
}

// TestTCPServe_SIGTERMDrainsCleanly: send SIGTERM and verify the process exits
// promptly (drain path) rather than hanging.
func TestTCPServe_SIGTERMDrainsCleanly(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "drain")
	bin := getWackypubBin(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	cmd := exec.Command(bin, "tcp-serve", "--listen", addr)
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Give it a moment to listen, then SIGTERM.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	select {
	case <-done:
		// drained and exited
	case <-time.After(6 * time.Second):
		t.Fatal("tcp-serve did not exit after SIGTERM")
	}
}

func withBearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// TestTCPServe_MultiAgentViaRoutingProxy is the card's multi-agent acceptance:
// one tcp-serve process hosting a workspace with TWO agent ids, both resolvable
// through the routing proxy on the same listener. This mirrors the stdio
// multi-agent shape (the #82 proxy is the serve-side resolution) over network.
func TestTCPServe_MultiAgentViaRoutingProxy(t *testing.T) {
	wsA, _ := stdioWorkspace(t, "multi a")
	// Add a second agent to the SAME workspace.
	agentB := "agentB"
	agentBDir := filepath.Join(wsA, agentB)
	if err := os.MkdirAll(agentBDir, 0755); err != nil {
		t.Fatalf("mkdir agentB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, "AGENTS.md"), []byte("b"), 0644); err != nil {
		t.Fatalf("AGENTS.md B: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, adkAgent.AllowedAgentsFile), []byte(agentB+"\n"), 0644); err != nil {
		t.Fatalf("allowlist B: %v", err)
	}

	client, _, _ := spawnTCPServe(t, wsA)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	ids := resp.GetAgentIds()
	if len(ids) != 2 {
		t.Fatalf("expected 2 agents over tcp, got %v", ids)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["stdioagent"] || !found[agentB] {
		t.Fatalf("both agent ids must resolve through the routing proxy, got %v", ids)
	}

	// And InspectAgent routes to each id on the same server.
	for _, id := range ids {
		insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: id, WorkspaceDir: wsA})
		if err != nil {
			t.Fatalf("InspectAgent %s: %v", id, err)
		}
		if !insp.GetAgentDirExists() {
			t.Errorf("agent %s reported missing", id)
		}
	}
}
