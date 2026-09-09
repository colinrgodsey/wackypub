package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// DefaultMaxToolTurns is the default cap on consecutive tool-call turns
// within a single GenerateTurn call, used wherever a caller doesn't specify
// one explicitly (the --max-tool-turns CLI flag, AgentSDK.NewSDK, and
// BuildADKAgent/LoadFolderAgent's own <= 0 fallback).
const DefaultMaxToolTurns = 300

// MaxEgressTextPartBytes caps a single TEXT part of a model response at the
// moment it leaves the model layer (D101 P1.3). Intentionally the same size
// as the D101 P0.2 persist cap so a capped part looks byte-for-byte identical
// regardless of which guard fires.
const MaxEgressTextPartBytes = 256 * 1024

// capOversizedEgressResponse returns a shallow copy of llmResponse with every
// oversized TEXT part truncated to the D101 P0.2 head/banner/tail format, or
// nil when no intervention is needed. Only the terminal (non-partial,
// non-error) response is inspected: ADK persists just the non-partial events,
// and a model adapter's streaming accumulator builds the final aggregate
// independently of what a callback returns for partials, so the terminal
// response is the single effective clamp point. UsageMetadata and all other
// fields ride along on the shallow copy untouched; ADK re-wraps a non-nil
// callback return with the original event ID
// (internal/llminternal/base_flow.go:811-815).
// NOTE: If streaming mode (e.g. runconfig.StreamingModeSSE) is ever re-enabled,
// streaming aggregation assumptions must be validated since partial chunks
// (llmResponse.Partial == true) currently bypass clamping.
func capOversizedEgressResponse(llmResponse *model.LLMResponse, llmResponseError error) *model.LLMResponse {
	if llmResponse == nil || llmResponseError != nil || llmResponse.Partial || llmResponse.Content == nil {
		return nil
	}
	oversized := 0
	for _, p := range llmResponse.Content.Parts {
		if isTextPart(p) && len(p.Text) > MaxEgressTextPartBytes {
			oversized++
		}
	}
	if oversized == 0 {
		return nil
	}
	capped := *llmResponse
	content := *llmResponse.Content
	content.Parts = make([]*genai.Part, len(llmResponse.Content.Parts))
	for i, p := range llmResponse.Content.Parts {
		if isTextPart(p) && len(p.Text) > MaxEgressTextPartBytes {
			cloned := *p
			cloned.Text = truncatePersistTextPart(p.Text)
			content.Parts[i] = &cloned
		} else {
			content.Parts[i] = p
		}
	}
	capped.Content = &content
	fmt.Fprintf(os.Stderr, "Warning: capped %d oversized text part(s) in model response at the D101 egress cap (%d bytes).\n", oversized, MaxEgressTextPartBytes)
	return &capped
}

// failureBreakerThreshold is the number of consecutive identical tool failures
// tolerated before the turn is aborted (D101 P1.4).
const failureBreakerThreshold = 10

// failureSnippet collapses whitespace and rune-safely shortens an error to a
// single-line snippet suitable for user-visible abort messages.
func failureSnippet(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return s
}

var (
	uuidRegex   = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	hexRegex    = regexp.MustCompile(`(?i)(?:0x[0-9a-f]+|\b[0-9a-f]{8,}\b)`)
	pathRegex   = regexp.MustCompile(`/(?:[a-zA-Z0-9_.-]+/)+[a-zA-Z0-9_.-]*`)
	digitsRegex = regexp.MustCompile(`[0-9]+`)
)

// normalizeError normalizes error text by collapsing volatile runtime tokens (UUIDs, hex strings,
// absolute paths, and digit runs) into placeholder tokens and canonicalizing whitespace.
// This serves as the secondary signal in breaker fingerprinting: errors with varying
// runtime tokens (e.g. timestamps, request IDs, PIDs, temp paths) normalize to the
// same signature so loop patterns are reliably caught.
func normalizeError(msg string) string {
	s := uuidRegex.ReplaceAllString(msg, "<UUID>")
	s = hexRegex.ReplaceAllString(s, "<HEX>")
	s = pathRegex.ReplaceAllString(s, "<PATH>")
	s = digitsRegex.ReplaceAllString(s, "<NUM>")
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// canonicalArgsHash computes a SHA-256 hash of the canonical JSON representation
// of the tool arguments. json.Marshal deterministically sorts map keys lexicographically.
// This serves as the primary key in breaker fingerprinting so that retries with
// distinct arguments do not trip the breaker.
func canonicalArgsHash(args map[string]any) string {
	if len(args) == 0 {
		h := sha256.Sum256([]byte("{}"))
		return hex.EncodeToString(h[:])
	}
	b, err := json.Marshal(args)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", args))
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// computeFailureSignature builds the composite fingerprint per spec:
// Primary key: tool name + sha256(canonical args)
// Secondary signal: normalized error text (collapsing numbers, hex, UUIDs, paths)
// Reasoning:
//  1. Primary key ensures that genuine retries varying arguments (e.g. searching
//     different paths or running different commands) never trip the breaker, even
//     if the underlying system returns identical generic errors ("no such file or directory").
//  2. Secondary signal ensures that varying runtime artifacts (timestamps, pids,
//     durations, uuids) do not disguise repeated identical failures and evade the breaker,
//     while still distinguishing between completely different categories of error.
func computeFailureSignature(toolName string, args map[string]any, err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s", toolName, canonicalArgsHash(args), normalizeError(err.Error()))
}

// isSchemaValidationError reports whether err is an argument/schema validation
// failure emitted by jsonschema-go or ADK's argument converter. These errors
// indicate the model generated arguments violating the tool's parameter schema
// (e.g. missing required properties or wrong types), which the model will
// deterministically repeat.
func isSchemaValidationError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	// jsonschema-go emits "required: missing properties: %q" or "dependentRequired: missing properties"
	if strings.Contains(lower, "missing properties") {
		return true
	}
	if strings.Contains(lower, "jsonschema") || strings.Contains(lower, "schema validation") {
		return true
	}
	// Scoped argument requirement validation (avoids bare "is required" which matches ADK internal errors)
	if (strings.Contains(lower, "argument") || strings.Contains(lower, "parameter") || strings.Contains(lower, "properties")) &&
		(strings.Contains(lower, "is required") || strings.Contains(lower, "missing required")) {
		return true
	}
	return false
}

// recordConsecutiveToolFailure feeds one tool failure into the turn-scoped
// circuit breaker (D101 P1.4). A schema-validation-shaped error aborts immediately (K=1) -
// the model will deterministically repeat it. Any other failure aborts only after
// failureBreakerThreshold consecutive identical failures (same tool, same canonical args,
// same normalized error) (K=3), so transient errors and varied retries still get their
// chances. Detection lives here, in the ADK tool-error callback, not in individual tool
// implementations. Access to tracker is mutex-synchronized for thread safety under ADK's
// concurrent tool execution goroutines.
func recordConsecutiveToolFailure(tracker *TurnUsageTracker, toolName string, args map[string]any, err error) {
	if tracker == nil || err == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if tracker.FailureBreakerTripped {
		return
	}

	sig := computeFailureSignature(toolName, args, err)
	if isSchemaValidationError(err) {
		// D110: route schema-validation errors through the same K=failureBreakerThreshold
		// counter as general failures, instead of aborting immediately. The previous
		// K=1 behavior aborted turns on planning-phase skeleton tool calls (just the
		// tool name, no args) which the model could not recover from. K=10 still
		// catches a model genuinely stuck on the same broken call.
		tracker.ConsecutiveFailureSignature = sig
		tracker.ConsecutiveFailureCount++
		if tracker.ConsecutiveFailureCount >= failureBreakerThreshold {
			tracker.FailureBreakerTripped = true
			tracker.FailureBreakerMessage = fmt.Sprintf("Tool %q failed schema validation %d consecutive times - aborting turn.", toolName, tracker.ConsecutiveFailureCount)
			fmt.Fprintf(os.Stderr, "Warning: %s\n", tracker.FailureBreakerMessage)
			return
		}
		fmt.Fprintf(os.Stderr, "Warning: Tool %q failed schema validation (attempt %d/%d) - continuing.\n", toolName, tracker.ConsecutiveFailureCount, failureBreakerThreshold)
		return
	}
	if sig == tracker.ConsecutiveFailureSignature {
		tracker.ConsecutiveFailureCount++
	} else {
		tracker.ConsecutiveFailureSignature = sig
		tracker.ConsecutiveFailureCount = 1
	}
	if tracker.ConsecutiveFailureCount >= failureBreakerThreshold {
		tracker.FailureBreakerTripped = true
		tracker.FailureBreakerMessage = fmt.Sprintf("Tool %q failed %d consecutive times with an identical error (%s) - aborting turn instead of retrying.", toolName, tracker.ConsecutiveFailureCount, failureSnippet(err))
		fmt.Fprintf(os.Stderr, "Warning: %s\n", tracker.FailureBreakerMessage)
	}
}

// resetConsecutiveToolFailure clears consecutive failure tracking on a successful tool execution (D101 P1.4).
func resetConsecutiveToolFailure(tracker *TurnUsageTracker) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.ConsecutiveFailureSignature = ""
	tracker.ConsecutiveFailureCount = 0
}

// CreateGeminiModel instantiates a native Gemini LLM model using Google ADK model package.
func CreateGeminiModel(ctx context.Context, modelName string, apiKey string) (model.LLM, error) {
	if modelName == "" {
		modelName = "gemini-2.5-flash"
	}

	clientCfg := &genai.ClientConfig{}
	if apiKey != "" {
		clientCfg.APIKey = apiKey
	}

	llmModel, err := gemini.NewModel(ctx, modelName, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize ADK Gemini model %q: %w", modelName, err)
	}

	return llmModel, nil
}

// convertThinkingLevel maps a string effort level ("low", "medium", "high", "minimal") to genai.ThinkingLevel.
func convertThinkingLevel(level string) genai.ThinkingLevel {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "MINIMAL":
		return genai.ThinkingLevelMinimal
	case "LOW":
		return genai.ThinkingLevelLow
	case "MEDIUM":
		return genai.ThinkingLevelMedium
	case "HIGH", "MAX":
		return genai.ThinkingLevelHigh
	default:
		return genai.ThinkingLevelUnspecified
	}
}

func getGeminiThinkingConfig(cfg *RuntimeConfig) (*genai.ThinkingConfig, error) {
	if cfg == nil || cfg.Provider != "gemini" {
		return nil, nil
	}

	budget := cfg.GeminiThinkingBudget
	if budget == nil {
		budget = cfg.ThinkingBudgetTokens
	}

	level := cfg.GeminiThinkingLevel
	if level == "" {
		level = cfg.ThinkingEffort
	}

	// Gemini's API rejects a request that sets both - "You can only set only
	// one of thinking budget and thinking level." Fail loudly here instead
	// of silently picking one: a runtime.json with both set is a config
	// mistake worth surfacing, not something to guess through.
	if budget != nil && level != "" {
		return nil, fmt.Errorf("runtime.json sets both geminiThinkingBudget/thinkingBudgetTokens and geminiThinkingLevel/thinkingEffort - Gemini only accepts one, set only one of them")
	}

	if budget == nil && level == "" && cfg.GeminiIncludeThoughts == nil {
		return nil, nil
	}

	include := true
	if cfg.GeminiIncludeThoughts != nil {
		include = *cfg.GeminiIncludeThoughts
	}

	tc := &genai.ThinkingConfig{
		IncludeThoughts: include,
	}
	if budget != nil {
		b := int32(*budget)
		tc.ThinkingBudget = &b
	}
	if level != "" {
		tc.ThinkingLevel = convertThinkingLevel(level)
	}
	return tc, nil
}

// TurnUsageTracker tracks real provider token usage and model call counts across model calls within an agent turn (D68, D77).
// All field mutations and reads during agent execution are synchronized via mu for thread safety under concurrent ADK tool execution.
type TurnUsageTracker struct {
	mu sync.Mutex

	ModelCalls                int
	LastPromptTokens          int32
	LastCandidatesTokens      int32
	LastTotalTokens           int32
	LastUsageMetadata         *genai.GenerateContentResponseUsageMetadata
	StoppedEarlyForCompaction bool
	DisableAutoContinuation   bool

	// Consecutive-identical-failure circuit breaker state (D101 P1.4), reset per turn.
	ConsecutiveFailureSignature string
	ConsecutiveFailureCount     int
	FailureBreakerTripped       bool
	FailureBreakerMessage       string
}

// BreakerState returns a thread-safe snapshot of the circuit breaker fields.
func (t *TurnUsageTracker) BreakerState() (tripped bool, message string, count int, signature string) {
	if t == nil {
		return false, "", 0, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.FailureBreakerTripped, t.FailureBreakerMessage, t.ConsecutiveFailureCount, t.ConsecutiveFailureSignature
}

// Reset clears turn usage and call count before starting a new turn or compaction pass.
func (t *TurnUsageTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ModelCalls = 0
	t.LastPromptTokens = 0
	t.LastCandidatesTokens = 0
	t.LastTotalTokens = 0
	t.LastUsageMetadata = nil
	t.StoppedEarlyForCompaction = false
	t.ConsecutiveFailureSignature = ""
	t.ConsecutiveFailureCount = 0
	t.FailureBreakerTripped = false
	t.FailureBreakerMessage = ""
}

// BuildADKAgentWithConfigAndTracker constructs a Google ADK LLMAgent for an agent directory, applying RuntimeConfig settings and tracking turn usage.
func BuildADKAgentWithConfigAndTracker(agentID string, renderedPrompt string, maxToolTurns int, runtimeCfg *RuntimeConfig, llmModel model.LLM, agentDir string, tracker *TurnUsageTracker, tools ...tool.Tool) (agent.Agent, error) {
	if maxToolTurns <= 0 {
		maxToolTurns = DefaultMaxToolTurns
	}

	if tracker == nil {
		tracker = &TurnUsageTracker{}
	}

	thinkingConfig, err := getGeminiThinkingConfig(runtimeCfg)
	if err != nil {
		return nil, fmt.Errorf("invalid thinking config for agent %q: %w", agentID, err)
	}

	cfg := llmagent.Config{
		Name:        agentID,
		Description: fmt.Sprintf("Agent %s", agentID),
		Instruction: renderedPrompt,
		Model:       llmModel,
		Tools:       tools,
		AfterModelCallbacks: []llmagent.AfterModelCallback{
			func(ctx agent.Context, llmResponse *model.LLMResponse, llmResponseError error) (*model.LLMResponse, error) {
				if tracker != nil {
					tracker.mu.Lock()
					if llmResponse != nil && llmResponse.UsageMetadata != nil {
						tracker.LastPromptTokens = llmResponse.UsageMetadata.PromptTokenCount
						tracker.LastCandidatesTokens = llmResponse.UsageMetadata.CandidatesTokenCount
						tracker.LastTotalTokens = llmResponse.UsageMetadata.TotalTokenCount
						if tracker.LastTotalTokens == 0 {
							tracker.LastTotalTokens = llmResponse.UsageMetadata.PromptTokenCount + llmResponse.UsageMetadata.CandidatesTokenCount
						}
						tracker.LastUsageMetadata = llmResponse.UsageMetadata
					}
					tracker.mu.Unlock()
				}
				if capped := capOversizedEgressResponse(llmResponse, llmResponseError); capped != nil {
					return capped, nil
				}
				return nil, nil
			},
		},
		OnToolErrorCallbacks: []llmagent.OnToolErrorCallback{
			func(ctx agent.Context, t tool.Tool, args map[string]any, err error) (map[string]any, error) {
				if err != nil {
					name := ""
					if t != nil {
						name = t.Name()
					}
					recordConsecutiveToolFailure(tracker, name, args, err)
				}
				return nil, nil
			},
		},
		AfterToolCallbacks: []llmagent.AfterToolCallback{
			func(ctx agent.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
				if err == nil && tracker != nil {
					resetConsecutiveToolFailure(tracker)
				}
				return nil, nil
			},
		},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
				// Circuit breaker check (D101 P1.4): takes precedence over the D88 mid-turn token
				// threshold check and model-call limits. If repeated identical failures or a schema
				// violation occurred, abort immediately rather than spending budget on compaction.
				var calls int
				var lastPrompt int32
				var disableAuto bool
				if tracker != nil {
					tracker.mu.Lock()
					if tracker.FailureBreakerTripped {
						msg := tracker.FailureBreakerMessage
						tracker.mu.Unlock()
						return &model.LLMResponse{
							Content: &genai.Content{
								Role:  "model",
								Parts: []*genai.Part{{Text: "[" + msg + "]"}},
							},
						}, nil
					}
					tracker.ModelCalls++
					calls = tracker.ModelCalls
					lastPrompt = tracker.LastPromptTokens
					disableAuto = tracker.DisableAutoContinuation
					tracker.mu.Unlock()
				}

				if thinkingConfig != nil {
					if req.Config == nil {
						req.Config = &genai.GenerateContentConfig{}
					}
					if req.Config.ThinkingConfig == nil {
						req.Config.ThinkingConfig = thinkingConfig
					}
				}
				// Mid-turn context budget check (D63, D68, D88)
				// If accumulated tool context reaches or exceeds contextWindow threshold on subsequent tool turns (ModelCalls > 1),
				// short-circuit early with a synthetic response so the next top-level turn gets a chance to trigger compaction.
				if calls > 1 && runtimeCfg != nil && runtimeCfg.ContextWindow > 0 {
					overheadPct := DefaultCompactionOverheadPct
					if agentDir != "" {
						if compactCfg, err := LoadCompactConfig(agentDir); err == nil && compactCfg != nil {
							if compactCfg.CompactOverheadPct >= 0 && compactCfg.CompactOverheadPct < 100 {
								overheadPct = compactCfg.CompactOverheadPct
							}
						}
					}
					threshold := int(float64(runtimeCfg.ContextWindow) * (1.0 - (overheadPct / 100.0)))

					var tokens int
					if lastPrompt > 0 {
						tokens = int(lastPrompt)
					} else {
						tokens = EstimateTokens(req.Contents, runtimeCfg.PreserveThinking)
					}

					if tokens >= threshold {
						if tracker != nil {
							tracker.mu.Lock()
							tracker.StoppedEarlyForCompaction = true
							tracker.mu.Unlock()
						}
						fmt.Fprintf(os.Stderr, "Warning: agent %q accumulated ~%d tokens in mid-turn tool context, reaching compaction threshold (%d / %d contextWindow) - stopping early for compaction.\n", agentID, tokens, threshold, runtimeCfg.ContextWindow)
						var continueHint string
						if disableAuto {
							continueHint = " Send another message (e.g. \"continue\") to proceed."
						}
						return &model.LLMResponse{
							Content: &genai.Content{
								Role: "model",
								Parts: []*genai.Part{
									{Text: fmt.Sprintf("[Accumulated tool context reached ~%d tokens (exceeding %d budget threshold for %d contextWindow) - stopping turn early to allow session compaction.%s]", tokens, threshold, runtimeCfg.ContextWindow, continueHint)},
								},
							},
						}, nil
					}
				}

				// Check for deferred image response from get_scratchpad per D49, D88
				if hasDef, deferredIDs := hasDeferredScratchpadResponse(req.Contents); hasDef {
					var continueHint string
					if disableAuto {
						continueHint = " Send another message to continue."
					}
					idList := strings.Join(deferredIDs, ", ")
					return &model.LLMResponse{
						Content: &genai.Content{
							Role: "model",
							Parts: []*genai.Part{
								{Text: fmt.Sprintf("Image from scratchpad %s has been queued. It will be available in your next turn.%s", idList, continueHint)},
							},
						},
					}, nil
				}

				// First model call is initial prompt; subsequent model calls are tool loop turns.
				// Stop short rather than error: the caller (human or controlling agent) gets a
				// clear, successful turn back with a hint to send another message to continue,
				// instead of losing whatever the tool loop already accomplished.
				if calls > maxToolTurns+1 {
					fmt.Fprintf(os.Stderr, "Warning: agent %q reached the maximum tool-call turn limit (%d) for this generation - stopping early. Send another message (e.g. \"continue\") to let it keep going.\n", agentID, maxToolTurns)
					return &model.LLMResponse{
						Content: &genai.Content{
							Role: "model",
							Parts: []*genai.Part{
								{Text: fmt.Sprintf("[Reached the maximum of %d consecutive tool calls for this turn - stopping here. Send another message (e.g. \"continue\") to keep going.]", maxToolTurns)},
							},
						},
					}, nil
				}
				return nil, nil
			},
		},
	}

	ag, err := llmagent.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to build ADK agent %q: %w", agentID, err)
	}

	return ag, nil
}

// BuildADKAgentWithConfig constructs a Google ADK LLMAgent for an agent directory, applying RuntimeConfig settings.
func BuildADKAgentWithConfig(agentID string, renderedPrompt string, maxToolTurns int, runtimeCfg *RuntimeConfig, llmModel model.LLM, tools ...tool.Tool) (agent.Agent, error) {
	return BuildADKAgentWithConfigAndTracker(agentID, renderedPrompt, maxToolTurns, runtimeCfg, llmModel, "", nil, tools...)
}

// BuildADKAgent constructs a Google ADK LLMAgent for an agent directory.
// Name is agentID (unique within workspace), renderedPrompt is AGENTS.md system prompt, maxToolTurns caps tool executions.
func BuildADKAgent(agentID string, renderedPrompt string, maxToolTurns int, llmModel model.LLM, tools ...tool.Tool) (agent.Agent, error) {
	return BuildADKAgentWithConfig(agentID, renderedPrompt, maxToolTurns, nil, llmModel, tools...)
}

// ExtractTextFromEvent parses plain text output from an ADK session event,
// excluding reasoning/thinking parts - mirrors ContentText's behavior.
func ExtractTextFromEvent(event *session.Event) string {
	if event == nil || event.Content == nil {
		return ""
	}
	var text string
	for _, part := range event.Content.Parts {
		if part != nil && part.Text != "" && !part.Thought {
			text += part.Text
		}
	}
	return text
}

// hasDeferredScratchpadResponse inspects request contents to detect if the most recent turn
// includes a get_scratchpad function response flagged with deferred: true per D49.
func hasDeferredScratchpadResponse(contents []*genai.Content) (bool, []string) {
	if len(contents) == 0 {
		return false, nil
	}
	last := contents[len(contents)-1]
	if last == nil {
		return false, nil
	}
	var deferredIDs []string
	hasDeferred := false
	for _, p := range last.Parts {
		if p != nil && p.FunctionResponse != nil && (p.FunctionResponse.Name == "get_scratchpad" || p.FunctionResponse.Name == "load_skill_extra") {
			respMap := p.FunctionResponse.Response
			if respMap != nil {
				if def, ok := respMap["deferred"].(bool); ok && def {
					hasDeferred = true
					if spID, ok := respMap["scratchpad_id"].(string); ok && spID != "" {
						deferredIDs = append(deferredIDs, spID)
					}
				}
			}
		}
	}
	return hasDeferred, deferredIDs
}
