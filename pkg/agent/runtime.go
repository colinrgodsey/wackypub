package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
}

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

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		if cfg.Endpoint != "" {
			provider = "openai"
		} else {
			provider = "gemini"
		}
	}
	cfg.Provider = provider

	return &cfg, nil
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

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		if cfg.Endpoint != "" {
			provider = "openai"
		} else {
			provider = "gemini"
		}
	}
	cfg.Provider = provider

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
