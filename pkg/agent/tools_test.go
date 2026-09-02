package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

func TestExecuteTool(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "echo_tool.sh")
	script := "#!/bin/sh\necho \"Arg1: $1, Env: $TEST_VAR\"\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	args := ExecToolArgs{
		Args: []string{"hello"},
		Env:  map[string]string{"TEST_VAR": "world"},
	}
	output, err := executeTool(context.Background(), agentDir, "echo_tool.sh", toolPath, args, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(output, "Arg1: hello, Env: world") {
		t.Fatalf("unexpected tool output: %q", output)
	}
}

// TestExecuteTool_NoStdinEchoWithoutExplicitStdin confirms D53: a tool called
// with Args but no explicit Stdin gets no stdin at all (reads EOF
// immediately), not a WACKYPUB_TOOL_ARGS-shaped JSON echo of its own
// invocation - that fallback used to feed every Args/Env-bearing call some
// stdin whether the caller wanted it or not, discovered live via wackyproc
// (a tool that itself forwards its own stdin to a spawned child) treating
// that echoed JSON as real input to relay.
func TestExecuteTool_NoStdinEchoWithoutExplicitStdin(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "read_stdin.sh")
	script := "#!/bin/sh\ndata=$(cat)\necho \"stdin was: [$data]\"\necho \"env was: [$WACKYPUB_TOOL_ARGS]\"\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	args := ExecToolArgs{Args: []string{"some", "args"}}
	output, err := executeTool(context.Background(), agentDir, "read_stdin.sh", toolPath, args, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(output, "stdin was: []") {
		t.Errorf("expected empty stdin (immediate EOF), got: %q", output)
	}
	if !strings.Contains(output, "env was: []") {
		t.Errorf("expected WACKYPUB_TOOL_ARGS to be unset, got: %q", output)
	}
}

func TestExecuteTool_SymlinkResolution(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "real_target")
	agentDir := filepath.Join(tmpDir, "agent")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir, "tools"), 0755); err != nil {
		t.Fatalf("failed to create agent tools dir: %v", err)
	}

	// Script uses dirname $0 to echo where it thinks it is located
	realScriptPath := filepath.Join(targetDir, "script.sh")
	scriptContent := "#!/bin/sh\nDIR=$(dirname \"$0\")\necho \"DIR=$DIR\"\n"
	if err := os.WriteFile(realScriptPath, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	// Create symlink inside agent tools directory pointing to real script
	symlinkPath := filepath.Join(agentDir, "tools", "script_link.sh")
	if err := os.Symlink(realScriptPath, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	out, err := executeTool(context.Background(), agentDir, "script_link.sh", symlinkPath, ExecToolArgs{}, nil)
	if err != nil {
		t.Fatalf("executeTool failed: %v", err)
	}

	realDir, _ := filepath.EvalSymlinks(targetDir)
	if !strings.Contains(out, "DIR="+realDir) {
		t.Fatalf("expected DIR=%s in output (evaluating symlink), got: %s", realDir, out)
	}
}

func TestExecuteTool_Failure(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "fail_tool.sh")
	script := "#!/bin/sh\necho \"stdout output before failure\"\necho \"something broke\" >&2\nexit 1\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fail tool script: %v", err)
	}

	out, err := executeTool(context.Background(), agentDir, "fail_tool.sh", toolPath, ExecToolArgs{}, nil)

	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "tool fail_tool.sh failed: exit status 1") {
		t.Fatalf("unexpected headline error message: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "stdout output before failure") {
		t.Fatalf("expected stdout output to be preserved in error: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "something broke") {
		t.Fatalf("expected stderr output to be preserved in error: %q", err.Error())
	}
	if !strings.Contains(out, "stdout output before failure") {
		t.Fatalf("expected stdout output to survive in failure result payload: %q", out)
	}
}

func TestBuildFolderAgentTools(t *testing.T) {
	agentDir := t.TempDir()
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}
	shPath := filepath.Join(toolsDir, "custom.sh")
	if err := os.WriteFile(shPath, []byte("#!/bin/sh\necho custom_out"), 0755); err != nil {
		t.Fatalf("failed to write custom.sh: %v", err)
	}

	toolMap, decls, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}

	// Should contain create_scratchpad, get_scratchpad, list_scratchpads, search_scratchpad, delete_scratchpad, run_command, load_skill, load_skill_extra, list_skill_extra, run_skill_script (10 tools)
	if len(toolMap) != 10 {
		t.Errorf("expected 10 tools, got %d", len(toolMap))
	}
	if len(decls) != 10 {
		t.Errorf("expected 10 decls, got %d", len(decls))
	}
	if _, ok := toolMap["create_scratchpad"]; !ok {
		t.Errorf("missing create_scratchpad in toolMap")
	}
	if _, ok := toolMap["get_scratchpad"]; !ok {
		t.Errorf("missing get_scratchpad in toolMap")
	}
	runCmd, ok := toolMap["run_command"]
	if !ok {
		t.Fatalf("missing run_command in toolMap")
	}

	decler, ok := runCmd.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	if !ok {
		t.Fatalf("run_command does not implement Declaration()")
	}

	decl := decler.Declaration()
	if !strings.Contains(decl.Description, "Available commands: custom.sh.") {
		t.Errorf("expected description to list custom.sh, got: %s", decl.Description)
	}
	if !strings.Contains(decl.Description, "Usage Guidance:") {
		t.Errorf("expected description to contain Usage Guidance, got: %s", decl.Description)
	}
}

func TestDiscoverAgentTools_SymlinkToolpack(t *testing.T) {
	tmpDir := t.TempDir()

	// External toolpack directory with 3 executable files
	toolpackDir := filepath.Join(tmpDir, "external_toolpack")
	if err := os.MkdirAll(toolpackDir, 0755); err != nil {
		t.Fatalf("failed to create external toolpack: %v", err)
	}
	for _, name := range []string{"cat", "ls", "man"} {
		path := filepath.Join(toolpackDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+name), 0755); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	// Agent directory with tools/ folder containing a symlink to external_toolpack
	agentDir := filepath.Join(tmpDir, "agent_bob")
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create agent tools dir: %v", err)
	}

	symlinkPath := filepath.Join(toolsDir, "read-only-fs")
	if err := os.Symlink(toolpackDir, symlinkPath); err != nil {
		t.Fatalf("failed to create toolpack symlink: %v", err)
	}

	toolMap, discovered, shadowed, err := DiscoverAgentToolsMap(agentDir)
	if err != nil {
		t.Fatalf("DiscoverAgentToolsMap failed: %v", err)
	}

	if len(shadowed) > 0 {
		t.Errorf("expected 0 shadowed warnings, got %v", shadowed)
	}

	// Must discover cat, ls, man (3 tools), NOT "read-only-fs"
	expected := []string{"cat", "ls", "man"}
	if len(discovered) != len(expected) {
		t.Fatalf("expected discovered tools %v, got %v", expected, discovered)
	}
	for i, name := range expected {
		if discovered[i] != name {
			t.Errorf("discovered[%d] = %q, expected %q", i, discovered[i], name)
		}
		if _, exists := toolMap[name]; !exists {
			t.Errorf("toolMap missing entry for %q", name)
		}
	}

	// Verify "read-only-fs" directory symlink itself is NOT registered as a tool
	if _, exists := toolMap["read-only-fs"]; exists {
		t.Errorf("directory symlink 'read-only-fs' was wrongly registered as an executable tool")
	}
}

func TestExecuteTool_RelativePath(t *testing.T) {
	// Setup subdirectories inside current CWD to test relative paths
	relAgentDir := filepath.Join("test_scratch", "agent_rel")
	if err := os.MkdirAll(relAgentDir, 0755); err != nil {
		t.Fatalf("failed to create rel agent dir: %v", err)
	}
	defer os.RemoveAll("test_scratch")

	toolPath := filepath.Join(relAgentDir, "tool.sh")
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\necho relative_ok"), 0755); err != nil {
		t.Fatalf("failed to write tool.sh: %v", err)
	}

	out, err := executeTool(context.Background(), relAgentDir, "tool.sh", toolPath, ExecToolArgs{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "relative_ok") {
		t.Fatalf("expected 'relative_ok', got: %q", out)
	}
}

type runCmdTestModel struct {
	command string
	args    []string
}

func (m *runCmdTestModel) Name() string { return "run-cmd-test-model" }

func (m *runCmdTestModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		res := &model.LLMResponse{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "run_command",
							Args: map[string]any{
								"command": m.command,
								"args":    m.args,
							},
						},
					},
				},
			},
		}
		yield(res, nil)
	}
}

func TestRunCommandToolValidationAndExecution(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"dummy-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// Create 2 discovered tools: echo.sh and greet.sh
	if err := os.WriteFile(filepath.Join(toolsDir, "echo.sh"), []byte("#!/bin/sh\necho \"echo: $1\""), 0755); err != nil {
		t.Fatalf("failed to write echo.sh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "greet.sh"), []byte("#!/bin/sh\necho \"greet: $1\""), 0755); err != nil {
		t.Fatalf("failed to write greet.sh: %v", err)
	}

	toolMap, decls, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}

	// 10 tools in toolMap: create_scratchpad, get_scratchpad, list_scratchpads, search_scratchpad, delete_scratchpad, run_command, load_skill, load_skill_extra, list_skill_extra, run_skill_script
	if len(toolMap) != 10 {
		t.Fatalf("expected 10 tools in toolMap, got %d", len(toolMap))
	}
	if len(decls) != 10 {
		t.Fatalf("expected 10 decls, got %d", len(decls))
	}

	runCmdTool, ok := toolMap["run_command"]
	if !ok {
		t.Fatalf("missing run_command tool in toolMap")
	}

	// Verify command list in description is alphabetically sorted: echo.sh, greet.sh
	decler := runCmdTool.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	desc := decler.Declaration().Description
	if !strings.Contains(desc, "Available commands: echo.sh, greet.sh.") {
		t.Errorf("expected sorted commands list in description, got: %s", desc)
	}

	// Test executing valid command 'echo.sh' via FolderAgent and ADK runner
	fa, err := LoadFolderAgent(wsDir, "bob", 1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	fa.Model = &runCmdTestModel{command: "echo.sh", args: []string{"world"}}

	toolsList := []tool.Tool{toolMap["create_scratchpad"], toolMap["get_scratchpad"], toolMap["list_scratchpads"], runCmdTool}
	fa.ADKAgent, err = BuildADKAgent("bob", fa.SystemPrompt, 1, fa.Model, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgent failed: %v", err)
	}

	// Add user turn to session.jsonl
	uMsg := genai.NewContentFromText("run echo", "user")
	if err := AppendSessionContent(agentDir, uMsg); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	// Run turn (expect max tool turns limit error because test model constantly returns FunctionCall)
	_, _ = fa.GenerateTurn(context.Background())

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	// session.jsonl should contain FunctionResponse with output "echo: world"
	foundOutput := false
	for _, turn := range turns {
		if turn.Role == "user" {
			for _, part := range turn.Parts {
				if part.FunctionResponse != nil {
					respJSON, _ := json.Marshal(part.FunctionResponse.Response)
					if strings.Contains(string(respJSON), "echo: world") {
						foundOutput = true
					}
				}
			}
		}
	}
	if !foundOutput {
		t.Errorf("expected to find FunctionResponse containing 'echo: world' in session.jsonl")
	}
}

func TestSearchScratchpad(t *testing.T) {
	agentDir := t.TempDir()

	text := "Line 1: Alpha\nLine 2: Beta\nLine 3: ALPHA\nLine 4: Gamma\nLine 5: alpha delta\n"
	entry, err := CreateScratchpad(agentDir, text, "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	// 1. Literal case-sensitive search for "Alpha"
	resCase, err := SearchScratchpad(agentDir, entry.ID, "Alpha", nil, false, 50)
	if err != nil {
		t.Fatalf("SearchScratchpad case-sensitive failed: %v", err)
	}
	if resCase.TotalMatches != 1 {
		t.Errorf("expected 1 match for 'Alpha', got %d", resCase.TotalMatches)
	}
	if len(resCase.Matches) > 0 {
		if resCase.Matches[0].Line != 1 || resCase.Matches[0].SkipLines != 0 {
			t.Errorf("expected match line 1, skip_lines 0, got line %d, skip_lines %d", resCase.Matches[0].Line, resCase.Matches[0].SkipLines)
		}
	}

	// 2. Case-insensitive search for "alpha"
	caseSensFalse := false
	resNoCase, err := SearchScratchpad(agentDir, entry.ID, "alpha", &caseSensFalse, false, 50)
	if err != nil {
		t.Fatalf("SearchScratchpad case-insensitive failed: %v", err)
	}
	if resNoCase.TotalMatches != 3 {
		t.Errorf("expected 3 matches for case-insensitive 'alpha', got %d", resNoCase.TotalMatches)
	}

	// 3. Regex search for "Alpha|Beta"
	resRegex, err := SearchScratchpad(agentDir, entry.ID, "Alpha|Beta", nil, true, 50)
	if err != nil {
		t.Fatalf("SearchScratchpad regex failed: %v", err)
	}
	if resRegex.TotalMatches != 2 {
		t.Errorf("expected 2 matches for regex 'Alpha|Beta', got %d", resRegex.TotalMatches)
	}

	// 4. Max results capping
	resCap, err := SearchScratchpad(agentDir, entry.ID, "alpha", &caseSensFalse, false, 2)
	if err != nil {
		t.Fatalf("SearchScratchpad capped failed: %v", err)
	}
	if resCap.TotalMatches != 3 {
		t.Errorf("expected total_matches 3, got %d", resCap.TotalMatches)
	}
	if len(resCap.Matches) != 2 {
		t.Errorf("expected len(Matches) 2 due to cap, got %d", len(resCap.Matches))
	}
}

func TestExecuteTool_Timeout(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "sleep_tool.sh")
	script := "#!/bin/sh\necho \"partial output before sleep\"\nsleep 5\necho done\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	out, err := executeTool(context.Background(), agentDir, "sleep_tool.sh", toolPath, ExecToolArgs{}, nil, 1)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "tool sleep_tool.sh timed out after 1 seconds") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if !strings.Contains(err.Error(), "partial output before sleep") {
		t.Fatalf("expected stdout output to be preserved in timeout error: %q", err.Error())
	}
	if !strings.Contains(out, "partial output before sleep") {
		t.Fatalf("expected stdout output to survive in timeout result payload: %q", out)
	}
}

func TestExecuteTool_ProcessGroupKill(t *testing.T) {
	agentDir := t.TempDir()
	pidFile := filepath.Join(agentDir, "bg.pid")
	toolPath := filepath.Join(agentDir, "bg_spawn.sh")

	// Script spawns a background sleeper, writes its PID to bg.pid, and sleeps forever
	script := fmt.Sprintf("#!/bin/sh\nsleep 30 &\necho $! > %s\nsleep 30\n", pidFile)
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	_, err := executeTool(context.Background(), agentDir, "bg_spawn.sh", toolPath, ExecToolArgs{}, nil, 1)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "tool bg_spawn.sh timed out after 1 seconds") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Read background PID from pidFile
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("failed to read bg.pid: %v", err)
	}
	bgPidStr := strings.TrimSpace(string(pidData))
	bgPid, err := strconv.Atoi(bgPidStr)
	if err != nil {
		t.Fatalf("invalid PID in bg.pid: %q", bgPidStr)
	}

	// Give the OS a tiny slice to process the SIGKILL
	time.Sleep(50 * time.Millisecond)

	// Check if background process is alive via syscall.Kill(bgPid, 0)
	if err := syscall.Kill(bgPid, 0); err == nil {
		t.Errorf("background process %d is still alive, expected it to be killed with process group", bgPid)
		// Clean up leaked process if test failed
		_ = syscall.Kill(bgPid, syscall.SIGKILL)
	}
}

func TestExecuteTool_TimeoutDisabled(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "quick.sh")
	script := "#!/bin/sh\necho quick_done\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	// timeout = -1 disables timeout wrapping
	out, err := executeTool(context.Background(), agentDir, "quick.sh", toolPath, ExecToolArgs{}, nil, -1)
	if err != nil {
		t.Fatalf("unexpected error with timeout disabled (-1): %v", err)
	}
	if !strings.Contains(out, "quick_done") {
		t.Fatalf("expected 'quick_done' in output, got: %q", out)
	}
}

func TestCommandTimeout_Precedence(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"dummy-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// 1. Default (no arg, no env var) -> 900
	_ = os.Unsetenv(EnvCommandTimeoutSeconds)
	fa1, err := LoadFolderAgent(wsDir, "bob", 1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	if fa1.CommandTimeoutSeconds != DefaultCommandTimeoutSeconds {
		t.Errorf("expected default timeout %d, got %d", DefaultCommandTimeoutSeconds, fa1.CommandTimeoutSeconds)
	}

	// 2. Env var precedence when arg is not supplied (or 0)
	t.Setenv(EnvCommandTimeoutSeconds, "42")
	fa2, err := LoadFolderAgent(wsDir, "bob", 1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	if fa2.CommandTimeoutSeconds != 42 {
		t.Errorf("expected env timeout 42, got %d", fa2.CommandTimeoutSeconds)
	}

	// 3. Explicit argument precedence over env var
	fa3, err := LoadFolderAgent(wsDir, "bob", 1, 100)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	if fa3.CommandTimeoutSeconds != 100 {
		t.Errorf("expected explicit timeout 100, got %d", fa3.CommandTimeoutSeconds)
	}

	// 4. Disabling timeout (-1) as explicit arg over env var
	fa4, err := LoadFolderAgent(wsDir, "bob", 1, -1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	if fa4.CommandTimeoutSeconds != -1 {
		t.Errorf("expected disabled timeout -1, got %d", fa4.CommandTimeoutSeconds)
	}
}

func TestRunCommandArgsSchemaD56(t *testing.T) {
	agentDir := t.TempDir()
	toolMap, _, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}

	runCmd, ok := toolMap["run_command"]
	if !ok {
		t.Fatalf("missing run_command in toolMap")
	}

	decler, ok := runCmd.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	if !ok {
		t.Fatalf("run_command does not implement Declaration()")
	}

	decl := decler.Declaration()
	declBytes, err := json.Marshal(decl)
	if err != nil {
		t.Fatalf("failed to marshal decl: %v", err)
	}

	var declMap map[string]any
	if err := json.Unmarshal(declBytes, &declMap); err != nil {
		t.Fatalf("failed to unmarshal decl JSON: %v", err)
	}

	// Schema can be in parameters or parametersJsonSchema
	var paramsMap map[string]any
	if p, ok := declMap["parameters"].(map[string]any); ok {
		paramsMap = p
	} else if p, ok := declMap["parametersJsonSchema"].(map[string]any); ok {
		paramsMap = p
	} else {
		t.Fatalf("neither parameters nor parametersJsonSchema found in decl: %s", string(declBytes))
	}

	props, ok := paramsMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'properties' in schema: %v", paramsMap)
	}

	argsProp, ok := props["args"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'args' property in schema: %v", props)
	}

	// D56: args must be plain "array", not a union like ["null", "array"]
	argsType := argsProp["type"]
	if argsType != "array" {
		t.Errorf("expected 'args' type to be 'array', got %v (%T)", argsType, argsType)
	}

	// Required properties must include "command" and "args"
	reqList, _ := paramsMap["required"].([]any)
	reqSet := make(map[string]bool)
	for _, r := range reqList {
		if s, ok := r.(string); ok {
			reqSet[s] = true
		}
	}
	if !reqSet["command"] {
		t.Errorf("expected 'command' in required, got %v", reqList)
	}
	if !reqSet["args"] {
		t.Errorf("expected 'args' in required, got %v", reqList)
	}
}

func TestDeterministicToolOrderingD57(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	expectedOrder := []string{
		"create_scratchpad",
		"delete_scratchpad",
		"get_scratchpad",
		"list_scratchpads",
		"list_skill_extra",
		"load_skill",
		"load_skill_extra",
		"run_command",
		"run_skill_script",
		"search_scratchpad",
	}

	for iter := 0; iter < 10; iter++ {
		var receivedToolNames []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bodyBytes, _ := io.ReadAll(r.Body)
			var bodyMap map[string]any
			_ = json.Unmarshal(bodyBytes, &bodyMap)
			if rawTools, ok := bodyMap["tools"].([]any); ok {
				for _, tAny := range rawTools {
					if tMap, ok := tAny.(map[string]any); ok {
						if fnMap, ok := tMap["function"].(map[string]any); ok {
							if name, ok := fnMap["name"].(string); ok {
								receivedToolNames = append(receivedToolNames, name)
							}
						}
					}
				}
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   "dummy-model",
				"choices": []any{
					map[string]any{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "ok",
						},
						"finish_reason": "stop",
					},
				},
			})
		}))

		cfgJSON := fmt.Sprintf(`{"model":"dummy-model","endpoint":%q}`, srv.URL)
		if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(cfgJSON), 0644); err != nil {
			srv.Close()
			t.Fatalf("failed to write runtime.json: %v", err)
		}

		sessionJSONL := filepath.Join(agentDir, "session.jsonl")
		userTurn := `{"role":"user","parts":[{"text":"hello"}]}` + "\n"
		if err := os.WriteFile(sessionJSONL, []byte(userTurn), 0644); err != nil {
			srv.Close()
			t.Fatalf("failed to write session.jsonl: %v", err)
		}

		fa, err := LoadFolderAgent(wsDir, "bob", 1)
		if err != nil {
			srv.Close()
			t.Fatalf("LoadFolderAgent iter %d failed: %v", iter, err)
		}

		_, err = fa.GenerateTurn(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("iter %d: GenerateTurn failed: %v", iter, err)
		}

		if len(receivedToolNames) != len(expectedOrder) {
			t.Fatalf("iter %d: expected %d tools on wire, got %d: %v", iter, len(expectedOrder), len(receivedToolNames), receivedToolNames)
		}

		for i := range expectedOrder {
			if receivedToolNames[i] != expectedOrder[i] {
				t.Fatalf("iter %d: tool order mismatch at index %d: got %q, expected %q (full received: %v)", iter, i, receivedToolNames[i], expectedOrder[i], receivedToolNames)
			}
		}
	}
}

func TestExecuteTool_CapturePath_NonExistentMacroDoesNotFail(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "large_macro.sh")

	// Output > ScratchpadOutputThreshold (4000 bytes) containing a macro referencing a non-existent ID
	largePayload := strings.Repeat("hello world line\n", 300) + `<SCRATCHPAD_DATA id="dead" />` + "\n"
	script := fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%sEOF\n", largePayload)
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	out, err := executeTool(context.Background(), agentDir, "large_macro.sh", toolPath, ExecToolArgs{}, nil)
	if err != nil {
		t.Fatalf("expected executeTool to succeed without expanding macro, got error: %v", err)
	}

	// Output should reference scratchpad entry
	if !strings.Contains(out, "<STDOUT><SCRATCHPAD_DATA id=") {
		t.Fatalf("expected output over threshold to be stored in scratchpad, got: %s", out)
	}

	// Read stored scratchpad entry and verify macro was preserved verbatim
	items, _, _, err := ListScratchpads(agentDir)
	if err != nil || len(items) == 0 {
		t.Fatalf("failed to list scratchpads: %v", err)
	}
	content, err := readScratchpadRaw(agentDir, items[0].ID, nil, nil)
	if err != nil {
		t.Fatalf("failed to read raw scratchpad: %v", err)
	}
	if !strings.Contains(content, `<SCRATCHPAD_DATA id="dead" />`) {
		t.Fatalf("expected literal macro in stored scratchpad, got: %s", content)
	}
}

func TestExecuteTool_CapturePath_LiveMacroDoesNotSplice(t *testing.T) {
	agentDir := t.TempDir()

	// Create a live scratchpad entry
	secretText := "TOP_SECRET_VERBATIM_CHECK"
	liveEntry, err := CreateScratchpad(agentDir, secretText, "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	toolPath := filepath.Join(agentDir, "live_macro.sh")
	macroTag := fmt.Sprintf(`<SCRATCHPAD_DATA id=%q />`, liveEntry.ID)
	largePayload := strings.Repeat("padding text line\n", 300) + macroTag + "\n"
	script := fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%sEOF\n", largePayload)
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	out, err := executeTool(context.Background(), agentDir, "live_macro.sh", toolPath, ExecToolArgs{}, nil)
	if err != nil {
		t.Fatalf("expected executeTool to succeed, got error: %v", err)
	}

	if !strings.Contains(out, "<STDOUT><SCRATCHPAD_DATA id=") {
		t.Fatalf("expected output over threshold to be stored in scratchpad, got: %s", out)
	}

	// Find the new scratchpad entry created for tool's output
	items, _, _, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("failed to list scratchpads: %v", err)
	}
	var toolEntryID string
	for _, it := range items {
		if it.ID != liveEntry.ID {
			toolEntryID = it.ID
			break
		}
	}
	if toolEntryID == "" {
		t.Fatalf("failed to find captured tool scratchpad entry")
	}

	content, err := readScratchpadRaw(agentDir, toolEntryID, nil, nil)
	if err != nil {
		t.Fatalf("failed to read raw scratchpad: %v", err)
	}

	// Must contain the literal tag without splicing the live entry's secret text
	if !strings.Contains(content, macroTag) {
		t.Fatalf("expected literal macro tag %q in stored scratchpad, got: %s", macroTag, content)
	}
	if strings.Contains(content, secretText) {
		t.Fatalf("live scratchpad content was incorrectly spliced into captured output: %s", content)
	}
}

func TestExecuteTool_FailureOverThreshold_PreservedInScratchpad(t *testing.T) {
	agentDir := t.TempDir()
	toolPath := filepath.Join(agentDir, "fail_large.sh")

	largePayload := strings.Repeat("failure output line before exit\n", 300)
	script := fmt.Sprintf("#!/bin/sh\ncat << 'EOF'\n%sEOF\necho \"error on stderr\" >&2\nexit 1\n", largePayload)
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	out, err := executeTool(context.Background(), agentDir, "fail_large.sh", toolPath, ExecToolArgs{}, nil)
	if err == nil {
		t.Fatalf("expected error from failing tool, got nil")
	}

	// Headline reason must be exit status
	if !strings.Contains(err.Error(), "tool fail_large.sh failed: exit status 1") {
		t.Fatalf("unexpected headline error: %q", err.Error())
	}
	// Output blocks in error must reference scratchpad entry for large stdout
	if !strings.Contains(err.Error(), "<STDOUT><SCRATCHPAD_DATA id=") {
		t.Fatalf("expected error to contain scratchpad reference for stdout: %q", err.Error())
	}
	// Error must contain preserved stderr
	if !strings.Contains(err.Error(), "<STDERR>") || !strings.Contains(err.Error(), "error on stderr") {
		t.Fatalf("expected error to contain stderr block: %q", err.Error())
	}
	// Result payload must also preserve the output blocks
	if !strings.Contains(out, "<STDOUT><SCRATCHPAD_DATA id=") {
		t.Fatalf("expected out payload to contain scratchpad reference for stdout: %q", out)
	}

	// Verify scratchpad entry actually exists on disk
	items, _, _, listErr := ListScratchpads(agentDir)
	if listErr != nil || len(items) == 0 {
		t.Fatalf("expected scratchpad entry to be created for failed tool: %v", listErr)
	}
	raw, readErr := readScratchpadRaw(agentDir, items[0].ID, nil, nil)
	if readErr != nil {
		t.Fatalf("failed to read created scratchpad: %v", readErr)
	}
	if !strings.Contains(raw, "failure output line before exit") {
		t.Fatalf("scratchpad content missing tool output: %q", raw)
	}
}
