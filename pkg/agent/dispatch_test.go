package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

var (
	shimBinOnce sync.Once
	shimBinPath string
	shimBinErr  error
)

func getShimBin(t *testing.T) string {
	t.Helper()
	shimBinOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "wackyshimbin-*")
		if err != nil {
			shimBinErr = err
			return
		}
		bin := filepath.Join(tmpDir, "wackyshimbin")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/colinrgodsey/wackypub/cmd/internal/wackyshimbin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			shimBinErr = fmt.Errorf("build wackyshimbin: %w\n%s", err, out)
			return
		}
		shimBinPath = bin
	})
	if shimBinErr != nil {
		t.Fatalf("failed to build wackyshimbin fixture: %v", shimBinErr)
	}
	return shimBinPath
}

func TestResolveAgentClient_NoManifest_ReturnsLocal(t *testing.T) {
	wsDir := t.TempDir()
	sdk := NewSDK(wsDir)

	client, cleanup, err := ResolveAgentClient(context.Background(), sdk, "nativeagent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup returned error: %v", err)
		}
	}()

	if _, ok := client.(*localAgentClient); !ok {
		t.Errorf("expected *localAgentClient, got %T", client)
	}
}

func TestResolveAgentClient_ManifestNoMatch_ReturnsLocal(t *testing.T) {
	wsDir := t.TempDir()
	manifestContent := "otheragent: wackybridge --port 8080\n"
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sdk := NewSDK(wsDir)
	client, cleanup, err := ResolveAgentClient(context.Background(), sdk, "nativeagent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup returned error: %v", err)
		}
	}()

	if _, ok := client.(*localAgentClient); !ok {
		t.Errorf("expected *localAgentClient, got %T", client)
	}
}

func TestResolveAgentClient_ManifestMatch_DialsBridge(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)

	manifestContent := fmt.Sprintf("bridgedagent: %s --behavior=echo\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sdk := NewSDK(wsDir)
	client, cleanup, err := ResolveAgentClient(context.Background(), sdk, "bridgedagent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if _, ok := client.(*localAgentClient); ok {
		t.Errorf("expected bridged client, got *localAgentClient")
	}

	resp, err := client.ReadSession(context.Background(), &agentv1.ReadSessionRequest{AgentId: "bridgedagent"})
	if err != nil {
		t.Fatalf("ReadSession via bridge: %v", err)
	}
	if len(resp.GetTurns()) == 0 {
		t.Fatalf("expected turns from shim, got 0")
	}
	partText := resp.GetTurns()[0].GetParts()[0].GetText()
	if partText != "echo from shim: bridgedagent" {
		t.Errorf("expected 'echo from shim: bridgedagent', got %q", partText)
	}
}

func TestDialBridge_CommandNotFound_ReturnsErrBridgeCommandNotFound(t *testing.T) {
	wsDir := t.TempDir()
	route := RemoteRoute{
		AgentID: "ghostagent",
		Command: "nonexistent-bridge-command-12345",
	}

	_, _, err := dialBridge(context.Background(), wsDir, "ghostagent", route)
	if err == nil {
		t.Fatalf("expected error for nonexistent command, got nil")
	}
	if !errors.Is(err, ErrBridgeCommandNotFound) {
		t.Errorf("expected ErrBridgeCommandNotFound, got: %v", err)
	}
}

func TestDialBridge_UnaryRoundTrip(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)
	route := RemoteRoute{
		AgentID: "unaryagent",
		Command: shim,
		Args:    []string{"--behavior=echo"},
	}

	client, cleanup, err := dialBridge(context.Background(), wsDir, "unaryagent", route)
	if err != nil {
		t.Fatalf("dialBridge: %v", err)
	}
	defer cleanup()

	resp, err := client.ReadSession(context.Background(), &agentv1.ReadSessionRequest{AgentId: "unaryagent"})
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(resp.GetTurns()) == 0 {
		t.Fatalf("expected turns from shim, got 0")
	}
	partText := resp.GetTurns()[0].GetParts()[0].GetText()
	if partText != "echo from shim: unaryagent" {
		t.Errorf("expected 'echo from shim: unaryagent', got %q", partText)
	}

	genResp, err := client.GenerateTurn(context.Background(), &agentv1.GenerateTurnRequest{AgentId: "unaryagent"})
	if err != nil {
		t.Fatalf("GenerateTurn: %v", err)
	}
	if genResp.GetText() != "canned generate turn for unaryagent" {
		t.Errorf("expected 'canned generate turn for unaryagent', got %q", genResp.GetText())
	}
	if genResp.GetUsage() == nil || genResp.GetUsage().GetTotalTokens() != 30 {
		t.Errorf("expected usage total tokens 30, got %+v", genResp.GetUsage())
	}
}

func TestDialBridge_StreamingRoundTrip_CleanEOF(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)
	route := RemoteRoute{
		AgentID: "streamagent",
		Command: shim,
		Args:    []string{"--behavior=stream-n=5"},
	}

	client, cleanup, err := dialBridge(context.Background(), wsDir, "streamagent", route)
	if err != nil {
		t.Fatalf("dialBridge: %v", err)
	}
	defer cleanup()

	stream, err := client.GenerateTurnStream(context.Background(), &agentv1.GenerateTurnStreamRequest{AgentId: "streamagent"})
	if err != nil {
		t.Fatalf("GenerateTurnStream: %v", err)
	}

	var chunks []string
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream.Recv error: %v", err)
		}
		chunks = append(chunks, chunk.GetText())
	}

	if len(chunks) != 5 {
		t.Fatalf("expected 5 chunks, got %d: %v", len(chunks), chunks)
	}
	for i := 1; i <= 5; i++ {
		expected := fmt.Sprintf("chunk %d", i)
		if chunks[i-1] != expected {
			t.Errorf("chunk %d mismatch: got %q, want %q", i, chunks[i-1], expected)
		}
	}
}

func TestDialBridge_StreamingRoundTrip_MidStreamCrash(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)
	route := RemoteRoute{
		AgentID: "crashagent",
		Command: shim,
		Args:    []string{"--behavior=crash-mid-stream"},
	}

	client, cleanup, err := dialBridge(context.Background(), wsDir, "crashagent", route)
	if err != nil {
		t.Fatalf("dialBridge: %v", err)
	}
	defer cleanup()

	stream, err := client.GenerateTurnStream(context.Background(), &agentv1.GenerateTurnStreamRequest{AgentId: "crashagent"})
	if err != nil {
		t.Fatalf("GenerateTurnStream: %v", err)
	}

	var chunks []string
	var finalErr error
	for {
		chunk, err := stream.Recv()
		if err != nil {
			finalErr = err
			break
		}
		chunks = append(chunks, chunk.GetText())
	}

	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks before crash, got %d: %v", len(chunks), chunks)
	}
	if finalErr == nil || finalErr == io.EOF {
		t.Fatalf("expected non-EOF error after crash, got: %v", finalErr)
	}
	if !errors.Is(finalErr, ErrBridgeProcessDied) {
		t.Errorf("expected ErrBridgeProcessDied, got: %v", finalErr)
	}
}

func TestDialBridge_ContextCancel_EscalatesToSIGKILL(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)
	route := RemoteRoute{
		AgentID: "slowagent",
		Command: shim,
		Args:    []string{"--behavior=slow-exit=30"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	client, cleanup, err := dialBridge(ctx, wsDir, "slowagent", route)
	if err != nil {
		t.Fatalf("dialBridge: %v", err)
	}

	// Trigger cancellation
	cancel()

	start := time.Now()
	// cleanup waits for cmd.Wait, which is bounded by cmd.WaitDelay (5s) after SIGTERM ignores
	_ = cleanup()
	elapsed := time.Since(start)

	// Since slow-exit sleeps for 30s upon SIGTERM, escalation to SIGKILL must fire at ~5s,
	// well before 30s.
	if elapsed > 15*time.Second {
		t.Errorf("cleanup took too long (%v), expected escalation to SIGKILL within ~5-7 seconds", elapsed)
	}

	// Verify client calls fail when context cancelled
	_, err = client.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: "slowagent"})
	if err == nil {
		t.Errorf("expected call to fail on cancelled context")
	}
}

func TestDialBridge_NeverStarts_ReturnsError(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShimBin(t)
	route := RemoteRoute{
		AgentID: "neveragent",
		Command: shim,
		Args:    []string{"--behavior=never-starts"},
	}

	client, cleanup, err := dialBridge(context.Background(), wsDir, "neveragent", route)
	if err != nil {
		// Immediate failure is fine
		return
	}
	defer cleanup()

	// If NewClient succeeded (lazy connect), first RPC call must fail with ErrBridgeProcessDied
	_, err = client.ReadSession(context.Background(), &agentv1.ReadSessionRequest{AgentId: "neveragent"})
	if err == nil {
		t.Fatalf("expected error from dead process, got nil")
	}
	if !errors.Is(err, ErrBridgeProcessDied) {
		t.Errorf("expected ErrBridgeProcessDied, got: %v", err)
	}
}
