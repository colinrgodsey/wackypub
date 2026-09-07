package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestCapOversizedEgressResponse_NilAndErrorGuards(t *testing.T) {
	if got := capOversizedEgressResponse(nil, nil); got != nil {
		t.Errorf("expected nil for nil response, got %+v", got)
	}

	big := strings.Repeat("x", MaxEgressTextPartBytes+1024)
	resp := &model.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: big}}},
	}
	if got := capOversizedEgressResponse(resp, context.DeadlineExceeded); got != nil {
		t.Errorf("expected nil when llmResponseError is set, got %+v", got)
	}

	partial := &model.LLMResponse{
		Partial: true,
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: big}}},
	}
	if got := capOversizedEgressResponse(partial, nil); got != nil {
		t.Errorf("expected nil for partial response, got %+v", got)
	}

	noContent := &model.LLMResponse{}
	if got := capOversizedEgressResponse(noContent, nil); got != nil {
		t.Errorf("expected nil for response without content, got %+v", got)
	}
}

func TestCapOversizedEgressResponse_UnderCapUnchanged(t *testing.T) {
	resp := &model.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: strings.Repeat("y", 1024)}}},
	}
	if got := capOversizedEgressResponse(resp, nil); got != nil {
		t.Errorf("expected nil (no intervention) under cap, got %+v", got)
	}
}

func TestCapOversizedEgressResponse_TruncatesWithBanner(t *testing.T) {
	original := strings.Repeat("abcdefgh", 40_000) // 320_000 bytes > 256KB cap
	usage := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 11, CandidatesTokenCount: 70157, TotalTokenCount: 70168}
	resp := &model.LLMResponse{
		Content:       &genai.Content{Role: "model", Parts: []*genai.Part{{Text: original}}},
		UsageMetadata: usage,
		FinishReason:  "stop",
	}

	capped := capOversizedEgressResponse(resp, nil)
	if capped == nil {
		t.Fatalf("expected capped response, got nil")
	}
	if capped.Content.Parts[0].Text == original {
		t.Fatalf("expected truncated text, got original length %d", len(capped.Content.Parts[0].Text))
	}
	text := capped.Content.Parts[0].Text
	if len(text) > 20_000 {
		t.Errorf("expected capped text well under original size, got %d bytes", len(text))
	}
	if !strings.Contains(text, "[...truncated - original part was 320000 chars...]") {
		t.Errorf("expected D101 P0.2 banner in capped text, got: %q", text[:200])
	}
	// Head and tail content survive
	if !strings.HasPrefix(text, "abcdefgh") {
		t.Errorf("expected original head preserved, got %q", text[:40])
	}
	if !strings.HasSuffix(text, "abcdefgh") {
		t.Errorf("expected original tail preserved, got %q", text[len(text)-40:])
	}
	// UsageMetadata preserved untouched (same pointer), finish reason preserved
	if capped.UsageMetadata != usage {
		t.Errorf("expected UsageMetadata preserved on capped response")
	}
	if capped.FinishReason != "stop" {
		t.Errorf("expected FinishReason preserved, got %q", capped.FinishReason)
	}
	// Original response object must not be mutated
	if len(resp.Content.Parts[0].Text) != len(original) {
		t.Errorf("original response mutated: %d -> %d", len(original), len(resp.Content.Parts[0].Text))
	}
	if resp.Content.Parts[0].Text != original {
		t.Errorf("original response text mutated")
	}
}

func TestCapOversizedEgressResponse_StructuredPartsUntouched(t *testing.T) {
	huge := strings.Repeat("z", MaxEgressTextPartBytes+1024)
	fcPart := &genai.Part{FunctionCall: &genai.FunctionCall{Name: "run_command", Args: map[string]any{"cmd": huge}}}
	frPart := &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "run_command", Response: map[string]any{"out": huge}}}
	binPart := &genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte(huge)}}
	nilPart := (*genai.Part)(nil)
	resp := &model.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{fcPart, frPart, binPart, nilPart}},
	}
	if got := capOversizedEgressResponse(resp, nil); got != nil {
		t.Errorf("expected nil - structured/binary parts follow the D48 path, got %+v", got)
	}
}

func TestCapOversizedEgressResponse_MixedParts(t *testing.T) {
	big := strings.Repeat("q", MaxEgressTextPartBytes+1024)
	fcPart := &genai.Part{FunctionCall: &genai.FunctionCall{Name: "run_command", Args: map[string]any{"cmd": big}}}
	resp := &model.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: big}, fcPart}},
	}
	capped := capOversizedEgressResponse(resp, nil)
	if capped == nil {
		t.Fatalf("expected capped response, got nil")
	}
	if len(capped.Content.Parts[0].Text) > 20_000 {
		t.Errorf("expected oversized text part capped, got %d bytes", len(capped.Content.Parts[0].Text))
	}
	if capped.Content.Parts[1] != fcPart {
		t.Errorf("expected function-call part passed through by pointer, untouched")
	}
}

func TestCapOversizedEgressResponse_MultiByteRuneSafe(t *testing.T) {
	original := strings.Repeat("汉字あ🐴", 100_000) // 4 runes, multi-byte each
	resp := &model.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: original}}},
	}
	capped := capOversizedEgressResponse(resp, nil)
	if capped == nil {
		t.Fatalf("expected capped response, got nil")
	}
	text := capped.Content.Parts[0].Text
	if _, err := json.Marshal(text); err != nil {
		t.Errorf("capped text must remain valid UTF-8 (marshalable): %v", err)
	}
	if strings.ContainsRune(text, '\uFFFD') {
		t.Errorf("capped text contains replacement character - truncation cut a multi-byte rune")
	}
}

// TestEgressCap_WireOverride verifies the full pipeline: an oversized
// completion from the (fake) provider is capped by the AfterModelCallback and
// ADK propagates the replacement downstream in place of the original event -
// the override semantics the D101 P1.3 task card gates on
// (internal/llminternal/base_flow.go:811-815).
func TestEgressCap_WireOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respBody := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": strings.Repeat("MEGA", 80_000), // 320_000 bytes > 256KB cap
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 70157, "total_tokens": 70168},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respBody)
	}))
	defer srv.Close()

	runtimeCfg := &RuntimeConfig{Model: "test-model", Endpoint: srv.URL}
	mockModel := NewOpenAIModel(runtimeCfg)
	tracker := &TurnUsageTracker{}

	ag, err := BuildADKAgentWithConfigAndTracker("egress-bot", "System prompt", DefaultMaxToolTurns, runtimeCfg, mockModel, "", tracker)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfigAndTracker failed: %v", err)
	}

	sessionSvc := session.InMemoryService()
	_, err = sessionSvc.Create(context.Background(), &session.CreateRequest{
		AppName: "wackypub", UserID: "user", SessionID: "egress-sess",
	})
	if err != nil {
		t.Fatalf("sessionSvc.Create failed: %v", err)
	}

	r, err := runner.New(runner.Config{AppName: "wackypub", Agent: ag, SessionService: sessionSvc})
	if err != nil {
		t.Fatalf("runner.New failed: %v", err)
	}

	var outputText string
	for event, err := range r.Run(context.Background(), "user", "egress-sess", genai.NewContentFromText("Hello", "user"), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run failed: %v", err)
		}
		if event != nil && event.Content != nil {
			outputText += ExtractTextFromEvent(event)
		}
	}

	if len(outputText) == 0 {
		t.Fatalf("expected a capped model event, got no output")
	}
	if len(outputText) > 20_000 {
		t.Errorf("expected downstream text capped, got %d bytes", len(outputText))
	}
	if !strings.Contains(outputText, "[...truncated - original part was") {
		t.Errorf("expected banner in downstream text, got: %q", outputText[:200])
	}
	// UsageMetadata from the original provider response must survive the cap
	if tracker.LastTotalTokens != 70168 {
		t.Errorf("expected provider usage metadata preserved (70168), got %d", tracker.LastTotalTokens)
	}
}
