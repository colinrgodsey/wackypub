package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

var (
	messageFlag        string
	compactMDFile      string
	compactRuntimeFile string
)

// stdinIsPipe reports whether stdin is connected to a pipe (not a TTY). Callers use it to
// decide whether to read a message from stdin. On Stat (or TTY-detection) error we default to
// non-pipe: treating an unreadable stdin as a pipe would make the command hang waiting for
// input that will never arrive.
func stdinIsPipe() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdCtx(cmd *cobra.Command) context.Context {
	if cmd != nil {
		if ctx := cmd.Context(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

func newSDK(wsDir string) *adkAgent.AgentSDK {
	sdk := adkAgent.NewSDK(wsDir)
	sdk.MaxToolTurns = GetMaxToolTurns()
	sdk.CommandTimeoutSeconds = GetCommandTimeoutSeconds()
	return sdk
}

// generateTurnStreamProto drives the D112 Phase 2 streaming RPC for a continue-only turn
// (AgentClient.GenerateTurnStream) and returns an iterator of text chunks for the CLI loop.
func generateTurnStreamProto(sdk *adkAgent.AgentSDK, ctx context.Context, agentID string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		client, cleanup, err := adkAgent.ResolveAgentClient(ctx, sdk, agentID)
		if err != nil {
			yield("", err)
			return
		}
		defer cleanup()

		stream, err := client.GenerateTurnStream(ctx, &agentv1.GenerateTurnStreamRequest{
			AgentId:      agentID,
			WorkspaceDir: sdk.WorkspaceDir,
		})
		if err != nil {
			yield("", err)
			return
		}
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				yield("", err)
				return
			}
			if !yield(chunk.GetText(), nil) {
				return
			}
		}
	}
}

func addAndGenerateTurnStreamProto(sdk *adkAgent.AgentSDK, ctx context.Context, agentID, userMsg string, onWarning func(string)) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		client, cleanup, err := adkAgent.ResolveAgentClient(ctx, sdk, agentID)
		if err != nil {
			yield("", err)
			return
		}
		defer cleanup()

		stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
			AgentId:      agentID,
			UserMessage:  userMsg,
			WorkspaceDir: sdk.WorkspaceDir,
		})
		if err != nil {
			yield("", err)
			return
		}
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				yield("", err)
				return
			}
			if w := chunk.GetWarning(); w != "" {
				if onWarning != nil {
					onWarning(w)
				}
				continue
			}
			if !yield(chunk.GetText(), nil) {
				return
			}
		}
	}
}

// asideQuestionProto drives the D112 protocol surface for a one-shot aside question through
// ResolveAgentClient - so it works for LOCAL folder agents and bridged/remote agents via the
// REMOTE_MANIFEST bridge dispatch, not just the SDK. Returns the complete answer text; the
// protocol path is unary (AsideQuestion), matching the other single-shot RPCs.
func asideQuestionProto(sdk *adkAgent.AgentSDK, ctx context.Context, agentID, question string, onWarning func(string)) (string, error) {
	client, cleanup, err := adkAgent.ResolveAgentClient(ctx, sdk, agentID)
	if err != nil {
		return "", err
	}
	defer cleanup()

	resp, err := client.AsideQuestion(ctx, &agentv1.AsideQuestionRequest{
		AgentId:      agentID,
		Question:     question,
		WorkspaceDir: sdk.WorkspaceDir,
	})
	if err != nil {
		return "", err
	}
	for _, w := range resp.GetWarnings() {
		if onWarning != nil {
			onWarning(w)
		}
	}
	return resp.GetText(), nil
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
			if stdinIsPipe() {
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		turnRes, err := client.AddUserTurn(cmdCtx(cmd), &agentv1.AddUserTurnRequest{
			AgentId:      agentID,
			Message:      userMsg,
			WorkspaceDir: wsDir,
		})
		if err != nil {
			return err
		}
		for _, w := range turnRes.GetWarnings() {
			cmd.PrintErrln(w)
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

		if !stdinIsPipe() {
			return fmt.Errorf("no image data provided on stdin. Pipe an image file, e.g.: wackypub agent %s add-media < image.jpg", agentID)
		}

		data, err := io.ReadAll(io.LimitReader(os.Stdin, adkAgent.MaxMediaPayloadBytes+1))
		if err != nil {
			return fmt.Errorf("failed to read media from stdin: %w", err)
		}
		if len(data) == 0 {
			return fmt.Errorf("no image data provided on stdin. Pipe an image file, e.g.: wackypub agent %s add-media < image.jpg", agentID)
		}
		if len(data) > adkAgent.MaxMediaPayloadBytes {
			return fmt.Errorf("media payload exceeds 10MB limit (%d bytes > %d bytes)", len(data), adkAgent.MaxMediaPayloadBytes)
		}

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.AddMedia(cmdCtx(cmd), &agentv1.AddMediaRequest{
			AgentId:      agentID,
			MediaData:    data,
			WorkspaceDir: wsDir,
		})
		if err != nil {
			return err
		}

		rawSize := resp.GetRawSize()

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

		ctx, stop := signalCtx()
		defer stop()
		first := true
		for text, err := range generateTurnStreamProto(sdk, ctx, agentID) {
			if err != nil {
				return err
			}
			if text != "" {
				if !first {
					fmt.Println()
				}
				fmt.Println(text)
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.StripSignatures(cmdCtx(cmd), &agentv1.StripSignaturesRequest{
			AgentId:      agentID,
			WorkspaceDir: wsDir,
		})
		if err != nil {
			return err
		}

		fmt.Printf("Stripped provider signatures from %d turn(s) in agent %q session (%s/session.jsonl).\n", resp.GetModifiedTurns(), agentID, sdk.AgentDir(agentID))
		return nil
	},
}

// wackypub agent <agent_id> read-session OR wackypub agent read-session <agent_id>
var agentReadSessionCmd = &cobra.Command{
	Use:   "read-session [agent_id]",
	Short: "Print the agent's session.jsonl turn history as JSON",
	Long: `Prints every turn currently stored in <ws_dir>/<agent_id>/session.jsonl to stdout, one
JSON-encoded agentv1.SessionTurn object per line ({"role": "user"|"model", "parts": [...]}).

Note: in v1, SessionTurn models text and inline data parts. Tool invocations
(FunctionCall/FunctionResponse) present in session.jsonl are omitted (accepted-lossy
conversion for v1).

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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.ReadSession(cmdCtx(cmd), &agentv1.ReadSessionRequest{
			AgentId: agentID,
		})
		if err != nil {
			return err
		}

		enc := json.NewEncoder(os.Stdout)
		for _, t := range resp.GetTurns() {
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.ReadMemory(cmdCtx(cmd), &agentv1.ReadMemoryRequest{
			AgentId: agentID,
		})
		if err != nil {
			return err
		}

		fmt.Println(resp.GetMemoryMd())
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.RenderSystemPrompt(cmdCtx(cmd), &agentv1.RenderSystemPromptRequest{
			AgentId: agentID,
		})
		if err != nil {
			return err
		}

		fmt.Println(resp.GetRenderedPrompt())
		return nil
	},
}

// wackypub agent <agent_id> compact OR wackypub agent compact <agent_id>
var agentCancelCmd = &cobra.Command{
	Use:   "cancel [agent_id]",
	Short: "Request cancellation of an agent's in-flight turn",
	Long: `Sends a cancellation request to the turn currently running for an agent, whether that
turn runs in this process or in another one, and prints which one it reached.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).

Authorization is the same WACKYPUB_ALLOWED_AGENTS gate that prompt, add, and generate
enforce, evaluated from the current working directory: cancelling ends another agent's
work, so it is a mutating call, not a diagnostic.

A turn holds the agent's session lock for its whole duration, which is how an outside
process finds it. The lock is probed without ever queueing behind it, and only a holder
that is a wackypub process receives SIGTERM, which is the same stop request that an
operator gets from Ctrl-C in that terminal. The turn unwinds at its existing cancellation
checkpoints: partial assistant text is not committed, the user message that started the
turn stays in session.jsonl, and the session lock is released, so the agent is immediately
usable again rather than wedged.

A lock file naming a process that no longer exists is leftover metadata from a finished
turn, not an in-flight turn: nothing is signalled, and the command exits non-zero saying
no turn is running. Use "wackypub workspace locks" to see holders, PIDs, and how long
each session has been quiet.`,
	Args: cobra.MaximumNArgs(1),
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
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> cancel")
		}

		result, err := sdk.CancelAgentTurn(agentID)
		if err != nil {
			return err
		}
		if result.Scope == adkAgent.CancelScopeInProcess {
			fmt.Printf("Cancelled the in-flight turn for agent %q.\n", result.AgentID)
			return nil
		}
		fmt.Printf("Sent SIGTERM to pid %d (%s), which holds agent %q's session lock; its turn stops at the next cancellation checkpoint.\n",
			result.PID, result.Program, result.AgentID)
		return nil
	},
}

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

		ctx, stop := signalCtx()
		defer stop()

		var cfgOverride *agentv1.CompactConfigOverride
		if compactCfg != nil {
			cfgOverride = &agentv1.CompactConfigOverride{
				AppendOnly:         compactCfg.AppendOnly,
				CompactPct:         compactCfg.CompactPct,
				CompactOverheadPct: compactCfg.CompactOverheadPct,
				CompactionNotice:   compactCfg.CompactionNotice,
				Prompt:             compactCfg.Prompt,
			}
		}

		client, cleanup, err := adkAgent.ResolveAgentClient(ctx, sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.CompactSession(ctx, &agentv1.CompactSessionRequest{
			AgentId:        agentID,
			Force:          true,
			ConfigOverride: cfgOverride,
			RuntimePath:    compactRuntimeFile,
			WorkspaceDir:   wsDir,
		})
		if err != nil {
			return err
		}

		if resp.GetCompacted() {
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
			if stdinIsPipe() {
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

		ctx, stop := signalCtx()
		defer stop()
		first := true
		for text, err := range addAndGenerateTurnStreamProto(sdk, ctx, agentID, userMsg, func(w string) {
			cmd.PrintErrln(w)
		}) {
			if err != nil {
				return err
			}
			if text != "" {
				if !first {
					fmt.Println()
				}
				fmt.Println(text)
				first = false
			}
		}
		return nil
	},
}

// wackypub agent aside [agent_id] [message] OR wackypub agent aside <agent_id> <question>
var agentAsideCmd = &cobra.Command{
	Use:   "aside [agent_id] [message]",
	Short: "One-shot question on a forked in-memory session (tools denied, nothing persisted)",
	Long: `Asks the agent a one-shot question using its accumulated session context WITHOUT any
side effects: the session is forked in memory (same disposable-session shape as compaction),
tool invocation is denied (the model sees its tools but cannot run them), and nothing is
persisted - session.jsonl, MEMORY.md, scratchpad/, workspace git/trace events, and usage all
stay untouched. The aside takes no exclusive session lock, so it never blocks a live turn.

Arguments:
  agent_id   Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  message    The aside question. Can also be supplied via the --message flag, or piped in on
             stdin. Exactly one of these three must be provided.

Prints the streamed aside answer to stdout like a normal prompt. The main session is
byte-identical afterward.`,
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
			if stdinIsPipe() {
				reader := bufio.NewReader(os.Stdin)
				bytesInput, err := io.ReadAll(reader)
				if err == nil {
					userMsg = string(bytesInput)
				}
			}
		}

		if agentID == "" {
			return fmt.Errorf("agent_id is required. Usage: wackypub agent <agent_id> aside [message]")
		}
		if userMsg == "" {
			return fmt.Errorf("message is required. Provide via argument, --message flag, or stdin pipe")
		}

		ctx, stop := signalCtx()
		defer stop()
		text, err := asideQuestionProto(sdk, ctx, agentID, userMsg, func(w string) {
			cmd.PrintErrln(w)
		})
		if err != nil {
			return err
		}
		if text != "" {
			fmt.Println(text)
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
		ctx, stop := signalCtx()
		defer stop()
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
			for text, err := range addAndGenerateTurnStreamProto(sdk, ctx, agentID, line, func(w string) {
				cmd.PrintErrln(w)
			}) {
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					break
				}
				if text != "" {
					if !first {
						fmt.Println()
					}
					fmt.Println(text)
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
			if stdinIsPipe() {
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.CreateScratchpad(cmdCtx(cmd), &agentv1.CreateScratchpadRequest{
			AgentId:      agentID,
			Text:         content,
			CreatedBy:    "cli",
			WorkspaceDir: wsDir,
		})
		if err != nil {
			return err
		}

		entry := resp.GetEntry()
		if len(entry.GetWarnings()) > 0 {
			for _, w := range entry.GetWarnings() {
				cmd.PrintErrln(w)
			}
		}

		fmt.Printf("Created scratchpad entry %q (%d bytes) for agent %q.\n", entry.GetEntryId(), entry.GetSize(), agentID)
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		req := &agentv1.GetScratchpadRequest{
			AgentId:      agentID,
			EntryId:      entryID,
			WorkspaceDir: wsDir,
		}
		if cmd.Flags().Changed("skip-lines") {
			s := int32(scratchpadSkipLines)
			req.SkipLines = &s
		}
		if cmd.Flags().Changed("num-lines") {
			n := int32(scratchpadNumLines)
			req.NumLines = &n
		}

		resp, err := client.GetScratchpad(cmdCtx(cmd), req)
		if err != nil {
			return err
		}

		out := resp.GetText()
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") && out != "" {
			fmt.Println()
		}
		return nil
	},
}

var scratchpadDiffCmd = &cobra.Command{
	Use:   "diff [agent_id] <before_id> <after_id>",
	Short: "Unified diff between two scratchpad entries",
	Long: `Renders a unified diff between two text entries of one agent, so an edit can be verified
without re-reading either version into context. Both entries are read in full from
<ws_dir>/<agent_id>/scratchpad/ by ID, and nothing is written.

Arguments:
  agent_id    Required. Identifies the agent directory (<ws_dir>/<agent_id>).
  before_id   Required. Entry ID holding the earlier state.
  after_id    Required. Entry ID holding the later state.

Identical entries print nothing and exit 0. An entry ID that does not exist, or that is not a
valid ID, exits 2 and writes nothing. Binary entries are rejected the way the read verb rejects
them. A diff too large to read in one turn stays ordinary output: pipe it into the create verb to
store it as its own entry, then search and paginate that entry.

Typical loop: snapshot the files a refactor will touch, run the refactor, snapshot them again,
diff the pairs, then search the resulting patch for a symbol that should be gone entirely.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		if len(args) < 3 {
			return fmt.Errorf("agent_id, before_id and after_id are required. Usage: wackypub agent <agent_id> scratchpad diff <before_id> <after_id>")
		}
		agentID := args[0]

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.DiffScratchpadEntries(cmdCtx(cmd), &agentv1.DiffScratchpadEntriesRequest{
			AgentId:       args[0],
			BeforeEntryId: args[1],
			AfterEntryId:  args[2],
			WorkspaceDir:  wsDir,
		})
		if err != nil {
			return err
		}
		fmt.Print(resp.GetDiff())
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		resp, err := client.ListScratchpads(cmdCtx(cmd), &agentv1.ListScratchpadsRequest{
			AgentId:      agentID,
			WorkspaceDir: wsDir,
		})
		if err != nil {
			return err
		}

		items := make([]adkAgent.ScratchpadItem, len(resp.GetEntries()))
		for i, e := range resp.GetEntries() {
			items[i] = adkAgent.ScratchpadItem{
				ID:        e.GetEntryId(),
				Size:      int(e.GetSize()),
				Lines:     int(e.GetLines()),
				CreatedBy: e.GetCreatedBy(),
				IsBinary:  e.GetIsBinary(),
				MIMEType:  e.GetMimeType(),
			}
		}

		result := struct {
			Entries []adkAgent.ScratchpadItem `json:"entries"`
			Count   int                       `json:"count"`
			Cap     int                       `json:"cap"`
		}{
			Entries: items,
			Count:   int(resp.GetTotalEntries()),
			Cap:     int(resp.GetMaxCapacity()),
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		req := &agentv1.SearchScratchpadRequest{
			AgentId:      agentID,
			EntryId:      entryID,
			Query:        query,
			UseRegex:     scratchpadRegex,
			MaxResults:   int32(scratchpadMaxResults),
			WorkspaceDir: wsDir,
		}
		if scratchpadCaseInsensitive {
			val := false
			req.CaseSensitive = &val
		}

		resp, err := client.SearchScratchpad(cmdCtx(cmd), req)
		if err != nil {
			return err
		}

		matches := make([]adkAgent.ScratchpadMatch, len(resp.GetMatches()))
		for i, m := range resp.GetMatches() {
			matches[i] = adkAgent.ScratchpadMatch{
				Line:      int(m.GetLine()),
				SkipLines: int(m.GetSkipLines()),
				Text:      m.GetText(),
			}
		}

		res := &adkAgent.SearchScratchpadResult{
			ID:           resp.GetEntryId(),
			Query:        resp.GetQuery(),
			TotalMatches: int(resp.GetTotalMatches()),
			MaxResults:   int(resp.GetMaxResults()),
			Matches:      matches,
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

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		_, err = client.DeleteScratchpad(cmdCtx(cmd), &agentv1.DeleteScratchpadRequest{
			AgentId:      agentID,
			EntryId:      entryID,
			WorkspaceDir: wsDir,
		})
		if err != nil {
			return err
		}

		fmt.Printf("Deleted scratchpad entry %q for agent %q.\n", entryID, agentID)
		return nil
	},
}

// ExecuteAgentDispatcher handles positional "wackypub agent <agent_id> <add|add-media|generate|prompt|cancel|repl|...>" syntax.
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
		} else if subCmd == "cancel" {
			return agentCancelCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "context" {
			return agentContextCmd.RunE(cmd, []string{agentID})
		} else if subCmd == "watch" {
			remainingArgs := []string{agentID}
			if len(args) > 2 {
				remainingArgs = append(remainingArgs, args[2:]...)
			}
			return agentWatchCmd.RunE(cmd, remainingArgs)
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
			case "diff":
				return scratchpadDiffCmd.RunE(cmd, rem)
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
	scratchpadCmd.AddCommand(scratchpadDiffCmd)
	scratchpadCmd.AddCommand(scratchpadListCmd)
	scratchpadCmd.AddCommand(scratchpadSearchCmd)
	scratchpadCmd.AddCommand(scratchpadDeleteCmd)

	agentCmd.RunE = executeAgentDispatcher

	agentCmd.AddCommand(agentAddCmd)
	agentCmd.AddCommand(agentAddMediaCmd)
	agentCmd.AddCommand(agentGenerateCmd)
	agentCmd.AddCommand(agentPromptCmd)
	agentCmd.AddCommand(agentAsideCmd)
	agentCmd.AddCommand(agentReplCmd)
	agentCmd.AddCommand(agentCancelCmd)
	agentCmd.AddCommand(agentStripSignaturesCmd)
	agentCmd.AddCommand(agentReadSessionCmd)
	agentCmd.AddCommand(agentReadMemoryCmd)
	agentCmd.AddCommand(agentRenderPromptCmd)
	agentCmd.AddCommand(agentCompactCmd)
	agentCmd.AddCommand(scratchpadCmd)
	agentContextCmd.Flags().BoolVar(&agentContextJSONFlag, "json", false, "Output report as JSON")
	agentCmd.AddCommand(agentContextCmd)

	agentWatchCmd.Flags().BoolVar(&watchRawFlag, "raw", false, "Emit JSONL-identical lines byte-for-byte")
	agentWatchCmd.Flags().Int64Var(&watchSinceSeqFlag, "since-seq", 0, "Resume streaming strictly after sequence number N")
	agentWatchCmd.Flags().Int32Var(&watchLastFlag, "last", 0, "Replay the last N events before live streaming")

	agentCmd.Flags().BoolVar(&watchRawFlag, "raw", false, "Emit JSONL-identical lines byte-for-byte")
	agentCmd.Flags().Int64Var(&watchSinceSeqFlag, "since-seq", 0, "Resume streaming strictly after sequence number N")
	agentCmd.Flags().Int32Var(&watchLastFlag, "last", 0, "Replay the last N events before live streaming")
	agentCmd.AddCommand(agentWatchCmd)

	RootCmd.AddCommand(agentCmd)
}

var agentContextJSONFlag bool

var agentContextCmd = &cobra.Command{
	Use:   "context [agent_id]",
	Short: "Inspect session context usage, limits, and token headroom",
	RunE: func(cmd *cobra.Command, args []string) error {
		var agentID string
		if len(args) > 0 {
			agentID = args[0]
		}
		if agentID == "" {
			return fmt.Errorf("agent ID is required")
		}
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		client, cleanup, err := adkAgent.ResolveAgentClient(cmdCtx(cmd), sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		report, err := client.InspectSessionContext(cmdCtx(cmd), &agentv1.InspectSessionContextRequest{
			AgentId: agentID,
		})
		if err != nil {
			return err
		}
		if agentContextJSONFlag {
			data, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return fmt.Errorf("serializing agent context report: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}

		fmt.Printf("Agent:       %s\n", report.GetAgentId())
		fmt.Printf("Model:       %s\n", report.GetModel())
		fmt.Printf("Context:     %d tokens (Threshold: %d / %.0f%% overhead)\n",
			report.GetContextWindow(), report.GetCompactionThreshold(), report.GetCompactionOverheadPct())
		fmt.Printf("Estimated:   %d total tokens (%.1f%% of threshold, %.1f%% of window)\n",
			report.GetEstimatedTotalTokens(), report.GetPercentToThreshold(), report.GetPercentToWindow())
		fmt.Printf("Breakdown:   %d turns tokens + %d prompt tokens + %d memory tokens\n",
			report.GetSessionTurnsTokens(), report.GetPromptTokensEstimate(), report.GetMemoryTokensEstimate())
		fmt.Printf("Session:     %d turns\n", report.GetTurnCount())
		if report.GetCompacted() {
			fmt.Printf("Compacted:   yes (session compacted, provider tokens reset)\n")
		} else if report.GetLastTotalTokens() > 0 {
			fmt.Printf("Last Call:   %d prompt + %d candidates = %d total tokens\n",
				report.GetLastPromptTokens(), report.GetLastCandidatesTokens(), report.GetLastTotalTokens())
		}
		return nil
	},
}

var (
	watchRawFlag      bool
	watchSinceSeqFlag int64
	watchLastFlag     int32
)

var agentWatchCmd = &cobra.Command{
	Use:   "watch [agent_id]",
	Short: "Stream or poll session events for an agent (turns, tool calls, compaction)",
	Long: `Streams session events (turns, tool calls, tool results, and compaction notices) for an agent.

Caveats for consumers:
  1. Two-file interleaving: The event stream merges records from both session.jsonl
     (turns) and tool-journal.jsonl (tool calls and results), strictly ordered by monotonic
     sequence number (seq). It represents the unified agent activity stream rather than
     session.jsonl alone.
  2. Trailing newlines: In --raw mode, each emitted line is the exact byte-for-byte JSONL
     record as persisted on disk, with newline termination normalized per line.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		defer func() {
			watchRawFlag = false
			watchSinceSeqFlag = 0
			watchLastFlag = 0
		}()

		var agentID string
		if len(args) > 0 {
			agentID = args[0]
		}
		if agentID == "" {
			return fmt.Errorf("agent_id required")
		}

		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return err
		}
		sdk := newSDK(wsDir)

		ctx, cancel := signal.NotifyContext(cmdCtx(cmd), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		client, cleanup, err := adkAgent.ResolveAgentClient(ctx, sdk, agentID)
		if err != nil {
			return err
		}
		defer cleanup()

		req := &agentv1.SubscribeSessionRequest{
			AgentId:      agentID,
			WorkspaceDir: wsDir,
		}
		if watchSinceSeqFlag > 0 {
			req.Cursor = &agentv1.SubscribeSessionRequest_SinceSeq{SinceSeq: watchSinceSeqFlag}
		} else if watchLastFlag > 0 {
			req.Cursor = &agentv1.SubscribeSessionRequest_LastN{LastN: watchLastFlag}
		}

		stream, err := client.SubscribeSession(ctx, req)
		if err != nil {
			return err
		}

		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF || ctx.Err() != nil {
					return nil
				}
				return err
			}

			if resp.GetDroppedEvents() > 0 {
				if !watchRawFlag {
					fmt.Fprintf(os.Stderr, "[Notice: %d events dropped due to slow consumer]\n", resp.GetDroppedEvents())
				}
			}

			ev := resp.GetEvent()
			if ev == nil {
				continue
			}

			if watchRawFlag {
				if ev.GetRaw() != "" {
					fmt.Println(ev.GetRaw())
				}
			} else {
				renderHumanEvent(ev)
			}
		}
	},
}

func renderHumanEvent(ev *agentv1.SessionEvent) {
	if ev == nil {
		return
	}
	switch e := ev.GetEvent().(type) {
	case *agentv1.SessionEvent_Turn:
		turn := e.Turn
		var textParts []string
		for _, p := range turn.GetParts() {
			if p.GetText() != "" {
				textParts = append(textParts, p.GetText())
			}
		}
		text := strings.Join(textParts, " ")
		fmt.Printf("[Turn #%d %s] %s\n", ev.GetSeq(), turn.GetRole(), strings.TrimSpace(text))

	case *agentv1.SessionEvent_ToolCall:
		tc := e.ToolCall
		if tc.GetDenied() {
			fmt.Printf("[Tool #%d DENIED] %s(%s)\n", ev.GetSeq(), tc.GetToolName(), tc.GetArgsSummary())
		} else {
			fmt.Printf("[Tool #%d] %s(%s) [call_id=%s]\n", ev.GetSeq(), tc.GetToolName(), tc.GetArgsSummary(), tc.GetCallId())
		}

	case *agentv1.SessionEvent_ToolCallUpdate:
		tcu := e.ToolCallUpdate
		head := tcu.GetResultHead()
		if len(head) > 80 {
			head = head[:77] + "..."
		}
		fmt.Printf("[Tool #%d %s] %s (bytes=%d) [call_id=%s]\n", ev.GetSeq(), strings.ToUpper(tcu.GetStatus()), head, tcu.GetResultBytes(), tcu.GetCallId())

	case *agentv1.SessionEvent_Compaction:
		comp := e.Compaction
		fmt.Printf("[Compaction #%d] %s\n", ev.GetSeq(), strings.TrimSpace(comp.GetSummary()))

	case *agentv1.SessionEvent_TokenDelta:
		fmt.Print(e.TokenDelta)

	case *agentv1.SessionEvent_Progress:
		fmt.Printf("[Progress #%d] %s\n", ev.GetSeq(), e.Progress)

	case *agentv1.SessionEvent_Usage:
		fmt.Printf("[Usage #%d] prompt=%d completion=%d total=%d\n", ev.GetSeq(), e.Usage.GetPromptTokens(), e.Usage.GetCompletionTokens(), e.Usage.GetTotalTokens())
	}
}
