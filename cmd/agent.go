package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

var (
	messageFlag        string
	compactMDFile      string
	compactRuntimeFile string
)

func newSDK(wsDir string) *adkAgent.AgentSDK {
	sdk := adkAgent.NewSDK(wsDir)
	sdk.MaxToolTurns = GetMaxToolTurns()
	sdk.CommandTimeoutSeconds = GetCommandTimeoutSeconds()
	return sdk
}

var agentCmd = &cobra.Command{
	Use:   "agent <agent_id>",
	Short: "Manage folder-based agent sessions (<ws_dir>/<agent_id>)",
	Long: `Manage agent sessions located in workspace folders (<ws_dir>/<agent_id>).
Supports adding user turns to session.jsonl and generating assistant responses powered by Google ADK.`,
}

// wackypub agent <agent_id> add [message] OR wackypub agent add <agent_id> [message]
var agentAddCmd = &cobra.Command{
	Use:   "add [agent_id] [message]",
	Short: "Add a user message turn to the agent session",
	Long: `Appends a single user-role turn to <ws_dir>/<agent_id>/session.jsonl. Does not generate a
response - use "generate" afterward, or use "prompt" to do both atomically.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  message    The text to append as a user turn. Can also be supplied via the --message flag,
             or piped in on stdin (e.g. "echo hello | wackypub agent <agent_id> add"). Exactly
             one of these three must be provided.

Acquires the session lock for the duration of the append. Creates the agent directory if it
does not already exist.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		var userMsg string

		if len(args) >= 2 {
			agentID = args[0]
			userMsg = args[1]
		} else if len(args) == 1 {
			agentID = args[0]
			userMsg = messageFlag
		} else {
			userMsg = messageFlag
		}

		// If userMsg is empty, check stdin (piped input)
		if userMsg == "" {
			stat, _ := os.Stdin.Stat()
			if (stat.Mode() & os.ModeCharDevice) == 0 {
				reader := bufio.NewReader(os.Stdin)
				bytesInput, err := io.ReadAll(reader)
				if err == nil {
					userMsg = string(bytesInput)
				}
			}
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> add [message]")
		}
		if userMsg == "" {
			return fmt.Errorf("user message is required. Provide via argument, --message flag, or stdin pipe")
		}

		if err := sdk.AddUserTurn(agentID, userMsg); err != nil {
			return err
		}

		fmt.Printf("Added user message to agent %q session (%s/session.jsonl).\n", agentID, sdk.AgentDir(agentID))
		return nil
	},
}

// wackypub agent <agent_id> add-media OR wackypub agent add-media <agent_id>
var agentAddMediaCmd = &cobra.Command{
	Use:   "add-media [agent_id]",
	Short: "Attach an image from standard input to the agent's session history",
	Long: `Reads image bytes from standard input (stdin), resizes/normalizes the image to JPEG format,
and appends it as a user-role image turn to <ws_dir>/<agent_id>/session.jsonl per D47.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Image bytes MUST be provided via stdin pipe (e.g. "wackypub agent <agent_id> add-media < photo.jpg").
No file-path flags or parameters are accepted, maintaining strict security isolation.

Gated by maxImageDimension in runtime.json - if maxImageDimension is absent or <= 0, image attachments
are rejected outright. Resizes downscale-only so the longer side does not exceed maxImageDimension.
Transparencies in PNG/GIF inputs are flattened onto a white background before JPEG encoding.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> add-media < image.jpg")
		}

		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) != 0 {
			return fmt.Errorf("no image data provided on stdin. Pipe an image file, e.g.: wackypub agent %s add-media < image.jpg", agentID)
		}

		content, err := sdk.AddMedia(agentID, os.Stdin)
		if err != nil {
			return err
		}

		var rawSize int
		if len(content.Parts) > 0 && content.Parts[0].InlineData != nil {
			rawSize = len(content.Parts[0].InlineData.Data)
		}

		fmt.Printf("Added image attachment (%d bytes JPEG) to agent %q session (%s/session.jsonl).\n", rawSize, agentID, sdk.AgentDir(agentID))
		return nil
	},
}

// wackypub agent <agent_id> generate OR wackypub agent generate <agent_id>
var agentGenerateCmd = &cobra.Command{
	Use:   "generate [agent_id]",
	Short: "Generate the agent's turn from current session using input previously queued with 'add'.",
	Long: `Loads the agent from <ws_dir>/<agent_id>, evaluates whether session compaction is needed
(based on runtime.json's contextWindow and COMPACT.md configuration - see docs/agents.md), then calls the
configured model with the system prompt, MEMORY.md, and current session.jsonl history, and
appends the resulting turn (including any reasoning/thinking part) to session.jsonl.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>). No message argument -
             this command takes no other input. Use "prompt" to send a message and generate in
             one call.

Prints the generated final-answer text to stdout (reasoning/thinking text is excluded from
what's printed, though it is still persisted to session.jsonl). Does not append a user turn
first - the session must already end on a user turn (errors otherwise, since generating
against anything else just hands the model no new input to react to). Use "prompt" to append
a user turn and generate in one call.

Acquires the session lock for the duration of the operation.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("generate takes only an agent_id, not a message (got %d extra argument(s)) - use \"wackypub agent prompt\" to send a message and generate in one call", len(args)-1)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> generate")
		}

		ctx := context.Background()
		first := true
		for chunk, err := range sdk.GenerateTurnStream(ctx, agentID) {
			if err != nil {
				return err
			}
			if chunk != "" {
				if !first {
					fmt.Println()
				}
				fmt.Println(chunk)
				first = false
			}
		}
		return nil
	},
}

// wackypub agent <agent_id> strip-signatures OR wackypub agent strip-signatures <agent_id>
var agentStripSignaturesCmd = &cobra.Command{
	Use:   "strip-signatures [agent_id]",
	Short: "Permanently remove provider-specific opaque reasoning/thought signatures from an agent's session.jsonl",
	Long: `Permanently removes provider-specific opaque reasoning/thought signatures - OpenRouter's
structured reasoning_details block metadata (including encrypted/signed reasoning tied to a
specific backend endpoint) and Gemini's ThoughtSignature field - from every turn in
<ws_dir>/<agent_id>/session.jsonl, rewriting the file in place. Readable plain-text reasoning
is left untouched.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Useful when switching an agent from one model/provider to another - a signature issued by
the old provider means nothing to the new one and gets the request rejected outright if
replayed (e.g. Anthropic 400s with "Invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block" when it receives
a Gemini ThoughtSignature carried over from an earlier session).

Prints the number of turns that were modified. Acquires the session lock for the duration of
the rewrite.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> strip-signatures")
		}

		modified, err := sdk.StripSignatures(agentID)
		if err != nil {
			return err
		}

		fmt.Printf("Stripped provider signatures from %d turn(s) in agent %q session (%s/session.jsonl).\n", modified, agentID, sdk.AgentDir(agentID))
		return nil
	},
}

// wackypub agent <agent_id> read-session OR wackypub agent read-session <agent_id>
var agentReadSessionCmd = &cobra.Command{
	Use:   "read-session [agent_id]",
	Short: "Print the agent's session.jsonl turn history as JSON",
	Long: `Prints every turn currently stored in <ws_dir>/<agent_id>/session.jsonl to stdout, one
JSON-encoded genai.Content object per line (the same shape used in session.jsonl itself -
{"role": "user"|"model", "parts": [...]}).

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Read-only: does not modify session.jsonl. Acquires the session lock for the duration of the
read.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> read-session")
		}

		turns, err := sdk.ReadSession(agentID)
		if err != nil {
			return err
		}

		enc := json.NewEncoder(os.Stdout)
		for _, t := range turns {
			if err := enc.Encode(t); err != nil {
				return fmt.Errorf("failed to encode turn: %w", err)
			}
		}
		return nil
	},
}

// wackypub agent <agent_id> read-memory OR wackypub agent read-memory <agent_id>
var agentReadMemoryCmd = &cobra.Command{
	Use:   "read-memory [agent_id]",
	Short: "Print the agent's MEMORY.md contents",
	Long: `Prints the current contents of <ws_dir>/<agent_id>/MEMORY.md to stdout.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Prints nothing (empty output, no error) if the agent has no MEMORY.md yet. Read-only: does not
modify anything. Acquires the session lock for the duration of the read.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> read-memory")
		}

		mem, err := sdk.ReadMemory(agentID)
		if err != nil {
			return err
		}

		fmt.Println(mem)
		return nil
	},
}

// wackypub agent <agent_id> render-prompt OR wackypub agent render-prompt <agent_id>
var agentRenderPromptCmd = &cobra.Command{
	Use:   "render-prompt [agent_id]",
	Short: "Print the agent's fully rendered system prompt (AGENTS.md after macro expansion)",
	Long: `Reads <ws_dir>/<agent_id>/AGENTS.md (falling back to a generic "You are agent <id>."
prompt if it doesn't exist) and expands @<FILE_PATH> macros, then prints the fully rendered
result to stdout - exactly the text that gets folded into the first turn of every generation
request (see docs/agents.md §3 MEMORY.md).

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Useful for validating AGENTS.md/macro output on its own: this command does not construct a
model and does not require runtime.json to exist or be valid, so it works even for an agent
whose backend isn't configured yet.

Read-only: does not modify anything.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> render-prompt")
		}

		prompt, err := sdk.RenderSystemPrompt(agentID)
		if err != nil {
			return err
		}

		fmt.Println(prompt)
		return nil
	},
}

// wackypub agent <agent_id> compact OR wackypub agent compact <agent_id>
var agentCompactCmd = &cobra.Command{
	Use:   "compact [agent_id]",
	Short: "Perform session compaction on an agent's history",
	Long: `Performs session compaction on <ws_dir>/<agent_id>/session.jsonl:
summarizes the oldest turns (default 50%, or compact-pct in COMPACT.md) into MEMORY.md and removes them
from session.jsonl. Explicitly invoking this command always performs compaction regardless of current
token count against contextWindow. On a genuinely empty session, this is a clean no-op.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Prints whether compaction actually ran. Acquires the session lock for the duration of the
operation.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if compactRuntimeFile != "" {
			if _, err := adkAgent.LoadRuntimeConfigFile(compactRuntimeFile); err != nil {
				return err
			}
		}

		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> compact")
		}

		var compactCfg *adkAgent.CompactConfig
		if compactMDFile != "" {
			data, err := os.ReadFile(compactMDFile)
			if err != nil {
				return fmt.Errorf("failed to read compact md file: %w", err)
			}
			cfg, err := adkAgent.ParseCompactConfig(string(data))
			if err != nil {
				return fmt.Errorf("failed to parse compact md file: %w", err)
			}
			compactCfg = cfg
		}

		ctx := context.Background()
		opts := adkAgent.CompactSessionOptions{
			ConfigOverride: compactCfg,
			RuntimePath:    compactRuntimeFile,
		}
		compacted, err := sdk.CompactSessionWithOptions(ctx, agentID, true, opts)
		if err != nil {
			return err
		}

		if compacted {
			fmt.Printf("Compacted agent %q session (%s/session.jsonl); MEMORY.md updated.\n", agentID, sdk.AgentDir(agentID))
		} else {
			fmt.Printf("No compaction performed for agent %q (session is empty).\n", agentID)
		}
		return nil
	},
}

// wackypub agent <agent_id> prompt [message] OR wackypub agent prompt <agent_id> [message]
var agentPromptCmd = &cobra.Command{
	Use:   "prompt [agent_id] [message]",
	Short: "Atomically append user message and generate agent response under a single lock",
	Long: `Appends a user-role turn and generates the assistant response in one call, holding the
session lock for both steps - the recommended way to drive an agent turn, since it can't race
with another process appending a turn in between the two steps the way separate "add" +
"generate" calls could.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  message    The user turn's text. Can also be supplied via the --message flag, or piped in on
             stdin. Exactly one of these three must be provided.

Prints the generated final-answer text to stdout (reasoning/thinking text is excluded from
what's printed, though it is still persisted to session.jsonl).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		var userMsg string

		if len(args) >= 2 {
			agentID = args[0]
			userMsg = args[1]
		} else if len(args) == 1 {
			agentID = args[0]
			userMsg = messageFlag
		} else {
			userMsg = messageFlag
		}

		if userMsg == "" {
			stat, _ := os.Stdin.Stat()
			if (stat.Mode() & os.ModeCharDevice) == 0 {
				reader := bufio.NewReader(os.Stdin)
				bytesInput, err := io.ReadAll(reader)
				if err == nil {
					userMsg = string(bytesInput)
				}
			}
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> prompt [message]")
		}
		if userMsg == "" {
			return fmt.Errorf("user message is required. Provide via argument, --message flag, or stdin pipe")
		}

		ctx := context.Background()
		first := true
		for chunk, err := range sdk.AddAndGenerateTurnStream(ctx, agentID, userMsg) {
			if err != nil {
				return err
			}
			if chunk != "" {
				if !first {
					fmt.Println()
				}
				fmt.Println(chunk)
				first = false
			}
		}
		return nil
	},
}

// wackypub agent <agent_id> repl OR wackypub agent repl <agent_id>
var agentReplCmd = &cobra.Command{
	Use:   "repl [agent_id]",
	Short: "Interactive read-eval-print loop for driving an agent by hand",
	Long: `Reads lines from stdin in a loop, appending each as a user turn and generating/printing
the agent's response - the interactive way to drive an agent without quoting every message
through a separate "wackypub agent prompt" call.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Type "exit" or "quit", or press Ctrl+D, to end the session. Blank lines are ignored. A failed
turn prints an error and continues the loop rather than exiting.

Refuses to run when the current directory is an agent's own directory (same detection D41
uses for trace/workspace snapshot/tag/push) - this is an interactive tool for a human at a
real terminal, not something an agent should invoke on itself via run_command.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseIfAgentContext("wackypub agent repl"); err != nil {
			return err
		}

		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		if len(args) >= 1 {
			agentID = args[0]
		}
		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> repl")
		}

		fmt.Printf("wackypub REPL - agent %q. Type \"exit\"/\"quit\" or press Ctrl+D to end.\n", agentID)
		ctx := context.Background()
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for {
			fmt.Printf("%s> ", agentID)
			if !scanner.Scan() {
				fmt.Println()
				break
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if line == "exit" || line == "quit" {
				break
			}

			first := true
			for chunk, err := range sdk.AddAndGenerateTurnStream(ctx, agentID, line) {
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					break
				}
				if chunk != "" {
					if !first {
						fmt.Println()
					}
					fmt.Println(chunk)
					first = false
				}
			}
			fmt.Println()
		}
		return nil
	},
}

// wackypub agent <id> scratchpad ... OR wackypub agent scratchpad ...
var scratchpadCmd = &cobra.Command{
	Use:   "scratchpad",
	Short: "Manage persistent scratchpad entries for an agent (<ws_dir>/<agent_id>/scratchpad/)",
	Long:  "Create, read, list, search, and delete persistent scratchpad entries stored in <ws_dir>/<agent_id>/scratchpad/.",
}

var scratchpadCreateCmd = &cobra.Command{
	Use:   "create [agent_id] [message]",
	Short: "Store a text payload into an agent's persistent scratchpad",
	Long: `Creates a new scratchpad entry in <ws_dir>/<agent_id>/scratchpad/ with a generated 4-character ID.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  message    The text to store. Can also be supplied via --message flag or piped in on stdin.

Atomic and collision-safe across processes. Automatically evicts the entry with the oldest mtime if capacity (300) is exceeded.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		var agentID string
		var content string

		if len(args) >= 2 {
			agentID = args[0]
			content = args[1]
		} else if len(args) == 1 {
			agentID = args[0]
			content = messageFlag
		} else {
			content = messageFlag
		}

		if content == "" {
			stat, _ := os.Stdin.Stat()
			if (stat.Mode() & os.ModeCharDevice) == 0 {
				reader := bufio.NewReader(os.Stdin)
				bytesInput, err := io.ReadAll(reader)
				if err == nil {
					content = string(bytesInput)
				}
			}
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> scratchpad create [message]")
		}
		if content == "" {
			return fmt.Errorf("scratchpad content is required. Provide via argument, --message flag, or stdin pipe")
		}

		entry, err := sdk.CreateScratchpad(agentID, content, "cli")
		if err != nil {
			return err
		}

		fmt.Printf("Created scratchpad entry %q (%d bytes) for agent %q.\n", entry.ID, entry.Size, agentID)
		return nil
	},
}

var (
	scratchpadSkipLines int
	scratchpadNumLines  int
)

var scratchpadReadCmd = &cobra.Command{
	Use:   "read [agent_id] <entry_id>",
	Short: "Read stored text from an agent's scratchpad entry",
	Long: `Retrieves stored text content from <ws_dir>/<agent_id>/scratchpad/ by entry ID.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  entry_id   Required. The 4-character scratchpad entry ID to read.

Pass --skip-lines N and/or --num-lines M for line-based pagination. Rejects binary (.dat) entries outright per D48.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		if len(args) < 2 {
			return fmt.Errorf("agent_id and entry_id are required. Usage: wackypub agent <agent_id> scratchpad read <entry_id>")
		}
		agentID := args[0]
		entryID := args[1]

		var skipPtr *int
		if cmd.Flags().Changed("skip-lines") {
			skipPtr = &scratchpadSkipLines
		}
		var numPtr *int
		if cmd.Flags().Changed("num-lines") {
			numPtr = &scratchpadNumLines
		}

		out, err := sdk.GetScratchpad(agentID, entryID, skipPtr, numPtr)
		if err != nil {
			return err
		}

		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") && out != "" {
			fmt.Println()
		}
		return nil
	},
}

var scratchpadListCmd = &cobra.Command{
	Use:   "list [agent_id]",
	Short: "List all live scratchpad entries for an agent",
	Long: `Lists metadata (ID, size, lines, created_by, is_binary, mime_type), ordered oldest-first by mtime, and capacity usage for all live scratchpad entries in <ws_dir>/<agent_id>/scratchpad/.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Outputs JSON metadata. Does not acquire the session lock.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		if len(args) < 1 {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> scratchpad list")
		}
		agentID := args[0]

		items, count, capVal, err := sdk.ListScratchpads(agentID)
		if err != nil {
			return err
		}

		result := struct {
			Entries []adkAgent.ScratchpadItem `json:"entries"`
			Count   int                       `json:"count"`
			Cap     int                       `json:"cap"`
		}{
			Entries: items,
			Count:   count,
			Cap:     capVal,
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	},
}

var (
	scratchpadRegex           bool
	scratchpadCaseInsensitive bool
	scratchpadMaxResults      int
)

var scratchpadSearchCmd = &cobra.Command{
	Use:   "search [agent_id] <entry_id> <query>",
	Short: "Search a text scratchpad entry for matching lines",
	Long: `Searches a specific text scratchpad entry in <ws_dir>/<agent_id>/scratchpad/ for matching lines.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  entry_id   Required. The 4-character scratchpad entry ID to search.
  query      Required. The text substring or regex pattern to search for.

Returns 1-indexed line numbers, precomputed skip_lines for get_scratchpad pagination, and truncated line text.
Rejects binary (.dat) entries outright per D48.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		if len(args) < 3 {
			return fmt.Errorf("agent_id, entry_id, and query are required. Usage: wackypub agent <agent_id> scratchpad search <entry_id> <query>")
		}
		agentID := args[0]
		entryID := args[1]
		query := args[2]

		var caseSensPtr *bool
		if scratchpadCaseInsensitive {
			val := false
			caseSensPtr = &val
		}

		res, err := sdk.SearchScratchpad(agentID, entryID, query, caseSensPtr, scratchpadRegex, scratchpadMaxResults)
		if err != nil {
			return err
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	},
}

var scratchpadDeleteCmd = &cobra.Command{
	Use:   "delete [agent_id] <entry_id>",
	Short: "Delete a scratchpad entry by ID",
	Long: `Deletes a specific text (.txt) or binary (.dat) scratchpad entry from <ws_dir>/<agent_id>/scratchpad/ per D48.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  entry_id   Required. The 4-character scratchpad entry ID to delete.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		if len(args) < 2 {
			return fmt.Errorf("agent_id and entry_id are required. Usage: wackypub agent <agent_id> scratchpad delete <entry_id>")
		}
		agentID := args[0]
		entryID := args[1]

		if err := sdk.DeleteScratchpad(agentID, entryID); err != nil {
			return err
		}

		fmt.Printf("Deleted scratchpad entry %q for agent %q.\n", entryID, agentID)
		return nil
	},
}

// ExecuteAgentDispatcher handles positional "wackypub agent <agent_id> <add|add-media|generate|prompt|repl|...>" syntax.
func executeAgentDispatcher(cmd *cobra.Command, args []string) error {
	if len(args) >= 2 {
		agentID := args[0]
		subCmd := args[1]

		if subCmd == "add" {
			remainingArgs := []string{agentID}
			if len(args) > 2 {
				remainingArgs = append(remainingArgs, args[2:]...)
			}
			return agentAddCmd.RunE(cmd, remainingArgs)
		} else if subCmd == "add-media" {
			return agentAddMediaCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "generate" {
			return agentGenerateCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "prompt" || subCmd == "turn" {
			remainingArgs := []string{agentID}
			if len(args) > 2 {
				remainingArgs = append(remainingArgs, args[2:]...)
			}
			return agentPromptCmd.RunE(cmd, remainingArgs)
		} else if subCmd == "repl" {
			return agentReplCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "strip-signatures" {
			return agentStripSignaturesCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "read-session" {
			return agentReadSessionCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "read-memory" {
			return agentReadMemoryCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "render-prompt" {
			return agentRenderPromptCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "compact" {
			return agentCompactCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "scratchpad" {
			if len(args) < 3 {
				return scratchpadCmd.Help()
			}
			action := args[2]
			rem := []string{agentID}
			if len(args) > 3 {
				rem = append(rem, args[3:]...)
			}
			switch action {
			case "create":
				return scratchpadCreateCmd.RunE(cmd, rem)
			case "read":
				return scratchpadReadCmd.RunE(cmd, rem)
			case "list":
				return scratchpadListCmd.RunE(cmd, rem)
			case "search":
				return scratchpadSearchCmd.RunE(cmd, rem)
			case "delete":
				return scratchpadDeleteCmd.RunE(cmd, rem)
			default:
				return scratchpadCmd.Help()
			}
		}
	}

	return cmd.Help()
}

func init() {
	// -m is intentionally not used here: RootCmd already binds it to --model (see cmd/root.go).
	// A local -m shorthand on this flagset would collide with that persistent flag and cobra
	// panics on the collision as soon as --help (or completion) merges the two flag sets.
	agentAddCmd.Flags().StringVar(&messageFlag, "message", "", "User message content")
	agentPromptCmd.Flags().StringVar(&messageFlag, "message", "", "User message content")
	agentCompactCmd.Flags().StringVar(&compactMDFile, "md-file", "", "Path to alternate COMPACT.md file to use for compaction recipe")
	agentCompactCmd.Flags().StringVar(&compactRuntimeFile, "runtime", "", "Path to alternate runtime.json file to use for compaction")
	scratchpadCreateCmd.Flags().StringVar(&messageFlag, "message", "", "Scratchpad text payload")

	scratchpadReadCmd.Flags().IntVar(&scratchpadSkipLines, "skip-lines", 0, "Number of lines to skip from start of entry")
	scratchpadReadCmd.Flags().IntVar(&scratchpadNumLines, "num-lines", 0, "Maximum number of lines to return")

	scratchpadSearchCmd.Flags().BoolVar(&scratchpadRegex, "regex", false, "Treat query as a regular expression pattern")
	scratchpadSearchCmd.Flags().BoolVar(&scratchpadCaseInsensitive, "case-insensitive", false, "Perform case-insensitive search (default: false)")
	scratchpadSearchCmd.Flags().IntVar(&scratchpadMaxResults, "max-results", 50, "Maximum number of matching lines to return")

	scratchpadCmd.AddCommand(scratchpadCreateCmd)
	scratchpadCmd.AddCommand(scratchpadReadCmd)
	scratchpadCmd.AddCommand(scratchpadListCmd)
	scratchpadCmd.AddCommand(scratchpadSearchCmd)
	scratchpadCmd.AddCommand(scratchpadDeleteCmd)

	agentCmd.RunE = executeAgentDispatcher

	agentCmd.AddCommand(agentAddCmd)
	agentCmd.AddCommand(agentAddMediaCmd)
	agentCmd.AddCommand(agentGenerateCmd)
	agentCmd.AddCommand(agentPromptCmd)
	agentCmd.AddCommand(agentReplCmd)
	agentCmd.AddCommand(agentStripSignaturesCmd)
	agentCmd.AddCommand(agentReadSessionCmd)
	agentCmd.AddCommand(agentReadMemoryCmd)
	agentCmd.AddCommand(agentRenderPromptCmd)
	agentCmd.AddCommand(agentCompactCmd)
	agentCmd.AddCommand(scratchpadCmd)

	RootCmd.AddCommand(agentCmd)
}
