package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
)

// DefaultHTTPTimeoutSeconds is the default timeout (15 minutes) for HTTP client calls to LLM backends.
const DefaultHTTPTimeoutSeconds = 900

// RuntimeConfig represents the agent's runtime.json configuration.
type RuntimeConfig struct {
	// Provider selects the model provider: "openai" (default when Endpoint is set),
	// "gemini" (default when Endpoint is empty), or "anthropic".
	Provider string `json:"provider,omitempty"`

	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	APIKey        string `json:"apiKey"`
	ContextWindow int    `json:"contextWindow"`
	// MaxOutputReserve is the token headroom kept free for the model's reply. The
	// mid-turn budget stop fires at ContextWindow - MaxOutputReserve - safety margin
	// instead of a flat share of the window. Unset or <= 0 selects
	// DefaultMaxOutputReserveTokens.
	MaxOutputReserve int `json:"maxOutputReserve,omitempty"`

	// TimeoutSeconds sets the HTTP client timeout in seconds for API calls to the LLM backend.
	// Defaults to DefaultHTTPTimeoutSeconds (900s / 15 minutes) when unset or <= 0.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// Anthropic-specific thinking fields:
	AnthropicThinkingBudgetTokens *int   `json:"anthropicThinkingBudgetTokens,omitempty"`
	AnthropicThinkingEffort       string `json:"anthropicThinkingEffort,omitempty"`
	AnthropicThinkingMode         string `json:"anthropicThinkingMode,omitempty"`

	// Gemini-specific thinking fields:
	GeminiThinkingBudget  *int   `json:"geminiThinkingBudget,omitempty"`
	GeminiThinkingLevel   string `json:"geminiThinkingLevel,omitempty"`
	GeminiIncludeThoughts *bool  `json:"geminiIncludeThoughts,omitempty"`

	// OpenAI / OpenRouter-specific reasoning fields:
	ReasoningEffort          string         `json:"reasoningEffort,omitempty"`
	ReasoningEgress          string         `json:"reasoningEgress,omitempty"`
	ReasoningField           string         `json:"reasoningField,omitempty"`
	SupportsReasoningDetails bool           `json:"supportsReasoningDetails,omitempty"`
	ExtraBody                map[string]any `json:"extraBody,omitempty"`

	// ExtraHeaders overrides the default identifying HTTP headers
	// (X-Title, HTTP-Referer) sent on every request - a key present here
	// replaces the default of the same name. See D43.
	ExtraHeaders map[string]string `json:"extraHeaders,omitempty"`

	// Generic thinking aliases (fallback if provider-specific fields are unset):
	ThinkingBudgetTokens *int   `json:"thinkingBudgetTokens,omitempty"`
	ThinkingEffort       string `json:"thinkingEffort,omitempty"`
	ThinkingMode         string `json:"thinkingMode,omitempty"`

	// PreserveThinking should be set for backends that resend and bill for
	// prior reasoning/thinking text on every turn (e.g. Kimi K2 Thinking,
	// DeepSeek V4 thinking mode, or any provider used with reasoning egress
	// enabled). When true, EstimateTokens includes Thought-marked part text
	// in its count, since that text is actually replayed to the model on
	// every subsequent request and consumes real context budget. Leave false
	// for backends that drop or ignore reasoning_content in history by
	// default (e.g. Qwen3), where thinking never counts toward future
	// requests' token usage.
	PreserveThinking bool `json:"preserveThinking,omitempty"`

	// MaxImageDimension gates "wackypub agent <id> add-media" (D47): the
	// longer side, in pixels, an attached image is downscaled to fit (never
	// upscaled). Absent or <= 0 means image attachments are rejected outright
	// - image support is opt-in per agent, not on by default.
	MaxImageDimension int `json:"maxImageDimension,omitempty"`

	// MaxAutoContinuations limits auto-continuation turns for this agent (D88).
	// When unset, defaults to DefaultMaxAutoContinuations (2) or DefaultMaxAutoContinuationsA2A (1) for A2A turns.
	MaxAutoContinuations *int `json:"maxAutoContinuations,omitempty"`

	// DisableAutoContinuation disables automatic continuation turns (D88).
	DisableAutoContinuation bool `json:"disableAutoContinuation,omitempty"`

	// Fallback is an optional fully-specified RuntimeConfig used when the primary
	// backend fails with a qualifying error (transport, 429-after-retries, 5xx) at turn
	// setup before any text has been yielded. Each level is self-describing - no sparse
	// inheritance - and may itself carry a Fallback, giving a recursive failover chain.
	// Validated at load time: depth capped at MaxFallbackDepth, cycles rejected.
	Fallback *RuntimeConfig `json:"fallback,omitempty"`
}

// MaxFallbackDepth caps how deeply a runtime.json fallback chain may nest. Inline JSON
// nesting makes true cycles structurally impossible (a config can only embed its children),
// so the caps guards pathological hand-edits and bounds the failover fan-out.
const MaxFallbackDepth = 4

// DefaultRuntimeJSON holds examples/runtimes/openrouter-auto.json's content (D74).
//
// Set from main.go, which embeds examples/runtimes/openrouter-auto.json and assigns
// it here before cmd.Execute() runs - mirrors DefaultCompactMD (D45), required because
// examples/ isn't reachable by a //go:embed directive living in pkg/agent.
// Tests populate this themselves (see TestMain in test files) or via direct assignment.
var DefaultRuntimeJSON string

// LoadRuntimeConfig reads and unmarshals runtime.json for an agent.
// Loads workspace root and per-agent .env files, expands environment variables (${VAR} / $VAR) in runtime.json data,
// and handles symlinks transparently using os.ReadFile / filepath.EvalSymlinks.
// Falls back to DefaultRuntimeJSON (bundled openrouter-auto) when runtime.json is absent (D74).
func LoadRuntimeConfig(agentDir string) (*RuntimeConfig, error) {
	// 0. Load root and per-agent .env files into environment
	_, _ = LoadAgentDotEnv(agentDir)

	runtimePath := filepath.Join(agentDir, "runtime.json")

	// Resolve symlink if runtimePath is a symlink
	realPath, err := filepath.EvalSymlinks(runtimePath)
	if err != nil {
		if os.IsNotExist(err) {
			// If missing, check if runtimePath itself exists (in case EvalSymlinks failed because dest is missing)
			realPath = runtimePath
		} else {
			realPath = runtimePath
		}
	}

	data, err := os.ReadFile(realPath)
	usingDefault := false
	if err != nil {
		if os.IsNotExist(err) && DefaultRuntimeJSON != "" {
			data = []byte(DefaultRuntimeJSON)
			usingDefault = true
		} else {
			return nil, fmt.Errorf("failed to read runtime config from %s: %w", runtimePath, err)
		}
	}

	// Expand environment variables (${VAR} / $VAR) in runtime.json data
	expandedData := os.ExpandEnv(string(data))

	var cfg RuntimeConfig
	if err := json.Unmarshal([]byte(expandedData), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse runtime.json at %s: %w", runtimePath, err)
	}

	if usingDefault && cfg.APIKey == "" {
		return nil, fmt.Errorf("no runtime.json found for agent in %s; using bundled default openrouter-auto configuration, but OPENROUTER_API_KEY is not set (see examples/runtimes/README.md for setup instructions)", agentDir)
	}

	normalizeProviderDefaults(&cfg)
	if err := validateFallbackChain(&cfg); err != nil {
		return nil, fmt.Errorf("invalid runtime.json at %s: %w", runtimePath, err)
	}

	return &cfg, nil
}

// normalizeProviderDefaults applies the provider defaulting rule to a config and each of
// its recursive fallback levels. Fallback levels are fully-specified configs, but the
// "endpoint set => openai, else gemini" shorthand is still honored per level so a fallback
// does not silently inherit the root provider.
func normalizeProviderDefaults(cfg *RuntimeConfig) {
	for cur := cfg; cur != nil; cur = cur.Fallback {
		provider := strings.ToLower(strings.TrimSpace(cur.Provider))
		if provider == "" {
			if cur.Endpoint != "" {
				provider = "openai"
			} else {
				provider = "gemini"
			}
		}
		cur.Provider = provider
	}
}

// validateFallbackChain walks the nested fallback pointers, enforcing the depth cap.
// Called at load time so a broken chain fails fast instead of at turn setup. Inline JSON
// nesting makes true pointer cycles structurally impossible (each unmarshaled level is a
// distinct object that can only embed its children), so the depth cap is the guard.
func validateFallbackChain(cfg *RuntimeConfig) error {
	depth := 0
	for cur := cfg; cur != nil; cur = cur.Fallback {
		if depth > MaxFallbackDepth {
			return fmt.Errorf("runtime fallback chain exceeds maximum depth %d: nested fallback at level %d", MaxFallbackDepth, depth)
		}
		depth++
	}
	return nil
}

// FallbackChain returns the ordered list of runtime configs to attempt for one turn:
// the primary first, then each nested fallback in depth order. Always non-empty (holds
// at least the receiver). Each element is fully-specified; no inheritance is applied.
func (c *RuntimeConfig) FallbackChain() []*RuntimeConfig {
	var chain []*RuntimeConfig
	for cur := c; cur != nil; cur = cur.Fallback {
		chain = append(chain, cur)
	}
	return chain
}

// IsQualifyingFallbackError reports whether a turn setup/generation error may flip to the
// next runtime fallback backend. Qualifying classes: transport failures (connection refused,
// timeout, DNS), 429-after-retries, and 5xx server errors. Explicitly NOT qualifying: 401/403
// auth failures (fallback would repeat them or mask credential rot - fail loud) and empty
// model output (a quality judgment, not an availability signal).
// urlRegex matches a quoted URL inside an error message so status-code
// substrings inside it (scheme, host, ephemeral port) cannot misclassify the
// error as another status (bug: httptest ports containing "401"/"403" were
// rejecting qualifying 429/503 fallback errors).
var urlRegex = regexp.MustCompile(`\"https?://[^\"]+\"`)

func IsQualifyingFallbackError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// Strip quoted URLs before numeric checks: httptest picks ephemeral ports that
	// can contain the digits of another status code (e.g. :44017 -> "401"), which
	// would otherwise misclassify a genuinely qualifying transport/429/5xx error.
	msg = urlRegex.ReplaceAllString(msg, " ")

	// Non-qualifying checks first so a message containing both "401" and "connection"
	// (e.g. a proxy 401 wrapping a dial failure) never flips.
	if strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "invalid api key") || strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "empty response from agent") {
		return false
	}

	// Transport: refused, reset, timeout, DNS, handshake, unreachable.
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") || strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "tls handshake") || strings.Contains(msg, "read: connection") ||
		strings.Contains(msg, "write: connection") || strings.Contains(msg, "lookup ") ||
		strings.Contains(msg, "eof") || strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "http: connection has been closed") {
		return true
	}

	// Rate limit after retries: 429 is conventionally the qualifying marker.
	if strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests") {
		return true
	}

	// 5xx server errors.
	for _, code := range []string{"500", "501", "502", "503", "504", "505", "507", "508"} {
		if strings.Contains(msg, code) {
			return true
		}
	}

	return false
}

// LoadRuntimeConfigFile reads and unmarshals a RuntimeConfig from an explicit file path (D84).
// Resolves symlinks, expands environment variables (${VAR} / $VAR), and sets default provider.
// Returns an error if the file is absent, unreadable, or invalid JSON (no bundled-default fallback).
func LoadRuntimeConfigFile(path string) (*RuntimeConfig, error) {
	if path == "" {
		return nil, fmt.Errorf("failed to read runtime config file: path cannot be empty")
	}

	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath = path
	}

	data, err := os.ReadFile(realPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime config file: %w", err)
	}

	expandedData := os.ExpandEnv(string(data))

	var cfg RuntimeConfig
	if err := json.Unmarshal([]byte(expandedData), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse runtime config file: %w", err)
	}

	normalizeProviderDefaults(&cfg)
	if err := validateFallbackChain(&cfg); err != nil {
		return nil, fmt.Errorf("invalid runtime config at %s: %w", path, err)
	}

	return &cfg, nil
}

// NewModelForRuntime initializes an ADK model.LLM adapter according to runtimeCfg.Provider.
func NewModelForRuntime(ctx context.Context, runtimeCfg *RuntimeConfig, agentID string) (model.LLM, error) {
	if runtimeCfg == nil {
		return nil, fmt.Errorf("runtime config cannot be nil")
	}

	switch runtimeCfg.Provider {
	case "anthropic":
		return NewAnthropicModel(runtimeCfg), nil
	case "openai", "openai-compatible":
		return NewOpenAIModel(runtimeCfg), nil
	case "gemini":
		geminiModel, err := CreateGeminiModel(ctx, runtimeCfg.Model, runtimeCfg.APIKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create Gemini model for %s: %w", agentID, err)
		}
		return geminiModel, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q in runtime configuration for agent %s (supported: openai, gemini, anthropic)", runtimeCfg.Provider, agentID)
	}
}

// quotaResetTimeLayout is the timestamp format z.ai (and similar providers) embed in 429
// bodies: "Your limit will reset at 2026-09-19 12:14:43". No timezone suffix means the
// provider's local clock; we treat it as UTC for the failed-until window, which is a
// conservative over-estimate of the outage (any offset makes us skip a little longer, never
// less).
const quotaResetTimeLayout = "2006-01-02 15:04:05"

// usecQuotaResetRe matches the two known quota-reset shapes in error text:
//
//	... limit will reset at 2026-09-19 12:14:43 ...
//	... resets at 2026-09-19 12:14:43 ...
var quotaResetRe = regexp.MustCompile(`(?i)reset\s+at\s+(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)

// ParseQuotaResetHint extracts a backend-supplied quota reset timestamp from an error
// message when present (e.g. z.ai 429 bodies "Your limit will reset at 2026-09-19 12:14:43",
// or generic "resets at <time>"). Returns (zero time, false) when absent or unparseable.
func ParseQuotaResetHint(err error) (time.Time, bool) {
	if err == nil {
		return time.Time{}, false
	}
	m := quotaResetRe.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return time.Time{}, false
	}
	t, perr := time.Parse(quotaResetTimeLayout, m[1])
	if perr != nil {
		return time.Time{}, false
	}
	return t, true
}
