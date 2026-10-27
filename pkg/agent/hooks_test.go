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

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
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
	resp, err := sdk.addAndGenerateTurnImpl(context.Background(), agentID, "What is your name?")
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
	turns, err := ReadSessionTurns(agentDir)
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
	if _, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
		AgentId: agentID,
		Message: "Original Quest",
	}); err != nil {
		t.Fatalf("AddUserTurn failed: %v", err)
	}

	resp, err := sdk.generateTurnImpl(context.Background(), agentID)
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
		res, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
			AgentId: "bob",
			Message: "human message",
		})
		if err != nil {
			t.Fatalf("AddUserTurn failed: %v", err)
		}
		if res.GetText() != "intercepted-by-hook" {
			t.Errorf("expected result text 'intercepted-by-hook', got %q", res.GetText())
		}
		turns, err := ReadSessionTurns(filepath.Join(wsDir, "bob"))
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

		res, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
			AgentId: "bob",
			Message: "peer agent message from alice",
		})
		if err != nil {
			t.Fatalf("AddUserTurn for A2A failed: %v", err)
		}
		if res.GetText() != "intercepted-by-hook" {
			t.Errorf("expected result text 'intercepted-by-hook', got %q", res.GetText())
		}
		turns, err := ReadSessionTurns(filepath.Join(wsDir, "bob"))
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
	res, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
		AgentId: agentID,
		Message: "hello",
	})
	if err != nil {
		t.Fatalf("AddUserTurn failed: %v", err)
	}
	if len(res.GetWarnings()) == 0 {
		t.Fatalf("expected warnings on UserTurnResult, got none")
	}
	if !strings.Contains(res.GetWarnings()[0], "exited with error") {
		t.Errorf("expected exit error in warning, got %q", res.GetWarnings()[0])
	}
	if res.GetText() != "hello" {
		t.Errorf("expected text unchanged 'hello', got %q", res.GetText())
	}

	// Subtest 2: A2A-style turn via AddUserTurn
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"peer","call_chain":["peer"],"trace_id":"tr-1"}`)

	resA2A, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
		AgentId: agentID,
		Message: "peer turn",
	})
	if err != nil {
		t.Fatalf("AddUserTurn for A2A failed: %v", err)
	}
	if len(resA2A.GetWarnings()) == 0 {
		t.Fatalf("expected warnings on A2A UserTurnResult, got none")
	}
	if !strings.Contains(resA2A.GetWarnings()[0], "exited with error") {
		t.Errorf("expected exit error in warning, got %q", resA2A.GetWarnings()[0])
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
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(context.Background(), agentID, "test prompt", func(w string) {
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
	res, err := sdk.addAndGenerateTurnImpl(context.Background(), agentID, "another turn")
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

// 14. Test unified HookAction extension semantics in RunHookChain
func TestHookActionExtensionSemantics(t *testing.T) {
	t.Run("short_circuit first-wins and stops chain on pre-run-command", func(t *testing.T) {
		agentDir := t.TempDir()
		writeEventHookScript(t, agentDir, EventPreRunCommand, "00-first", `#!/bin/sh
printf '{"action":{"type":"short_circuit","output":"claimed-by-00","stderr":"warn-00","reason":"deferred"}}\n'
`)
		writeEventHookScript(t, agentDir, EventPreRunCommand, "01-second", `#!/bin/sh
echo "ran" > second_ran.marker
printf '{"action":{"type":"short_circuit","output":"claimed-by-01"}}\n'
`)

		res, err := RunHookChain(context.Background(), agentDir, EventPreRunCommand, "payload")
		if err != nil {
			t.Fatalf("RunHookChain failed: %v", err)
		}
		if res.Action == nil {
			t.Fatalf("expected non-nil Action")
		}
		if res.Action.Type != HookActionShortCircuit {
			t.Errorf("expected Type=%q, got %q", HookActionShortCircuit, res.Action.Type)
		}
		if res.Action.Output != "claimed-by-00" || res.Action.Stderr != "warn-00" || res.Action.Reason != "deferred" {
			t.Errorf("unexpected Action fields: %+v", res.Action)
		}
		if _, err := os.Stat(filepath.Join(agentDir, "second_ran.marker")); !os.IsNotExist(err) {
			t.Errorf("expected 01-second NOT to run after 00-first short-circuited")
		}
	})

	t.Run("continue runs all hooks and composes reasons and outputs on end-of-turn", func(t *testing.T) {
		agentDir := t.TempDir()
		writeEventHookScript(t, agentDir, EventEndOfTurn, "00-first", `#!/bin/sh
printf '{"action":{"type":"continue","reason":"reason-1","output":"msg-1"}}\n'
`)
		writeEventHookScript(t, agentDir, EventEndOfTurn, "01-decline", `#!/bin/sh
printf '{}\n'
`)
		writeEventHookScript(t, agentDir, EventEndOfTurn, "02-third", `#!/bin/sh
printf '{"action":{"type":"continue","reason":"reason-2","output":"msg-2"}}\n'
`)

		res, err := RunHookChain(context.Background(), agentDir, EventEndOfTurn, "payload")
		if err != nil {
			t.Fatalf("RunHookChain failed: %v", err)
		}
		if res.Action == nil || res.Action.Type != HookActionContinue {
			t.Fatalf("expected Action.Type=%q, got %+v", HookActionContinue, res.Action)
		}
		if len(res.Actions) != 2 {
			t.Fatalf("expected 2 Actions, got %d", len(res.Actions))
		}
		if res.Action.Reason != "reason-1; reason-2" {
			t.Errorf("expected composed reason 'reason-1; reason-2', got %q", res.Action.Reason)
		}
		if res.Action.Output != "msg-1\nmsg-2" {
			t.Errorf("expected composed output 'msg-1\\nmsg-2', got %q", res.Action.Output)
		}
	})

	t.Run("unknown action type emits warning and degrades to nil Action", func(t *testing.T) {
		agentDir := t.TempDir()
		writeEventHookScript(t, agentDir, EventPreRunCommand, "00-unknown", `#!/bin/sh
printf '{"action":{"type":"bogus_directive","output":"ignored"}}\n'
`)

		res, err := RunHookChain(context.Background(), agentDir, EventPreRunCommand, "payload")
		if err != nil {
			t.Fatalf("RunHookChain failed: %v", err)
		}
		if res.Action != nil {
			t.Errorf("expected nil Action for unsupported action type, got %+v", res.Action)
		}
		if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "unsupported action type") {
			t.Errorf("expected unsupported action type warning, got %v", res.Warnings)
		}
	})
}

// TestPhoebeProbeActionPrecedence pins P2 event-aware action coherence:
//   - A valid-but-unhonored action emitted by an earlier hook must emit a warning,
//     be ignored (never setting Action or appending to Actions), and NEVER break/truncate
//     the chain.
//   - Only an action in the event's honored action set may claim or break the chain.
func TestPhoebeProbeActionPrecedence(t *testing.T) {
	t.Run("pre-run-command: 00=continue(unhonored) 01=short_circuit(CLAIMED) 02=witness", func(t *testing.T) {
		wsDir := t.TempDir()
		agentDir := filepath.Join(wsDir, "probe_pre")
		toolsDir := filepath.Join(agentDir, "tools")
		if err := os.MkdirAll(toolsDir, 0755); err != nil {
			t.Fatalf("mkdir tools: %v", err)
		}
		if err := os.WriteFile(filepath.Join(toolsDir, "real_cmd"), []byte("#!/bin/sh\necho 'real_ran' > real_ran.marker\n"), 0755); err != nil {
			t.Fatalf("write tool: %v", err)
		}

		writeEventHookScript(t, agentDir, EventPreRunCommand, "00-irrelevant-continue", `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"continue","reason":"wrong-event"}}\n'
`)
		writeEventHookScript(t, agentDir, EventPreRunCommand, "01-valid-claim", `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"short_circuit","output":"CLAIMED","reason":"auto-deferred"}}\n'
`)
		writeEventHookScript(t, agentDir, EventPreRunCommand, "02-witness", `#!/bin/sh
cat > /dev/null
touch witness.marker
printf '{}\n'
`)

		res, err := RunHookChain(context.Background(), agentDir, EventPreRunCommand, "{}")
		if err != nil {
			t.Fatalf("RunHookChain: %v", err)
		}
		if res.Action == nil || res.Action.Type != HookActionShortCircuit || res.Action.Output != "CLAIMED" {
			t.Fatalf("expected winner=short_circuit(CLAIMED), got %+v", res.Action)
		}
		if len(res.Actions) != 1 || res.Actions[0].Type != HookActionShortCircuit {
			t.Fatalf("expected Actions=[short_circuit], got %+v", res.Actions)
		}
		if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "not honored by event") {
			t.Errorf("expected unhonored action warning for 00-irrelevant-continue, got %v", res.Warnings)
		}
		// 01 legitimately claimed short_circuit, so 02 must not run.
		if _, err := os.Stat(filepath.Join(agentDir, "witness.marker")); !os.IsNotExist(err) {
			t.Errorf("expected 02-witness not to run after honored 01-valid-claim short-circuited")
		}

		// End-to-end via executeRunCommand: 00's unhonored continue must not prevent 01 from claiming the command.
		discovered, _, _, err := DiscoverAgentToolsMap(agentDir)
		if err != nil {
			t.Fatalf("DiscoverAgentToolsMap: %v", err)
		}
		cmdRes, err := executeRunCommand(context.Background(), agentDir, discovered, nil, 5, RunCommandArgs{
			Command: "real_cmd",
			Args:    []string{},
		})
		if err != nil {
			t.Fatalf("executeRunCommand: %v", err)
		}
		if cmdRes.Output != "<STDOUT>\nCLAIMED\n</STDOUT>" {
			t.Errorf("cmdRes.Output = %q, want '<STDOUT>\\nCLAIMED\\n</STDOUT>'", cmdRes.Output)
		}
		if _, err := os.Stat(filepath.Join(agentDir, "real_ran.marker")); !os.IsNotExist(err) {
			t.Errorf("real command executed despite 01-valid-claim short-circuiting")
		}
	})

	t.Run("end-of-turn: 00=short_circuit(unhonored) 01=continue 02=witness", func(t *testing.T) {
		agentDir := t.TempDir()

		writeEventHookScript(t, agentDir, EventEndOfTurn, "00-irrelevant-short-circuit", `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"short_circuit","output":"wrong-event"}}\n'
`)
		writeEventHookScript(t, agentDir, EventEndOfTurn, "01-valid-continue", `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"continue","reason":"job-complete","output":"Job 7 finished."}}\n'
`)
		writeEventHookScript(t, agentDir, EventEndOfTurn, "02-witness", `#!/bin/sh
cat > /dev/null
touch witness.marker
printf '{}\n'
`)

		res, err := RunHookChain(context.Background(), agentDir, EventEndOfTurn, "{}")
		if err != nil {
			t.Fatalf("RunHookChain: %v", err)
		}
		if res.Action == nil || res.Action.Type != HookActionContinue {
			t.Fatalf("expected winner=continue, got %+v", res.Action)
		}
		if len(res.Actions) != 1 || res.Actions[0].Type != HookActionContinue {
			t.Fatalf("expected Actions=[continue], got %+v", res.Actions)
		}
		if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "not honored by event") {
			t.Errorf("expected unhonored action warning for 00-irrelevant-short-circuit, got %v", res.Warnings)
		}
		// Unhonored short_circuit in 00 must NOT break the chain: 02-witness must have run!
		if _, err := os.Stat(filepath.Join(agentDir, "witness.marker")); err != nil {
			t.Fatalf("expected 02-witness to run (unhonored short_circuit in 00 must not truncate chain): %v", err)
		}
	})

	t.Run("passive event (on-user-message) ignores both short_circuit and continue without truncating chain", func(t *testing.T) {
		agentDir := t.TempDir()
		writeEventHookScript(t, agentDir, EventOnUserMessage, "00-try-short-circuit", `#!/bin/sh
cat > /dev/null
printf '{"text":"step-00","action":{"type":"short_circuit","output":"ignored"}}\n'
`)
		writeEventHookScript(t, agentDir, EventOnUserMessage, "01-witness", `#!/bin/sh
cat > /dev/null
touch witness_user_msg.marker
printf '{"text":"step-01","action":{"type":"continue"}}\n'
`)

		res, err := RunHookChain(context.Background(), agentDir, EventOnUserMessage, "orig")
		if err != nil {
			t.Fatalf("RunHookChain: %v", err)
		}
		if res.Action != nil || len(res.Actions) != 0 {
			t.Errorf("expected nil Action and empty Actions on passive event, got Action=%+v Actions=%+v", res.Action, res.Actions)
		}
		if res.Text != "step-01" {
			t.Errorf("expected chain to complete and produce Text='step-01', got %q", res.Text)
		}
		if _, err := os.Stat(filepath.Join(agentDir, "witness_user_msg.marker")); err != nil {
			t.Errorf("expected 01-witness to run on passive event: %v", err)
		}
		if len(res.Warnings) != 2 {
			t.Errorf("expected 2 unhonored-action warnings, got %v", res.Warnings)
		}
	})
}

func writeEventHookScript(t *testing.T, agentDir, event, filename, content string) string {
	t.Helper()
	dir := filepath.Join(agentDir, "hooks", event)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	hookPath := filepath.Join(dir, filename)
	if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write hook script %s: %v", filename, err)
	}
	return hookPath
}

// 15. Test pre-run-command hook event: zero-hooks parity, payload delivery, short-circuit, decline, multi-hook precedence, and failure degradation.
func TestPreRunCommandHook(t *testing.T) {
	setupAgentWithTool := func(t *testing.T, toolName, toolBody string) (string, map[string]string) {
		t.Helper()
		wsDir := t.TempDir()
		agentDir := filepath.Join(wsDir, "cmd_agent")
		toolsDir := filepath.Join(agentDir, "tools")
		if err := os.MkdirAll(toolsDir, 0755); err != nil {
			t.Fatalf("mkdir tools: %v", err)
		}
		toolPath := filepath.Join(toolsDir, toolName)
		if err := os.WriteFile(toolPath, []byte(toolBody), 0755); err != nil {
			t.Fatalf("write tool: %v", err)
		}
		discovered, _, _, err := DiscoverAgentToolsMap(agentDir)
		if err != nil {
			t.Fatalf("DiscoverAgentToolsMap: %v", err)
		}
		return agentDir, discovered
	}

	t.Run("zero hooks registered behaves byte-for-byte identically", func(t *testing.T) {
		agentDir, discovered := setupAgentWithTool(t, "echo_tool", "#!/bin/sh\nprintf 'hello %s' \"$1\"\necho 'err-line' >&2\n")
		res, err := executeRunCommand(context.Background(), agentDir, discovered, nil, 5, RunCommandArgs{
			Command: "echo_tool",
			Args:    []string{"world"},
		})
		if err != nil {
			t.Fatalf("executeRunCommand failed: %v", err)
		}
		want := "<STDOUT>\nhello world\n</STDOUT><STDERR>\nerr-line\n\n</STDERR>"
		if res.Output != want {
			t.Errorf("got output %q, want %q", res.Output, want)
		}
		if res.Warning != "" || len(res.Warnings) != 0 {
			t.Errorf("expected no warnings, got %v", res.Warnings)
		}
	})

	t.Run("short-circuit prevents real command from running and returns substituted tool output", func(t *testing.T) {
		// Real tool writes a marker file and exits 1 if it ever runs.
		agentDir, discovered := setupAgentWithTool(t, "long_task", "#!/bin/sh\necho 'executed' > real_cmd_ran.marker\nexit 1\n")
		if err := os.WriteFile(filepath.Join(agentDir, ".env"), []byte("DOT_VAR=from_dotenv\n"), 0644); err != nil {
			t.Fatalf("write .env: %v", err)
		}

		writeEventHookScript(t, agentDir, EventPreRunCommand, "00-defer", `#!/bin/sh
cat > captured_payload.json
printf '{"action":{"type":"short_circuit","output":"started background job 9f8e7d6c","stderr":"note: backgrounded","reason":"auto-defer"}}\n'
`)

		res, err := executeRunCommand(context.Background(), agentDir, discovered, nil, 5, RunCommandArgs{
			Command: "long_task",
			Args:    []string{"--target", "alpha"},
			Env:     map[string]string{"CALL_VAR": "from_call"},
		})
		if err != nil {
			t.Fatalf("expected short-circuited run_command to succeed without error, got: %v", err)
		}

		// Real binary must never have executed.
		if _, statErr := os.Stat(filepath.Join(agentDir, "real_cmd_ran.marker")); !os.IsNotExist(statErr) {
			t.Fatalf("real command executed despite hook short-circuit")
		}

		wantOut := "<STDOUT>\nstarted background job 9f8e7d6c\n</STDOUT><STDERR>\nnote: backgrounded\n</STDERR>"
		if res.Output != wantOut {
			t.Errorf("short-circuit output = %q, want %q", res.Output, wantOut)
		}

		// Verify captured payload fields.
		rawPayload, err := os.ReadFile(filepath.Join(agentDir, "captured_payload.json"))
		if err != nil {
			t.Fatalf("read captured_payload.json: %v", err)
		}
		var payload PreRunCommandPayload
		if err := json.Unmarshal(rawPayload, &payload); err != nil {
			t.Fatalf("unmarshal PreRunCommandPayload: %v (raw: %s)", err, string(rawPayload))
		}
		if payload.AgentID != "cmd_agent" {
			t.Errorf("payload.AgentID = %q, want 'cmd_agent'", payload.AgentID)
		}
		if payload.Command != "long_task" {
			t.Errorf("payload.Command = %q, want 'long_task'", payload.Command)
		}
		if !strings.HasSuffix(payload.CommandPath, filepath.Join("tools", "long_task")) {
			t.Errorf("payload.CommandPath = %q, expected tools/long_task suffix", payload.CommandPath)
		}
		if len(payload.Args) != 2 || payload.Args[0] != "--target" || payload.Args[1] != "alpha" {
			t.Errorf("payload.Args = %v, want [--target alpha]", payload.Args)
		}
		if payload.Cwd != agentDir {
			t.Errorf("payload.Cwd = %q, want %q", payload.Cwd, agentDir)
		}
		if payload.Env["DOT_VAR"] != "from_dotenv" {
			t.Errorf("payload.Env[DOT_VAR] = %q, want 'from_dotenv'", payload.Env["DOT_VAR"])
		}
		if payload.Env["CALL_VAR"] != "from_call" {
			t.Errorf("payload.Env[CALL_VAR] = %q, want 'from_call'", payload.Env["CALL_VAR"])
		}
	})

	t.Run("hook declining short-circuit lets real command run with optional env mutation", func(t *testing.T) {
		agentDir, discovered := setupAgentWithTool(t, "check_env", "#!/bin/sh\nprintf '%s:%s' \"$1\" \"$HOOK_INJECTED\"\n")
		writeEventHookScript(t, agentDir, EventPreRunCommand, "00-pass", `#!/bin/sh
cat > /dev/null
printf '{"env":{"HOOK_INJECTED":"from-pre-hook"}}\n'
`)

		res, err := executeRunCommand(context.Background(), agentDir, discovered, nil, 5, RunCommandArgs{
			Command: "check_env",
			Args:    []string{"ok"},
		})
		if err != nil {
			t.Fatalf("executeRunCommand failed: %v", err)
		}
		want := "<STDOUT>\nok:from-pre-hook\n</STDOUT>"
		if res.Output != want {
			t.Errorf("output = %q, want %q", res.Output, want)
		}
	})

	t.Run("multiple hooks: first to short-circuit wins and prevents later hooks and command from running", func(t *testing.T) {
		agentDir, discovered := setupAgentWithTool(t, "work", "#!/bin/sh\necho 'real' > real.marker\n")
		writeEventHookScript(t, agentDir, EventPreRunCommand, "00-decline", `#!/bin/sh
cat > /dev/null
printf '{}\n'
`)
		writeEventHookScript(t, agentDir, EventPreRunCommand, "01-claim", `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"short-circuit","output":"claimed-by-01"}}\n'
`)
		writeEventHookScript(t, agentDir, EventPreRunCommand, "02-late", `#!/bin/sh
echo 'late' > late_hook.marker
printf '{"action":{"type":"short_circuit","output":"claimed-by-02"}}\n'
`)

		res, err := executeRunCommand(context.Background(), agentDir, discovered, nil, 5, RunCommandArgs{
			Command: "work",
			Args:    []string{},
		})
		if err != nil {
			t.Fatalf("executeRunCommand failed: %v", err)
		}
		if res.Output != "<STDOUT>\nclaimed-by-01\n</STDOUT>" {
			t.Errorf("output = %q, want '<STDOUT>\\nclaimed-by-01\\n</STDOUT>'", res.Output)
		}
		if _, err := os.Stat(filepath.Join(agentDir, "late_hook.marker")); !os.IsNotExist(err) {
			t.Errorf("02-late hook ran after 01-claim already short-circuited")
		}
		if _, err := os.Stat(filepath.Join(agentDir, "real.marker")); !os.IsNotExist(err) {
			t.Errorf("real command ran after 01-claim short-circuited")
		}
	})

	t.Run("broken hook degrades to normal command execution (P5)", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			script string
		}{
			{"non-zero exit", "#!/bin/sh\nexit 2\n"},
			{"malformed json", "#!/bin/sh\necho 'not-json'\n"},
			{"empty stdout", "#!/bin/sh\nexit 0\n"},
			{"unknown action type", "#!/bin/sh\nprintf '{\"action\":{\"type\":\"invalid\"}}'\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				agentDir, discovered := setupAgentWithTool(t, "normal_cmd", "#!/bin/sh\nprintf 'ran-normally'\n")
				writeEventHookScript(t, agentDir, EventPreRunCommand, "00-broken", tc.script)

				res, err := executeRunCommand(context.Background(), agentDir, discovered, nil, 5, RunCommandArgs{
					Command: "normal_cmd",
					Args:    []string{},
				})
				if err != nil {
					t.Fatalf("expected broken hook to degrade to normal execution, got error: %v", err)
				}
				if res.Output != "<STDOUT>\nran-normally\n</STDOUT>" {
					t.Errorf("output = %q, want '<STDOUT>\\nran-normally\\n</STDOUT>'", res.Output)
				}
			})
		}
	})
}

// 16. Test end-of-turn hook event: zero-hooks parity, payload delivery, continuation trigger, D88 budget cap, multi-hook composition, and failure degradation.
func TestEndOfTurnHook(t *testing.T) {
	setupTurnAgent := func(t *testing.T, agentID string, maxAutoCont *int, handler http.HandlerFunc) (string, string, *httptest.Server) {
		t.Helper()
		wsDir := t.TempDir()
		agentDir := filepath.Join(wsDir, agentID)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("mkdir agentDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt."), 0644); err != nil {
			t.Fatalf("write AGENTS.md: %v", err)
		}
		if err := AppendSessionTurn(agentDir, "user", "Initial user task"); err != nil {
			t.Fatalf("AppendSessionTurn: %v", err)
		}
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)

		rt := RuntimeConfig{
			Provider:             "openai",
			Model:                "test-model",
			Endpoint:             srv.URL,
			APIKey:               "test-key",
			MaxAutoContinuations: maxAutoCont,
		}
		rtBytes, err := json.Marshal(rt)
		if err != nil {
			t.Fatalf("marshal runtime.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), rtBytes, 0644); err != nil {
			t.Fatalf("write runtime.json: %v", err)
		}
		return wsDir, agentDir, srv
	}

	makeSimpleHandler := func(callCount *int, responses ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*callCount++
			idx := *callCount - 1
			reply := fmt.Sprintf("reply-%d", *callCount)
			if idx < len(responses) {
				reply = responses[idx]
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"id": "chatcmpl-%d",
				"object": "chat.completion",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": %q}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
			}`, *callCount, reply)
		}
	}

	t.Run("zero hooks registered behaves byte-for-byte identically (P3)", func(t *testing.T) {
		calls := 0
		wsDir, agentDir, _ := setupTurnAgent(t, "eot_zero", nil, makeSimpleHandler(&calls, "single turn response"))

		fa, err := LoadFolderAgent(wsDir, "eot_zero", DefaultMaxToolTurns)
		if err != nil {
			t.Fatalf("LoadFolderAgent: %v", err)
		}
		resp, err := fa.GenerateTurn(context.Background())
		if err != nil {
			t.Fatalf("GenerateTurn: %v", err)
		}
		if resp != "single turn response" {
			t.Errorf("resp = %q, want 'single turn response'", resp)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
		turns, err := ReadSessionTurns(agentDir)
		if err != nil {
			t.Fatalf("ReadSessionTurns: %v", err)
		}
		if len(turns) != 2 {
			t.Errorf("len(turns) = %d, want 2 (user + model)", len(turns))
		}
	})

	t.Run("hook requesting continuation triggers next turn and delivers payload context", func(t *testing.T) {
		calls := 0
		wsDir, agentDir, _ := setupTurnAgent(t, "eot_cont", nil, makeSimpleHandler(&calls, "first turn reply", "second turn after hook continuation"))

		// Hook requests continuation on first invocation, then declines on second.
		writeEventHookScript(t, agentDir, EventEndOfTurn, "00-check", `#!/bin/sh
if [ ! -f turn1_payload.json ]; then
	cat > turn1_payload.json
	printf '{"action":{"type":"continue","reason":"job-42-done","output":"Background job 42 finished (exit 0)."}}\n'
else
	cat > turn2_payload.json
	printf '{}\n'
fi
`)

		fa, err := LoadFolderAgent(wsDir, "eot_cont", DefaultMaxToolTurns)
		if err != nil {
			t.Fatalf("LoadFolderAgent: %v", err)
		}
		resp, err := fa.GenerateTurn(context.Background())
		if err != nil {
			t.Fatalf("GenerateTurn: %v", err)
		}
		if calls != 2 {
			t.Fatalf("expected 2 model calls (initial + 1 hook continuation), got %d", calls)
		}
		wantResp := "first turn reply\n\nsecond turn after hook continuation"
		if resp != wantResp {
			t.Errorf("resp = %q, want %q", resp, wantResp)
		}

		// Verify payload 1 and payload 2 captured by hook.
		raw1, err := os.ReadFile(filepath.Join(agentDir, "turn1_payload.json"))
		if err != nil {
			t.Fatalf("read turn1_payload.json: %v", err)
		}
		var p1 EndOfTurnPayload
		if err := json.Unmarshal(raw1, &p1); err != nil {
			t.Fatalf("unmarshal p1: %v", err)
		}
		if p1.AgentID != "eot_cont" || p1.StopReason != TurnStopCompleted || p1.DurationMs < 0 || p1.ContinuationCount != 0 || p1.MaxContinuations != DefaultMaxAutoContinuations || !p1.CanContinue {
			t.Errorf("unexpected p1: %+v", p1)
		}

		raw2, err := os.ReadFile(filepath.Join(agentDir, "turn2_payload.json"))
		if err != nil {
			t.Fatalf("read turn2_payload.json: %v", err)
		}
		var p2 EndOfTurnPayload
		if err := json.Unmarshal(raw2, &p2); err != nil {
			t.Fatalf("unmarshal p2: %v", err)
		}
		if p2.ContinuationCount != 1 {
			t.Errorf("p2.ContinuationCount = %d, want 1", p2.ContinuationCount)
		}

		// Verify session.jsonl persisted the <CONTINUATION> sentinel turn.
		turns, err := ReadSessionTurns(agentDir)
		if err != nil {
			t.Fatalf("ReadSessionTurns: %v", err)
		}
		if len(turns) != 4 {
			t.Fatalf("expected 4 turns in session.jsonl, got %d", len(turns))
		}
		wantSentinel := `<CONTINUATION reason="job-42-done">Background job 42 finished (exit 0).</CONTINUATION>`
		if turns[2].Role != "user" || ContentText(turns[2]) != wantSentinel {
			t.Errorf("turns[2] = (%s, %q), want (user, %q)", turns[2].Role, ContentText(turns[2]), wantSentinel)
		}
	})

	t.Run("hook declining continuation leaves turn ending normally", func(t *testing.T) {
		calls := 0
		wsDir, agentDir, _ := setupTurnAgent(t, "eot_decline", nil, makeSimpleHandler(&calls, "only turn"))
		writeEventHookScript(t, agentDir, EventEndOfTurn, "00-noop", "#!/bin/sh\ncat > /dev/null\nprintf '{}\\n'\n")

		fa, err := LoadFolderAgent(wsDir, "eot_decline", DefaultMaxToolTurns)
		if err != nil {
			t.Fatalf("LoadFolderAgent: %v", err)
		}
		resp, err := fa.GenerateTurn(context.Background())
		if err != nil {
			t.Fatalf("GenerateTurn: %v", err)
		}
		if calls != 1 || resp != "only turn" {
			t.Errorf("calls=%d resp=%q, want 1 and 'only turn'", calls, resp)
		}
	})

	t.Run("hook always requesting continuation is bounded by D88 MaxAutoContinuations (P4)", func(t *testing.T) {
		calls := 0
		capVal := 2
		wsDir, agentDir, _ := setupTurnAgent(t, "eot_bounded", &capVal, makeSimpleHandler(&calls, "t0", "t1", "t2", "t3"))

		// Hook unconditionally requests continuation on every single turn.
		writeEventHookScript(t, agentDir, EventEndOfTurn, "00-forever", `#!/bin/sh
cat > /dev/null
printf '{"action":{"type":"continue","reason":"endless-hook"}}\n'
`)

		fa, err := LoadFolderAgent(wsDir, "eot_bounded", DefaultMaxToolTurns)
		if err != nil {
			t.Fatalf("LoadFolderAgent: %v", err)
		}
		resp, err := fa.GenerateTurn(context.Background())
		if err != nil {
			t.Fatalf("GenerateTurn: %v", err)
		}
		// 1 initial turn + 2 continuations = 3 model calls total.
		if calls != 3 {
			t.Fatalf("expected exactly 3 model calls (1 initial + 2 maxAutoContinuations), got %d", calls)
		}
		for _, want := range []string{"t0", "t1", "t2", "Auto-continuation budget exhausted: 2 of 2 used", "maxAutoContinuations"} {
			if !strings.Contains(resp, want) {
				t.Errorf("response missing %q; got:\n%s", want, resp)
			}
		}
	})

	t.Run("multiple end-of-turn hooks all run and compose when any requests continuation", func(t *testing.T) {
		calls := 0
		wsDir, agentDir, _ := setupTurnAgent(t, "eot_multi", nil, makeSimpleHandler(&calls, "turn-1", "turn-2"))

		writeEventHookScript(t, agentDir, EventEndOfTurn, "00-first", `#!/bin/sh
cat > /dev/null
if [ ! -f hook00.done ]; then
	touch hook00.done
	printf '{"action":{"type":"continue","reason":"r1","output":"out1"}}\n'
else
	printf '{}\n'
fi
`)
		writeEventHookScript(t, agentDir, EventEndOfTurn, "01-middle-decline", `#!/bin/sh
cat > /dev/null
touch hook01.ran
printf '{}\n'
`)
		writeEventHookScript(t, agentDir, EventEndOfTurn, "02-third", `#!/bin/sh
cat > /dev/null
if [ ! -f hook02.done ]; then
	touch hook02.done
	printf '{"action":{"type":"continue","reason":"r2","output":"out2"}}\n'
else
	printf '{}\n'
fi
`)

		fa, err := LoadFolderAgent(wsDir, "eot_multi", DefaultMaxToolTurns)
		if err != nil {
			t.Fatalf("LoadFolderAgent: %v", err)
		}
		if _, err := fa.GenerateTurn(context.Background()); err != nil {
			t.Fatalf("GenerateTurn: %v", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
		if _, err := os.Stat(filepath.Join(agentDir, "hook01.ran")); err != nil {
			t.Errorf("expected 01-middle-decline to run: %v", err)
		}
		turns, err := ReadSessionTurns(agentDir)
		if err != nil {
			t.Fatalf("ReadSessionTurns: %v", err)
		}
		wantSentinel := "<CONTINUATION reason=\"r1; r2\">out1\nout2</CONTINUATION>"
		if len(turns) < 3 || ContentText(turns[2]) != wantSentinel {
			t.Errorf("sentinel = %q, want %q", ContentText(turns[2]), wantSentinel)
		}
	})

	t.Run("broken end-of-turn hook degrades to normal turn completion (P5)", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			script string
		}{
			{"non-zero exit", "#!/bin/sh\nexit 1\n"},
			{"malformed json", "#!/bin/sh\necho 'corrupt'\n"},
			{"empty stdout", "#!/bin/sh\nexit 0\n"},
			{"unknown action type", "#!/bin/sh\nprintf '{\"action\":{\"type\":\"bogus\"}}'\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				calls := 0
				wsDir, agentDir, _ := setupTurnAgent(t, "eot_broken", nil, makeSimpleHandler(&calls, "clean completion"))
				writeEventHookScript(t, agentDir, EventEndOfTurn, "00-broken", tc.script)

				fa, err := LoadFolderAgent(wsDir, "eot_broken", DefaultMaxToolTurns)
				if err != nil {
					t.Fatalf("LoadFolderAgent: %v", err)
				}
				resp, err := fa.GenerateTurn(context.Background())
				if err != nil {
					t.Fatalf("expected broken hook to degrade cleanly, got error: %v", err)
				}
				if calls != 1 || resp != "clean completion" {
					t.Errorf("calls=%d resp=%q, want 1 and 'clean completion'", calls, resp)
				}
			})
		}
	})
}
