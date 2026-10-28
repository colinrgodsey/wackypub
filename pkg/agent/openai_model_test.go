package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func reasoningDetailPart(text string, extra map[string]any) *genai.Part {
	detail := map[string]any{"type": "reasoning.text", "text": text, "index": 0}
	for k, v := range extra {
		detail[k] = v
	}
	return &genai.Part{
		Text:         text,
		Thought:      true,
		PartMetadata: map[string]any{"openai.reasoning_detail": detail},
	}
}

func TestStripSignatures(t *testing.T) {
	t.Run("nil content", func(t *testing.T) {
		if got := StripSignatures(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("no signature: returned unchanged", func(t *testing.T) {
		c := &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "plain answer"}}}
		got := StripSignatures(c)
		if got != c {
			t.Errorf("expected the same pointer back when nothing needs stripping")
		}
	})

	t.Run("readable block: metadata stripped, text kept", func(t *testing.T) {
		c := &genai.Content{
			Role: "model",
			Parts: []*genai.Part{
				reasoningDetailPart("readable summary", nil),
				{Text: "final answer"},
			},
		}
		got := StripSignatures(c)
		if len(got.Parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(got.Parts))
		}
		if got.Parts[0].Text != "readable summary" || !got.Parts[0].Thought {
			t.Errorf("expected readable thought text kept: %+v", got.Parts[0])
		}
		if got.Parts[0].PartMetadata != nil {
			t.Errorf("expected PartMetadata stripped, got %+v", got.Parts[0].PartMetadata)
		}
		if got.Parts[1].Text != "final answer" {
			t.Errorf("expected final answer part untouched: %+v", got.Parts[1])
		}
	})

	t.Run("text-less encrypted block: part dropped entirely", func(t *testing.T) {
		encrypted := &genai.Part{
			Thought:      true,
			PartMetadata: map[string]any{"openai.reasoning_detail": map[string]any{"type": "reasoning.encrypted", "data": "opaque"}},
		}
		c := &genai.Content{Role: "model", Parts: []*genai.Part{encrypted, {Text: "final answer"}}}
		got := StripSignatures(c)
		if len(got.Parts) != 1 {
			t.Fatalf("expected the text-less encrypted part to be dropped, got %d parts: %+v", len(got.Parts), got.Parts)
		}
		if got.Parts[0].Text != "final answer" {
			t.Errorf("expected the remaining part to be the final answer: %+v", got.Parts[0])
		}
	})

	t.Run("other PartMetadata keys are preserved", func(t *testing.T) {
		p := reasoningDetailPart("summary", nil)
		p.PartMetadata["some.other.key"] = "keep me"
		c := &genai.Content{Role: "model", Parts: []*genai.Part{p}}
		got := StripSignatures(c)
		if got.Parts[0].PartMetadata["openai.reasoning_detail"] != nil {
			t.Errorf("expected reasoning_detail key removed")
		}
		if got.Parts[0].PartMetadata["some.other.key"] != "keep me" {
			t.Errorf("expected unrelated metadata key preserved, got %+v", got.Parts[0].PartMetadata)
		}
	})

	t.Run("Gemini ThoughtSignature: stripped, readable text kept", func(t *testing.T) {
		c := &genai.Content{
			Role: "model",
			Parts: []*genai.Part{
				{Text: "final answer", ThoughtSignature: []byte("opaque-gemini-blob")},
			},
		}
		got := StripSignatures(c)
		if len(got.Parts) != 1 {
			t.Fatalf("expected 1 part, got %d", len(got.Parts))
		}
		if got.Parts[0].Text != "final answer" {
			t.Errorf("expected text kept: %+v", got.Parts[0])
		}
		if len(got.Parts[0].ThoughtSignature) != 0 {
			t.Errorf("expected ThoughtSignature stripped, got %v", got.Parts[0].ThoughtSignature)
		}
	})

	t.Run("text-less ThoughtSignature-only part: dropped entirely", func(t *testing.T) {
		c := &genai.Content{
			Role: "model",
			Parts: []*genai.Part{
				{Thought: true, ThoughtSignature: []byte("opaque-gemini-blob")},
				{Text: "final answer"},
			},
		}
		got := StripSignatures(c)
		if len(got.Parts) != 1 {
			t.Fatalf("expected the text-less part to be dropped, got %d parts: %+v", len(got.Parts), got.Parts)
		}
		if got.Parts[0].Text != "final answer" {
			t.Errorf("expected the remaining part to be the final answer: %+v", got.Parts[0])
		}
	})
}

func TestStripSessionSignatures(t *testing.T) {
	tempDir := t.TempDir()

	turns := []*genai.Content{
		genai.NewContentFromText("hello", "user"),
		{Role: "model", Parts: []*genai.Part{reasoningDetailPart("thinking about it", nil), {Text: "hi there"}}},
		genai.NewContentFromText("another question", "user"),
		{Role: "model", Parts: []*genai.Part{{Text: "plain answer, no reasoning"}}},
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed to write session turns: %v", err)
	}

	modified, err := StripSessionSignatures(tempDir)
	if err != nil {
		t.Fatalf("StripSessionSignatures failed: %v", err)
	}
	if modified != 1 {
		t.Errorf("expected 1 turn modified, got %d", modified)
	}

	reread, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("failed to re-read session turns: %v", err)
	}
	if len(reread) != 4 {
		t.Fatalf("expected 4 turns after stripping, got %d", len(reread))
	}
	for _, turn := range reread {
		for _, p := range turn.Parts {
			if _, ok := p.PartMetadata["openai.reasoning_detail"]; ok {
				t.Errorf("expected no reasoning_detail metadata to survive: %+v", p)
			}
		}
	}
	if ContentText(reread[1]) != "hi there" {
		t.Errorf("expected readable answer text preserved, got %q", ContentText(reread[1]))
	}

	t.Run("no-op when nothing has signatures", func(t *testing.T) {
		cleanDir := t.TempDir()
		clean := []*genai.Content{genai.NewContentFromText("hi", "user"), genai.NewContentFromText("hello", "model")}
		if err := WriteSessionTurns(cleanDir, clean); err != nil {
			t.Fatalf("failed to write session turns: %v", err)
		}
		modified, err := StripSessionSignatures(cleanDir)
		if err != nil {
			t.Fatalf("StripSessionSignatures failed: %v", err)
		}
		if modified != 0 {
			t.Errorf("expected 0 turns modified, got %d", modified)
		}
	})
}

// captureLastRequestBody starts an httptest server that records the JSON
// body of every request it receives and always responds with a minimal,
// valid, non-reasoning chat completion.
func captureLastRequestBody(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("failed to unmarshal outgoing request body: %v\nraw: %s", err, raw)
		}
		bodies = append(bodies, body)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func historyWithThought(reasoning, answer string) []*genai.Content {
	return []*genai.Content{
		genai.NewContentFromText("what's the weather?", "user"),
		{
			Role: "model",
			Parts: []*genai.Part{
				{Text: reasoning, Thought: true},
				{Text: answer},
			},
		},
		genai.NewContentFromText("and tomorrow?", "user"),
	}
}

func generateOnce(t *testing.T, m model.LLM, contents []*genai.Content) {
	t.Helper()
	req := &model.LLMRequest{Model: m.Name(), Contents: contents}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent failed: %v", err)
		}
	}
}

func lastAssistantMessage(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("expected messages array in body: %+v", body)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]any)
		if ok && msg["role"] == "assistant" {
			return msg
		}
	}
	t.Fatalf("no assistant message found in body: %+v", body)
	return nil
}

func TestNewOpenAIModel_ReasoningEgress(t *testing.T) {
	tests := []struct {
		name   string
		egress string
		check  func(t *testing.T, assistantMsg map[string]any)
	}{
		{
			name:   "native (default): reasoning as its own field, not merged into content",
			egress: "",
			check: func(t *testing.T, msg map[string]any) {
				if msg["reasoning_content"] != "thinking hard" {
					t.Errorf("expected reasoning_content field, got %+v", msg)
				}
				if msg["content"] != "the answer" {
					t.Errorf("expected content to be just the answer, got %+v", msg["content"])
				}
			},
		},
		{
			name:   "think_tags: reasoning folded into content, no extra field",
			egress: "think_tags",
			check: func(t *testing.T, msg map[string]any) {
				if _, ok := msg["reasoning_content"]; ok {
					t.Errorf("expected no reasoning_content field in think_tags mode, got %+v", msg)
				}
				content, _ := msg["content"].(string)
				if !strings.Contains(content, "<think>") || !strings.Contains(content, "thinking hard") {
					t.Errorf("expected content to contain a <think> block with the reasoning text, got %q", content)
				}
			},
		},
		{
			name:   "omit: no reasoning sent in any shape",
			egress: "omit",
			check: func(t *testing.T, msg map[string]any) {
				if _, ok := msg["reasoning_content"]; ok {
					t.Errorf("expected no reasoning_content field in omit mode, got %+v", msg)
				}
				content, _ := msg["content"].(string)
				if strings.Contains(content, "thinking hard") {
					t.Errorf("expected reasoning text to be absent from content, got %q", content)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, bodies := captureLastRequestBody(t)
			m := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL, ReasoningEgress: tt.egress})
			generateOnce(t, m, historyWithThought("thinking hard", "the answer"))

			if len(*bodies) != 1 {
				t.Fatalf("expected 1 request, got %d", len(*bodies))
			}
			tt.check(t, lastAssistantMessage(t, (*bodies)[0]))
		})
	}
}

func TestNewOpenAIModel_ReasoningField(t *testing.T) {
	srv, bodies := captureLastRequestBody(t)
	m := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL, ReasoningField: "reasoning"})
	generateOnce(t, m, historyWithThought("thinking hard", "the answer"))

	msg := lastAssistantMessage(t, (*bodies)[0])
	if msg["reasoning"] != "thinking hard" {
		t.Errorf("expected custom 'reasoning' field to carry the thought text, got %+v", msg)
	}
	if _, ok := msg["reasoning_content"]; ok {
		t.Errorf("expected no default reasoning_content field when a custom ReasoningField is set, got %+v", msg)
	}
}

func TestNewOpenAIModel_ExtraBody(t *testing.T) {
	srv, bodies := captureLastRequestBody(t)
	m := NewOpenAIModel(&RuntimeConfig{
		Model:     "test-model",
		Endpoint:  srv.URL,
		ExtraBody: map[string]any{"reasoning": map[string]any{"effort": "high"}},
	})
	generateOnce(t, m, []*genai.Content{genai.NewContentFromText("hi", "user")})

	body := (*bodies)[0]
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level 'reasoning' key from ExtraBody, got %+v", body)
	}
	if reasoning["effort"] != "high" {
		t.Errorf("expected reasoning.effort=high from ExtraBody, got %+v", reasoning)
	}
}

// TestNewOpenAIModel_IdentifyingHeaders verifies the D43 default identifying
// headers (X-Title, HTTP-Referer) go out on the wire, and that ExtraHeaders
// overrides a default by key while leaving the other default untouched.
func TestNewOpenAIModel_IdentifyingHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	m := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	generateOnce(t, m, []*genai.Content{genai.NewContentFromText("hi", "user")})

	if got := gotHeaders.Get("X-Title"); got != "WackyPub" {
		t.Errorf("expected default X-Title WackyPub, got %q", got)
	}
	if got := gotHeaders.Get("HTTP-Referer"); got != "https://github.com/colinrgodsey/wackypub" {
		t.Errorf("expected default HTTP-Referer, got %q", got)
	}

	m2 := NewOpenAIModel(&RuntimeConfig{
		Model:        "test-model",
		Endpoint:     srv.URL,
		ExtraHeaders: map[string]string{"X-Title": "MyCoolApp"},
	})
	generateOnce(t, m2, []*genai.Content{genai.NewContentFromText("hi", "user")})

	if got := gotHeaders.Get("X-Title"); got != "MyCoolApp" {
		t.Errorf("expected overridden X-Title MyCoolApp, got %q", got)
	}
	if got := gotHeaders.Get("HTTP-Referer"); got != "https://github.com/colinrgodsey/wackypub" {
		t.Errorf("expected HTTP-Referer to remain the default when only X-Title is overridden, got %q", got)
	}
}

// TestNewOpenAIModel_OpenRouterConditionalHeaders verifies X-OpenRouter-Title
// and X-OpenRouter-Categories only go out when the endpoint is detected as
// OpenRouter, per D43.
func TestNewOpenAIModel_OpenRouterConditionalHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	// Endpoint string contains "openrouter.ai" in the path - host stays local
	// so the request actually lands on the test server, no real network call.
	mOR := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL + "/openrouter.ai/v1"})
	generateOnce(t, mOR, []*genai.Content{genai.NewContentFromText("hi", "user")})

	if got := gotHeaders.Get("X-OpenRouter-Title"); got != "WackyPub" {
		t.Errorf("expected X-OpenRouter-Title WackyPub for an OpenRouter endpoint, got %q", got)
	}
	if got := gotHeaders.Get("X-OpenRouter-Categories"); got != "creative-writing,personal-agent" {
		t.Errorf("expected X-OpenRouter-Categories creative-writing,personal-agent, got %q", got)
	}

	mPlain := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	generateOnce(t, mPlain, []*genai.Content{genai.NewContentFromText("hi", "user")})

	if got := gotHeaders.Get("X-OpenRouter-Categories"); got != "" {
		t.Errorf("expected no X-OpenRouter-Categories for a non-OpenRouter endpoint, got %q", got)
	}
}

// TestNewOpenAIModel_SupportsReasoningDetails exercises the exact round trip
// that broke against OpenRouter's "auto" routing (DECISIONS.md D6): a
// response carrying structured reasoning_details blocks is captured on
// ingest, then replayed as history on request 2. Whether the block array
// actually gets resent is controlled by SupportsReasoningDetails.
func TestNewOpenAIModel_SupportsReasoningDetails(t *testing.T) {
	for _, supports := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[supports], func(t *testing.T) {
			var requestCount int
			var secondBody map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				w.Header().Set("Content-Type", "application/json")
				if requestCount == 1 {
					// First response: carries a structured reasoning_details block.
					io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"first answer","reasoning_details":[{"type":"reasoning.text","text":"first thought","index":0}]},"finish_reason":"stop"}]}`)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &secondBody); err != nil {
					t.Fatalf("failed to unmarshal second request body: %v", err)
				}
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"second answer"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			m := NewOpenAIModel(&RuntimeConfig{
				Model:                    "test-model",
				Endpoint:                 srv.URL,
				ReasoningField:           "reasoning",
				SupportsReasoningDetails: supports,
			})

			// Request 1: get a response with reasoning_details, capture it.
			req1 := &model.LLMRequest{Model: m.Name(), Contents: []*genai.Content{genai.NewContentFromText("hi", "user")}}
			var firstContent *genai.Content
			for resp, err := range m.GenerateContent(context.Background(), req1, false) {
				if err != nil {
					t.Fatalf("first GenerateContent failed: %v", err)
				}
				if resp != nil {
					firstContent = resp.Content
				}
			}
			if firstContent == nil {
				t.Fatalf("expected a response from the first call")
			}

			// Request 2: replay the captured content as history.
			history := []*genai.Content{genai.NewContentFromText("hi", "user"), firstContent, genai.NewContentFromText("again?", "user")}
			generateOnce(t, m, history)

			if secondBody == nil {
				t.Fatalf("expected a second request to have been captured")
			}
			msg := lastAssistantMessage(t, secondBody)

			_, hasDetails := msg["reasoning_details"]
			if supports && !hasDetails {
				t.Errorf("expected reasoning_details to be replayed when SupportsReasoningDetails is true, got %+v", msg)
			}
			if !supports && hasDetails {
				t.Errorf("expected no reasoning_details replayed when SupportsReasoningDetails is false, got %+v", msg)
			}
		})
	}
}

// TestNewOpenAIModel_ReasoningEffortShape confirms RuntimeConfig.ReasoningEffort
// produces the right wire shape for the actual endpoint: OpenRouter's nested
// {"reasoning": {"effort": ...}} convention, or real OpenAI's top-level
// reasoning_effort field for everything else - sending the wrong shape to
// either one silently fails to set reasoning effort at all.
func TestNewOpenAIModel_ReasoningEffortShape(t *testing.T) {
	tests := []struct {
		name     string
		endpoint func(srvURL string) string
		check    func(t *testing.T, body map[string]any)
	}{
		{
			name:     "OpenRouter endpoint: nested reasoning.effort",
			endpoint: func(srvURL string) string { return srvURL + "/openrouter.ai/api/v1" },
			check: func(t *testing.T, body map[string]any) {
				reasoning, ok := body["reasoning"].(map[string]any)
				if !ok {
					t.Fatalf("expected a top-level 'reasoning' object, got %+v", body)
				}
				if reasoning["effort"] != "high" {
					t.Errorf("expected reasoning.effort=high, got %+v", reasoning)
				}
				if _, ok := body["reasoning_effort"]; ok {
					t.Errorf("did not expect a top-level reasoning_effort field for OpenRouter, got %+v", body)
				}
			},
		},
		{
			name:     "Non-OpenRouter endpoint (real OpenAI shape): top-level reasoning_effort",
			endpoint: func(srvURL string) string { return srvURL },
			check: func(t *testing.T, body map[string]any) {
				if body["reasoning_effort"] != "high" {
					t.Errorf("expected top-level reasoning_effort=high, got %+v", body)
				}
				if _, ok := body["reasoning"]; ok {
					t.Errorf("did not expect a nested 'reasoning' object for a non-OpenRouter endpoint, got %+v", body)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, bodies := captureLastRequestBody(t)
			m := NewOpenAIModel(&RuntimeConfig{
				Model:           "test-model",
				Endpoint:        tt.endpoint(srv.URL),
				ReasoningEffort: "high",
			})
			generateOnce(t, m, []*genai.Content{genai.NewContentFromText("hi", "user")})

			if len(*bodies) != 1 {
				t.Fatalf("expected 1 request, got %d", len(*bodies))
			}
			tt.check(t, (*bodies)[0])
		})
	}
}

func TestInspectBobMessages(t *testing.T) {
	srv, bodies := captureLastRequestBody(t)
	agentDir := "../../testws/bob"
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		agentDir = "../../test_agents/bob"
		if _, err := os.Stat(agentDir); os.IsNotExist(err) {
			t.Skip("neither testws/bob nor test_agents/bob exists")
		}
	}
	cfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}
	cfg.Endpoint = srv.URL

	sessionTurns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	memContent, _ := ReadMemoryFile(agentDir)
	memTurnText := FormatPersistentMemoryTurn(memContent)
	memContentTurn := genai.NewContentFromText(memTurnText, "user")

	var rawContents []*genai.Content
	rawContents = append(rawContents, memContentTurn)
	rawContents = append(rawContents, sessionTurns...)
	cleaned := CleanSessionTurns(rawContents)

	m := NewOpenAIModel(cfg)
	generateOnce(t, m, cleaned)

	body := (*bodies)[0]
	msgs, _ := body["messages"].([]any)
	var lastAssistantToolCallIDs []string
	for idx, mAny := range msgs {
		mMap, _ := mAny.(map[string]any)
		role, _ := mMap["role"].(string)
		if role == "assistant" {
			lastAssistantToolCallIDs = nil
			if tcs, ok := mMap["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					if tcMap, ok := tcAny.(map[string]any); ok {
						if id, ok := tcMap["id"].(string); ok && id != "" {
							lastAssistantToolCallIDs = append(lastAssistantToolCallIDs, id)
						}
					}
				}
			}
		} else if role == "tool" {
			toolCallID, _ := mMap["tool_call_id"].(string)
			if toolCallID == "" {
				t.Errorf("Msg %d [tool] has empty tool_call_id", idx)
			}
			var found bool
			for _, id := range lastAssistantToolCallIDs {
				if id == toolCallID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Msg %d [tool] tool_call_id %q was not in last assistant tool_calls %v", idx, toolCallID, lastAssistantToolCallIDs)
			}
		}
	}
}

// rateLimitModelStub scripts one status per request so a test can count exactly how
// many attempts the configured policy is allowed to make through the real model client.
func rateLimitModelStub(t *testing.T, statuses []int) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		status := statuses[len(statuses)-1]
		if n <= len(statuses) {
			status = statuses[n-1]
		}
		mu.Unlock()

		if status != http.StatusOK {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"Rate limit exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestNewOpenAIModelRateLimitRecovery wires the real model client, so it proves the
// retry transport is actually installed rather than only correct in isolation.
func TestNewOpenAIModelRateLimitRecovery(t *testing.T) {
	srv, requests := rateLimitModelStub(t, []int{
		http.StatusTooManyRequests,
		http.StatusTooManyRequests,
		http.StatusOK,
	})
	maxRetries := 2
	m := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL, APIKey: "k", ContextWindow: 200000, MaxRetries: &maxRetries})
	generateOnce(t, m, []*genai.Content{genai.NewContentFromText("hi", "user")})

	if *requests != 3 {
		t.Errorf("expected two throttled attempts then a success, got %d requests", *requests)
	}
}

func TestNewOpenAIModelMaxRetriesZeroSurfacesRateLimit(t *testing.T) {
	srv, requests := rateLimitModelStub(t, []int{http.StatusTooManyRequests})
	noRetries := 0
	m := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL, APIKey: "k", ContextWindow: 200000, MaxRetries: &noRetries})

	req := &model.LLMRequest{Model: m.Name(), Contents: []*genai.Content{genai.NewContentFromText("hi", "user")}}
	var seenErr error
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			seenErr = err
		}
	}
	if seenErr == nil {
		t.Fatal("a 429 with retries disabled must surface as an error")
	}
	if !strings.Contains(seenErr.Error(), "429") {
		t.Errorf("the surfaced error must name the status, got %v", seenErr)
	}
	if *requests != 1 {
		t.Errorf("maxRetries=0 must send exactly one request, got %d", *requests)
	}
}

// TestNewOpenAIModelEmptyChoicesIsAnError covers the panic the card was opened for:
// a 200 response carrying no choices used to index Choices[0] and take the process down.
func TestNewOpenAIModelEmptyChoicesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-empty","choices":[]}`)
	}))
	t.Cleanup(srv.Close)

	m := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL, APIKey: "k", ContextWindow: 200000})
	req := &model.LLMRequest{Model: m.Name(), Contents: []*genai.Content{genai.NewContentFromText("hi", "user")}}

	var seenErr error
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			seenErr = err
		}
	}
	if seenErr == nil {
		t.Fatal("an empty-choices response must be an error, not a silent empty turn")
	}
	if !strings.Contains(strings.ToLower(seenErr.Error()), "choice") {
		t.Errorf("the error should say what was missing, got %v", seenErr)
	}
}
