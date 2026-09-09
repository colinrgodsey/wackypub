package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestCmdAgentPrompt_DetachFlagRegistered verifies that --detach and
// --detach-timeout flags are registered on both the subcommand and the
// positional dispatcher.
func TestCmdAgentPrompt_DetachFlagRegistered(t *testing.T) {
	if agentPromptCmd.Flags().Lookup("detach") == nil {
		t.Fatalf("expected --detach flag to be registered on agentPromptCmd")
	}
	if agentPromptCmd.Flags().Lookup("detach-timeout") == nil {
		t.Fatalf("expected --detach-timeout flag to be registered on agentPromptCmd")
	}
	if agentCmd.Flags().Lookup("detach") == nil {
		t.Fatalf("expected --detach flag to be registered on agentCmd dispatcher")
	}
	if agentCmd.Flags().Lookup("detach-timeout") == nil {
		t.Fatalf("expected --detach-timeout flag to be registered on agentCmd dispatcher")
	}
}

// TestCmdAgentPrompt_DetachParentValidatesBeforeSpawn verifies D103 Item 1:
// the parent validates the target before spawning the child and fails
// synchronously for unauthorized targets.
func TestCmdAgentPrompt_DetachParentValidatesBeforeSpawn(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	callerDir := filepath.Join(wsDir, "caller")
	if err := os.MkdirAll(callerDir, 0755); err != nil {
		t.Fatalf("failed to create caller dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(callerDir, adkAgent.AllowedAgentsFile), []byte("allowed_target\n"), 0644); err != nil {
		t.Fatalf("failed to write allowlist: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	defer os.Chdir(origCwd)

	if err := os.Chdir(callerDir); err != nil {
		t.Fatalf("failed to chdir to callerDir: %v", err)
	}

	RootCmd.SetArgs([]string{"agent", "prompt", "--detach", "forbidden_target", "Hello"})
	err = RootCmd.Execute()
	if err == nil {
		t.Fatalf("expected synchronous failure for unauthorized target under --detach, got nil")
	}
	if !strings.Contains(err.Error(), "not in WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestCmdAgentPrompt_DetachFailFastBusy verifies D103 Item 4: when the target
// session is locked, detached dispatch fails fast with a clear error message.
func TestCmdAgentPrompt_DetachFailFastBusy(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	targetID := "busy_bot"
	targetDir := filepath.Join(wsDir, targetID)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, adkAgent.AllowedAgentsFile), []byte("busy_bot\n"), 0644); err != nil {
		t.Fatalf("failed to write allowlist: %v", err)
	}

	lock, err := adkAgent.AcquireSessionLock(targetDir)
	if err != nil {
		t.Fatalf("failed to acquire session lock: %v", err)
	}
	defer lock.Release()

	origCwd, _ := os.Getwd()
	defer os.Chdir(origCwd)
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("failed to chdir to wsDir: %v", err)
	}

	RootCmd.SetArgs([]string{"agent", "prompt", "--detach", targetID, "Hello"})
	err = RootCmd.Execute()
	if err == nil {
		t.Fatalf("expected fail-fast error when target session is busy, got nil")
	}
	if !strings.Contains(err.Error(), "is busy") {
		t.Errorf("expected 'is busy' error, got: %v", err)
	}
}

// TestRemoveFlagHelper verifies removeFlag removes both bare and valued flags.
func TestRemoveFlagHelper(t *testing.T) {
	args := []string{"agent", "prompt", "--detach", "--model", "gpt-4", "--detach=true", "bob", "hi"}
	cleaned := removeFlag(args, "--detach")
	expected := []string{"agent", "prompt", "--model", "gpt-4", "bob", "hi"}

	if len(cleaned) != len(expected) {
		t.Fatalf("expected length %d, got %d: %v", len(expected), len(cleaned), cleaned)
	}
	for i, v := range expected {
		if cleaned[i] != v {
			t.Errorf("at index %d: expected %s, got %s", i, v, cleaned[i])
		}
	}
}
