package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	DefaultCommandTimeoutSeconds = 900
	EnvCommandTimeoutSeconds     = "WACKYPUB_COMMAND_TIMEOUT_SECONDS"
)

type ExecToolArgs struct {
	Args  []string          `json:"args,omitempty" jsonschema_description:"List of CLI command line arguments passed positionally to the tool (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
	Env   map[string]string `json:"env,omitempty" jsonschema_description:"Key-value object map of environment variables to set for the tool invocation (not macro-expanded)"`
	Stdin string            `json:"stdin,omitempty" jsonschema_description:"Optional stdin template string to pipe into the command (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
}

// BuildFolderAgentTools constructs ADK functiontool instances for built-in tools (create_scratchpad, get_scratchpad, list_scratchpads, search_scratchpad, delete_scratchpad, diff_scratchpad)
// and a single generic run_command tool covering executables discovered under <agent_dir>/tools/.
func BuildFolderAgentTools(agentDir string, commandTimeoutSeconds ...int) (map[string]tool.Tool, []*genai.FunctionDeclaration, error) {
	return BuildFolderAgentToolsWithA2A(agentDir, nil, commandTimeoutSeconds...)
}

// BuildFolderAgentToolsWithA2A constructs ADK functiontool instances, injecting a2aMeta directly into spawned child process environments (D59).
func BuildFolderAgentToolsWithA2A(agentDir string, a2aMeta *A2AMetadata, commandTimeoutSeconds ...int) (map[string]tool.Tool, []*genai.FunctionDeclaration, error) {
	toolMap := make(map[string]tool.Tool)
	var decls []*genai.FunctionDeclaration

	timeoutSeconds := DefaultCommandTimeoutSeconds
	if len(commandTimeoutSeconds) > 0 {
		timeoutSeconds = commandTimeoutSeconds[0]
	}

	addTool := func(t tool.Tool) {
		toolMap[t.Name()] = t
		if decler, ok := t.(interface {
			Declaration() *genai.FunctionDeclaration
		}); ok {
			decls = append(decls, decler.Declaration())
		}
	}

	if err := registerScratchpadTools(agentDir, addTool); err != nil {
		return nil, nil, err
	}
	if err := registerCommandTools(agentDir, a2aMeta, timeoutSeconds, addTool); err != nil {
		return nil, nil, err
	}
	if err := registerSkillTools(agentDir, a2aMeta, timeoutSeconds, addTool); err != nil {
		return nil, nil, err
	}

	return toolMap, decls, nil
}

// prepareToolArgs validates scratchpad references, expands macros, and checks arg size limits.
func prepareToolArgs(agentDir string, rawArgs []string) ([]string, []string, error) {
	var warnings []string
	cmdArgs := make([]string, len(rawArgs))
	for i, rawArg := range rawArgs {
		// Check for binary scratchpad references in args (D48: args reject .dat entries outright)
		if strings.Contains(rawArg, "<SCRATCHPAD_DATA") {
			matches := scratchpadMacroRegex.FindAllString(rawArg, -1)
			for _, m := range matches {
				if strings.HasPrefix(m, "\\") {
					continue
				}
				idMatch := macroIDRegex.FindStringSubmatch(m)
				if len(idMatch) >= 2 {
					id := idMatch[1]
					_, _, isBinary, err := findScratchpadFile(agentDir, id)
					if err == nil && isBinary {
						return nil, nil, fmt.Errorf("cannot pass binary scratchpad entry %q in command args", id)
					}
				}
			}
		}

		expanded, w, err := ExpandScratchpadMacros(agentDir, rawArg)
		if err != nil {
			return nil, nil, err
		}
		for _, warn := range w {
			found := false
			for _, existing := range warnings {
				if existing == warn {
					found = true
					break
				}
			}
			if !found {
				warnings = append(warnings, warn)
			}
		}
		if len(expanded) > MaxExpandedArgBytes {
			return nil, nil, fmt.Errorf("expanded argument exceeds 500000 bytes (was %d) - use stdin/stdout scratchpad redirection instead", len(expanded))
		}
		cmdArgs[i] = expanded
	}
	return cmdArgs, warnings, nil
}

// resolveExecutableToolPath evaluates symlinks and resolves relative tool paths.
func resolveExecutableToolPath(toolPath string) string {
	absToolPath, err := filepath.Abs(toolPath)
	if err != nil {
		absToolPath = toolPath
	}
	if evalPath, err := filepath.EvalSymlinks(absToolPath); err == nil {
		absToolPath = evalPath
	}
	return absToolPath
}

// toolTimeoutContext creates a cancellation context for tool execution when timeout > 0.
func toolTimeoutContext(ctx context.Context, timeout int) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	return ctx, func() {}
}

// prepareToolStdin parses and sets up stdin for tool execution, handling binary scratchpad redirection or macro expansion.
func prepareToolStdin(agentDir string, rawStdin string, warnings []string) (io.Reader, *os.File, []string, error) {
	if rawStdin == "" {
		return nil, nil, warnings, nil
	}
	trimmedStdin := strings.TrimSpace(rawStdin)
	// Check if stdin contains binary scratchpad references per D48
	if strings.Contains(trimmedStdin, "<SCRATCHPAD_DATA") {
		var binaryID string
		matches := scratchpadMacroRegex.FindAllString(trimmedStdin, -1)
		for _, m := range matches {
			if strings.HasPrefix(m, "\\") {
				continue
			}
			idMatch := macroIDRegex.FindStringSubmatch(m)
			if len(idMatch) >= 2 {
				id := idMatch[1]
				_, _, isBinary, err := findScratchpadFile(agentDir, id)
				if err == nil && isBinary {
					binaryID = id
					break
				}
			}
		}

		if binaryID != "" {
			// Exact match check: stdin must be ONLY this single macro reference
			isExact := false
			if len(matches) == 1 && scratchpadMacroRegex.FindString(trimmedStdin) == trimmedStdin {
				isExact = true
			}

			if !isExact {
				return nil, nil, nil, fmt.Errorf("cannot mix binary scratchpad entry %q with text in stdin", binaryID)
			}

			// Check for pagination/escaping attributes on binary reference per D48
			if macroSkipLinesRegex.MatchString(trimmedStdin) || macroNumLinesRegex.MatchString(trimmedStdin) || macroJsonEscapeRegex.MatchString(trimmedStdin) {
				return nil, nil, nil, fmt.Errorf("cannot use pagination or escaping attributes (skip_lines, num_lines, json_escape) with binary scratchpad entry %q", binaryID)
			}

			filePath, _, _, err := findScratchpadFile(agentDir, binaryID)
			if err != nil {
				return nil, nil, nil, err
			}
			f, err := os.Open(filePath)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to open binary scratchpad entry %q: %w", binaryID, err)
			}
			return f, f, warnings, nil
		}
	}

	expandedStdin, w, err := ExpandScratchpadMacros(agentDir, rawStdin)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, warn := range w {
		found := false
		for _, existing := range warnings {
			if existing == warn {
				found = true
				break
			}
		}
		if !found {
			warnings = append(warnings, warn)
		}
	}
	return strings.NewReader(expandedStdin), nil, warnings, nil
}

// formatToolError formats the error and warning block for a failed tool execution.
func formatToolError(err error, execCtx context.Context, toolName string, timeout int, output string, warnings []string, stdoutBytes, stderrBytes []byte) error {
	var headline string
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		headline = fmt.Sprintf("tool %s timed out after %d seconds", toolName, timeout)
	} else {
		headline = fmt.Sprintf("tool %s failed: %s", toolName, err.Error())
	}

	errOut := output
	if len(warnings) > 0 {
		warningBlock := fmt.Sprintf("<WARNING>\n%s\n</WARNING>", strings.Join(warnings, "\n"))
		errOut = warningBlock + errOut
	}

	if len(stdoutBytes) > 0 || len(stderrBytes) > 0 || len(warnings) > 0 {
		return fmt.Errorf("%s\n%s", headline, errOut)
	}
	return fmt.Errorf("%s", headline)
}

func executeTool(ctx context.Context, agentDir string, toolName string, toolPath string, args ExecToolArgs, a2aMeta *A2AMetadata, timeoutSeconds ...int) (string, []string, error) {
	timeout := DefaultCommandTimeoutSeconds
	if len(timeoutSeconds) > 0 {
		timeout = timeoutSeconds[0]
	}

	cmdArgs, warnings, err := prepareToolArgs(agentDir, args.Args)
	if err != nil {
		return "", nil, err
	}

	absToolPath := resolveExecutableToolPath(toolPath)

	execCtx, cancel := toolTimeoutContext(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, absToolPath, cmdArgs...)
	cmd.Dir = agentDir
	baseEnv := os.Environ()
	// Harness-owned variables (A2A trust channel, PATH/HOME/TMPDIR/LANG) are captured here so a
	// model-supplied or .env-supplied value can never win the last-wins race (oracle audit A2).
	harnessEnv := harnessEnvFromBase(baseEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	// D59: Propagate A2A Metadata and legacy CallChain to the child process environment. These land
	// in harnessEnv rather than straight onto cmd.Env so they stay non-overridable by args.Env below.
	if a2aMeta != nil {
		denseJSON, err := a2aMeta.Encode()
		if err == nil && denseJSON != "" {
			harnessEnv[Agent2AgentEnvVar] = denseJSON
		}
		if len(a2aMeta.CallChain) > 0 {
			harnessEnv[CallChainEnvVar] = strings.Join(a2aMeta.CallChain, ",")
		}
	}

	dotEnv, err := LoadAgentDotEnv(agentDir)
	if err != nil {
		return "", nil, fmt.Errorf("failed to load agent .env: %w", err)
	}

	// Order: base environment, then .env, then the model-supplied args.Env, with the harness-owned
	// entries forced on top last. args.Env still wins for every variable the harness does not own,
	// which is the precedence TestExecuteTool_DotEnvInjectionAndPrecedence pins.
	cmd.Env = childEnv(baseEnv, harnessEnv, dotEnv, args.Env)

	stdinReader, stdinFile, warnings, err := prepareToolStdin(agentDir, args.Stdin, warnings)
	if err != nil {
		return "", nil, err
	}
	if stdinReader != nil {
		cmd.Stdin = stdinReader
	}
	if stdinFile != nil {
		defer stdinFile.Close()
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Commit before dispatch, in this process, so any A2A hop this call
	// makes carries a workspace_revision reflecting everything up to and
	// including this call - see D35's "Revised again" section for why this
	// can't live inside ValidateAgentTarget (runs in the spawned child, not
	// here) and why it applies uniformly to every run_command call rather
	// than only ones that happen to be cross-agent.
	wsDir := filepath.Dir(agentDir)
	agentID := filepath.Base(agentDir)
	_ = CommitWorkspaceEvent(wsDir, agentID, fmt.Sprintf("tool call (%s)", toolName))

	err = cmd.Run()
	stdoutBytes := stdout.Bytes()
	stderrBytes := stderr.Bytes()

	output, buildErr := buildOutputBlocks(agentDir, stdoutBytes, stderrBytes)
	if buildErr != nil {
		if err != nil {
			return "", warnings, fmt.Errorf("tool %s failed: %v (failed to process output: %w)", toolName, err, buildErr)
		}
		return "", warnings, buildErr
	}

	if err != nil {
		return output, warnings, formatToolError(err, execCtx, toolName, timeout, output, warnings, stdoutBytes, stderrBytes)
	}

	return output, warnings, nil
}

// buildOutputBlocks formats captured stdout and stderr into blocks (<STDOUT>, <STDERR>).
// For text exceeding ScratchpadOutputThreshold, stores text verbatim using createScratchpadRaw (no macro expansion per D80).
// For binary content, stores verbatim using CreateBinaryScratchpad (D48).
func buildOutputBlocks(agentDir string, stdoutBytes, stderrBytes []byte) (string, error) {
	var stdoutBlock string
	if isBinary, mimeType := DetectMediaType(stdoutBytes); isBinary {
		// D48: Binary stdout is always routed to a .dat scratchpad entry regardless of size
		entry, err := CreateBinaryScratchpad(agentDir, stdoutBytes, "run_command", mimeType)
		if err != nil {
			return "", fmt.Errorf("failed to create binary stdout scratchpad entry: %w", err)
		}
		stdoutBlock = fmt.Sprintf("<STDOUT><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"0\" mime=%q /></STDOUT>", entry.ID, entry.Size, mimeType)
	} else if len(stdoutBytes) > ScratchpadOutputThreshold {
		entry, err := createScratchpadRaw(agentDir, string(stdoutBytes), "run_command")
		if err != nil {
			return "", fmt.Errorf("failed to create stdout scratchpad entry: %w", err)
		}
		stdoutBlock = fmt.Sprintf("<STDOUT><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"%d\" /></STDOUT>", entry.ID, entry.Size, entry.Lines)
	} else if len(stdoutBytes) > 0 {
		stdoutBlock = fmt.Sprintf("<STDOUT>\n%s\n</STDOUT>", string(stdoutBytes))
	} else {
		stdoutBlock = "<STDOUT></STDOUT>"
	}

	var stderrBlock string
	if isBinary, mimeType := DetectMediaType(stderrBytes); isBinary {
		// D48: Binary stderr is always routed to a .dat scratchpad entry regardless of size
		entry, err := CreateBinaryScratchpad(agentDir, stderrBytes, "run_command", mimeType)
		if err != nil {
			return "", fmt.Errorf("failed to create binary stderr scratchpad entry: %w", err)
		}
		stderrBlock = fmt.Sprintf("<STDERR><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"0\" mime=%q /></STDERR>", entry.ID, entry.Size, mimeType)
	} else if len(stderrBytes) > ScratchpadOutputThreshold {
		entry, err := createScratchpadRaw(agentDir, string(stderrBytes), "run_command")
		if err != nil {
			return "", fmt.Errorf("failed to create stderr scratchpad entry: %w", err)
		}
		stderrBlock = fmt.Sprintf("<STDERR><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"%d\" /></STDERR>", entry.ID, entry.Size, entry.Lines)
	} else if len(stderrBytes) > 0 {
		stderrBlock = fmt.Sprintf("<STDERR>\n%s\n</STDERR>", string(stderrBytes))
	}

	return stdoutBlock + stderrBlock, nil
}

// ContinuationReason defines the reason for an automatic continuation turn under D88.
type ContinuationReason string

const (
	ContinuationNone          ContinuationReason = ""
	ContinuationDeferredImage ContinuationReason = "deferred_image"
	ContinuationCompactedBail ContinuationReason = "compacted_bail"

	// DefaultMaxAutoContinuations is the budget guard cap (2) for standard sessions (D88).
	DefaultMaxAutoContinuations = 2
	// DefaultMaxAutoContinuationsA2A is the budget guard cap (1) for A2A sessions (D88).
	DefaultMaxAutoContinuationsA2A = 1
)

// FolderAgent encapsulates an agent loaded from a folder environment (<ws_dir>/<agent_id>).
type FolderAgent struct {
	AgentID       string
	AgentDir      string
	DotEnv        map[string]string
	RuntimeConfig *RuntimeConfig
	SystemPrompt  string
	MemoryPrompt  string
	Model         model.LLM
	ADKAgent      agent.Agent
	// CompactionAgent is the ADK agent used for the compaction turn only (D45 routes
	// compaction through the real runner for cache-prefix identity). It carries the same
	// tools/declarations as ADKAgent but with a BeforeToolCallback that denies all tool
	// INVOCATION: the compaction model sees the agent's capabilities but cannot execute any.
	// CompactionToolDenials points at the counter the deny callback increments (shared with
	// the compaction-scoped agent's BeforeToolCallback) so callers can read the denial count
	// after a compaction run and include it in the post-compact payload.
	CompactionAgent         agent.Agent
	CompactionToolDenials   *int64
	MaxToolTurns            int
	CommandTimeoutSeconds   int
	A2AMeta                 *A2AMetadata
	UsageTracker            *TurnUsageTracker
	HookEnv                 map[string]string
	Tools                   []tool.Tool
	MaxAutoContinuations    *int
	DisableAutoContinuation bool
}

// LoadFolderAgent loads and initializes an agent from <wsDir>/<agentID>.
func LoadFolderAgent(wsDir string, agentID string, maxToolTurns int, commandTimeoutSeconds ...int) (*FolderAgent, error) {
	return LoadFolderAgentWithA2A(wsDir, agentID, nil, maxToolTurns, commandTimeoutSeconds...)
}

// LoadFolderAgentWithA2A loads and initializes an agent with explicit A2AMetadata context (D59).
func LoadFolderAgentWithA2A(wsDir string, agentID string, a2aMeta *A2AMetadata, maxToolTurns int, commandTimeoutSeconds ...int) (*FolderAgent, error) {
	return LoadFolderAgentWithHookEnv(wsDir, agentID, a2aMeta, nil, maxToolTurns, commandTimeoutSeconds...)
}

// LoadFolderAgentWithHookEnv loads and initializes an agent with explicit A2AMetadata context and hook environment mutations (D87).
// LoadFolderAgentWithHookEnv loads and initializes an agent with explicit A2AMetadata context
// and hook environment mutations (D87), resolving the runtime.json fallback chain to its
// primary level. The fallback machinery re-invokes loadFolderAgentFromRuntime per level at
// turn setup when the primary backend fails with a qualifying error.
func LoadFolderAgentWithHookEnv(wsDir string, agentID string, a2aMeta *A2AMetadata, hookEnv map[string]string, maxToolTurns int, commandTimeoutSeconds ...int) (*FolderAgent, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	agentDir := filepath.Join(wsDir, agentID)
	if !pathExists(agentDir) {
		return nil, fmt.Errorf("agent directory %s does not exist", agentDir)
	}

	// 0. Load .env file
	dotEnv, err := LoadAgentDotEnv(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent .env: %w", err)
	}

	// 1. Load runtime.json (primary level of the fallback chain)
	runtimeCfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime config: %w", err)
	}

	return loadFolderAgentFromRuntime(agentDir, wsDir, agentID, a2aMeta, hookEnv, runtimeCfg, dotEnv, maxToolTurns, commandTimeoutSeconds...)
}

// loadFolderAgentFromRuntime initializes an agent from an already-resolved runtime config
// (one level of the fallback chain). Each level is fully self-describing: the model
// constructor runs per level, so a fallback may be a different provider entirely.
func loadFolderAgentFromRuntime(agentDir, wsDir, agentID string, a2aMeta *A2AMetadata, hookEnv map[string]string, runtimeCfg *RuntimeConfig, dotEnv map[string]string, maxToolTurns int, commandTimeoutSeconds ...int) (*FolderAgent, error) {
	if runtimeCfg == nil {
		return nil, fmt.Errorf("runtime config cannot be nil for agent %s", agentID)
	}

	// 2. Render AGENTS.md (expanding @<FILE_PATH> macros)
	expandedPrompt, err := RenderAgentSystemPrompt(wsDir, agentID, hookEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to render system prompt for agent %s: %w", agentID, err)
	}

	// 3. Read MEMORY.md
	memoryContent, err := ReadMemoryFile(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read memory file for agent %s: %w", agentID, err)
	}

	// 4. Initialize LLM Model adapter
	llmModel, err := NewModelForRuntime(context.Background(), runtimeCfg, agentID)
	if err != nil {
		return nil, err
	}

	resolvedTimeout := DefaultCommandTimeoutSeconds
	if len(commandTimeoutSeconds) > 0 && commandTimeoutSeconds[0] != 0 {
		resolvedTimeout = commandTimeoutSeconds[0]
	} else if envVal := os.Getenv(EnvCommandTimeoutSeconds); envVal != "" {
		if val, err := strconv.Atoi(envVal); err == nil {
			resolvedTimeout = val
		}
	}

	// 5. Build ADK functiontools for agent
	adkToolsMap, _, err := BuildFolderAgentToolsWithA2A(agentDir, a2aMeta, resolvedTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to build agent tools: %w", err)
	}
	var toolsList []tool.Tool
	for _, t := range adkToolsMap {
		toolsList = append(toolsList, t)
	}
	sort.Slice(toolsList, func(i, j int) bool {
		return toolsList[i].Name() < toolsList[j].Name()
	})

	if maxToolTurns <= 0 {
		maxToolTurns = DefaultMaxToolTurns
	}

	var maxAutoCont *int
	disableAutoCont := false
	if runtimeCfg != nil {
		if runtimeCfg.MaxAutoContinuations != nil {
			maxAutoCont = runtimeCfg.MaxAutoContinuations
		}
		if runtimeCfg.DisableAutoContinuation {
			disableAutoCont = true
		}
	}

	// 6. Construct ADK llmagent with agentID, expanded prompt instruction, maxToolTurns cap, runtimeCfg, model, tracker, and tools
	tracker := &TurnUsageTracker{
		DisableAutoContinuation: disableAutoCont,
	}
	ag, err := BuildADKAgentWithConfigAndTracker(agentID, expandedPrompt, maxToolTurns, runtimeCfg, llmModel, agentDir, tracker, toolsList...)
	if err != nil {
		return nil, fmt.Errorf("failed to build ADK agent for folder agent %s: %w", agentID, err)
	}

	// Build the compaction-scoped agent with tool invocation denied. Zero request bytes
	// change (same declarations in the payload); only the impl path is vetoed.
	var compactionDenials int64
	compactionAgent, err := BuildADKAgentWithConfigAndTrackerForCompaction(agentID, expandedPrompt, maxToolTurns, runtimeCfg, llmModel, agentDir, tracker, &compactionDenials, toolsList...)
	if err != nil {
		return nil, fmt.Errorf("failed to build compaction agent for folder agent %s: %w", agentID, err)
	}

	return &FolderAgent{
		AgentID:                 agentID,
		AgentDir:                agentDir,
		DotEnv:                  dotEnv,
		RuntimeConfig:           runtimeCfg,
		SystemPrompt:            expandedPrompt,
		MemoryPrompt:            memoryContent,
		Model:                   llmModel,
		ADKAgent:                ag,
		CompactionAgent:         compactionAgent,
		CompactionToolDenials:   &compactionDenials,
		MaxToolTurns:            maxToolTurns,
		CommandTimeoutSeconds:   resolvedTimeout,
		A2AMeta:                 a2aMeta,
		UsageTracker:            tracker,
		HookEnv:                 hookEnv,
		Tools:                   toolsList,
		MaxAutoContinuations:    maxAutoCont,
		DisableAutoContinuation: disableAutoCont,
	}, nil
}

// refreshSystemPromptAndAgent re-renders the agent's system prompt and rebuilds its ADKAgent
// when MEMORY.md was rewritten during compaction (D88).
func (fa *FolderAgent) refreshSystemPromptAndAgent() error {
	wsDir := filepath.Dir(fa.AgentDir)
	var hookEnvs []map[string]string
	if fa.HookEnv != nil {
		hookEnvs = append(hookEnvs, fa.HookEnv)
	}
	newPrompt, err := RenderAgentSystemPrompt(wsDir, fa.AgentID, hookEnvs...)
	if err != nil {
		return err
	}
	fa.SystemPrompt = newPrompt
	if fa.Model != nil {
		ag, err := BuildADKAgentWithConfigAndTracker(fa.AgentID, fa.SystemPrompt, fa.MaxToolTurns, fa.RuntimeConfig, fa.Model, fa.AgentDir, fa.UsageTracker, fa.Tools...)
		if err != nil {
			return err
		}
		fa.ADKAgent = ag

		compactionDenials := int64(0)
		ca, err := BuildADKAgentWithConfigAndTrackerForCompaction(fa.AgentID, fa.SystemPrompt, fa.MaxToolTurns, fa.RuntimeConfig, fa.Model, fa.AgentDir, fa.UsageTracker, &compactionDenials, fa.Tools...)
		if err != nil {
			return err
		}
		fa.CompactionAgent = ca
		fa.CompactionToolDenials = &compactionDenials
	}
	return nil
}

// readMemoryForChangeDetection reads MEMORY.md for the prompt-freshness detector. The bool
// reports whether the read succeeded; on error the caller must keep lastMemory at its last
// known value - conflating unreadable with empty would make an I/O error look like the
// operator cleared memory (or two errors in a row look like no change).
func readMemoryForChangeDetection(agentDir string) (string, bool) {
	content, err := ReadMemoryFile(agentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to read MEMORY.md for change detection at %s: %v\n", filepath.Join(agentDir, "MEMORY.md"), err)
		return "", false
	}
	return content, true
}

// loadRuntimeCfgForGating loads runtime.json for image-deferral gating decisions. An
// absent runtime.json is fine (defaults apply); a parse failure logs the path so a broken
// config that silently disables image deferral is diagnosable.
func loadRuntimeCfgForGating(agentDir string) *RuntimeConfig {
	runtimeCfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: failed to load runtime config at %s for image gating: %v\n", filepath.Join(agentDir, "runtime.json"), err)
		}
		return nil
	}
	return runtimeCfg
}

// persistTurn appends a session turn and records the accompanying workspace audit event,
// returning the first error with context about which step failed. The turn cannot be
// considered durable unless both writes succeed, so strict call sites (failure records and
// continuation sentinels) surface the error instead of discarding it.
func persistTurn(agentDir, wsDir, agentID string, turn *genai.Content, eventLabel string) error {
	if err := AppendSessionContent(agentDir, turn); err != nil {
		return fmt.Errorf("appending turn (%s) for agent %s: %w", eventLabel, agentID, err)
	}
	if err := CommitWorkspaceEvent(wsDir, agentID, eventLabel); err != nil {
		return fmt.Errorf("committing workspace event (%s) for agent %s: %w", eventLabel, agentID, err)
	}
	return nil
}

// commitEventBestEffort records a workspace audit event, logging failures instead of
// discarding them. Used for audit-trail events where a lost entry is tolerable but must
// still be visible in the log rather than silent.
func commitEventBestEffort(wsDir, agentID, eventLabel string) {
	if err := CommitWorkspaceEvent(wsDir, agentID, eventLabel); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record workspace event %q for agent %s: %v\n", eventLabel, agentID, err)
	}
}

// appendDeferredImages decodes, normalizes, and appends deferred image user turns per D49/D88.
// Returns the number of successfully appended <IMAGE> turns.
func (fa *FolderAgent) appendDeferredImages(wsDir string, deferredScratchpadIDs []string) int {
	if fa.RuntimeConfig == nil || fa.RuntimeConfig.MaxImageDimension <= 0 {
		return 0
	}
	validCount := 0
	for _, spID := range deferredScratchpadIDs {
		filePath, _, isBinary, err := findScratchpadFile(fa.AgentDir, spID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to find deferred scratchpad %q for agent %q: %v\n", spID, fa.AgentID, err)
			failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to load deferred image from scratchpad '%s': %v</IMAGE_ERROR>", spID, err), "user")
			if err := persistTurn(fa.AgentDir, wsDir, fa.AgentID, failTurn, "user (deferred image error)"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record deferred-image failure turn for agent %s: %v\n", fa.AgentID, err)
			}
			continue
		}
		if !isBinary {
			fmt.Fprintf(os.Stderr, "Warning: deferred scratchpad %q for agent %q is not binary data\n", spID, fa.AgentID)
			failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to load deferred image from scratchpad '%s': entry is not binary image data</IMAGE_ERROR>", spID), "user")
			if err := persistTurn(fa.AgentDir, wsDir, fa.AgentID, failTurn, "user (deferred image error)"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record deferred-image failure turn for agent %s: %v\n", fa.AgentID, err)
			}
			continue
		}
		imgData, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to read deferred scratchpad file %s for agent %q: %v\n", filePath, fa.AgentID, err)
			failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to read deferred image from scratchpad '%s': %v</IMAGE_ERROR>", spID, err), "user")
			if err := persistTurn(fa.AgentDir, wsDir, fa.AgentID, failTurn, "user (deferred image error)"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record deferred-image failure turn for agent %s: %v\n", fa.AgentID, err)
			}
			continue
		}
		jpegBytes, mimeType, err := NormalizeAndResizeImage(bytes.NewReader(imgData), fa.RuntimeConfig.MaxImageDimension)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to decode/resize deferred image from scratchpad %q for agent %q: %v\n", spID, fa.AgentID, err)
			failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to process deferred image from scratchpad '%s': %v</IMAGE_ERROR>", spID, err), "user")
			if err := persistTurn(fa.AgentDir, wsDir, fa.AgentID, failTurn, "user (deferred image error)"); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record deferred-image failure turn for agent %s: %v\n", fa.AgentID, err)
			}
			continue
		}
		turn := &genai.Content{
			Role: "user",
			Parts: []*genai.Part{
				{Text: fmt.Sprintf("<IMAGE>The following image is stored in scratchpad '%s'</IMAGE>", spID)},
				{
					InlineData: &genai.Blob{
						MIMEType: mimeType,
						Data:     jpegBytes,
					},
				},
			},
		}
		if err := persistTurn(fa.AgentDir, wsDir, fa.AgentID, turn, "user (deferred image)"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to append deferred image turn for agent %s: %v\n", fa.AgentID, err)
			continue
		}
		validCount++
	}
	return validCount
}

// checkPostTurnCompaction runs post-turn compaction if real token usage or estimated usage exceeds threshold (D68, D88).
func (fa *FolderAgent) checkPostTurnCompaction(ctx context.Context, wsDir string) {
	if fa.RuntimeConfig != nil && fa.RuntimeConfig.ContextWindow > 0 {
		compactCfg, err := LoadCompactConfig(fa.AgentDir)
		overheadPct := DefaultCompactionOverheadPct
		if err == nil && compactCfg != nil {
			if compactCfg.CompactOverheadPct >= 0 && compactCfg.CompactOverheadPct < 100 {
				overheadPct = compactCfg.CompactOverheadPct
			}
		}
		threshold := int(float64(fa.RuntimeConfig.ContextWindow) * (1.0 - (overheadPct / 100.0)))

		var usedTokens int
		if fa.UsageTracker != nil && (fa.UsageTracker.LastTotalTokens > 0 || fa.UsageTracker.LastPromptTokens > 0) {
			if fa.UsageTracker.LastTotalTokens > 0 {
				usedTokens = int(fa.UsageTracker.LastTotalTokens)
			} else {
				usedTokens = int(fa.UsageTracker.LastPromptTokens)
			}
			// D93: Persist real turn usage for context inspection and cold-start compaction.
			// A silent failure here makes the next process fall back to EstimateTokens, which runs
			// 26-39% low, so the compaction warning fires late with no way to tell it happened.
			if err := WriteLastUsage(fa.AgentDir, &LastUsageRecord{
				PromptTokens:     fa.UsageTracker.LastPromptTokens,
				CandidatesTokens: fa.UsageTracker.LastCandidatesTokens,
				TotalTokens:      fa.UsageTracker.LastTotalTokens,
				Timestamp:        time.Now(),
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to persist usage record for agent %s (compaction estimate will fall back to token estimation): %v\n", fa.AgentID, err)
			}
		} else {
			if curTurns, err := ReadSessionTurns(fa.AgentDir); err == nil {
				usedTokens = EstimateTokens(curTurns, fa.RuntimeConfig.PreserveThinking)
			}
		}

		if usedTokens >= threshold {
			if fa.UsageTracker != nil {
				fa.UsageTracker.Reset()
			}
			_, err = CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.CompactionAgent, true, nil, fa.CompactionToolDenials)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: post-turn session compaction error: %v\n", err)
			}
		}
	}
}

// checkColdStartCompaction performs an emergency cold-start compaction guard before turn 1 for uncompacted sessions.
func (fa *FolderAgent) checkColdStartCompaction(ctx context.Context, turns []*genai.Content, lastMemory string) ([]*genai.Content, string, error) {
	if fa.RuntimeConfig == nil || fa.RuntimeConfig.ContextWindow <= 0 {
		return turns, lastMemory, nil
	}
	compactCfg, err := LoadCompactConfig(fa.AgentDir)
	overheadPct := DefaultCompactionOverheadPct
	if err == nil && compactCfg != nil {
		if compactCfg.CompactOverheadPct >= 0 && compactCfg.CompactOverheadPct < 100 {
			overheadPct = compactCfg.CompactOverheadPct
		}
	}
	threshold := int(float64(fa.RuntimeConfig.ContextWindow) * (1.0 - (overheadPct / 100.0)))
	if EstimateTokens(turns, fa.RuntimeConfig.PreserveThinking) >= threshold {
		compacted, err := CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.CompactionAgent, true, nil, fa.CompactionToolDenials)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cold-start session compaction error: %v\n", err)
		} else if compacted {
			refreshed, err := ReadSessionTurns(fa.AgentDir)
			if err != nil {
				// Compaction already rewrote session.jsonl on disk; continuing with the
				// pre-compaction transcript would generate against the very history we
				// just paid to shrink. Abort loudly instead of proceeding silently.
				return nil, lastMemory, fmt.Errorf("compaction succeeded but reload of session for agent %q failed: %w", fa.AgentID, err)
			}
			if len(refreshed) > 0 {
				turns = refreshed
			}
			curMem, ok := readMemoryForChangeDetection(fa.AgentDir)
			if ok && curMem != lastMemory {
				lastMemory = curMem
				if err := fa.refreshSystemPromptAndAgent(); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to refresh system prompt after memory change for agent %s: %v\n", fa.AgentID, err)
				}
			}
		}
	}
	return turns, lastMemory, nil
}

func (fa *FolderAgent) compactForContinuation(ctx context.Context, yield func(string, error) bool) bool {
	beforeTurns, err := ReadSessionTurns(fa.AgentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-continuation compaction error: %v\n", err)
		yield(fmt.Sprintf("\n\n[Auto-continuation aborted: failed to read session turns: %v - incomplete status.]", err), nil)
		return false
	}
	tokensBefore := EstimateTokens(beforeTurns, fa.RuntimeConfig != nil && fa.RuntimeConfig.PreserveThinking)

	if fa.UsageTracker != nil {
		fa.UsageTracker.Reset()
	}
	compacted, err := CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.CompactionAgent, true, nil, fa.CompactionToolDenials)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-continuation compaction error: %v\n", err)
		yield(fmt.Sprintf("\n\n[Auto-continuation aborted: session compaction error: %v - incomplete status.]", err), nil)
		return false
	}
	afterTurns, err := ReadSessionTurns(fa.AgentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-continuation compaction error: %v\n", err)
		yield(fmt.Sprintf("\n\n[Auto-continuation aborted: failed to read session turns: %v - incomplete status.]", err), nil)
		return false
	}
	tokensAfter := EstimateTokens(afterTurns, fa.RuntimeConfig != nil && fa.RuntimeConfig.PreserveThinking)

	hasReduction := len(afterTurns) < len(beforeTurns) || tokensAfter < tokensBefore
	if !compacted || !hasReduction {
		// Fail-safe abort: If a mid-turn bail occurs but compaction fails or produces no reduction,
		// auto-continuation aborts with an incomplete status rather than re-tripping the context budget in an infinite loop.
		fmt.Fprintf(os.Stderr, "Warning: auto-continuation compaction produced no reduction (session may exceed safe read limits)\n")
		yield("\n\n[Auto-continuation aborted: session compaction produced no reduction - incomplete status.]", nil)
		return false
	}
	return true
}

// handleContinuationOrCompaction determines whether to continue generation (due to mid-turn compaction bail
// or deferred images) or finish and run post-turn compaction. Returns true if another turn iteration should run.
func (fa *FolderAgent) handleContinuationOrCompaction(
	ctx context.Context,
	wsDir string,
	deferredScratchpadIDs []string,
	lastContinuationReason *ContinuationReason,
	continuationCount *int,
	maxContinuations int,
	yield func(string, error) bool,
) bool {
	hasDeferredImages := fa.RuntimeConfig != nil && fa.RuntimeConfig.MaxImageDimension > 0 && len(deferredScratchpadIDs) > 0
	hasCompactedBail := fa.UsageTracker != nil && fa.UsageTracker.StoppedEarlyForCompaction

	// Image Re-inflation Edge Case:
	// If an image blob re-inflates context past budget on the continuation turn,
	// the continuation bails and reports an explicit incomplete status rather than looping.
	if hasCompactedBail && *lastContinuationReason == ContinuationDeferredImage {
		yield("\n\n[Auto-continuation aborted: image re-inflated context past budget - incomplete status.]", nil)
		return false
	}

	if fa.DisableAutoContinuation {
		fa.checkPostTurnCompaction(ctx, wsDir)
		return false
	}

	if hasCompactedBail || hasDeferredImages {
		if *continuationCount >= maxContinuations {
			fa.checkPostTurnCompaction(ctx, wsDir)
			yield(fmt.Sprintf("\n\n[Reached maximum auto-continuations (%d) - stopping with incomplete status.]", maxContinuations), nil)
			return false
		}
	}

	// Coincidence Ordering (Image + Bail in same turn):
	// If both conditions occur in the same turn:
	// 1. Post-turn compaction runs first on the text/tool results to free headroom.
	// 2. The deferred <IMAGE> user turn is appended second.
	// 3. Exactly one continuation is triggered (ContinuationDeferredImage),
	//    allowing the image turn to drive the resumption with maximum context headroom (no redundant compaction marker).
	if hasCompactedBail && hasDeferredImages {
		if !fa.compactForContinuation(ctx, yield) {
			return false
		}

		validImages := fa.appendDeferredImages(wsDir, deferredScratchpadIDs)
		if validImages == 0 {
			return false
		}

		*lastContinuationReason = ContinuationDeferredImage
		*continuationCount++
		return true
	}

	// Mid-turn Bail only:
	// When a turn bails mid-turn (StoppedEarlyForCompaction = true),
	// post-turn compaction executes immediately in the turn's cleanup block
	// using the real LastPromptTokens that triggered the bail (superseding D77's skip).
	if hasCompactedBail {
		if !fa.compactForContinuation(ctx, yield) {
			return false
		}

		// The harness appends an imperative sentinel user turn
		sentinelTurn := genai.NewContentFromText(`<CONTINUATION reason="post-compaction">Session context was compacted. Resume and complete your task from where you left off, referencing any updated persistent memory.</CONTINUATION>`, "user")
		if err := persistTurn(fa.AgentDir, wsDir, fa.AgentID, sentinelTurn, "user"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to record continuation sentinel for agent %s: %v\n", fa.AgentID, err)
			yield("\n\n[Auto-continuation aborted: failed to record post-compaction sentinel - incomplete status.]", nil)
			return false
		}

		*lastContinuationReason = ContinuationCompactedBail
		*continuationCount++
		return true
	}

	// Deferred Image only:
	if hasDeferredImages {
		// Post-turn compaction check if real tokens >= threshold before adding image
		fa.checkPostTurnCompaction(ctx, wsDir)
		validImages := fa.appendDeferredImages(wsDir, deferredScratchpadIDs)
		if validImages == 0 {
			return false
		}

		*lastContinuationReason = ContinuationDeferredImage
		*continuationCount++
		return true
	}

	// Normal turn completion:
	// When a turn completes normally and real tokens >= threshold, compact post-turn.
	fa.checkPostTurnCompaction(ctx, wsDir)
	return false
}

// GenerateTurnStream performs the agent generation turn yielding an iterator (iter.Seq2[string, error])
// that produces each text chunk as it is generated by the model across tool events and auto-continuation turns (D88).
func (fa *FolderAgent) GenerateTurnStream(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if ctx.Err() != nil {
			yield("", ctx.Err())
			return
		}

		// 0. The session must already end on a user turn - generating against a
		// session that doesn't (empty, or already ends on a model turn) hands
		// the model no new input to react to, which just produces a confused
		// response. AddAndGenerateTurn (the "prompt" command) always satisfies
		// this itself by appending a user turn first.
		turns, err := ReadSessionTurns(fa.AgentDir)
		if err != nil {
			yield("", fmt.Errorf("failed to read session turns: %w", err))
			return
		}
		if len(turns) == 0 || turns[len(turns)-1].Role != "user" {
			yield("", fmt.Errorf("cannot generate: session for agent %q does not end on a user turn - add one first (\"wackypub agent add\") or use \"wackypub agent prompt\" to do both in one call", fa.AgentID))
			return
		}

		wsDir := filepath.Dir(fa.AgentDir)
		sessionSvc := NewFileSessionService(wsDir)
		lastMemory, _ := readMemoryForChangeDetection(fa.AgentDir)

		// 1. Emergency Cold-Start Pre-Turn Guard: Retain a pre-turn check in GenerateTurnStream
		// *only* as a defensive valve before call 1 for uncompacted cold-start sessions.
		// When EstimateTokens(turns) >= threshold, it calls CheckAndCompactSession(..., force: true)
		// so it forcefully shrinks the oversized session.
		turns, lastMemory, err = fa.checkColdStartCompaction(ctx, turns, lastMemory)
		if err != nil {
			yield("", err)
			return
		}

		if len(turns) == 0 || turns[len(turns)-1].Role != "user" {
			yield("", fmt.Errorf("cannot generate: session for agent %q does not end on a user turn - add one first (\"wackypub agent add\") or use \"wackypub agent prompt\" to do both in one call", fa.AgentID))
			return
		}

		// Budget Guard: MaxAutoContinuations = 2 for standard sessions.
		// For A2A-context turns (A2AMeta != nil), MaxAutoContinuations = 1 to prevent caller turn timeouts.
		// Resets on every external user message.
		maxContinuations := DefaultMaxAutoContinuations
		if fa.A2AMeta != nil {
			maxContinuations = DefaultMaxAutoContinuationsA2A
		}
		if fa.MaxAutoContinuations != nil {
			maxContinuations = *fa.MaxAutoContinuations
		}
		if fa.DisableAutoContinuation {
			maxContinuations = 0
		}

		continuationCount := 0
		lastContinuationReason := ContinuationNone

		for {
			if ctx.Err() != nil {
				yield("", ctx.Err())
				return
			}

			// Reset usage tracker for this generation turn so continuations do not inherit stale call counts or flags
			if fa.UsageTracker != nil {
				fa.UsageTracker.Reset()
			}

			// Prompt & Memory Freshness: If post-turn compaction rewrote MEMORY.md,
			// the continuation turn re-renders the system prompt before building the runner
			// so the model sees updated memory immediately.
			curMem, ok := readMemoryForChangeDetection(fa.AgentDir)
			if ok && curMem != lastMemory {
				lastMemory = curMem
				if err := fa.refreshSystemPromptAndAgent(); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to refresh system prompt after memory change for agent %s: %v\n", fa.AgentID, err)
				}
			}

			// Fresh runner per turn iteration (re-reads clean disk state from FileSessionService)
			r, err := runner.New(runner.Config{
				AppName:           "wackypub",
				Agent:             fa.ADKAgent,
				SessionService:    sessionSvc,
				AutoCreateSession: true,
			})
			if err != nil {
				yield("", fmt.Errorf("failed to create runner: %w", err))
				return
			}

			var deferredScratchpadIDs []string
			var turnYieldedAny bool

			for event, err := range r.Run(ctx, "user", fa.AgentID, nil, agent.RunConfig{}) {
				if err != nil {
					if ctx.Err() != nil {
						yield("", ctx.Err())
					} else {
						yield("", fmt.Errorf("runner execution error: %w", err))
					}
					return
				}
				if ctx.Err() != nil {
					yield("", ctx.Err())
					return
				}
				if event != nil {
					if event.Content != nil {
						for _, p := range event.Content.Parts {
							if p != nil && p.FunctionResponse != nil && (p.FunctionResponse.Name == "get_scratchpad" || p.FunctionResponse.Name == "load_skill_extra") {
								respMap := p.FunctionResponse.Response
								if respMap != nil {
									if def, ok := respMap["deferred"].(bool); ok && def {
										if spID, ok := respMap["scratchpad_id"].(string); ok && spID != "" {
											deferredScratchpadIDs = append(deferredScratchpadIDs, spID)
										}
									}
								}
							}
						}
					}
					text := ExtractTextFromEvent(event)
					if text != "" {
						turnYieldedAny = true
						if !yield(text, nil) {
							return
						}
					}
				}
			}

			if ctx.Err() != nil {
				yield("", ctx.Err())
				return
			}

			if !turnYieldedAny {
				yield("", fmt.Errorf("received empty response from agent"))
				return
			}

			// Commit workspace event per turn boundary ("assistant")
			commitEventBestEffort(wsDir, fa.AgentID, "assistant")

			if !fa.handleContinuationOrCompaction(ctx, wsDir, deferredScratchpadIDs, &lastContinuationReason, &continuationCount, maxContinuations, yield) {
				return
			}
		}
	}
}

// GenerateTurn performs the agent generation turn for the current session using Google ADK runner.Runner.
// Uses FileSessionService to read and write session history directly to session.jsonl.
// Returns the full assistant response text joined across all yielded chunks with \n\n.
func (fa *FolderAgent) GenerateTurn(ctx context.Context) (string, error) {
	var chunks []string
	for chunk, err := range fa.GenerateTurnStream(ctx) {
		if err != nil {
			return "", err
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	if len(chunks) == 0 {
		return "", fmt.Errorf("received empty response from agent")
	}
	return strings.Join(chunks, "\n\n"), nil
}
