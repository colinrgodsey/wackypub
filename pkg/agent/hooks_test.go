package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helper to create an executable script in agent's hooks/on-user-message directory.
func writeHookScript(t *testing.T, agentDir, filename, content string) string {
	t.Helper()
	dir := filepath.Join(agentDir, "hooks", EventOnUserMessage)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	hookPath := filepath.Join(dir, filename)
	if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write hook script %s: %v", filename, err)
	}
	return hookPath
}

// 1. Test chain ordering: 00 then 01; second sees first's env mutations and altered text
func TestHookChainOrderingAndEnvPassing(t *testing.T) {
	agentDir := t.TempDir()

	// 00-first sets FIRST_VAR=step1 and alters text to "from-hook-00"
	writeHookScript(t, agentDir, "00-first", `#!/bin/sh
cat > /dev/null
printf '{"env":{"FIRST_VAR":"step1"},"text":"from-hook-00"}\n'
`)

	// 01-second verifies it sees FIRST_VAR=step1 and receives "from-hook-00" on stdin
	writeHookScript(t, agentDir, "01-second", `#!/bin/sh
read stdin_text
if [ "$FIRST_VAR" = "step1" ] && [ "$stdin_text" = "from-hook-00" ]; then
	printf '{"env":{"SECOND_VAR":"step2"},"text":"from-hook-01"}\n'
else
	printf '{"env":{"ERROR":"mismatch"}}\n'
	exit 1
fi
`)

	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "initial-message")
	if err != nil {
		t.Fatalf("RunHookChain failed: %v", err)
	}

	if res.Text != "from-hook-01" {
		t.Errorf("expected text 'from-hook-01', got %q", res.Text)
	}
	if res.MutatedEnv["FIRST_VAR"] != "step1" {
		t.Errorf("expected FIRST_VAR=step1, got %q", res.MutatedEnv["FIRST_VAR"])
	}
	if res.MutatedEnv["SECOND_VAR"] != "step2" {
		t.Errorf("expected SECOND_VAR=step2, got %q", res.MutatedEnv["SECOND_VAR"])
	}
}

// 2. Test env set, replace, remove, and ignore semantics
func TestHookEnvSemantics(t *testing.T) {
	agentDir := t.TempDir()

	writeHookScript(t, agentDir, "00-init", `#!/bin/sh
printf '{"env":{"KEPT":"keep_val","REPLACED":"old_val","REMOVED":"to_remove"}}\n'
`)

	// 01 replaces REPLACED, removes REMOVED with null, adds NEW_VAR, leaves KEPT missing (ignored/unchanged)
	writeHookScript(t, agentDir, "01-mut", `#!/bin/sh
printf '{"env":{"REPLACED":"new_val","REMOVED":null,"NEW_VAR":"added"}}\n'
`)

	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "msg")
	if err != nil {
		t.Fatalf("RunHookChain failed: %v", err)
	}

	if res.MutatedEnv["KEPT"] != "keep_val" {
		t.Errorf("expected KEPT=keep_val, got %q", res.MutatedEnv["KEPT"])
	}
	if res.MutatedEnv["REPLACED"] != "new_val" {
		t.Errorf("expected REPLACED=new_val, got %q", res.MutatedEnv["REPLACED"])
	}
	if val, ok := res.MutatedEnv["REMOVED"]; ok {
		t.Errorf("expected REMOVED to be absent after null, but found value %q", val)
	}
	if res.MutatedEnv["NEW_VAR"] != "added" {
		t.Errorf("expected NEW_VAR=added, got %q", res.MutatedEnv["NEW_VAR"])
	}
}

// 3. Test text altered (non-null) vs null vs absent
func TestHookTextAlteredSemantics(t *testing.T) {
	t.Run("non-null string alters text", func(t *testing.T) {
		agentDir := t.TempDir()
		writeHookScript(t, agentDir, "00-text", `#!/bin/sh
printf '{"text":"altered text"}\n'
`)
		res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "altered text" {
			t.Errorf("expected 'altered text', got %q", res.Text)
		}
	})

	t.Run("null text leaves text unchanged", func(t *testing.T) {
		agentDir := t.TempDir()
		writeHookScript(t, agentDir, "00-null-text", `#!/bin/sh
printf '{"text":null,"env":{"FOO":"bar"}}\n'
`)
		res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "orig" {
			t.Errorf("expected 'orig', got %q", res.Text)
		}
		if res.MutatedEnv["FOO"] != "bar" {
			t.Errorf("expected FOO=bar, got %q", res.MutatedEnv["FOO"])
		}
	})

	t.Run("absent text leaves text unchanged", func(t *testing.T) {
		agentDir := t.TempDir()
		writeHookScript(t, agentDir, "00-absent-text", `#!/bin/sh
printf '{"env":{"BAR":"baz"}}\n'
`)
		res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text != "orig" {
			t.Errorf("expected 'orig', got %q", res.Text)
		}
	})
}

// 4. Test malformed JSON output -> proceed unchanged + warning
func TestHookMalformedJSON(t *testing.T) {
	agentDir := t.TempDir()
	writeHookScript(t, agentDir, "00-bad-json", `#!/bin/sh
printf 'this is not json at all\n'
`)

	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
	if err != nil {
		t.Fatalf("expected nil error on hook failure, got: %v", err)
	}
	if res.Text != "orig" {
		t.Errorf("expected text unchanged 'orig', got %q", res.Text)
	}
	if len(res.MutatedEnv) > 0 {
		t.Errorf("expected empty mutated env, got %v", res.MutatedEnv)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("expected warning for malformed JSON, got none")
	}
}

// 5. Test timeout kicks in -> proceed unchanged
func TestHookTimeout(t *testing.T) {
	agentDir := t.TempDir()
	writeHookScript(t, agentDir, "00-slow", `#!/bin/sh
sleep 2
printf '{"text":"too-late"}\n'
`)

	start := time.Now()
	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig", HookOptions{Timeout: 100 * time.Millisecond})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("hook did not time out quickly, elapsed: %v", elapsed)
	}
	if res.Text != "orig" {
		t.Errorf("expected text unchanged 'orig', got %q", res.Text)
	}
	if len(res.MutatedEnv) > 0 {
		t.Errorf("expected empty mutated env, got %v", res.MutatedEnv)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("expected warning for timeout, got none")
	}
}

// 6. Test non-zero exit -> proceed unchanged
func TestHookNonZeroExit(t *testing.T) {
	agentDir := t.TempDir()
	writeHookScript(t, agentDir, "00-fail", `#!/bin/sh
printf '{"text":"should-not-apply","env":{"BAD":"val"}}\n'
exit 1
`)

	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if res.Text != "orig" {
		t.Errorf("expected text unchanged 'orig', got %q", res.Text)
	}
	if len(res.MutatedEnv) > 0 {
		t.Errorf("expected empty mutated env, got %v", res.MutatedEnv)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("expected warning for non-zero exit, got none")
	}
}

// 7. Test WACKYPUB_HOOKS=0 skips hook execution
func TestHookOptOutViaEnv(t *testing.T) {
	agentDir := t.TempDir()
	writeHookScript(t, agentDir, "00-modify", `#!/bin/sh
printf '{"text":"altered","env":{"HOOKED":"true"}}\n'
`)

	orig := os.Getenv(EnvHooks)
	defer os.Setenv(EnvHooks, orig)
	os.Setenv(EnvHooks, "0")

	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if res.Text != "orig" {
		t.Errorf("expected text unchanged 'orig', got %q", res.Text)
	}
	if len(res.MutatedEnv) > 0 {
		t.Errorf("expected empty mutated env when opt-out active, got %v", res.MutatedEnv)
	}
}

// 8. Test altered text reaches the model (stub-model turn test asserting the actual received user text)
func TestHookAlteredTextReachesModel(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "hook_agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
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

	var capturedUserMessage string
	var capturedSystemPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqPayload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &reqPayload)
		for _, m := range reqPayload.Messages {
			if m.Role == "user" {
				capturedUserMessage = m.Content
			}
			if m.Role == "system" || m.Role == "developer" {
				capturedSystemPrompt = m.Content
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 123456789,
			"model": "test-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "Model response to altered turn"
				},
				"finish_reason": "stop"
			}]
		}`))
	}))
	defer srv.Close()

	// Write runtime.json pointing to httptest server
	runtimeJSON := fmt.Sprintf(`{
		"provider": "openai",
		"endpoint": "%s",
		"model": "test-model",
		"apiKey": "test-key"
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// Write AGENTS.md
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt."), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Write hook script that alters message text and sets env
	writeHookScript(t, agentDir, "00-rewrite", `#!/bin/sh
cat > /dev/null
printf '{"text":"What is your secret quest?","env":{"TODAY":"2026-09-03"}}\n'
`)

	sdk := NewSDK(wsDir)
	resp, err := sdk.AddAndGenerateTurn(context.Background(), agentID, "What is your name?")
	if err != nil {
		t.Fatalf("AddAndGenerateTurn failed: %v", err)
	}
	if resp.Text != "Model response to altered turn" {
		t.Errorf("unexpected response: %q", resp.Text)
	}

	if !strings.Contains(capturedUserMessage, "What is your secret quest?") {
		t.Errorf("expected model to receive 'What is your secret quest?', got %q", capturedUserMessage)
	}

	if !strings.Contains(capturedSystemPrompt, "[hook env]") || !strings.Contains(capturedSystemPrompt, "TODAY=2026-09-03") {
		t.Errorf("expected system prompt to contain [hook env] with TODAY=2026-09-03, got %q", capturedSystemPrompt)
	}

	// Verify session.jsonl stored the altered text
	turns, err := sdk.ReadSession(agentID)
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if len(turns) < 2 {
		t.Fatalf("expected at least 2 turns in session, got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Parts[0].Text != "What is your secret quest?" {
		t.Errorf("expected session.jsonl turn 0 to be altered user text, got %v", turns[0])
	}
}

// TestHookAddUserTurnFollowedByGenerateTurn tests AddUserTurn followed by GenerateTurn
func TestHookAddUserTurnFollowedByGenerateTurn(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "split_agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
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

	var capturedUserMessage string
	var capturedSystemPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqPayload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &reqPayload)
		for _, m := range reqPayload.Messages {
			if m.Role == "user" {
				capturedUserMessage = m.Content
			}
			if m.Role == "system" || m.Role == "developer" {
				capturedSystemPrompt = m.Content
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 123456789,
			"model": "test-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "Done"
				},
				"finish_reason": "stop"
			}]
		}`))
	}))
	defer srv.Close()

	runtimeJSON := fmt.Sprintf(`{
		"provider": "openai",
		"endpoint": "%s",
		"model": "test-model",
		"apiKey": "test-key"
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt."), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	writeHookScript(t, agentDir, "00-rewrite", `#!/bin/sh
cat > /dev/null
printf '{"text":"Altered Quest","env":{"SPLIT_ENV":"active"}}\n'
`)

	sdk := NewSDK(wsDir)
	if _, err := sdk.AddUserTurn(agentID, "Original Quest"); err != nil {
		t.Fatalf("AddUserTurn failed: %v", err)
	}

	resp, err := sdk.GenerateTurn(context.Background(), agentID)
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}
	if resp != "Done" {
		t.Errorf("unexpected response: %q", resp)
	}

	if !strings.Contains(capturedUserMessage, "Altered Quest") {
		t.Errorf("expected model to receive 'Altered Quest', got %q", capturedUserMessage)
	}
	if !strings.Contains(capturedSystemPrompt, "[hook env]") || !strings.Contains(capturedSystemPrompt, "SPLIT_ENV=active") {
		t.Errorf("expected system prompt to contain SPLIT_ENV=active, got %q", capturedSystemPrompt)
	}
}

// 9. Test hook-set env appears in rendered prompt's [hook env] section AND host env vars do NOT appear
func TestHookEnvInRenderedPrompt(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "env_agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// Set a host env var that must NEVER appear in the rendered prompt
	const hostSecret = "SUPER_SECRET_HOST_API_KEY_DO_NOT_LEAK"
	origSecret := os.Getenv("SECRET_KEY")
	defer os.Setenv("SECRET_KEY", origSecret)
	os.Setenv("SECRET_KEY", hostSecret)

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Base agent instruction."), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	hookMutations := map[string]string{
		"TODAY":     "2026-09-03",
		"AGENT_LOC": "station-alpha",
	}

	prompt, err := RenderAgentSystemPrompt(wsDir, agentID, hookMutations)
	if err != nil {
		t.Fatalf("RenderAgentSystemPrompt failed: %v", err)
	}

	if !strings.Contains(prompt, "[hook env]") {
		t.Fatalf("expected rendered prompt to contain '[hook env]', got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "TODAY=2026-09-03") {
		t.Errorf("expected prompt to contain TODAY=2026-09-03, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "AGENT_LOC=station-alpha") {
		t.Errorf("expected prompt to contain AGENT_LOC=station-alpha, got:\n%s", prompt)
	}

	// Host env variables must NOT appear
	if strings.Contains(prompt, hostSecret) {
		t.Fatalf("CRITICAL LEAK: host secret %q found in rendered prompt:\n%s", hostSecret, prompt)
	}
	if strings.Contains(prompt, "SECRET_KEY=") {
		t.Fatalf("CRITICAL LEAK: SECRET_KEY found in rendered prompt:\n%s", prompt)
	}
}

// 10. Test both human-style and A2A-style inbound messages fire the hook
func TestHookInboundMessagesHumanAndA2A(t *testing.T) {
	wsDir := t.TempDir()

	// Setup agent "bob"
	bobDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(bobDir, 0755); err != nil {
		t.Fatalf("failed to create bob dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("bob\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(bobDir); err != nil {
		t.Fatalf("failed to chdir to bobDir: %v", err)
	}
	defer os.Chdir(origCwd)

	writeHookScript(t, bobDir, "00-mark", `#!/bin/sh
cat > /dev/null
printf '{"text":"intercepted-by-hook","env":{"INTERCEPTED":"true"}}\n'
`)

	sdk := NewSDK(wsDir)

	// Subtest 1: Human-style inbound message via AddUserTurn
	t.Run("human-style AddUserTurn", func(t *testing.T) {
		res, err := sdk.AddUserTurn("bob", "human message")
		if err != nil {
			t.Fatalf("AddUserTurn failed: %v", err)
		}
		if res.Text != "intercepted-by-hook" {
			t.Errorf("expected result text 'intercepted-by-hook', got %q", res.Text)
		}
		turns, err := sdk.ReadSession("bob")
		if err != nil {
			t.Fatalf("ReadSession failed: %v", err)
		}
		last := turns[len(turns)-1]
		if last.Role != "user" || last.Parts[0].Text != "intercepted-by-hook" {
			t.Errorf("expected hook to intercept human message, got %v", last)
		}
	})

	// Subtest 2: A2A-style inbound message with AGENT2AGENT metadata
	t.Run("A2A-style inbound message", func(t *testing.T) {
		origA2A := os.Getenv(Agent2AgentEnvVar)
		defer os.Setenv(Agent2AgentEnvVar, origA2A)

		// Set A2A context representing caller "alice"
		a2aPayload := `{"caller_id":"alice","call_chain":["alice"],"trace_id":"trace-a2a-test"}`
		os.Setenv(Agent2AgentEnvVar, a2aPayload)

		res, err := sdk.AddUserTurn("bob", "peer agent message from alice")
		if err != nil {
			t.Fatalf("AddUserTurn for A2A failed: %v", err)
		}
		if res.Text != "intercepted-by-hook" {
			t.Errorf("expected result text 'intercepted-by-hook', got %q", res.Text)
		}
		turns, err := sdk.ReadSession("bob")
		if err != nil {
			t.Fatalf("ReadSession failed: %v", err)
		}
		last := turns[len(turns)-1]
		if last.Role != "user" || last.Parts[0].Text != "intercepted-by-hook" {
			t.Errorf("expected hook to intercept A2A message, got %v", last)
		}
	})
}

// 11. Test the committed scaffold example examples/hooks/on-user-message/00-date
func TestScaffoldExample00Date(t *testing.T) {
	agentDir := t.TempDir()
	hookDir := filepath.Join(agentDir, "hooks", EventOnUserMessage)
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}

	// Read scaffold file from repo root
	scaffoldPath := filepath.Join("..", "..", "examples", "hooks", EventOnUserMessage, "00-date")
	data, err := os.ReadFile(scaffoldPath)
	if err != nil {
		t.Fatalf("failed to read scaffold file at %s: %v", scaffoldPath, err)
	}

	targetPath := filepath.Join(hookDir, "00-date")
	if err := os.WriteFile(targetPath, data, 0755); err != nil {
		t.Fatalf("failed to copy scaffold file: %v", err)
	}

	res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "hello")
	if err != nil {
		t.Fatalf("RunHookChain failed with scaffold: %v", err)
	}

	// Scaffold prepends a datetime preamble to the message text (per Colin: no env var).
	scaffoldText := res.Text
	prefix := "[Current datetime: "
	if !strings.HasPrefix(scaffoldText, prefix) {
		t.Errorf("expected text to start with %q preamble, got %q", prefix, scaffoldText)
	}
	if !strings.HasSuffix(scaffoldText, "hello") {
		t.Errorf("expected original message text to be preserved, got %q", scaffoldText)
	}
	if res.MutatedEnv != nil {
		t.Errorf("scaffold should set no env vars, got: %v", res.MutatedEnv)
	}
}

// 12. Test failing hook warning surfaced via AddUserTurn result surface (human and A2A)
func TestHookFailureWarningSurfacedInAddUserTurn(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "warn_agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
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

	// Failing hook script
	writeHookScript(t, agentDir, "00-fail", `#!/bin/sh
echo "something went wrong on stderr" >&2
exit 1
`)

	sdk := NewSDK(wsDir)

	// Subtest 1: Human turn via AddUserTurn
	res, err := sdk.AddUserTurn(agentID, "hello")
	if err != nil {
		t.Fatalf("AddUserTurn failed: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("expected warnings on UserTurnResult, got none")
	}
	if !strings.Contains(res.Warnings[0], "exited with error") {
		t.Errorf("expected exit error in warning, got %q", res.Warnings[0])
	}
	if res.Text != "hello" {
		t.Errorf("expected text unchanged 'hello', got %q", res.Text)
	}

	// Subtest 2: A2A-style turn via AddUserTurn
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"peer","call_chain":["peer"],"trace_id":"tr-1"}`)

	resA2A, err := sdk.AddUserTurn(agentID, "peer turn")
	if err != nil {
		t.Fatalf("AddUserTurn for A2A failed: %v", err)
	}
	if len(resA2A.Warnings) == 0 {
		t.Fatalf("expected warnings on A2A UserTurnResult, got none")
	}
	if !strings.Contains(resA2A.Warnings[0], "exited with error") {
		t.Errorf("expected exit error in warning, got %q", resA2A.Warnings[0])
	}
}

// 13. Test failing hook warning surfaced out-of-band in AddAndGenerateTurnStream and AddAndGenerateTurn
func TestHookFailureWarningSurfacedInAddAndGenerateTurnStream(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "stream_warn_agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-stream-warn",
			"object": "chat.completion",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "agent reply"
				},
				"finish_reason": "stop"
			}]
		}`))
	}))
	defer srv.Close()

	runtimeJSON := fmt.Sprintf(`{
		"provider": "openai",
		"endpoint": "%s",
		"model": "test-model",
		"apiKey": "test-key"
	}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	writeHookScript(t, agentDir, "00-bad", `#!/bin/sh
echo "corrupt json output"
`)

	sdk := NewSDK(wsDir)

	// Test 1: AddAndGenerateTurnStream yields ONLY model chunks; warnings arrive via onWarning callback
	var capturedWarnings []string
	var chunks []string
	for chunk, err := range sdk.AddAndGenerateTurnStream(context.Background(), agentID, "test prompt", func(w string) {
		capturedWarnings = append(capturedWarnings, w)
	}) {
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}

	// Warnings must arrive out-of-band via callback
	if len(capturedWarnings) == 0 {
		t.Fatalf("expected warnings via onWarning callback, got none")
	}
	if !strings.HasPrefix(capturedWarnings[0], "Warning: hook ") || !strings.Contains(capturedWarnings[0], "malformed JSON") {
		t.Errorf("expected hook warning via callback, got %q", capturedWarnings[0])
	}

	// Stream chunks must contain ONLY model speech, never hook warnings
	for _, chunk := range chunks {
		if strings.HasPrefix(chunk, "Warning: ") {
			t.Errorf("stream chunk contained hook warning! Chunks must only contain model speech: %q", chunk)
		}
	}
	if len(chunks) != 1 || !strings.Contains(chunks[0], "agent reply") {
		t.Errorf("expected stream to contain only model reply, got %v", chunks)
	}

	// Test 2: AddAndGenerateTurn captures warnings on GenerateTurnResult without polluting response text
	res, err := sdk.AddAndGenerateTurn(context.Background(), agentID, "another turn")
	if err != nil {
		t.Fatalf("AddAndGenerateTurn failed: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("expected warnings on GenerateTurnResult, got none")
	}
	if res.Text != "agent reply" {
		t.Errorf("expected clean model response 'agent reply', got %q", res.Text)
	}
}
