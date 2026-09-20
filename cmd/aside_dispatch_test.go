package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestRemoteDispatch_AsideQuestion proves the aside subcommand routes through the protocol
// dispatch layer: for a REMOTE_MANIFEST-routed agent it hits the bridge (wackyshimbin)
// instead of the local SDK, exactly like read-session/generate. This is the surface Colin
// asked for - aside available across the protocol, not just the SDK.
func TestRemoteDispatch_AsideQuestion(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShim(t)

	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.AllowedAgentsFile), []byte("bridgedagent\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}

	manifestContent := fmt.Sprintf("bridgedagent: %s --behavior=echo\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RemoteManifestFile), []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "aside", "bridgedagent", "what is the state?"})
	out, err := captureStdout(t, func() error {
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	if !strings.Contains(out, "aside from shim: what is the state?") {
		t.Errorf("expected aside to route through the bridge, got:\n%s", out)
	}
}
