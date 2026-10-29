package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type RunCommandArgs struct {
	Command string            `json:"command" jsonschema_description:"Name of the command executable to run from the discovered tools list"`
	Args    []string          `json:"args" jsonschema_description:"List of CLI command line arguments passed positionally to the tool (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
	Env     map[string]string `json:"env,omitempty" jsonschema_description:"Key-value object map of environment variables to set for the tool invocation (not macro-expanded)"`
	Stdin   string            `json:"stdin,omitempty" jsonschema_description:"Optional stdin template string to pipe into the command (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
}

type RunCommandResult struct {
	Output   string   `json:"output"`
	Warning  string   `json:"warning,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ListCommandsArgs is intentionally empty: the command set is a property of the agent
// directory, not of the call.
type ListCommandsArgs struct{}

type ListCommandsResult struct {
	Commands []string `json:"commands"`
}

// listCommandsSnapshot returns the set run_command can execute, so the list and the
// executor can never disagree.
func listCommandsSnapshot(discoveredNames []string) ListCommandsResult {
	commands := make([]string, len(discoveredNames))
	copy(commands, discoveredNames)
	return ListCommandsResult{Commands: commands}
}

// registerCommandTools builds the generic run_command tool covering all discovered executables under <agent_dir>/tools/.
func registerCommandTools(agentDir string, a2aMeta *A2AMetadata, timeoutSeconds int, addTool func(tool.Tool)) error {
	discoveredMap, discoveredNames, _, err := DiscoverAgentToolsMap(agentDir)
	if err != nil {
		return fmt.Errorf("failed to discover agent tools: %w", err)
	}

	runCmdDesc :=
		"Execute a command binary from tools/. Use list_commands to discover the available commands.\n\n" +
			"Usage Guidance:\n" +
			"- The working directory is always the agent's own directory - there's no way to cd elsewhere, since commands don't chain.\n" +
			"- args entries are passed as literal argv elements, not shell-parsed - no quoting or escaping needed for spaces/special characters.\n" +
			"- The agent's scratchpad may already contain the data it needs - check before running a command to regenerate something already available.\n" +
			"- Large payloads: pass a scratchpad entry reference, not raw content. Stage the data with create_scratchpad, then put <SCRATCHPAD_DATA id=\"X\" /> in args or stdin and the server substitutes the entry bytes at execution time - no need to read or re-emit them.\n" +
			"- Running a command with no arguments or --help is a legitimate way to learn what it is, how to use it, and what arguments it takes.\n" +
			"- args entries and the stdin field both support inline <SCRATCHPAD_DATA id=\"X\" skip_lines=\"N\" num_lines=\"M\" json_escape=\"true\" /> macros (skip_lines/num_lines/json_escape optional) - this substitutes the referenced scratchpad entry's content directly, without you ever having to read or repaste it yourself. When json_escape=\"true\" is set, content is substituted as JSON-escaped text (quotes, newlines, and backslashes escaped per RFC 8259) without adding surrounding quotes. Large stdout/stderr from this same tool is automatically captured into a fresh scratchpad entry and returned as <SCRATCHPAD_DATA id=\"X\" size=\"BYTES\" lines=\"LINES\" />, so it can be piped straight into another command's args/stdin this way."

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
		return executeRunCommand(ctx, agentDir, discoveredMap, a2aMeta, timeoutSeconds, args)
	})
	if err != nil {
		return fmt.Errorf("failed to create run_command tool: %w", err)
	}
	addTool(runCmdTool)

	listCmdTool, err := functiontool.New(functiontool.Config{
		Name:        "list_commands",
		Description: "List the commands available to run_command: every executable discovered under tools/ for this agent. Takes no arguments. The returned set is authoritative for this agent; call it when unsure which commands exist.",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, func(ctx agent.Context, args ListCommandsArgs) (ListCommandsResult, error) {
		return listCommandsSnapshot(discoveredNames), nil
	})
	if err != nil {
		return fmt.Errorf("failed to create list_commands tool: %w", err)
	}
	addTool(listCmdTool)
	return nil
}

func executeRunCommand(ctx context.Context, agentDir string, discoveredMap map[string]string, a2aMeta *A2AMetadata, timeoutSeconds int, args RunCommandArgs) (RunCommandResult, error) {
	toolPath, ok := discoveredMap[args.Command]
	if !ok {
		return RunCommandResult{}, fmt.Errorf("unknown command %q. Use list_commands to see the available commands", args.Command)
	}

	execArgs := ExecToolArgs{
		Args:  args.Args,
		Env:   args.Env,
		Stdin: args.Stdin,
	}
	out, warnings, err := executeTool(ctx, agentDir, args.Command, toolPath, execArgs, a2aMeta, timeoutSeconds)
	var warnStr string
	if len(warnings) > 0 {
		warnStr = strings.Join(warnings, "\n")
	}
	return RunCommandResult{Output: out, Warning: warnStr, Warnings: warnings}, err
}
