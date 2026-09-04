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
	"syscall"
	"time"
	"unicode"
)

const (
	EventOnUserMessage        = "on-user-message"
	EnvHooks                  = "WACKYPUB_HOOKS"
	DefaultHookTimeoutSeconds = 5
	EnvHookTimeoutSeconds     = "WACKYPUB_HOOK_TIMEOUT_SECONDS"
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
