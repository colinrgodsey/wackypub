package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// HookOutput defines the JSON schema emitted on stdout by a hook script.
type HookOutput struct {
	Env  map[string]any `json:"env,omitempty"`
	Text *string        `json:"text,omitempty"`
}

// HookOptions specifies optional configuration for hook execution.
type HookOptions struct {
	Timeout     time.Duration
	WriteStderr bool
}

// HookChainResult contains the final altered text, mutated environment variables, and any warnings.
type HookChainResult struct {
	Text       string
	MutatedEnv map[string]string
	Warnings   []string
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

	mutatedVars := make(map[string]string)
	currentText := initialText

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
	}

	var resMutated map[string]string
	if len(mutatedVars) > 0 {
		resMutated = mutatedVars
	}

	return &HookChainResult{
		Text:       currentText,
		MutatedEnv: resMutated,
		Warnings:   warnings,
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
