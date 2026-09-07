package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func seedCapContentText(c *genai.Content) string {
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func stubSnippet(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
func TestCapSeedTokensForCompaction_UnderBudgetUnchanged(t *testing.T) {
	turns := []*genai.Content{
		genai.NewContentFromText("memory", "user"),
		genai.NewContentFromText("hello", "user"),
		genai.NewContentFromText("hi", "model"),
	}
	got := capSeedTokensForCompaction(turns, 10_000, false)
	if &got[0] != &turns[0] {
		t.Errorf("expected the original slice back when the seed fits, got a copy")
	}
}

func TestCapSeedTokensForCompaction_StubsOversizedHistoryTurn(t *testing.T) {
	huge := strings.Repeat("X", 600_000)
	turns := []*genai.Content{
		genai.NewContentFromText("<PERSISTENT_MEMORY>memory content</PERSISTENT_MEMORY>", "user"),
		genai.NewContentFromText(huge, "user"),
		genai.NewContentFromText("small model turn", "model"),
	}
	maxTokens := 2000

	got := capSeedTokensForCompaction(turns, maxTokens, false)

	if est := EstimateTokens(got, false); est > maxTokens {
		t.Errorf("expected capped seed estimate <= %d, got %d", maxTokens, est)
	}
	if len(got) != len(turns) {
		t.Fatalf("expected turn count preserved, got %d", len(got))
	}
	text := seedCapContentText(got[1])
	if !strings.Contains(text, "[...truncated - original part was 600000 chars...]") {
		t.Errorf("expected D101 banner in stubbed turn, got %q", stubSnippet(text))
	}
	if !strings.HasPrefix(text, "XXXX") {
		t.Errorf("expected stub to keep the head, got %q", text[:40])
	}
	// The memory turn fit its share and must be untouched.
	if got[0] != turns[0] {
		t.Errorf("memory turn must not be stubbed while history stubbing suffices")
	}
	// Originals are never mutated.
	if len(turns[1].Parts[0].Text) != 600_000 {
		t.Errorf("input turns mutated")
	}
}

func TestCapSeedTokensForCompaction_StubsMemoryTurnAsLastResort(t *testing.T) {
	turns := []*genai.Content{
		genai.NewContentFromText(strings.Repeat("M", 400_000), "user"),
	}
	got := capSeedTokensForCompaction(turns, 1000, false)
	if est := EstimateTokens(got, false); est > 1000 {
		t.Errorf("expected capped seed estimate <= 1000, got %d", est)
	}
	if !strings.Contains(seedCapContentText(got[0]), "truncated - original part was") {
		t.Errorf("expected stubbed memory turn, got %q", seedCapContentText(got[0]))
	}
}

func TestStubTurnForSeedShare_DropsNonTextParts(t *testing.T) {
	turn := &genai.Content{Role: "model", Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{Name: "run_command", Args: map[string]any{"cmd": "ls"}}},
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("binary")}},
	}}
	stub := stubTurnForSeedShare(turn, 500)
	if len(stub.Parts) != 1 {
		t.Fatalf("expected a single marker part, got %d", len(stub.Parts))
	}
	if !strings.Contains(stub.Parts[0].Text, "2 non-text part(s) dropped") {
		t.Errorf("expected drop marker, got %q", stub.Parts[0].Text)
	}
	if stub.Role != "model" {
		t.Errorf("role must be preserved, got %q", stub.Role)
	}
}

func TestTruncateTurnTextToBudget_RuneSafe(t *testing.T) {
	text := strings.Repeat("汉字あ🐴", 50_000)
	out := truncateTurnTextToBudget(text, 4096)
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("stub text must marshal as valid UTF-8: %v", err)
	}
	if strings.ContainsRune(out, '\ufffd') {
		t.Errorf("stub cut a multi-byte rune")
	}
	if !strings.Contains(out, "truncated - original part was") {
		t.Errorf("expected D101 P0.2 banner format, got %q", out[len(out)-100:])
	}
	if len(out) > 4096+128 {
		t.Errorf("expected stub near budget size, got %d", len(out))
	}
	if under := truncateTurnTextToBudget("short", 4096); under != "short" {
		t.Errorf("text under budget must pass through, got %q", under)
	}
}

// TestCheckAndCompactSession_SeedCappedOnWire proves the guard where the
// incident actually failed: the HTTP request the compaction runner sends must
// stay bounded even when session.jsonl holds a megabyte-scale turn.
func TestCheckAndCompactSession_SeedCappedOnWire(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "seed-cap-agent")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	turns := []*genai.Content{
		genai.NewContentFromText("small user turn", "user"),
		genai.NewContentFromText(strings.Repeat("BLOAT", 120_000), "model"), // 600KB
		genai.NewContentFromText("recent user turn", "user"),
		genai.NewContentFromText("recent model turn", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed writing session turns: %v", err)
	}

	var bodyBytes int
	var sawBanner bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyBytes = len(body)
		sawBanner = strings.Contains(string(body), "truncated - original part was")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"addendum text"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	// 600KB alone is ~150k estimated tokens; a 4000-token context window makes
	// the session compactible while the raw seed would exceed the window 37x.
	runtimeCfg := &RuntimeConfig{ContextWindow: 4000, Model: "test-model", Endpoint: srv.URL}
	llmModel := NewOpenAIModel(runtimeCfg)
	adkAgent := mustBuildTestADKAgent(t, tempDir, "system prompt", runtimeCfg, llmModel)

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false, nil)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}
	if !sawBanner {
		t.Errorf("expected the compaction request to carry stubbed-turn banners")
	}
	// System prompt + tools + directive overhead aside, the wire request must
	// be orders of magnitude below the 600KB raw turn.
	if bodyBytes > 40_000 {
		t.Errorf("expected bounded compaction request, got %d bytes", bodyBytes)
	}
}
