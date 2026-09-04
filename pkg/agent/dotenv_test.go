package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("non existent file", func(t *testing.T) {
		env, err := ParseDotEnv(filepath.Join(tmpDir, "does_not_exist.env"))
		if err != nil {
			t.Fatalf("unexpected error for non-existent file: %v", err)
		}
		if len(env) != 0 {
			t.Errorf("expected empty map, got %v", env)
		}
	})

	t.Run("parsing syntax and comments", func(t *testing.T) {
		dotenvContent := `
# Comment line
FOO=bar
BAR="hello world"
BAZ='single quoted'
EXPORTED=export_val
export EXPORTED2=export_val2
WITH_COMMENT=value # inline comment
WITH_TAB_COMMENT=value2	# tab comment
QUOTED_COMMENT="hello # not comment"
ESCAPED="line1\nline2"
EMPTY_KEY=
`
		envPath := filepath.Join(tmpDir, ".env")
		if err := os.WriteFile(envPath, []byte(dotenvContent), 0644); err != nil {
			t.Fatalf("failed to write .env file: %v", err)
		}

		parsed, err := ParseDotEnv(envPath)
		if err != nil {
			t.Fatalf("ParseDotEnv failed: %v", err)
		}

		expected := map[string]string{
			"FOO":              "bar",
			"BAR":              "hello world",
			"BAZ":              "single quoted",
			"EXPORTED":         "export_val",
			"EXPORTED2":        "export_val2",
			"WITH_COMMENT":     "value",
			"WITH_TAB_COMMENT": "value2",
			"QUOTED_COMMENT":   "hello # not comment",
			"ESCAPED":          "line1\nline2",
			"EMPTY_KEY":        "",
		}

		for k, exp := range expected {
			got, ok := parsed[k]
			if !ok {
				t.Errorf("missing key %q in parsed result", k)
				continue
			}
			if got != exp {
				t.Errorf("key %q: expected %q, got %q", k, exp, got)
			}
		}
	})
}

func TestExecuteTool_DotEnvInjectionAndPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "bob")
	if err := os.MkdirAll(filepath.Join(agentDir, "tools"), 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// Write .env file
	dotenvContent := `
MY_VAR=from_dotenv
OVERRIDDEN_VAR=from_dotenv
`
	if err := os.WriteFile(filepath.Join(agentDir, ".env"), []byte(dotenvContent), 0644); err != nil {
		t.Fatalf("failed to write .env: %v", err)
	}

	// Create executable script tool in tools/
	toolPath := filepath.Join(agentDir, "tools", "env_test.sh")
	script := `#!/bin/sh
echo "MY_VAR=$MY_VAR"
echo "OVERRIDDEN_VAR=$OVERRIDDEN_VAR"
`
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	// Execute tool with invocation args.Env overriding OVERRIDDEN_VAR
	args := ExecToolArgs{
		Env: map[string]string{
			"OVERRIDDEN_VAR": "from_args",
		},
	}

	output, _, err := executeTool(context.Background(), agentDir, "env_test.sh", toolPath, args, nil)
	if err != nil {
		t.Fatalf("executeTool failed: %v", err)
	}

	if !strings.Contains(output, "MY_VAR=from_dotenv") {
		t.Errorf("expected MY_VAR=from_dotenv in output, got: %s", output)
	}
	if !strings.Contains(output, "OVERRIDDEN_VAR=from_args") {
		t.Errorf("expected OVERRIDDEN_VAR=from_args in output, got: %s", output)
	}
}

func TestRootAndAgentDotEnvHierarchyAndRuntimeExpansion(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write root marker: %v", err)
	}

	// 1. Write workspace root .env
	rootDotEnv := `
SHARED_KEY=root_value
OVERRIDDEN_KEY=root_value
ROOT_ONLY_KEY=root_secret_123
`
	if err := os.WriteFile(filepath.Join(wsDir, ".env"), []byte(rootDotEnv), 0644); err != nil {
		t.Fatalf("failed to write root .env: %v", err)
	}

	// 2. Create agent directory 'clerk' with per-agent .env
	agentDir := filepath.Join(wsDir, "clerk")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	agentDotEnv := `
OVERRIDDEN_KEY=agent_override_value
AGENT_ONLY_KEY=agent_secret_456
`
	if err := os.WriteFile(filepath.Join(agentDir, ".env"), []byte(agentDotEnv), 0644); err != nil {
		t.Fatalf("failed to write agent .env: %v", err)
	}

	// 3. Write runtime.json with environment variable place-holders
	runtimeContent := `{
		"model": "openrouter/anthropic/claude-3.5-sonnet",
		"apiKey": "${ROOT_ONLY_KEY}",
		"reasoningEffort": "${OVERRIDDEN_KEY}"
	}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeContent), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// 4. Test LoadAgentDotEnv
	loadedMap, err := LoadAgentDotEnv(agentDir)
	if err != nil {
		t.Fatalf("LoadAgentDotEnv failed: %v", err)
	}

	if loadedMap["SHARED_KEY"] != "root_value" {
		t.Errorf("expected SHARED_KEY=root_value, got %q", loadedMap["SHARED_KEY"])
	}
	if loadedMap["OVERRIDDEN_KEY"] != "agent_override_value" {
		t.Errorf("expected OVERRIDDEN_KEY=agent_override_value, got %q", loadedMap["OVERRIDDEN_KEY"])
	}

	// 5. Test LoadRuntimeConfig expansion
	cfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}

	if cfg.APIKey != "root_secret_123" {
		t.Errorf("expected expanded apiKey 'root_secret_123', got %q", cfg.APIKey)
	}
	if cfg.ReasoningEffort != "agent_override_value" {
		t.Errorf("expected expanded reasoningEffort 'agent_override_value', got %q", cfg.ReasoningEffort)
	}
}
