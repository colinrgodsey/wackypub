package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimeConfig(t *testing.T) {
	tempDir := t.TempDir()

	jsonContent := `{
		"endpoint": "http://localhost:11434/v1",
		"model": "llama3",
		"apiKey": "test-key",
		"contextWindow": 4096
	}`

	runtimeFile := filepath.Join(tempDir, "runtime.json")
	if err := os.WriteFile(runtimeFile, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	cfg, err := LoadRuntimeConfig(tempDir)
	if err != nil {
		t.Fatalf("failed to load runtime config: %v", err)
	}

	if cfg.Endpoint != "http://localhost:11434/v1" {
		t.Errorf("expected endpoint http://localhost:11434/v1, got %s", cfg.Endpoint)
	}
	if cfg.Model != "llama3" {
		t.Errorf("expected model llama3, got %s", cfg.Model)
	}
	if cfg.ContextWindow != 4096 {
		t.Errorf("expected context window 4096, got %d", cfg.ContextWindow)
	}
}

func TestLoadRuntimeConfigSymlink(t *testing.T) {
	tempDir := t.TempDir()

	realJson := filepath.Join(tempDir, "global_runtime.json")
	symlinkJson := filepath.Join(tempDir, "runtime.json")

	content := `{
		"endpoint": "https://api.openai.com/v1",
		"model": "gpt-4o",
		"apiKey": "sk-test",
		"contextWindow": 8192
	}`

	if err := os.WriteFile(realJson, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write global_runtime.json: %v", err)
	}

	if err := os.Symlink(realJson, symlinkJson); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	cfg, err := LoadRuntimeConfig(tempDir)
	if err != nil {
		t.Fatalf("failed to load runtime config via symlink: %v", err)
	}

	if cfg.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o via symlink, got %s", cfg.Model)
	}
}

func TestLoadRuntimeConfig_DefaultFallback_WithAPIKey(t *testing.T) {
	tempDir := t.TempDir()

	origKey := os.Getenv("OPENROUTER_API_KEY")
	defer os.Setenv("OPENROUTER_API_KEY", origKey)
	os.Setenv("OPENROUTER_API_KEY", "sk-or-v1-testkey123")

	cfg, err := LoadRuntimeConfig(tempDir)
	if err != nil {
		t.Fatalf("expected LoadRuntimeConfig fallback to succeed, got: %v", err)
	}

	if cfg.Endpoint != "https://openrouter.ai/api/v1" {
		t.Errorf("expected endpoint https://openrouter.ai/api/v1, got %s", cfg.Endpoint)
	}
	if cfg.Model != "auto" {
		t.Errorf("expected model auto, got %s", cfg.Model)
	}
	if cfg.APIKey != "sk-or-v1-testkey123" {
		t.Errorf("expected apiKey sk-or-v1-testkey123, got %s", cfg.APIKey)
	}
	if cfg.Provider != "openai" {
		t.Errorf("expected provider openai, got %s", cfg.Provider)
	}
	if cfg.ContextWindow != 200000 {
		t.Errorf("expected contextWindow 200000, got %d", cfg.ContextWindow)
	}
	if cfg.ReasoningField != "reasoning" {
		t.Errorf("expected reasoningField reasoning, got %s", cfg.ReasoningField)
	}
	if cfg.SupportsReasoningDetails {
		t.Errorf("expected supportsReasoningDetails false, got true")
	}
}

func TestLoadRuntimeConfig_DefaultFallback_MissingAPIKey(t *testing.T) {
	tempDir := t.TempDir()

	origKey := os.Getenv("OPENROUTER_API_KEY")
	defer os.Setenv("OPENROUTER_API_KEY", origKey)
	os.Unsetenv("OPENROUTER_API_KEY")

	_, err := LoadRuntimeConfig(tempDir)
	if err == nil {
		t.Fatalf("expected error when OPENROUTER_API_KEY is missing on default fallback, got nil")
	}

	if !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("expected error to mention OPENROUTER_API_KEY, got: %v", err)
	}
	if !strings.Contains(err.Error(), "examples/runtimes/README.md") {
		t.Errorf("expected error to mention examples/runtimes/README.md, got: %v", err)
	}
}

func TestLoadRuntimeConfig_BrokenJSONDoesNotFallback(t *testing.T) {
	tempDir := t.TempDir()

	origKey := os.Getenv("OPENROUTER_API_KEY")
	defer os.Setenv("OPENROUTER_API_KEY", origKey)
	os.Setenv("OPENROUTER_API_KEY", "sk-or-v1-testkey123")

	runtimeFile := filepath.Join(tempDir, "runtime.json")
	if err := os.WriteFile(runtimeFile, []byte("{invalid-json"), 0644); err != nil {
		t.Fatalf("failed writing broken runtime.json: %v", err)
	}

	_, err := LoadRuntimeConfig(tempDir)
	if err == nil {
		t.Fatalf("expected error on broken runtime.json, got nil")
	}

	if !strings.Contains(err.Error(), "failed to parse runtime.json") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestInspectAgent_RuntimeMissing_ReportsDiagnosticFact(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	insp, err := InspectAgentDir(wsDir, "bob")
	if err != nil {
		t.Fatalf("InspectAgentDir failed: %v", err)
	}

	if insp.RuntimeJSONExists {
		t.Errorf("expected RuntimeJSONExists to be false when runtime.json is absent")
	}
	if insp.RuntimeJSONValid {
		t.Errorf("expected RuntimeJSONValid to be false when runtime.json is absent")
	}
}
