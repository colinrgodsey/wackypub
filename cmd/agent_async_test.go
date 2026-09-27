package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func TestAgentPrompt_AsyncRefusedWithoutWackyprocSupervised(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), nil, 0644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
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

	// Ensure WACKYPROC_SUPERVISED is completely absent
	t.Setenv(adkAgent.WackyprocSupervisedEnvVar, "")
	os.Unsetenv(adkAgent.WackyprocSupervisedEnvVar)

	expectedErrMsg := "--async requires WACKYPROC_SUPERVISED in environment: the dispatch must be wackyproc-supervised (dangerous flag; output must be captured)"

	// Test form 1: wackypub agent prompt --async bob "hello"
	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "prompt", "--async", "bob", "hello"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected error running --async without WACKYPROC_SUPERVISED, got nil")
	}
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Fatalf("unexpected error message: %v\nwant: %s", err, expectedErrMsg)
	}

	// Test form 2: wackypub agent bob prompt --async "hello"
	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "bob", "prompt", "--async", "hello"})
	err = RootCmd.Execute()
	if err == nil {
		t.Fatal("expected error running agent <id> prompt --async without WACKYPROC_SUPERVISED, got nil")
	}
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Fatalf("unexpected error message: %v\nwant: %s", err, expectedErrMsg)
	}
}

func TestAgentPrompt_AsyncGatingPassesWithWackyprocSupervised(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), nil, 0644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
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

	// Set WACKYPROC_SUPERVISED in environment
	t.Setenv(adkAgent.WackyprocSupervisedEnvVar, "1")

	// When WACKYPROC_SUPERVISED is present, the gate check passes.
	// Execution proceeds to agent resolution/session handling (which may error because no backend model is configured in test, but NOT with the wackyproc supervision gate error).
	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "prompt", "--async", "bob", "hello"})
	err := RootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "the dispatch must be wackyproc-supervised") {
		t.Fatalf("gate check failed even with WACKYPROC_SUPERVISED set: %v", err)
	}
}
