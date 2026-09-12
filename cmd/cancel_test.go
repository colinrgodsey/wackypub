package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func TestAgentCancelCmd_RequiresAgentID(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), nil, 0644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "cancel"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("cancel without an agent_id returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "agent_id is required") {
		t.Fatalf("error = %v, want it to say agent_id is required", err)
	}
}

func TestAgentCancelCmd_NothingInFlight(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), nil, 0644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}
	agentDir := filepath.Join(wsDir, "idle")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("idle"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, adkAgent.AllowedAgentsFile), []byte("idle\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	t.Chdir(agentDir)

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "idle", "cancel"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("cancel with no turn running returned nil, want an error")
	}
	if !strings.Contains(err.Error(), `no in-flight turn for agent "idle"`) {
		t.Fatalf("error = %v, want the no-in-flight-turn error", err)
	}
}
