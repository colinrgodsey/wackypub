package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// Every other RPC refuses to route on an unreadable or malformed REMOTE_MANIFEST. ListAgents
// used to merge only what it could read, so a corrupt manifest quietly reported a workspace
// with no bridged agents instead of reporting the corruption.
func TestRoutingServerListAgentsFailsClosedOnCorruptManifest(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	nativeDir := filepath.Join(wsDir, "nativeagent")
	if err := os.MkdirAll(nativeDir, 0755); err != nil {
		t.Fatalf("mkdir native: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "AGENTS.md"), []byte("native prompt"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte("route-missing-a-colon\n"), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	resp, err := client.ListAgents(context.Background(), &agentv1.ListAgentsRequest{})
	if err == nil {
		t.Fatalf("expected ListAgents to fail closed on a corrupt %s, got agents %v",
			RemoteManifestFile, resp.GetAgentIds())
	}
	if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(RemoteManifestFile)) {
		t.Errorf("expected the error to name %s, got %v", RemoteManifestFile, err)
	}
}
