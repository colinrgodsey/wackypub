package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkspaceDir_Explicit(t *testing.T) {
	tmpDir := t.TempDir()

	// Explicit path without RootMarkerFile should fail
	_, err := ResolveWorkspaceDir(tmpDir, true)
	if err == nil {
		t.Fatalf("expected error when RootMarkerFile is missing from explicit path")
	}

	// Create RootMarkerFile
	markerPath := filepath.Join(tmpDir, RootMarkerFile)
	if err := os.WriteFile(markerPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create RootMarkerFile: %v", err)
	}

	// Explicit path with RootMarkerFile should succeed
	resolved, err := ResolveWorkspaceDir(tmpDir, true)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resolved != filepath.Clean(tmpDir) {
		t.Fatalf("expected %s, got %s", filepath.Clean(tmpDir), resolved)
	}
}

func TestResolveWorkspaceDir_WalkUp(t *testing.T) {
	wsDir := t.TempDir()
	markerPath := filepath.Join(wsDir, RootMarkerFile)
	if err := os.WriteFile(markerPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create RootMarkerFile: %v", err)
	}

	subDir := filepath.Join(wsDir, "bob", "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get wd: %v", err)
	}
	defer os.Chdir(origWd)

	if err := os.Chdir(subDir); err != nil {
		t.Fatalf("failed to chdir to subDir: %v", err)
	}

	// Unspecified --ws should walk up from subDir and find wsDir
	resolved, err := ResolveWorkspaceDir(".", false)
	if err != nil {
		t.Fatalf("expected walk-up to find workspace, got: %v", err)
	}
	if resolved != wsDir {
		t.Fatalf("expected workspace %s, got %s", wsDir, resolved)
	}
}

func TestValidateAgentTarget_CallChain(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "jax")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("bob\nalice\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, "")

	origChain := os.Getenv(CallChainEnvVar)
	defer os.Setenv(CallChainEnvVar, origChain)

	os.Setenv(CallChainEnvVar, "bob,jax")

	// Target 'bob' should fail because it's already in CallChainEnvVar
	_, err = ValidateAgentTarget("bob")
	if err == nil {
		t.Fatalf("expected deadlock error for agent already in call chain")
	}
	if !strings.Contains(err.Error(), "deadlock cycle") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Target 'alice' should succeed and return updated A2AMetadata with 'alice' appended
	newMeta, err := ValidateAgentTarget("alice")
	if err != nil {
		t.Fatalf("expected success targeting alice, got: %v", err)
	}
	if strings.Join(newMeta.CallChain, ",") != "bob,jax,alice" {
		t.Fatalf("expected chain 'bob,jax,alice', got %v", newMeta.CallChain)
	}
}

func TestValidateAgentTarget_Allowlist(t *testing.T) {
	tmpDir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, "")

	origChain := os.Getenv(CallChainEnvVar)
	defer os.Setenv(CallChainEnvVar, origChain)
	os.Setenv(CallChainEnvVar, "")

	// Create an agent directory structure: <tmpDir>/bob
	bobDir := filepath.Join(tmpDir, "bob")
	os.MkdirAll(bobDir, 0755)
	os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("You are Bob"), 0644)

	os.Chdir(bobDir)

	// Bob has no AllowedAgentsFile -> default deny all
	_, err := ValidateAgentTarget("alice")
	if err == nil || !strings.Contains(err.Error(), "no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected deny-all error, got: %v", err)
	}

	// Add AllowedAgentsFile with 'alice'
	os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("# allowed\nalice\n"), 0644)

	// Targeting 'alice' should now succeed
	meta, err := ValidateAgentTarget("alice")
	if err != nil {
		t.Fatalf("expected allowed access to alice, got: %v", err)
	}
	if meta == nil || len(meta.CallChain) != 1 || meta.CallChain[0] != "alice" {
		t.Fatalf("unexpected call chain: %v", meta)
	}

	// Targeting 'charlie' should fail
	_, err = ValidateAgentTarget("charlie")
	if err == nil || !strings.Contains(err.Error(), "not in WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected unauthorized error for charlie, got: %v", err)
	}
}

func TestDiscoverAgentTools(t *testing.T) {
	agentDir := t.TempDir()
	toolsDir := filepath.Join(agentDir, ToolsDirName)
	os.MkdirAll(toolsDir, 0755)

	tool1 := filepath.Join(toolsDir, "helper.sh")
	os.WriteFile(tool1, []byte("#!/bin/sh\necho hi"), 0755)

	tool2 := filepath.Join(toolsDir, "sub", "helper.sh")
	os.MkdirAll(filepath.Dir(tool2), 0755)
	os.WriteFile(tool2, []byte("#!/bin/sh\necho shadow"), 0755)

	discovered, shadowed, err := DiscoverAgentTools(agentDir)
	if err != nil {
		t.Fatalf("DiscoverAgentTools failed: %v", err)
	}

	// Unique tool names should be 1 ("helper.sh"), with 1 shadowing warning
	if len(discovered) != 1 || discovered[0] != "helper.sh" {
		t.Fatalf("unexpected discovered tools: %v", discovered)
	}
	if len(shadowed) != 1 || !strings.Contains(shadowed[0], "shadowed") {
		t.Fatalf("expected 1 shadowing warning, got: %v", shadowed)
	}
}
