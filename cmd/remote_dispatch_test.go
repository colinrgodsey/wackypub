package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

var (
	shimOnce sync.Once
	shimPath string
	shimErr  error
)

func getShim(t *testing.T) string {
	t.Helper()
	shimOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "wackyshimbin-cmd-*")
		if err != nil {
			shimErr = err
			return
		}
		bin := filepath.Join(tmpDir, "wackyshimbin")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/colinrgodsey/wackypub/cmd/internal/wackyshimbin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			shimErr = fmt.Errorf("build wackyshimbin: %w\n%s", err, out)
			return
		}
		shimPath = bin
	})
	if shimErr != nil {
		t.Fatalf("failed to build wackyshimbin fixture: %v", shimErr)
	}
	return shimPath
}

func TestRemoteDispatchIntegration_ReadSession(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShim(t)

	// Setup workspace markers
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.AllowedAgentsFile), []byte("bridgedagent\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}

	// Create REMOTE_MANIFEST routing bridgedagent through wackyshimbin
	manifestContent := fmt.Sprintf("bridgedagent: %s --behavior=echo\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RemoteManifestFile), []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "bridgedagent", "read-session"})
	out, err := captureStdout(t, func() error {
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	if !strings.Contains(out, "echo from shim: bridgedagent") {
		t.Errorf("expected output to contain 'echo from shim: bridgedagent', got:\n%s", out)
	}
}

func TestRemoteDispatchIntegration_StreamingGenerate(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShim(t)

	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.AllowedAgentsFile), []byte("streamagent\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}

	manifestContent := fmt.Sprintf("streamagent: %s --behavior=stream-n=3\n", shim)
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RemoteManifestFile), []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "streamagent", "generate"})
	out, err := captureStdout(t, func() error {
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	for _, expected := range []string{"chunk 1", "chunk 2", "chunk 3"} {
		if !strings.Contains(out, expected) {
			t.Errorf("expected output to contain %q, got:\n%s", expected, out)
		}
	}
}
