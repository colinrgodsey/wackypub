package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	"google.golang.org/genai"
)

func TestBundledSkillOutput(t *testing.T) {
	BundledA2ASkill = "---\nname: wackypub-a2a\ndescription: test a2a skill\n---\n# Test A2A Skill\n"
	BundledWSSkill = "---\nname: wackypub-ws\ndescription: test ws skill\n---\n# Test WS Skill\n"

	t.Run("skill a2a subcommand", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		RootCmd.SetArgs([]string{"skill", "a2a"})
		err := RootCmd.Execute()

		w.Close()
		os.Stdout = oldStdout

		if err != nil {
			t.Fatalf("unexpected error executing skill a2a: %v", err)
		}

		var buf bytes.Buffer
		io.Copy(&buf, r)
		out := buf.String()

		if !strings.Contains(out, "name: wackypub-a2a") || !strings.Contains(out, "# Test A2A Skill") {
			t.Errorf("unexpected skill output: %q", out)
		}
	})

	t.Run("skill with no argument lists instead of defaulting", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		RootCmd.SetArgs([]string{"skill"})
		err := RootCmd.Execute()

		w.Close()
		os.Stdout = oldStdout

		if err != nil {
			t.Fatalf("unexpected error executing bare skill command: %v", err)
		}

		var buf bytes.Buffer
		io.Copy(&buf, r)
		out := buf.String()

		if !strings.Contains(out, "a2a") || !strings.Contains(out, "test a2a skill") {
			t.Errorf("expected listing to include a2a's name and description, got: %q", out)
		}
		if !strings.Contains(out, "ws") || !strings.Contains(out, "test ws skill") {
			t.Errorf("expected listing to include ws's name and description, got: %q", out)
		}
		if strings.Contains(out, "# Test A2A Skill") || strings.Contains(out, "# Test WS Skill") {
			t.Errorf("expected a listing, not full skill body content, got: %q", out)
		}
	})

	t.Run("skill ws subcommand", func(t *testing.T) {
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		RootCmd.SetArgs([]string{"skill", "ws"})
		err := RootCmd.Execute()

		w.Close()
		os.Stdout = oldStdout

		if err != nil {
			t.Fatalf("unexpected error executing skill ws: %v", err)
		}

		var buf bytes.Buffer
		io.Copy(&buf, r)
		out := buf.String()

		if !strings.Contains(out, "name: wackypub-ws") || !strings.Contains(out, "# Test WS Skill") {
			t.Errorf("unexpected skill output: %q", out)
		}
	})
}

func resetCompactFlags(t *testing.T) {
	t.Helper()
	clearCompactFlags()
	t.Cleanup(clearCompactFlags)
}

func clearCompactFlags() {
	if f := agentCompactCmd.Flags().Lookup("md-file"); f != nil {
		f.Changed = false
		_ = f.Value.Set("")
	}
	if f := agentCompactCmd.Flags().Lookup("runtime"); f != nil {
		f.Changed = false
		_ = f.Value.Set("")
	}
	compactMDFile = ""
	compactRuntimeFile = ""
	RootCmd.SetOut(os.Stdout)
	RootCmd.SetErr(os.Stderr)
}

func TestAgentCompactCmd_BadPath(t *testing.T) {
	resetCompactFlags(t)

	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "compact", "bob", "--md-file", "/nonexistent/COMPACT.md"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent --md-file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read compact md file:") {
		t.Errorf("expected 'failed to read compact md file:', got: %v", err)
	}
}

func TestAgentCompactCmd_InvalidYAML(t *testing.T) {
	resetCompactFlags(t)

	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	badFile := filepath.Join(wsDir, "BAD_COMPACT.md")
	if err := os.WriteFile(badFile, []byte("---\n[invalid yaml: {foo\n---\nPrompt"), 0644); err != nil {
		t.Fatalf("failed to write bad file: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "compact", "bob", "--md-file", badFile})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid YAML in --md-file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse compact md file:") {
		t.Errorf("expected 'failed to parse compact md file:', got: %v", err)
	}
}

func TestAgentCompactCmd_ValidMDFileAndDefault(t *testing.T) {
	resetCompactFlags(t)

	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	agentID := "testbot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}

	turns := []*genai.Content{
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1", "model"),
	}
	if err := adkAgent.WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("write turns: %v", err)
	}

	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 {
			capturedPrompt = req.Messages[len(req.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* summary"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	rt := &adkAgent.RuntimeConfig{
		Model:    "test-model",
		Endpoint: srv.URL,
	}
	rtData, _ := json.Marshal(rt)
	_ = os.WriteFile(filepath.Join(agentDir, "runtime.json"), rtData, 0644)
	_ = os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("# testbot\n"), 0644)
	_ = os.WriteFile(filepath.Join(agentDir, "COMPACT.md"), []byte("Disk default prompt"), 0644)

	// 1. With --md-file
	altFile := filepath.Join(wsDir, "CUSTOM_RECIPE.md")
	altContent := "---\ncompact-pct: 50\nappend-only: false\n---\nAlternate CLI recipe prompt"
	if err := os.WriteFile(altFile, []byte(altContent), 0644); err != nil {
		t.Fatalf("write alt file: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "compact", agentID, "--md-file", altFile})
	if err := RootCmd.Execute(); err != nil {
		t.Fatalf("RootCmd.Execute with --md-file failed: %v", err)
	}
	if capturedPrompt != "Alternate CLI recipe prompt" {
		t.Errorf("expected prompt %q, got %q", "Alternate CLI recipe prompt", capturedPrompt)
	}

	// 2. Without --md-file (default behavior)
	resetCompactFlags(t)
	// Re-seed turns
	if err := adkAgent.WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("write turns: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "compact", agentID})
	if err := RootCmd.Execute(); err != nil {
		t.Fatalf("RootCmd.Execute without --md-file failed: %v", err)
	}
	if capturedPrompt != "Disk default prompt" {
		t.Errorf("expected prompt %q, got %q", "Disk default prompt", capturedPrompt)
	}
}

func TestAgentCompactCmd_RuntimeMissing(t *testing.T) {
	resetCompactFlags(t)

	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "compact", "bob", "--runtime", "/nonexistent/runtime.json"})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent --runtime, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read runtime config file:") {
		t.Errorf("expected 'failed to read runtime config file:', got: %v", err)
	}
}

func TestAgentCompactCmd_RuntimeInvalidJSON(t *testing.T) {
	resetCompactFlags(t)

	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	badFile := filepath.Join(wsDir, "bad-runtime.json")
	if err := os.WriteFile(badFile, []byte("{invalid json}"), 0644); err != nil {
		t.Fatalf("failed to write bad file: %v", err)
	}

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "compact", "bob", "--runtime", badFile})
	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid JSON in --runtime, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse runtime config file:") {
		t.Errorf("expected 'failed to parse runtime config file:', got: %v", err)
	}
}

func TestAgentCompactCmd_RuntimeAndMDFileCombined(t *testing.T) {
	resetCompactFlags(t)

	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	agentID := "combo-agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}

	turns := []*genai.Content{
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1", "model"),
	}
	if err := adkAgent.WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("write turns: %v", err)
	}

	var capturedModel string
	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		capturedModel = req.Model
		if len(req.Messages) > 0 {
			capturedPrompt = req.Messages[len(req.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* combined summary"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	// Agent's live files (should be ignored for model & recipe)
	liveRt := &adkAgent.RuntimeConfig{
		Model:    "live-model",
		Endpoint: "http://unused.live",
	}
	liveRtData, _ := json.Marshal(liveRt)
	_ = os.WriteFile(filepath.Join(agentDir, "runtime.json"), liveRtData, 0644)
	_ = os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("# combo-agent\n"), 0644)
	_ = os.WriteFile(filepath.Join(agentDir, "COMPACT.md"), []byte("Disk COMPACT prompt"), 0644)

	// Custom runtime override
	customRtFile := filepath.Join(wsDir, "override-runtime.json")
	customRt := &adkAgent.RuntimeConfig{
		Model:    "combo-override-model",
		Endpoint: srv.URL,
	}
	customRtData, _ := json.Marshal(customRt)
	_ = os.WriteFile(customRtFile, customRtData, 0644)

	// Custom compact md override
	customMDFile := filepath.Join(wsDir, "override-recipe.md")
	customMDContent := "---\ncompact-pct: 50\nappend-only: false\n---\nCombo override prompt directive"
	_ = os.WriteFile(customMDFile, []byte(customMDContent), 0644)

	RootCmd.SetArgs([]string{
		"--ws", wsDir,
		"agent", "compact", agentID,
		"--runtime", customRtFile,
		"--md-file", customMDFile,
	})
	if err := RootCmd.Execute(); err != nil {
		t.Fatalf("RootCmd.Execute with --runtime and --md-file failed: %v", err)
	}

	if capturedModel != "combo-override-model" {
		t.Errorf("expected model %q, got %q", "combo-override-model", capturedModel)
	}
	if capturedPrompt != "Combo override prompt directive" {
		t.Errorf("expected prompt %q, got %q", "Combo override prompt directive", capturedPrompt)
	}
}

func TestSignalCtx(t *testing.T) {
	ctx, stop := signalCtx()
	defer stop()

	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	if ctx.Err() != nil {
		t.Fatalf("expected active context, got: %v", ctx.Err())
	}
	stop()
	if ctx.Err() == nil {
		t.Fatal("expected cancelled context after calling stop()")
	}
}

func TestD90_CLI_ScratchpadCreate_WarningOnStderr(t *testing.T) {
	wsDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644)

	agentID := "testagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	t.Run("non-escaped missing id emits warning on stderr and creates entry", func(t *testing.T) {
		errBuf := new(bytes.Buffer)
		RootCmd.SetErr(errBuf)
		defer RootCmd.SetErr(os.Stderr)

		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", agentID, "scratchpad", "create", `<SCRATCHPAD_DATA id="no99" />`})
		if err := RootCmd.Execute(); err != nil {
			t.Fatalf("scratchpad create failed: %v", err)
		}

		if !strings.Contains(errBuf.String(), `warning: scratchpad entry "no99" not found; macro passed through literally`) {
			t.Errorf("expected warning on stderr, got: %q", errBuf.String())
		}

		// Verify entry was stored literally
		items, count, _, err := adkAgent.ListScratchpads(agentDir)
		if err != nil || count != 1 {
			t.Fatalf("expected 1 scratchpad entry, got count=%d, err=%v", count, err)
		}
		content, err := adkAgent.GetScratchpad(agentDir, items[0].ID, nil, nil)
		if err != nil {
			t.Fatalf("GetScratchpad failed: %v", err)
		}
		if content != `<SCRATCHPAD_DATA id="no99" />` {
			t.Errorf("expected content to be literal macro, got: %q", content)
		}
	})

	t.Run("backslash-escaped macro emits no warning on stderr", func(t *testing.T) {
		errBuf := new(bytes.Buffer)
		RootCmd.SetErr(errBuf)
		defer RootCmd.SetErr(os.Stderr)

		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", agentID, "scratchpad", "create", `\<SCRATCHPAD_DATA id="no99" />`})
		if err := RootCmd.Execute(); err != nil {
			t.Fatalf("scratchpad create failed: %v", err)
		}

		if errBuf.Len() > 0 {
			t.Errorf("expected no warning on stderr for escaped macro, got: %q", errBuf.String())
		}
	})
}
