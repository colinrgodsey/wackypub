package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

const (
	DefaultCommandTimeoutSeconds = 900
	EnvCommandTimeoutSeconds     = "WACKYPUB_COMMAND_TIMEOUT_SECONDS"
)

type CreateScratchpadArgs struct {
	Text string `json:"text" jsonschema_description:"Text content to store in a persistent scratchpad entry"`
}

type CreateScratchpadResult struct {
	ID   string `json:"id"`
	Size int    `json:"size"`
}

type GetScratchpadArgs struct {
	ID        string `json:"id" jsonschema_description:"4-character ID of the scratchpad entry to read"`
	SkipLines *int   `json:"skip_lines,omitempty" jsonschema_description:"Optional number of lines to skip from the beginning"`
	NumLines  *int   `json:"num_lines,omitempty" jsonschema_description:"Optional maximum number of lines to retrieve"`
}

type GetScratchpadResult struct {
	Output       string `json:"output"`
	Deferred     bool   `json:"deferred,omitempty"`
	ScratchpadID string `json:"scratchpad_id,omitempty"`
}

type ListScratchpadsArgs struct{}

type ListScratchpadsResult struct {
	Entries []ScratchpadItem `json:"entries"`
	Count   int              `json:"count"`
	Cap     int              `json:"cap"`
}

type SearchScratchpadArgs struct {
	ID            string `json:"id" jsonschema_description:"Required scratchpad entry ID to search"`
	Query         string `json:"query" jsonschema_description:"Search query string"`
	CaseSensitive *bool  `json:"case_sensitive,omitempty" jsonschema_description:"Whether search is case-sensitive (default: true)"`
	Regex         bool   `json:"regex,omitempty" jsonschema_description:"Opt-in to treat query as a regular expression (default: false)"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema_description:"Maximum number of matching lines to return (default: 50)"`
}

type DeleteScratchpadArgs struct {
	ID string `json:"id" jsonschema_description:"4-character ID of the scratchpad entry to delete"`
}

type DeleteScratchpadResult struct {
	Status string `json:"status"`
}

type ExecToolArgs struct {
	Args  []string          `json:"args,omitempty" jsonschema_description:"List of CLI command line arguments passed positionally to the tool (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
	Env   map[string]string `json:"env,omitempty" jsonschema_description:"Key-value object map of environment variables to set for the tool invocation (not macro-expanded)"`
	Stdin string            `json:"stdin,omitempty" jsonschema_description:"Optional stdin template string to pipe into the command (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
}

type RunCommandArgs struct {
	Command string            `json:"command" jsonschema_description:"Name of the command executable to run from the discovered tools list"`
	Args    []string          `json:"args" jsonschema_description:"List of CLI command line arguments passed positionally to the tool (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
	Env     map[string]string `json:"env,omitempty" jsonschema_description:"Key-value object map of environment variables to set for the tool invocation (not macro-expanded)"`
	Stdin   string            `json:"stdin,omitempty" jsonschema_description:"Optional stdin template string to pipe into the command (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
}

type RunCommandResult struct {
	Output string `json:"output"`
}

type LoadSkillArgs struct {
	Name string `json:"name" jsonschema_description:"Name of the skill to load into conversation context"`
}

type LoadSkillResult struct {
	Output string `json:"output"`
}

// BuildFolderAgentTools constructs ADK functiontool instances for built-in tools (create_scratchpad, get_scratchpad, list_scratchpads, search_scratchpad, delete_scratchpad)
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

	// 1. create_scratchpad
	createTool, err := functiontool.New(functiontool.Config{
		Name:        "create_scratchpad",
		Description: "Store a text payload in a persistent, session-level scratchpad entry. Returns a freshly generated 4-character ID.",
	}, func(ctx agent.Context, args CreateScratchpadArgs) (CreateScratchpadResult, error) {
		entry, err := CreateScratchpad(agentDir, args.Text, "create_scratchpad")
		if err != nil {
			return CreateScratchpadResult{}, fmt.Errorf("failed to create scratchpad entry: %w", err)
		}
		return CreateScratchpadResult{
			ID:   entry.ID,
			Size: entry.Size,
		}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create create_scratchpad tool: %w", err)
	}
	addTool(createTool)

	// 2. get_scratchpad
	getTool, err := functiontool.New(functiontool.Config{
		Name:        "get_scratchpad",
		Description: "Retrieve stored text from a scratchpad entry by ID, optionally paginated by line range. If the entry contains an image and image support is enabled, it is queued for your next turn.",
	}, func(ctx agent.Context, args GetScratchpadArgs) (GetScratchpadResult, error) {
		filePath, _, isBinary, err := findScratchpadFile(agentDir, args.ID)
		if err != nil {
			return GetScratchpadResult{}, err
		}

		if isBinary {
			header, err := ReadMediaHeader(filePath)
			if err != nil {
				return GetScratchpadResult{}, err
			}
			_, mimeType := DetectMediaType(header)

			// Gating: only defer if image support is enabled on runtime config
			runtimeCfg, _ := LoadRuntimeConfig(agentDir)
			if runtimeCfg != nil && runtimeCfg.MaxImageDimension > 0 && strings.HasPrefix(mimeType, "image/") {
				return GetScratchpadResult{
					Output:       fmt.Sprintf("This scratchpad contains an image (%s) that will be available in your next turn.", mimeType),
					Deferred:     true,
					ScratchpadID: args.ID,
				}, nil
			}

			return GetScratchpadResult{}, fmt.Errorf("scratchpad entry %q is binary data (%s) and cannot be read as text", args.ID, mimeType)
		}

		out, err := GetScratchpad(agentDir, args.ID, args.SkipLines, args.NumLines)
		if err != nil {
			return GetScratchpadResult{}, err
		}
		return GetScratchpadResult{Output: out}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create get_scratchpad tool: %w", err)
	}
	addTool(getTool)

	// 3. list_scratchpads
	listTool, err := functiontool.New(functiontool.Config{
		Name:        "list_scratchpads",
		Description: "List metadata for all currently-live scratchpad entries (ID, size, lines, created_by, is_binary, mime_type), ordered oldest-first, and current capacity usage.",
	}, func(ctx agent.Context, args ListScratchpadsArgs) (ListScratchpadsResult, error) {
		items, count, capVal, err := ListScratchpads(agentDir)
		if err != nil {
			return ListScratchpadsResult{}, fmt.Errorf("failed to list scratchpads: %w", err)
		}
		return ListScratchpadsResult{
			Entries: items,
			Count:   count,
			Cap:     capVal,
		}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create list_scratchpads tool: %w", err)
	}
	addTool(listTool)

	// 4. search_scratchpad
	searchTool, err := functiontool.New(functiontool.Config{
		Name:        "search_scratchpad",
		Description: "Search a specific text scratchpad entry by ID for matching lines. Returns 1-indexed line numbers and precomputed skip_lines for get_scratchpad pagination.",
	}, func(ctx agent.Context, args SearchScratchpadArgs) (*SearchScratchpadResult, error) {
		return SearchScratchpad(agentDir, args.ID, args.Query, args.CaseSensitive, args.Regex, args.MaxResults)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create search_scratchpad tool: %w", err)
	}
	addTool(searchTool)

	// 5. delete_scratchpad
	deleteTool, err := functiontool.New(functiontool.Config{
		Name:        "delete_scratchpad",
		Description: "Delete a scratchpad entry by ID. Recommended for releasing large binary entries (images, audio) once they are no longer needed.",
	}, func(ctx agent.Context, args DeleteScratchpadArgs) (DeleteScratchpadResult, error) {
		err := DeleteScratchpad(agentDir, args.ID)
		if err != nil {
			return DeleteScratchpadResult{}, err
		}
		return DeleteScratchpadResult{Status: "deleted"}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create delete_scratchpad tool: %w", err)
	}
	addTool(deleteTool)

	// 6. Generic run_command tool covering all discovered executables under <agent_dir>/tools/
	discoveredMap, discoveredNames, _, err := DiscoverAgentToolsMap(agentDir)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to discover agent tools: %w", err)
	}

	var cmdListStr string
	if len(discoveredNames) > 0 {
		cmdListStr = strings.Join(discoveredNames, ", ")
	} else {
		cmdListStr = "none"
	}

	runCmdDesc := fmt.Sprintf(
		"Execute a command binary from tools/. Available commands: %s.\n\n"+
			"Usage Guidance:\n"+
			"- The working directory is always the agent's own directory - there's no way to cd elsewhere, since commands don't chain.\n"+
			"- args entries are passed as literal argv elements, not shell-parsed - no quoting or escaping needed for spaces/special characters.\n"+
			"- The agent's scratchpad may already contain the data it needs - check before running a command to regenerate something already available.\n"+
			"- Running a command with no arguments or --help is a legitimate way to learn what it is, how to use it, and what arguments it takes.\n"+
			"- args entries and the stdin field both support inline <SCRATCHPAD_DATA id=\"X\" skip_lines=\"N\" num_lines=\"M\" json_escape=\"true\" /> macros (skip_lines/num_lines/json_escape optional) - this substitutes the referenced scratchpad entry's content directly, without you ever having to read or repaste it yourself. When json_escape=\"true\" is set, content is substituted as JSON-escaped text (quotes, newlines, and backslashes escaped per RFC 8259) without adding surrounding quotes. Large stdout/stderr from this same tool is automatically captured into a fresh scratchpad entry and returned as <SCRATCHPAD_DATA id=\"X\" size=\"BYTES\" lines=\"LINES\" />, so it can be piped straight into another command's args/stdin this way.",
		cmdListStr,
	)

	// Explicitly construct InputSchema to enforce required fields and plain array type for 'args' (D56)
	runCmdInputSchema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"command": {
				Type:        "string",
				Description: "Name of the command executable to run from the discovered tools list",
			},
			"args": {
				Type:        "array",
				Description: "List of CLI command line arguments passed positionally to the tool (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)",
				Items: &jsonschema.Schema{
					Type: "string",
				},
			},
			"env": {
				Type:        "object",
				Description: "Key-value object map of environment variables to set for the tool invocation (not macro-expanded)",
				AdditionalProperties: &jsonschema.Schema{
					Type: "string",
				},
			},
			"stdin": {
				Type:        "string",
				Description: "Optional stdin template string to pipe into the command (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)",
			},
		},
		Required:      []string{"command", "args"},
		PropertyOrder: []string{"command", "args", "env", "stdin"},
	}

	runCmdTool, err := functiontool.New(functiontool.Config{
		Name:        "run_command",
		Description: runCmdDesc,
		InputSchema: runCmdInputSchema,
	}, func(ctx agent.Context, args RunCommandArgs) (RunCommandResult, error) {
		toolPath, ok := discoveredMap[args.Command]
		if !ok {
			return RunCommandResult{}, fmt.Errorf("unknown command %q. See the tool description for the list of available commands", args.Command)
		}

		execArgs := ExecToolArgs{
			Args:  args.Args,
			Env:   args.Env,
			Stdin: args.Stdin,
		}
		out, err := executeTool(ctx, agentDir, args.Command, toolPath, execArgs, a2aMeta, timeoutSeconds)
		if err != nil {
			return RunCommandResult{}, err
		}
		return RunCommandResult{Output: out}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create run_command tool: %w", err)
	}
	addTool(runCmdTool)

	// 7. load_skill tool for on-demand skills
	skillsMap, onDemandSkills, _, err := DiscoverAgentSkills(agentDir)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to discover agent skills: %w", err)
	}

	var skillLines []string
	for _, sk := range onDemandSkills {
		skillLines = append(skillLines, fmt.Sprintf("- %s: %s", sk.Name, sk.Description))
	}

	var skillListStr string
	if len(skillLines) > 0 {
		skillListStr = strings.Join(skillLines, "\n")
	} else {
		skillListStr = "none"
	}

	loadSkillDesc := fmt.Sprintf(
		"Load pre-written distilled guidance and instructions for a specific skill into conversation context.\n\n"+
			"Available skills:\n%s",
		skillListStr,
	)

	loadSkillTool, err := functiontool.New(functiontool.Config{
		Name:        "load_skill",
		Description: loadSkillDesc,
	}, func(ctx agent.Context, args LoadSkillArgs) (LoadSkillResult, error) {
		sk, ok := skillsMap[args.Name]
		if !ok || sk.AlwaysLoad {
			return LoadSkillResult{}, fmt.Errorf("unknown skill %q. See the tool description for the list of available skills", args.Name)
		}
		return LoadSkillResult{Output: sk.Body}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create load_skill tool: %w", err)
	}
	addTool(loadSkillTool)

	return toolMap, decls, nil
}

func executeTool(ctx context.Context, agentDir string, toolName string, toolPath string, args ExecToolArgs, a2aMeta *A2AMetadata, timeoutSeconds ...int) (string, error) {
	timeout := DefaultCommandTimeoutSeconds
	if len(timeoutSeconds) > 0 {
		timeout = timeoutSeconds[0]
	}

	cmdArgs := make([]string, len(args.Args))
	for i, rawArg := range args.Args {
		// Check for binary scratchpad references in args (D48: args reject .dat entries outright)
		if strings.Contains(rawArg, "<SCRATCHPAD_DATA") {
			matches := scratchpadMacroRegex.FindAllString(rawArg, -1)
			for _, m := range matches {
				idMatch := macroIDRegex.FindStringSubmatch(m)
				if len(idMatch) >= 2 {
					id := idMatch[1]
					_, _, isBinary, err := findScratchpadFile(agentDir, id)
					if err == nil && isBinary {
						return "", fmt.Errorf("cannot pass binary scratchpad entry %q in command args", id)
					}
				}
			}
		}

		expanded, err := ExpandScratchpadMacros(agentDir, rawArg)
		if err != nil {
			return "", err
		}
		if len(expanded) > MaxExpandedArgBytes {
			return "", fmt.Errorf("expanded argument exceeds 500000 bytes (was %d) - use stdin/stdout scratchpad redirection instead", len(expanded))
		}
		cmdArgs[i] = expanded
	}

	absToolPath, err := filepath.Abs(toolPath)
	if err != nil {
		absToolPath = toolPath
	}
	if evalPath, err := filepath.EvalSymlinks(absToolPath); err == nil {
		absToolPath = evalPath
	}

	var execCtx context.Context = ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(execCtx, absToolPath, cmdArgs...)
	cmd.Dir = agentDir
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	// D59: Propagate A2A Metadata and legacy CallChain directly to child process environment
	if a2aMeta != nil {
		denseJSON, err := a2aMeta.Encode()
		if err == nil && denseJSON != "" {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", Agent2AgentEnvVar, denseJSON))
		}
		if len(a2aMeta.CallChain) > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", CallChainEnvVar, strings.Join(a2aMeta.CallChain, ",")))
		}
	}

	dotEnv, err := LoadAgentDotEnv(agentDir)
	if err != nil {
		return "", fmt.Errorf("failed to load agent .env: %w", err)
	}
	for k, v := range dotEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if len(args.Env) > 0 {
		for k, v := range args.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	var stdinFile *os.File
	if args.Stdin != "" {
		trimmedStdin := strings.TrimSpace(args.Stdin)
		// Check if stdin contains binary scratchpad references per D48
		if strings.Contains(trimmedStdin, "<SCRATCHPAD_DATA") {
			var binaryID string
			matches := scratchpadMacroRegex.FindAllString(trimmedStdin, -1)
			for _, m := range matches {
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
					return "", fmt.Errorf("cannot mix binary scratchpad entry %q with text in stdin", binaryID)
				}

				// Check for pagination/escaping attributes on binary reference per D48
				if macroSkipLinesRegex.MatchString(trimmedStdin) || macroNumLinesRegex.MatchString(trimmedStdin) || macroJsonEscapeRegex.MatchString(trimmedStdin) {
					return "", fmt.Errorf("cannot use pagination or escaping attributes (skip_lines, num_lines, json_escape) with binary scratchpad entry %q", binaryID)
				}

				filePath, _, _, err := findScratchpadFile(agentDir, binaryID)
				if err != nil {
					return "", err
				}
				f, err := os.Open(filePath)
				if err != nil {
					return "", fmt.Errorf("failed to open binary scratchpad entry %q: %w", binaryID, err)
				}
				stdinFile = f
				cmd.Stdin = stdinFile
			}
		}

		if stdinFile == nil {
			expandedStdin, err := ExpandScratchpadMacros(agentDir, args.Stdin)
			if err != nil {
				return "", err
			}
			cmd.Stdin = strings.NewReader(expandedStdin)
		}
	}
	// No explicit stdin: leave cmd.Stdin unset (spawned tool gets /dev/null,
	// exec.Cmd's own default) rather than echoing the raw call args in as a
	// side channel - see D53, this used to feed a WACKYPUB_TOOL_ARGS-shaped
	// JSON blob into every tool's stdin unconditionally whenever Args/Env
	// were non-empty, with no documented reason and no way for a wrapper
	// tool like wackyproc to distinguish it from stdin the agent actually
	// meant to pipe through.

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
	if err != nil {
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("tool %s timed out after %d seconds", toolName, timeout)
		}
		errStr := stderr.String()
		if errStr == "" {
			errStr = err.Error()
		}
		return "", fmt.Errorf("tool %s failed: %s", toolName, errStr)
	}

	stdoutBytes := stdout.Bytes()
	stderrBytes := stderr.Bytes()

	var stdoutBlock string
	if isBinary, mimeType := DetectMediaType(stdoutBytes); isBinary {
		// D48: Binary stdout is always routed to a .dat scratchpad entry regardless of size
		entry, err := CreateBinaryScratchpad(agentDir, stdoutBytes, "run_command", mimeType)
		if err != nil {
			return "", fmt.Errorf("failed to create binary stdout scratchpad entry: %w", err)
		}
		stdoutBlock = fmt.Sprintf("<STDOUT><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"0\" mime=%q /></STDOUT>", entry.ID, entry.Size, mimeType)
	} else if len(stdoutBytes) > ScratchpadOutputThreshold {
		entry, err := CreateScratchpad(agentDir, string(stdoutBytes), "run_command")
		if err != nil {
			return "", fmt.Errorf("failed to create stdout scratchpad entry: %w", err)
		}
		stdoutBlock = fmt.Sprintf("<STDOUT><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"%d\" /></STDOUT>", entry.ID, entry.Size, entry.Lines)
	} else if len(stdoutBytes) > 0 {
		stdoutBlock = fmt.Sprintf("<STDOUT>%s</STDOUT>", string(stdoutBytes))
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
		entry, err := CreateScratchpad(agentDir, string(stderrBytes), "run_command")
		if err != nil {
			return "", fmt.Errorf("failed to create stderr scratchpad entry: %w", err)
		}
		stderrBlock = fmt.Sprintf("<STDERR><SCRATCHPAD_DATA id=%q size=\"%d\" lines=\"%d\" /></STDERR>", entry.ID, entry.Size, entry.Lines)
	} else if len(stderrBytes) > 0 {
		stderrBlock = fmt.Sprintf("<STDERR>%s</STDERR>", string(stderrBytes))
	}

	output := stdoutBlock + stderrBlock
	return output, nil
}

// FolderAgent encapsulates an agent loaded from a folder environment (<ws_dir>/<agent_id>).
type FolderAgent struct {
	AgentID               string
	AgentDir              string
	DotEnv                map[string]string
	RuntimeConfig         *RuntimeConfig
	SystemPrompt          string
	MemoryPrompt          string
	Model                 model.LLM
	ADKAgent              agent.Agent
	MaxToolTurns          int
	CommandTimeoutSeconds int
	A2AMeta               *A2AMetadata
	UsageTracker          *TurnUsageTracker
}

// LoadFolderAgent loads and initializes an agent from <wsDir>/<agentID>.
func LoadFolderAgent(wsDir string, agentID string, maxToolTurns int, commandTimeoutSeconds ...int) (*FolderAgent, error) {
	return LoadFolderAgentWithA2A(wsDir, agentID, nil, maxToolTurns, commandTimeoutSeconds...)
}

// LoadFolderAgentWithA2A loads and initializes an agent with explicit A2AMetadata context (D59).
func LoadFolderAgentWithA2A(wsDir string, agentID string, a2aMeta *A2AMetadata, maxToolTurns int, commandTimeoutSeconds ...int) (*FolderAgent, error) {
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

	// 1. Load runtime.json
	runtimeCfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime config: %w", err)
	}

	// 2. Render AGENTS.md (expanding @<FILE_PATH> macros)
	expandedPrompt, err := RenderAgentSystemPrompt(wsDir, agentID)
	if err != nil {
		return nil, fmt.Errorf("failed to render system prompt for agent %s: %w", agentID, err)
	}

	// 3. Read MEMORY.md
	memoryContent, err := ReadMemoryFile(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read memory file for agent %s: %w", agentID, err)
	}

	// 4. Initialize LLM Model adapter
	var llmModel model.LLM
	switch runtimeCfg.Provider {
	case "anthropic":
		llmModel = NewAnthropicModel(runtimeCfg)
	case "openai", "openai-compatible":
		llmModel = NewOpenAIModel(runtimeCfg)
	case "gemini":
		geminiModel, err := CreateGeminiModel(context.Background(), runtimeCfg.Model, runtimeCfg.APIKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create Gemini model for %s: %w", agentID, err)
		}
		llmModel = geminiModel
	default:
		return nil, fmt.Errorf("unsupported provider %q in runtime.json for agent %s (supported: openai, gemini, anthropic)", runtimeCfg.Provider, agentID)
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

	// 6. Construct ADK llmagent with agentID, expanded prompt instruction, maxToolTurns cap, runtimeCfg, model, tracker, and tools
	tracker := &TurnUsageTracker{}
	ag, err := BuildADKAgentWithConfigAndTracker(agentID, expandedPrompt, maxToolTurns, runtimeCfg, llmModel, agentDir, tracker, toolsList...)
	if err != nil {
		return nil, fmt.Errorf("failed to build ADK agent for folder agent %s: %w", agentID, err)
	}

	return &FolderAgent{
		AgentID:               agentID,
		AgentDir:              agentDir,
		DotEnv:                dotEnv,
		RuntimeConfig:         runtimeCfg,
		SystemPrompt:          expandedPrompt,
		MemoryPrompt:          memoryContent,
		Model:                 llmModel,
		ADKAgent:              ag,
		MaxToolTurns:          maxToolTurns,
		CommandTimeoutSeconds: resolvedTimeout,
		A2AMeta:               a2aMeta,
		UsageTracker:          tracker,
	}, nil
}

// GenerateTurnStream performs the agent generation turn yielding an iterator (iter.Seq2[string, error])
// that produces each text chunk as it is generated by the model across tool events.
func (fa *FolderAgent) GenerateTurnStream(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
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

		// 1. Check for context window compaction trigger before generating - never
		// forced here, only wackypub agent compact / AgentSDK.CompactSession
		// can force (D44, D68).
		_, err = CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.ADKAgent, false)
		if err != nil {
			// Log compaction warning, but continue execution if possible
			fmt.Fprintf(os.Stderr, "Warning: session compaction error: %v\n", err)
		}

		// Reset usage tracker for this generation turn
		if fa.UsageTracker != nil {
			fa.UsageTracker.Reset()
		}

		wsDir := filepath.Dir(fa.AgentDir)
		sessionSvc := NewFileSessionService(wsDir)

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
		var yieldedAny bool

		defer func() {
			// Append deferred image user turns per D49, gated by maxImageDimension
			if fa.RuntimeConfig != nil && fa.RuntimeConfig.MaxImageDimension > 0 {
				for _, spID := range deferredScratchpadIDs {
					filePath, _, isBinary, err := findScratchpadFile(fa.AgentDir, spID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to find deferred scratchpad %q for agent %q: %v\n", spID, fa.AgentID, err)
						failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to load deferred image from scratchpad '%s': %v</IMAGE_ERROR>", spID, err), "user")
						_ = AppendSessionContent(fa.AgentDir, failTurn)
						_ = CommitWorkspaceEvent(wsDir, fa.AgentID, "user (deferred image error)")
						continue
					}
					if !isBinary {
						fmt.Fprintf(os.Stderr, "Warning: deferred scratchpad %q for agent %q is not binary data\n", spID, fa.AgentID)
						failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to load deferred image from scratchpad '%s': entry is not binary image data</IMAGE_ERROR>", spID), "user")
						_ = AppendSessionContent(fa.AgentDir, failTurn)
						_ = CommitWorkspaceEvent(wsDir, fa.AgentID, "user (deferred image error)")
						continue
					}
					imgData, err := os.ReadFile(filePath)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to read deferred scratchpad file %s for agent %q: %v\n", filePath, fa.AgentID, err)
						failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to read deferred image from scratchpad '%s': %v</IMAGE_ERROR>", spID, err), "user")
						_ = AppendSessionContent(fa.AgentDir, failTurn)
						_ = CommitWorkspaceEvent(wsDir, fa.AgentID, "user (deferred image error)")
						continue
					}
					jpegBytes, mimeType, err := NormalizeAndResizeImage(bytes.NewReader(imgData), fa.RuntimeConfig.MaxImageDimension)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to decode/resize deferred image from scratchpad %q for agent %q: %v\n", spID, fa.AgentID, err)
						failTurn := genai.NewContentFromText(fmt.Sprintf("<IMAGE_ERROR>Failed to process deferred image from scratchpad '%s': %v</IMAGE_ERROR>", spID, err), "user")
						_ = AppendSessionContent(fa.AgentDir, failTurn)
						_ = CommitWorkspaceEvent(wsDir, fa.AgentID, "user (deferred image error)")
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
					_ = AppendSessionContent(fa.AgentDir, turn)
					_ = CommitWorkspaceEvent(wsDir, fa.AgentID, "user (deferred image)")
				}
			}

			// Post-turn compaction check using real provider usage data (D68.1)
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
				} else {
					if curTurns, err := ReadSessionTurns(fa.AgentDir); err == nil {
						usedTokens = EstimateTokens(curTurns, fa.RuntimeConfig.PreserveThinking)
					}
				}

				if usedTokens >= threshold {
					// Reset tracker so compaction pass starts with clean call count and state
					if fa.UsageTracker != nil {
						fa.UsageTracker.Reset()
					}
					_, err = CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.ADKAgent, true)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Warning: post-turn session compaction error: %v\n", err)
					}
				}
			}

			if yieldedAny {
				_ = CommitWorkspaceEvent(wsDir, fa.AgentID, "assistant")
			}
		}()

		for event, err := range r.Run(ctx, "user", fa.AgentID, nil, agent.RunConfig{}) {
			if err != nil {
				yield("", fmt.Errorf("runner execution error: %w", err))
				return
			}
			if event != nil {
				if event.Content != nil {
					for _, p := range event.Content.Parts {
						if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "get_scratchpad" {
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
					yieldedAny = true
					if !yield(text, nil) {
						return
					}
				}
			}
		}

		if !yieldedAny {
			yield("", fmt.Errorf("received empty response from agent"))
			return
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

// Helper to run ADK runner session for folder agent
func (fa *FolderAgent) RunWithRunner(ctx context.Context, sessionID string, prompt string) ([]*session.Event, error) {
	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:           "wackypub",
		Agent:             fa.ADKAgent,
		SessionService:    sessionService,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create runner: %w", err)
	}

	userMsg := genai.NewContentFromText(prompt, "user")
	var events []*session.Event
	for event, err := range r.Run(ctx, "user", sessionID, userMsg, agent.RunConfig{}) {
		if err != nil {
			return events, err
		}
		if event != nil {
			events = append(events, event)
		}
	}

	return events, nil
}
