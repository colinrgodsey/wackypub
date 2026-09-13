package cmd

import (
	"os"
	"path/filepath"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func TestIsProtoMethodEnabled(t *testing.T) {
	tests := []struct {
		name       string
		envVal     string
		methodName string
		want       bool
	}{
		{
			name:       "empty env var",
			envVal:     "",
			methodName: "ListAgents",
			want:       false,
		},
		{
			name:       "unrelated method enabled",
			envVal:     "InspectAgent,ReadSession",
			methodName: "ListAgents",
			want:       false,
		},
		{
			name:       "single exact match",
			envVal:     "ListAgents",
			methodName: "ListAgents",
			want:       true,
		},
		{
			name:       "in comma-separated list",
			envVal:     "InspectAgent,ListAgents,ReadSession",
			methodName: "ListAgents",
			want:       true,
		},
		{
			name:       "with whitespace around entries",
			envVal:     "  InspectAgent ,  ListAgents  , ReadSession ",
			methodName: "ListAgents",
			want:       true,
		},
		{
			name:       "case-insensitive match",
			envVal:     "listagents",
			methodName: "ListAgents",
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WACKYPUB_PROTO_METHODS", tt.envVal)
			got := isProtoMethodEnabled(tt.methodName)
			if got != tt.want {
				t.Errorf("isProtoMethodEnabled(%q) with env %q = %v, want %v", tt.methodName, tt.envVal, got, tt.want)
			}
		})
	}
}

func TestListAgentsFlagParity(t *testing.T) {
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

	// 1. Run with proto flag disabled (legacy path)
	t.Setenv("WACKYPUB_PROTO_METHODS", "")
	legacyOut, err := captureStdout(t, func() error {
		return printWorkspaceOverview(sdk, wsDir)
	})
	if err != nil {
		t.Fatalf("legacy path failed: %v", err)
	}

	// 2. Run with proto flag enabled (proto path)
	t.Setenv("WACKYPUB_PROTO_METHODS", "ListAgents")
	protoOut, err := captureStdout(t, func() error {
		return printWorkspaceOverview(sdk, wsDir)
	})
	if err != nil {
		t.Fatalf("proto path failed: %v", err)
	}

	// 3. Both must be non-empty and byte-for-byte identical
	if legacyOut == "" {
		t.Fatal("legacy output is unexpectedly empty")
	}
	if legacyOut != protoOut {
		t.Fatalf("output mismatch between flag-off and flag-on paths:\n--- LEGACY ---\n%s\n--- PROTO ---\n%s", legacyOut, protoOut)
	}
}
