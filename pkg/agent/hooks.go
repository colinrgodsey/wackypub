package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	EventOnUserMessage        = "on-user-message"
	EnvHooks                  = "WACKYPUB_HOOKS"
	DefaultHookTimeoutSeconds = 5
	EnvHookTimeoutSeconds     = "WACKYPUB_HOOK_TIMEOUT_SECONDS"

	// EventPreCompact fires once CheckAndCompactSession has committed to compacting, before
	// the compaction runner executes. Observe-only: nothing in its HookOutput is applied, so
	// a hook cannot veto or alter the compaction that is about to happen.
	EventPreCompact = "pre-compact"
	// EventPostCompact fires after a compaction completes successfully. Run asynchronously
	// (see runPostCompactHookAsync) so a slow hook script never delays the next turn.
	EventPostCompact = "post-compact"
	// EventCompactFailed fires whenever CheckAndCompactSession returns an error, after it has
	// read at least the session turns for the agent. Run synchronously: there is nothing left
	// to delay, and an async dispatch risks losing the diagnostic if the process exits first.
	EventCompactFailed = "compact-failed"

	// EventPreRunCommand fires in executeTool before a discovered command binary executes.
	// A hook may short-circuit execution by returning HookOutput.Action with
	// Type == HookActionShortCircuit, substituting Action.Output (and optional Action.Stderr)
	// as the successful tool response without executing the real binary.
	EventPreRunCommand = "pre-run-command"

	// EventEndOfTurn fires when a turn reaches its end in GenerateTurnStream. A hook may
	// request an automatic continuation turn by returning HookOutput.Action with
	// Type == HookActionContinue, consuming from D88's general MaxAutoContinuations budget.
	// All registered end-of-turn hooks execute in ascending numeric order; if any hook
	// requests continuation, a continuation turn fires (subject to D88 budget).
	EventEndOfTurn = "end-of-turn"

	// TurnStopReason values delivered in EndOfTurnPayload.StopReason.
	TurnStopCompleted      = "completed"
	TurnStopMaxToolTurns   = "max_tool_turns"
	TurnStopFailureBreaker = "failure_breaker"
	TurnStopCompactedBail  = "compacted_bail"
	TurnStopDeferredImage  = "deferred_image"

	// HookActionShortCircuit is the HookAction.Type value that instructs a pre-execution
	// hook point (such as EventPreRunCommand) to skip running the real target and return
	// HookAction.Output (and optional HookAction.Stderr) as a successful result.
	HookActionShortCircuit = "short_circuit"
	// HookActionContinue is the HookAction.Type value that instructs an end-of-turn hook
	// point (EventEndOfTurn) to schedule a follow-up continuation turn bounded by D88's
	// MaxAutoContinuations budget.
	HookActionContinue = "continue"
)

// HookAction is the unified control-flow extension on HookOutput shared by active hook
// events (EventPreRunCommand and EventEndOfTurn). Passive hooks omit Action; active hooks
// set Action.Type to direct the harness either to short-circuit a pending command
// execution (HookActionShortCircuit) or to request a D88-bounded continuation turn
// (HookActionContinue).
type HookAction struct {
	// Type selects the control-flow directive: HookActionShortCircuit ("short_circuit",
	// or alias "short-circuit" / "replace") or HookActionContinue ("continue").
	Type string `json:"type"`
	// Output carries the replacement stdout text when Type == HookActionShortCircuit,
	// or optional message content for the continuation sentinel turn when Type == HookActionContinue.
	Output string `json:"output,omitempty"`
	// Stderr carries optional replacement stderr text when Type == HookActionShortCircuit.
	Stderr string `json:"stderr,omitempty"`
	// Reason carries an optional diagnostic/audit reason for the action (recorded in the
	// continuation sentinel turn when Type == HookActionContinue).
	Reason string `json:"reason,omitempty"`
}

// normalizeHookActionType canonicalizes supported HookAction.Type values and reports
// whether raw is a recognized control-flow directive.
func normalizeHookActionType(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case HookActionShortCircuit, "short-circuit", "replace":
		return HookActionShortCircuit, true
	case HookActionContinue:
		return HookActionContinue, true
	default:
		return "", false
	}
}

// defaultAllowedActionsForEvent returns the control-flow HookAction.Type directives
// honored by the given hook event when HookOptions.AllowedActions is not explicitly set.
// Passive/observe-only events return nil so no control-flow actions can claim or truncate
// their hook chains.
func defaultAllowedActionsForEvent(event string) []string {
	switch event {
	case EventPreRunCommand:
		return []string{HookActionShortCircuit}
	case EventEndOfTurn:
		return []string{HookActionContinue}
	case EventTurnCompleted, EventTurnFailed:
		// Observe-only by design: a turn that has already ended cannot be altered, so no
		// action is honored and a script that returns one gets a warning. This case is
		// spelled out rather than left to default so the passivity is discoverable.
		return nil
	default:
		return nil
	}
}

func isHookActionAllowed(normType string, allowed []string) bool {
	for _, a := range allowed {
		if n, ok := normalizeHookActionType(a); ok && n == normType {
			return true
		}
	}
	return false
}

// HookOutput defines the JSON schema emitted on stdout by a hook script.
type HookOutput struct {
	Env    map[string]any `json:"env,omitempty"`
	Text   *string        `json:"text,omitempty"`
	Action *HookAction    `json:"action,omitempty"`
}

// HookOptions specifies optional configuration for hook execution.
type HookOptions struct {
	Timeout     time.Duration
	WriteStderr bool
	// InitialEnv, when non-nil, is merged into the starting process environment of the
	// hook chain (after os.Environ()) before the first hook script runs.
	InitialEnv map[string]string
	// AllowedActions, when non-nil, overrides defaultAllowedActionsForEvent(event) for
	// this chain invocation. Any recognized HookAction.Type not in the honored set is
	// ignored with a warning and never claims or truncates the chain.
	AllowedActions []string
}

// HookChainResult contains the final altered text, mutated environment variables,
// any control-flow action directives, and any warnings.
type HookChainResult struct {
	Text       string
	MutatedEnv map[string]string
	Warnings   []string
	// Action holds the effective control-flow directive from the chain: for
	// HookActionShortCircuit, the first hook that short-circuited (which also terminates
	// the chain); for HookActionContinue, the combined continuation directive across all
	// hooks in the chain that requested continuation.
	Action *HookAction
	// Actions holds all valid HookAction directives emitted by hooks in execution order.
	Actions []HookAction
}

func extractLeadingNumber(name string) (int64, bool) {
	var digits []rune
	for _, r := range name {
		if unicode.IsDigit(r) {
			digits = append(digits, r)
		} else {
			break
		}
	}
	if len(digits) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// DiscoverHooks finds all executable scripts under <agentDir>/hooks/<event>/
// sorted in ascending numeric order.
func DiscoverHooks(agentDir string, event string) ([]string, error) {
	hooksDir := filepath.Join(agentDir, "hooks", event)
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read hooks dir %s: %w", hooksDir, err)
	}

	var scripts []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		scriptPath := filepath.Join(hooksDir, entry.Name())
		info, err := os.Stat(scriptPath)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() || (info.Mode()&0111 == 0) {
			continue
		}
		scripts = append(scripts, scriptPath)
	}

	sort.Slice(scripts, func(i, j int) bool {
		baseI := filepath.Base(scripts[i])
		baseJ := filepath.Base(scripts[j])
		numI, hasI := extractLeadingNumber(baseI)
		numJ, hasJ := extractLeadingNumber(baseJ)
		if hasI && hasJ {
			if numI != numJ {
				return numI < numJ
			}
			return baseI < baseJ
		}
		if hasI != hasJ {
			return hasI
		}
		return baseI < baseJ
	})

	return scripts, nil
}

// RunHookChain executes the chain of discovered hook scripts for the given event in agentDir.
// Scripts run sequentially in ascending numeric order; each sees predecessor env mutations
// in its process environment and the current text on stdin.
// Warnings are returned as part of HookChainResult and optionally written to stderr.
func RunHookChain(ctx context.Context, agentDir string, event string, initialText string, opts ...HookOptions) (*HookChainResult, error) {
	if os.Getenv(EnvHooks) == "0" {
		return &HookChainResult{
			Text:       initialText,
			MutatedEnv: nil,
			Warnings:   nil,
		}, nil
	}

	var opt HookOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	var warnings []string
	emitWarning := func(msg string) {
		warnings = append(warnings, msg)
		if opt.WriteStderr {
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	hookPaths, err := DiscoverHooks(agentDir, event)
	if err != nil {
		emitWarning(fmt.Sprintf("Warning: error discovering hooks for event %q in %s: %v", event, agentDir, err))
		return &HookChainResult{
			Text:       initialText,
			MutatedEnv: nil,
			Warnings:   warnings,
		}, nil
	}

	if len(hookPaths) == 0 {
		return &HookChainResult{
			Text:       initialText,
			MutatedEnv: nil,
			Warnings:   nil,
		}, nil
	}

	hookTimeout := time.Duration(DefaultHookTimeoutSeconds) * time.Second
	if opt.Timeout > 0 {
		hookTimeout = opt.Timeout
	} else if envVal := os.Getenv(EnvHookTimeoutSeconds); envVal != "" {
		if sec, err := strconv.Atoi(envVal); err == nil && sec > 0 {
			hookTimeout = time.Duration(sec) * time.Second
		}
	}

	currentEnvMap := make(map[string]string)
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx > 0 {
			currentEnvMap[kv[:idx]] = kv[idx+1:]
		}
	}
	for k, v := range opt.InitialEnv {
		currentEnvMap[k] = v
	}

	mutatedVars := make(map[string]string)
	currentText := initialText
	var winningAction *HookAction
	var actions []HookAction
	allowedActions := opt.AllowedActions
	if allowedActions == nil {
		allowedActions = defaultAllowedActionsForEvent(event)
	}

	for _, hookPath := range hookPaths {
		cmdCtx, cancel := context.WithTimeout(ctx, hookTimeout)

		cmd := exec.CommandContext(cmdCtx, hookPath)
		cmd.Dir = agentDir
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
		}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}

		envSlice := make([]string, 0, len(currentEnvMap))
		for k, v := range currentEnvMap {
			envSlice = append(envSlice, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(envSlice)
		cmd.Env = envSlice

		cmd.Stdin = strings.NewReader(currentText)

		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf

		runErr := cmd.Run()
		cancel()

		if cmdCtx.Err() == context.DeadlineExceeded {
			emitWarning(fmt.Sprintf("Warning: hook %s timed out after %v", hookPath, hookTimeout))
			continue
		}

		if runErr != nil {
			emitWarning(fmt.Sprintf("Warning: hook %s exited with error: %v (stderr: %s)", hookPath, runErr, strings.TrimSpace(stderrBuf.String())))
			continue
		}

		trimmedStdout := bytes.TrimSpace(stdoutBuf.Bytes())
		if len(trimmedStdout) == 0 {
			emitWarning(fmt.Sprintf("Warning: hook %s produced empty output, expected JSON", hookPath))
			continue
		}

		var out HookOutput
		if jsonErr := json.Unmarshal(trimmedStdout, &out); jsonErr != nil {
			emitWarning(fmt.Sprintf("Warning: hook %s produced malformed JSON: %v", hookPath, jsonErr))
			continue
		}

		if out.Text != nil {
			currentText = *out.Text
		}

		if out.Env != nil {
			for k, v := range out.Env {
				if v == nil {
					delete(currentEnvMap, k)
					delete(mutatedVars, k)
				} else {
					var valStr string
					if s, ok := v.(string); ok {
						valStr = s
					} else {
						valStr = fmt.Sprintf("%v", v)
					}
					currentEnvMap[k] = valStr
					mutatedVars[k] = valStr
				}
			}
		}

		if out.Action != nil {
			rawType := strings.TrimSpace(out.Action.Type)
			if rawType == "" && out.Action.Output == "" && out.Action.Stderr == "" && out.Action.Reason == "" {
				continue
			}
			normType, ok := normalizeHookActionType(rawType)
			if !ok {
				emitWarning(fmt.Sprintf("Warning: hook %s produced unsupported action type %q", hookPath, out.Action.Type))
				continue
			}
			if !isHookActionAllowed(normType, allowedActions) {
				emitWarning(fmt.Sprintf("Warning: hook %s produced action type %q which is not honored by event %q", hookPath, normType, event))
				continue
			}
			act := *out.Action
			act.Type = normType
			actions = append(actions, act)
			if winningAction == nil {
				actCopy := act
				winningAction = &actCopy
			} else if winningAction.Type == HookActionContinue && act.Type == HookActionContinue {
				if act.Reason != "" {
					if winningAction.Reason != "" {
						winningAction.Reason += "; " + act.Reason
					} else {
						winningAction.Reason = act.Reason
					}
				}
				if act.Output != "" {
					if winningAction.Output != "" {
						winningAction.Output += "\n" + act.Output
					} else {
						winningAction.Output = act.Output
					}
				}
			}
			// Short-circuiting is an exclusive claim: the first hook to emit an honored
			// short_circuit action wins and terminates the chain so subsequent hooks
			// cannot double-dispatch.
			if normType == HookActionShortCircuit {
				break
			}
		}
	}

	var resMutated map[string]string
	if len(mutatedVars) > 0 {
		resMutated = mutatedVars
	}

	return &HookChainResult{
		Text:       currentText,
		MutatedEnv: resMutated,
		Warnings:   warnings,
		Action:     winningAction,
		Actions:    actions,
	}, nil
}

// RunUserMessageHooks executes the on-user-message hook chain for agentDir using background context.
func RunUserMessageHooks(agentDir string, text string, opts ...HookOptions) (string, map[string]string, []string, error) {
	return RunUserMessageHooksWithContext(context.Background(), agentDir, text, opts...)
}

// RunUserMessageHooksWithContext executes the on-user-message hook chain for agentDir using the provided context.
func RunUserMessageHooksWithContext(ctx context.Context, agentDir string, text string, opts ...HookOptions) (string, map[string]string, []string, error) {
	res, err := RunHookChain(ctx, agentDir, EventOnUserMessage, text, opts...)
	if err != nil {
		return text, nil, nil, err
	}
	return res.Text, res.MutatedEnv, res.Warnings, nil
}

// PreCompactPayload is delivered as JSON text on stdin to every pre-compact hook script,
// exactly the way on-user-message delivers its message text on stdin - the same one shape,
// just carrying a JSON-encoded struct instead of raw text.
type PreCompactPayload struct {
	AgentID       string `json:"agent_id"`
	SessionPath   string `json:"session_path"`
	TurnCount     int    `json:"turn_count"`
	TokenEstimate int    `json:"token_estimate"`
	// Trigger is "auto" (CheckAndCompactSession's own token-threshold check decided to
	// compact) or "forced" (a caller - an explicit CLI/RPC force, or a mid-turn short-circuit
	// that already made the decision itself - asked to compact unconditionally).
	Trigger string `json:"trigger"`
}

// PostCompactPayload is delivered as JSON text on stdin to every post-compact hook script,
// after a compaction has completed successfully.
type PostCompactPayload struct {
	AgentID       string `json:"agent_id"`
	SessionPath   string `json:"session_path"`
	TurnsArchived int    `json:"turns_archived"`
	TokensBefore  int    `json:"tokens_before"`
	TokensAfter   int    `json:"tokens_after"`
	// CompactionModel is the model identifier the compaction runner used, when known.
	CompactionModel string `json:"compaction_model,omitempty"`
	// ToolDenials is the number of tool invocations the compaction model attempted and were
	// denied (D50/CompactionToolDenials); always 0 for a no-tools compaction agent.
	ToolDenials int64 `json:"tool_denials"`
}

// CompactFailedPayload is delivered as JSON text on stdin to every compact-failed hook
// script.
type CompactFailedPayload struct {
	AgentID     string `json:"agent_id"`
	SessionPath string `json:"session_path"`
	// Stage names which phase of CheckAndCompactSession failed (e.g. "read-session",
	// "generation", "write-session"), so a hook can tell a transient I/O failure apart from
	// a genuine compaction-model error without parsing the error text.
	Stage string `json:"stage"`
	Error string `json:"error"`
}

// postCompactHookDone is invoked, if non-nil, with the agentDir immediately after an async
// post-compact hook chain finishes running for that agent. It exists purely so tests can
// observe the fire-and-forget goroutine's completion deterministically instead of
// sleep-polling; production code leaves it nil. Guarded by its own mutex (not
// compaction-critical-path) and keyed by agentDir because many tests in this package compact
// successfully without any awareness of hooks - each installs its own callback and must
// ignore completions for a different agentDir, since a still-running goroutine from an
// earlier, unrelated test can otherwise fire after a later test has installed its own.
var (
	postCompactHookDoneMu sync.Mutex
	postCompactHookDone   func(agentDir string)
)

// runCompactionHookSync marshals payload to JSON and runs the hook chain for a compaction
// lifecycle event synchronously, matching the on-user-message convention exactly: the
// payload travels as JSON text on the same stdin channel RunHookChain already provides, and
// a hook script answers with the same HookOutput JSON schema on stdout. These three events
// are observe-only - any Text/Env a hook returns is discarded here, never applied to
// anything - so a hook failure (timeout, non-zero exit, malformed JSON) can only ever
// produce a warning, never abort or alter the compaction that is already happening.
func runCompactionHookSync(ctx context.Context, agentDir string, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: compaction hook %s: failed to marshal payload: %v\n", event, err)
		return
	}
	res, err := RunHookChain(ctx, agentDir, event, string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: compaction hook %s: %v\n", event, err)
		return
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: compaction hook %s: %s\n", event, w)
	}
}

// runPostCompactHookAsync fires the post-compact hook chain in its own goroutine so a slow
// or hanging hook script never taxes the next turn (D-compaction-hooks). This is
// best-effort: the goroutine is bounded by the same per-hook timeout RunHookChain already
// enforces (~5s default, WACKYPUB_HOOK_TIMEOUT_SECONDS-configurable), but if the process
// exits before it completes, the hook is simply abandoned mid-flight - the same risk any
// fire-and-forget background work carries at process exit. Callers that need every
// post-compact hook to have actually run before a process exits (e.g. a short-lived CLI
// invocation) are not yet served by this v1; that is a documented gap, not an oversight.
func runPostCompactHookAsync(agentDir string, payload PostCompactPayload) {
	go func() {
		runCompactionHookSync(context.Background(), agentDir, EventPostCompact, payload)
		postCompactHookDoneMu.Lock()
		fn := postCompactHookDone
		postCompactHookDoneMu.Unlock()
		if fn != nil {
			fn(agentDir)
		}
	}()
}

// PreRunCommandPayload is delivered as JSON text on stdin to every pre-run-command hook
// script before a dispatched command binary executes.
type PreRunCommandPayload struct {
	AgentID     string            `json:"agent_id"`
	Command     string            `json:"command"`
	CommandPath string            `json:"command_path"`
	Args        []string          `json:"args"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env"`
}

func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			m[kv[:idx]] = kv[idx+1:]
		}
	}
	return m
}

// runPreRunCommandHooks executes the pre-run-command hook chain for agentDir when one or
// more executable hook scripts exist under <agentDir>/hooks/pre-run-command/. When no
// hooks are registered (or hooks are disabled via WACKYPUB_HOOKS=0), it returns nil
// immediately without marshaling the payload or allocating subprocess state.
func runPreRunCommandHooks(ctx context.Context, agentDir, commandName, commandPath string, args, cmdEnv []string) *HookChainResult {
	if os.Getenv(EnvHooks) == "0" {
		return nil
	}
	hookPaths, err := DiscoverHooks(agentDir, EventPreRunCommand)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: pre-run-command hook: error discovering hooks in %s: %v\n", agentDir, err)
		return nil
	}
	if len(hookPaths) == 0 {
		return nil
	}

	argsSlice := args
	if argsSlice == nil {
		argsSlice = []string{}
	}
	envMap := envSliceToMap(cmdEnv)

	payload := PreRunCommandPayload{
		AgentID:     filepath.Base(agentDir),
		Command:     commandName,
		CommandPath: commandPath,
		Args:        argsSlice,
		Cwd:         agentDir,
		Env:         envMap,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: pre-run-command hook: failed to marshal payload: %v\n", err)
		return nil
	}

	res, err := RunHookChain(ctx, agentDir, EventPreRunCommand, string(data), HookOptions{InitialEnv: envMap})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: pre-run-command hook: %v\n", err)
		return nil
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: pre-run-command hook: %s\n", w)
	}
	return res
}

// EndOfTurnPayload is delivered as JSON text on stdin to every end-of-turn hook script
// once a turn reaches its end.
//
// Contract note for continuation consumers: EventEndOfTurn fires on all terminal turn
// paths, including aborted, disabled-continuation, failure-breaker, deferred-image, and
// budget-exhausted paths where CanContinue is false. Those paths are notify-only and
// discard any HookActionContinue emitted by the chain. Only the normal turn completion
// path honors a continuation action (subject to the hard D88 MaxAutoContinuations gate
// in handleContinuationOrCompaction). A hook that performs side effects in exchange for
// a continuation (for example, marking a background completion as notified) MUST check
// CanContinue == true before committing that state change, or the notification will be
// consumed for a continuation turn that will be dropped.
type EndOfTurnPayload struct {
	AgentID           string `json:"agent_id"`
	SessionPath       string `json:"session_path"`
	StopReason        string `json:"stop_reason"`
	DurationMs        int64  `json:"duration_ms"`
	ContinuationCount int    `json:"continuation_count"`
	MaxContinuations  int    `json:"max_continuations"`
	// CanContinue is advisory for hooks: when false, this end-of-turn invocation is
	// notify-only and any HookActionContinue returned by the hook will be dropped.
	CanContinue bool `json:"can_continue"`
}

// runEndOfTurnHooks executes the end-of-turn hook chain for agentDir when one or more
// executable hook scripts exist under <agentDir>/hooks/end-of-turn/. When no hooks are
// registered (or hooks are disabled via WACKYPUB_HOOKS=0), it returns nil immediately
// with zero overhead.
func runEndOfTurnHooks(ctx context.Context, agentDir string, payload EndOfTurnPayload) *HookChainResult {
	if agentDir == "" || os.Getenv(EnvHooks) == "0" {
		return nil
	}
	hookPaths, err := DiscoverHooks(agentDir, EventEndOfTurn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: end-of-turn hook: error discovering hooks in %s: %v\n", agentDir, err)
		return nil
	}
	if len(hookPaths) == 0 {
		return nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: end-of-turn hook: failed to marshal payload: %v\n", err)
		return nil
	}

	res, err := RunHookChain(ctx, agentDir, EventEndOfTurn, string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: end-of-turn hook: %v\n", err)
		return nil
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: end-of-turn hook: %s\n", w)
	}
	return res
}

// buildEndOfTurnContinuationSentinel formats the user sentinel turn persisted to
// session.jsonl from the honored HookActionContinue directives in HookChainResult.Actions,
// and reports whether any continuation action was present.
func buildEndOfTurnContinuationSentinel(actions []HookAction) (string, bool) {
	var reasons []string
	var outputs []string
	hasContinue := false
	for _, a := range actions {
		if a.Type != HookActionContinue {
			continue
		}
		hasContinue = true
		if r := strings.TrimSpace(a.Reason); r != "" {
			reasons = append(reasons, r)
		}
		if o := strings.TrimSpace(a.Output); o != "" {
			outputs = append(outputs, o)
		}
	}
	if !hasContinue {
		return "", false
	}
	reason := "end-of-turn-hook"
	if len(reasons) > 0 {
		reason = strings.Join(reasons, "; ")
	}
	body := "Continuation requested by end-of-turn hook."
	if len(outputs) > 0 {
		body = strings.Join(outputs, "\n")
	} else if len(reasons) > 0 {
		body = fmt.Sprintf("Continuation requested by end-of-turn hook (%s).", reason)
	}
	return fmt.Sprintf("<CONTINUATION reason=%q>%s</CONTINUATION>", reason, body), true
}

// Turn completion events fire once per attempted turn, from the single point in
// runTurnWithRuntimeFallback where the turn has stopped for good: after the final session
// write, after post-turn compaction, and while the session lock is still held, so a consumer
// sees a settled session and cannot be interleaved with continuation turns. They are the
// turn-level counterpart of the compaction family (post-compact, compact-failed):
// observe-only, so defaultAllowedActionsForEvent returns nil for both and any action a script
// returns is reported and discarded rather than honored.
//
// They do NOT fire for aside turns, which deliberately run without the session lock, nor for
// turn-setup failures where no stream was ever attempted.
const (
	// EventTurnCompleted fires when a turn finishes without an error. A turn that stopped
	// because the D88 auto-continuation budget ran out is delivered here, not to
	// EventTurnFailed, with Reason budget_exhausted: no error was surfaced, and the long
	// turns that hit the cap are exactly the ones an always-fresh consumer wants erased.
	EventTurnCompleted = "turn-completed"
	// EventTurnFailed fires when the turn ended by surfacing an error: a watchdog deadline,
	// a stream error, a client cancel, or a consumer that stopped reading.
	EventTurnFailed = "turn-failed"
)

// TurnTerminalReason values delivered in TurnCompletionPayload.Reason.
const (
	TurnTerminalCompleted       = "completed"
	TurnTerminalBudgetExhausted = "budget_exhausted"
	TurnTerminalStreamError     = "stream_error"
	TurnTerminalWatchdog        = "watchdog"
	TurnTerminalConsumerStopped = "consumer_stopped"
	TurnTerminalCancelled       = "cancelled"
)

// autoContinuationBudgetExhaustedNotice is the prefix of the notices
// handleContinuationOrCompaction emits when a D88 budget is spent. It is exported into the
// turn's visible text, and turnCompletionReason classifies on it, so the three emission
// sites in agent_folder.go are built from this constant rather than repeating the literal.
const autoContinuationBudgetExhaustedNotice = "[Auto-continuation budget exhausted"

// TurnCompletionPayload is the JSON handed to the turn-completed / turn-failed chain.
type TurnCompletionPayload struct {
	AgentID    string `json:"agent_id"`
	AgentDir   string `json:"agent_dir"`
	Event      string `json:"event"`
	Reason     string `json:"reason"`
	DurationMs int64  `json:"duration_ms"`
	// Backend names the endpoint/model that served the turn, empty when the level is unknown.
	Backend string `json:"backend,omitempty"`
	// Error is the surfaced turn error, empty for turn-completed.
	Error string `json:"error,omitempty"`
}

// turnCompletionReason classifies a finished turn. It is a pure function so the mapping is
// testable without driving a whole turn: a client cancel outranks everything because the
// operator asked for it, then a surfaced error, then the budget notice in the turn's own
// text, then a clean completion.
// errTurnConsumerStopped names a turn whose reader stopped taking chunks: the turn did not
// fail on its own, but no consumer should read it as completed either.
var errTurnConsumerStopped = errors.New("turn consumer stopped reading")

func turnCompletionReason(turnErr error, ctxErr error, sawBudgetNotice bool) (event string, reason string) {
	if ctxErr != nil {
		return EventTurnFailed, TurnTerminalCancelled
	}
	if turnErr != nil {
		if errors.Is(turnErr, errTurnConsumerStopped) {
			return EventTurnFailed, TurnTerminalConsumerStopped
		}
		if strings.Contains(turnErr.Error(), "deadline exceeded") {
			return EventTurnFailed, TurnTerminalWatchdog
		}
		return EventTurnFailed, TurnTerminalStreamError
	}
	if sawBudgetNotice {
		return EventTurnCompleted, TurnTerminalBudgetExhausted
	}
	return EventTurnCompleted, TurnTerminalCompleted
}

// runTurnCompletionHook runs one turn-completion chain, observe-only: any Text, Env or
// Action a script returns is discarded, so a broken hook can only ever produce a warning and
// can never alter a turn that has already finished (the same contract as
// runCompactionHookSync).
//
// The chain runs on a context detached from the turn's, because the turn context is on its
// way out at this point and a hook would otherwise be born already expired; the detachment
// keeps the context values (A2A metadata and the like) while dropping the cancellation that
// no longer applies. Keep the caller's session lock in mind: this runs synchronously inside
// it, so a slow script taxes teardown by at most the hook timeout, and a consumer that wants
// long work should detach it inside the script.
func runTurnCompletionHook(ctx context.Context, agentDir string, payload TurnCompletionPayload) {
	if agentDir == "" || os.Getenv(EnvHooks) == "0" {
		return
	}
	hookPaths, err := DiscoverHooks(agentDir, payload.Event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s hook: error discovering hooks in %s: %v\n", payload.Event, agentDir, err)
		return
	}
	if len(hookPaths) == 0 {
		return
	}

	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s hook: failed to marshal payload: %v\n", payload.Event, err)
		return
	}

	timeout := time.Duration(DefaultHookTimeoutSeconds) * time.Second
	if envVal := os.Getenv(EnvHookTimeoutSeconds); envVal != "" {
		if secs, err := strconv.Atoi(envVal); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}
	hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	res, err := RunHookChain(hookCtx, agentDir, payload.Event, string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s hook: %v\n", payload.Event, err)
		return
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s hook: %s\n", payload.Event, w)
	}
}
