package agent

import (
	"net/http"
	"strings"
	"time"

	adkopenai "github.com/achetronic/adk-utils-go/genai/openai/completions"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// NewOpenAIModel instantiates an OpenAI-compatible model adapter for ADK,
// backed by the official OpenAI Go SDK via achetronic/adk-utils-go. Works
// against OpenAI itself and OpenAI-compatible providers (Ollama, vLLM,
// llama.cpp, DeepSeek, OpenRouter, etc.), including ones that emit the
// non-standard reasoning_content field for chain-of-thought, which the
// adapter surfaces as a Thought-marked genai.Part.
//
// go.mod currently replaces achetronic/adk-utils-go with
// github.com/colinrgodsey/adk-utils-go (master, currently commit ee4a5294,
// "fix: treat `reasoning_content` appropriately" — this commit hash
// supersedes earlier ones referenced in prior history/commit messages here,
// since the fork's master was squashed/rewritten upstream). The fork
// re-emits Thought parts as a proper reasoning_content field on egress
// instead of merging them into plain "content" — required by DeepSeek V4
// thinking mode and Kimi K2 Thinking, which 400 without it — preserves
// OpenRouter's structured reasoning_details blocks (needed for
// encrypted/signed reasoning), fixes streamed turns losing reasoning on the
// terminal response, and adds Config.ExtraBody for provider-specific request
// extensions Chat Completions doesn't define (e.g. OpenRouter's
// `{"reasoning": {"effort": "high"}}` to request extended thinking from
// models like Claude that don't emit it by default). See
// ADK_UTILS_GO_REASONING_EGRESS_BUG.md at the repo root for the original bug
// writeup. Once the fix lands upstream and is tagged, drop the replace
// directive and go back to depending on achetronic/adk-utils-go directly.
func NewOpenAIModel(runtimeCfg *RuntimeConfig) model.LLM {
	isOpenRouter := strings.Contains(strings.ToLower(runtimeCfg.Endpoint), "openrouter.ai")

	effort := runtimeCfg.ReasoningEffort
	if effort == "" {
		effort = runtimeCfg.ThinkingEffort
	}

	extraBody := runtimeCfg.ExtraBody
	if effort != "" {
		if extraBody == nil {
			extraBody = make(map[string]any)
		} else {
			clone := make(map[string]any, len(extraBody)+1)
			for k, v := range extraBody {
				clone[k] = v
			}
			extraBody = clone
		}
		if isOpenRouter {
			if _, ok := extraBody["reasoning"]; !ok {
				extraBody["reasoning"] = map[string]any{"effort": effort}
			}
		} else {
			if _, ok := extraBody["reasoning_effort"]; !ok {
				extraBody["reasoning_effort"] = effort
			}
		}
	}

	timeoutSec := runtimeCfg.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = DefaultHTTPTimeoutSeconds
	}

	// Identifying headers sent on every request - defaults so a client never
	// shows up as "unknown" on OpenRouter's dashboard/rankings, which key off
	// these two specifically, overridable per agent via runtime.json's
	// extraHeaders for anyone embedding wackypub under their own name. No
	// User-Agent override here - confirmed live it doesn't survive openai-go
	// setting its own default (getDefaultHeaders in its requestconfig
	// package) regardless of what's passed through HTTPOptions.Headers; not
	// worth patching the adk-utils-go fork for. See D43.
	headers := http.Header{
		"X-Title":      []string{"WackyPub"},
		"HTTP-Referer": []string{"https://github.com/colinrgodsey/wackypub"},
	}
	if isOpenRouter {
		// OpenRouter-specific: X-Title above already covers app naming (its
		// docs list X-Title as an accepted alias for X-OpenRouter-Title,
		// confirmed by OpenRouter's own support), but the marketplace
		// category header has no non-prefixed alias, so it's only worth
		// sending here, not unconditionally like the two above.
		headers.Set("X-OpenRouter-Title", "WackyPub")
		headers.Set("X-OpenRouter-Categories", "creative-writing,personal-agent")
	}
	for k, v := range runtimeCfg.ExtraHeaders {
		headers.Set(k, v)
	}

	var dialect adkopenai.Dialect
	if runtimeCfg.SupportsReasoningDetails {
		dialect = adkopenai.OpenRouter
	} else if runtimeCfg.ReasoningField != "" {
		dialect = &adkopenai.TextDialect{
			WriteField: runtimeCfg.ReasoningField,
			ReadFields: []string{runtimeCfg.ReasoningField},
		}
	} else {
		dialect = adkopenai.NewTextDialect()
	}

	return adkopenai.New(adkopenai.Config{
		APIKey:    runtimeCfg.APIKey,
		BaseURL:   strings.TrimSuffix(runtimeCfg.Endpoint, "/"),
		ModelName: runtimeCfg.Model,
		HTTPOptions: adkopenai.HTTPOptions{
			Client:     &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
			Headers:    headers,
			MaxRetries: runtimeCfg.MaxRetries,
		},
		Dialect:         dialect,
		ReasoningEgress: adkopenai.ReasoningEgressMode(runtimeCfg.ReasoningEgress),
		ExtraBody:       extraBody,
	})
}

// StripSignatures returns a copy of c with provider-specific opaque
// reasoning/thought signatures removed from every part: adk-utils-go's
// OpenRouter reasoning_details block metadata, and Gemini's ThoughtSignature
// field. Both are opaque blobs a specific backend issues to let its own
// thinking be replayed in a later request; neither means anything to a
// different provider, and replaying one to the wrong provider gets the
// request rejected outright (confirmed live: an Anthropic request 400s with
// "Invalid `signature` in `thinking` block" when it receives a Gemini
// ThoughtSignature carried over from an earlier session).
//
// Ingest captures reasoning_details blocks unconditionally, regardless of
// SupportsReasoningDetails — so a block (including an opaque encrypted one
// from e.g. an OpenAI model routed through OpenRouter) ends up in
// session.jsonl even when the runtime config has egress disabled. Left in
// place, it's dead weight at best; at worst, it becomes a stale, unreplayable
// blob if SupportsReasoningDetails is later toggled back on for the same
// session but the routed endpoint has changed (see
// ADK_UTILS_GO_REASONING_EGRESS_BUG.md for the underlying encrypted-payload
// endpoint-pinning issue). Callers should apply this before persisting a
// turn when RuntimeConfig.SupportsReasoningDetails is false.
//
// A thought Part that carries nothing but a signature (no readable text) is
// dropped entirely once stripped, since nothing would remain to preserve.
func StripSignatures(c *genai.Content) *genai.Content {
	if c == nil || !contentHasSignatures(c) {
		return c
	}

	parts := make([]*genai.Part, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		if !partHasSignature(p) {
			parts = append(parts, p)
			continue
		}

		clone := *p
		clone.ThoughtSignature = nil
		clone.PartMetadata = nil
		for k, v := range p.PartMetadata {
			if k == adkopenai.ReasoningDetailMetadataKey {
				continue
			}
			if clone.PartMetadata == nil {
				clone.PartMetadata = make(map[string]any, len(p.PartMetadata)-1)
			}
			clone.PartMetadata[k] = v
		}

		if clone.Text == "" && clone.PartMetadata == nil {
			continue // nothing left worth keeping
		}
		parts = append(parts, &clone)
	}
	return &genai.Content{Role: c.Role, Parts: parts}
}

// partHasSignature reports whether p carries a provider-specific opaque
// reasoning/thought signature: adk-utils-go's OpenRouter reasoning_details
// block metadata, or Gemini's ThoughtSignature field.
func partHasSignature(p *genai.Part) bool {
	if p == nil {
		return false
	}
	if len(p.ThoughtSignature) > 0 {
		return true
	}
	if p.PartMetadata == nil {
		return false
	}
	_, ok := p.PartMetadata[adkopenai.ReasoningDetailMetadataKey]
	return ok
}

// contentHasSignatures reports whether any part of c carries a
// provider-specific opaque reasoning/thought signature.
func contentHasSignatures(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if partHasSignature(p) {
			return true
		}
	}
	return false
}

// StripSessionSignatures rewrites <agentDir>/session.jsonl in place,
// removing provider-specific opaque reasoning/thought signatures (OpenRouter
// reasoning_details block metadata and Gemini's ThoughtSignature field — see
// StripSignatures) from every turn. Readable plain-text reasoning (Thought
// parts with text) is left untouched.
//
// Useful when permanently moving an agent off a model/provider that emitted
// signed reasoning: those signatures would otherwise sit in session.jsonl as
// a landmine, either as a stale, unreplayable blob if SupportsReasoningDetails
// is toggled back on for a different backend that can't decrypt them (see
// ADK_UTILS_GO_REASONING_EGRESS_BUG.md), or - as confirmed live - an outright
// 400 the moment a session carrying another provider's thought signatures is
// replayed against a new provider (e.g. switching an agent's runtime.json
// from Gemini to Anthropic).
//
// Returns the number of turns that were actually modified. Does not acquire
// the session lock; callers must hold it (see AgentSDK.StripSignatures).
func StripSessionSignatures(agentDir string) (int, error) {
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		return 0, err
	}

	modified := 0
	stripped := make([]*genai.Content, len(turns))
	for i, t := range turns {
		if contentHasSignatures(t) {
			modified++
		}
		stripped[i] = StripSignatures(t)
	}

	if modified == 0 {
		return 0, nil
	}

	if err := WriteSessionTurns(agentDir, stripped); err != nil {
		return 0, err
	}
	return modified, nil
}
