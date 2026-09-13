package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func TestWorkspaceOverviewProto(t *testing.T) {
	wsDir := t.TempDir()

	// Create test agent directories
	for _, id := range []string{"agent-1", "agent-2"} {
		agentDir := filepath.Join(wsDir, id)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", agentDir, err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("prompt"), 0644); err != nil {
			t.Fatalf("write AGENTS.md: %v", err)
		}
	}

	sdk := adkAgent.NewSDK(wsDir)

	protoOut, err := captureStdout(t, func() error {
		return printWorkspaceOverview(sdk, wsDir)
	})
	if err != nil {
		t.Fatalf("printWorkspaceOverview failed: %v", err)
	}

	if protoOut == "" {
		t.Fatal("proto output is unexpectedly empty")
	}

	if !strings.Contains(protoOut, "agent-1") || !strings.Contains(protoOut, "agent-2") {
		t.Fatalf("expected output to contain agent-1 and agent-2, got:\n%s", protoOut)
	}
}

func TestAgentInspectionProto(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "testbot")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are testbot"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	sdk := adkAgent.NewSDK(wsDir)
	out, err := captureStdout(t, func() error {
		return printAgentInspection(sdk, "testbot")
	})
	if err != nil {
		t.Fatalf("printAgentInspection failed: %v", err)
	}

	if !strings.Contains(out, "Agent: testbot") || !strings.Contains(out, "AGENTS.md") {
		t.Fatalf("unexpected agent inspection output:\n%s", out)
	}
}
