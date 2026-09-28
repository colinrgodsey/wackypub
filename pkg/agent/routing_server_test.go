package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func startTestRoutingClient(t *testing.T, wsDir string) (agentv1.AgentServiceClient, func()) {
	t.Helper()
	sdk := NewSDK(wsDir)
	router := NewRoutingServer(sdk)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := grpc.NewServer(router.UnknownServiceHandler())
	go func() {
		_ = srv.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		srv.Stop()
		_ = lis.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	client := agentv1.NewAgentServiceClient(conn)
	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
	return client, cleanup
}

func TestRoutingServer_ListAgents_MergesBridged(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	// Create native agent directory
	nativeDir := filepath.Join(wsDir, "nativeagent")
	if err := os.MkdirAll(nativeDir, 0755); err != nil {
		t.Fatalf("mkdir native: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "AGENTS.md"), []byte("native prompt"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	// Add bridged agent to REMOTE_MANIFEST without a directory
	manifest := "bridgedagent: /bin/echo\n"
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifest), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	resp, err := client.ListAgents(context.Background(), &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	if len(resp.GetAgentIds()) != 2 {
		t.Fatalf("expected 2 agents, got %v", resp.GetAgentIds())
	}
	if resp.GetAgentIds()[0] != "bridgedagent" || resp.GetAgentIds()[1] != "nativeagent" {
		t.Fatalf("expected [bridgedagent, nativeagent], got %v", resp.GetAgentIds())
	}
}

func TestRoutingServer_BridgedAgentRouting(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)

	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	manifest := fmt.Sprintf("bridgedagent: %s --behavior=echo\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifest), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. InspectAgent routes to shim
	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "bridgedagent"})
	if err != nil {
		t.Fatalf("InspectAgent on bridged agent: %v", err)
	}
	if insp.GetAgentId() != "bridgedagent" {
		t.Errorf("expected agent ID %q, got %q", "bridgedagent", insp.GetAgentId())
	}

	// 2. AddAndGenerateTurnStream streams from shim
	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "bridgedagent",
		UserMessage: "hello bridge",
	})
	if err != nil {
		t.Fatalf("AddAndGenerateTurnStream: %v", err)
	}
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream recv: %v", err)
	}
	if chunk.GetText() != "echo: hello bridge" {
		t.Errorf("expected 'echo: hello bridge', got %q", chunk.GetText())
	}

	// 3. ReadSession routes to shim
	readResp, err := client.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: "bridgedagent"})
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(readResp.GetTurns()) == 0 || readResp.GetTurns()[0].GetParts()[0].GetText() != "echo from shim: bridgedagent" {
		t.Errorf("expected shim read session echo, got %+v", readResp.GetTurns())
	}
}

func TestRoutingServer_RemoteManifestStrictPrecedence(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)

	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	// Create local folder for 'collisionagent' with local AGENTS.md
	localDir := filepath.Join(wsDir, "collisionagent")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("mkdir collisionagent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "AGENTS.md"), []byte("local directory"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	// Seed local session.jsonl with a local turn
	localSession := `{"seq":1,"role":"assistant","parts":[{"text":"local turn"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(localDir, "session.jsonl"), []byte(localSession), 0644); err != nil {
		t.Fatalf("write session.jsonl: %v", err)
	}

	// Configure REMOTE_MANIFEST for collisionagent -> wackyshimbin
	manifest := fmt.Sprintf("collisionagent: %s --behavior=echo\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifest), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ReadSession must hit the bridge (wackyshimbin), NEVER the local session.jsonl
	readResp, err := client.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: "collisionagent"})
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(readResp.GetTurns()) == 0 {
		t.Fatal("expected turns from shim")
	}
	if readResp.GetTurns()[0].GetParts()[0].GetText() == "local turn" {
		t.Fatal("COLLISION VIOLATION: routing server returned local session turn instead of bridge response!")
	}
	if readResp.GetTurns()[0].GetParts()[0].GetText() != "echo from shim: collisionagent" {
		t.Fatalf("expected shim echo, got %q", readResp.GetTurns()[0].GetParts()[0].GetText())
	}
}

func TestRoutingServer_MissingBridgeBinary_NoFallback(t *testing.T) {
	wsDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	// Create local folder for 'ghostagent'
	localDir := filepath.Join(wsDir, "ghostagent")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("mkdir ghostagent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "AGENTS.md"), []byte("local ghost"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	// Route to non-existent binary
	manifest := "ghostagent: /nonexistent/path/to/bridge-bin\n"
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifest), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Must fail with bridge command not found error, and NEVER fall back to local directory
	_, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "ghostagent"})
	if err == nil {
		t.Fatal("expected error for missing bridge binary, got nil")
	}
	if !strings.Contains(err.Error(), ErrBridgeCommandNotFound.Error()) && !errors.Is(err, ErrBridgeCommandNotFound) {
		t.Fatalf("expected bridge command not found error, got %v", err)
	}
}

func TestRoutingServer_NativeAgentRouting(t *testing.T) {
	wsDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	nativeDir := filepath.Join(wsDir, "agy")
	if err := os.MkdirAll(nativeDir, 0755); err != nil {
		t.Fatalf("mkdir native: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "AGENTS.md"), []byte("hello native agent"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. InspectAgent via reflection calls targetSDK.InspectAgent
	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "agy"})
	if err != nil {
		t.Fatalf("InspectAgent on native agent: %v", err)
	}
	if !insp.GetAgentDirExists() {
		t.Fatal("expected native agent dir to exist")
	}

	// 2. RenderSystemPrompt via reflection calls targetSDK.RenderSystemPrompt
	promptResp, err := client.RenderSystemPrompt(ctx, &agentv1.RenderSystemPromptRequest{AgentId: "agy"})
	if err != nil {
		t.Fatalf("RenderSystemPrompt on native agent: %v", err)
	}
	if promptResp.GetRenderedPrompt() != "hello native agent" {
		t.Errorf("expected 'hello native agent', got %q", promptResp.GetRenderedPrompt())
	}

	// 3. InspectAgentLocks via reflection stays local
	locksResp, err := client.InspectAgentLocks(ctx, &agentv1.InspectAgentLocksRequest{})
	if err != nil {
		t.Fatalf("InspectAgentLocks: %v", err)
	}
	if len(locksResp.GetObservations()) != 1 || locksResp.GetObservations()[0].GetAgentId() != "agy" {
		t.Errorf("expected 1 observation for agy, got %+v", locksResp.GetObservations())
	}
}
