package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestSetModelConfig_NativeAgentReturnsUnimplemented pins the scope decision: native
// (non-bridged) agents take their model from runtime.json and must NOT be silently
// changed - SetModelConfig is a bridged-ACP-session passthrough only.
func TestSetModelConfig_NativeAgentReturnsUnimplemented(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), nil, 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are bob"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, adkAgent.AllowedAgentsFile), []byte("bob\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "setmodel", "bob", "sonnet"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error setting model on a native agent; got nil")
	}
	if !strings.Contains(err.Error(), "bridged") && !strings.Contains(err.Error(), "not supported") && !strings.Contains(err.Error(), "Unimplemented") {
		t.Fatalf("error should explain native agents are unsupported, got: %v", err)
	}
}

// TestSetModelConfig_RequiresModel pins argument validation before any dispatch.
func TestSetModelConfig_RequiresModel(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), nil, 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, adkAgent.AllowedAgentsFile), []byte("bob\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "setmodel", "bob"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected missing-model error; got nil")
	}
	if !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("error should require model, got: %v", err)
	}
}
