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

// assertFunctionCallResponsePairing checks that every FunctionCall is followed by a user
// turn with a matching FunctionResponse, and every FunctionResponse is preceded by a model
// turn with a matching FunctionCall.
func assertFunctionCallResponsePairing(t *testing.T, turns []*genai.Content) {
	t.Helper()
	for i, turn := range turns {
		if turn == nil {
			continue
		}
		for _, p := range turn.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				if i+1 >= len(turns) {
					t.Fatalf("turn %d has FunctionCall %q without subsequent turn", i, p.FunctionCall.Name)
				}
				nextTurn := turns[i+1]
				if nextTurn.Role != "user" {
					t.Fatalf("turn %d has FunctionCall %q followed by turn %d with role %q (want 'user')", i, p.FunctionCall.Name, i+1, nextTurn.Role)
				}
				found := false
				for _, np := range nextTurn.Parts {
					if np != nil && np.FunctionResponse != nil {
						if p.FunctionCall.ID != "" && np.FunctionResponse.ID != "" {
							if p.FunctionCall.ID == np.FunctionResponse.ID {
								found = true
								break
							}
						} else if p.FunctionCall.Name == np.FunctionResponse.Name {
							found = true
							break
						}
					}
				}
				if !found {
					t.Fatalf("turn %d has FunctionCall %q (id=%q) with no matching FunctionResponse in turn %d", i, p.FunctionCall.Name, p.FunctionCall.ID, i+1)
				}
			}
			if p.FunctionResponse != nil {
				if i == 0 {
					t.Fatalf("turn 0 has FunctionResponse %q with no preceding turn", p.FunctionResponse.Name)
				}
				prevTurn := turns[i-1]
				if prevTurn.Role != "model" {
					t.Fatalf("turn %d has FunctionResponse %q preceded by turn %d with role %q (want 'model')", i, p.FunctionResponse.Name, i-1, prevTurn.Role)
				}
				found := false
				for _, pp := range prevTurn.Parts {
					if pp != nil && pp.FunctionCall != nil {
						if p.FunctionResponse.ID != "" && pp.FunctionCall.ID != "" {
							if p.FunctionResponse.ID == pp.FunctionCall.ID {
								found = true
								break
							}
						} else if p.FunctionResponse.Name == pp.FunctionCall.Name {
							found = true
							break
						}
					}
				}
				if !found {
					t.Fatalf("turn %d has FunctionResponse %q (id=%q) with no matching FunctionCall in preceding turn %d", i, p.FunctionResponse.Name, p.FunctionResponse.ID, i-1)
				}
			}
		}
	}
}

// TestCapSeedTokensForCompaction_PreservesFunctionCallPairing verifies that when
// turns participating in tool-call/response exchanges are stubbed, both the call
// and the response are stubbed together (MAJOR-4), preserving pairing invariant.
func TestCapSeedTokensForCompaction_PreservesFunctionCallPairing(t *testing.T) {
	hugeToolOutput := strings.Repeat("OUTPUT_DATA ", 40_000) // ~480KB
	turns := []*genai.Content{
		genai.NewContentFromText("<PERSISTENT_MEMORY>mem</PERSISTENT_MEMORY>", "user"), // 0
		genai.NewContentFromText("please run some tools", "user"),                      // 1
		{ // 2: Call 1
			Role: "model",
			Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "call_huge", Name: "bash", Args: map[string]any{"cmd": "run_big"}}},
			},
		},
		{ // 3: Response 1 (huge)
			Role: "user",
			Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "call_huge", Name: "bash", Response: map[string]any{"stdout": hugeToolOutput}}},
			},
		},
		{ // 4: Call 2
			Role: "model",
			Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "call_small", Name: "read_file", Args: map[string]any{"path": "foo"}}},
			},
		},
		{ // 5: Response 2 (small)
			Role: "user",
			Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "call_small", Name: "read_file", Response: map[string]any{"content": "ok"}}},
			},
		},
		genai.NewContentFromText("finished all tools", "model"), // 6
	}

	maxTokens := 2000
	got := capSeedTokensForCompaction(turns, maxTokens, false)

	if est := EstimateTokens(got, false); est > maxTokens {
		t.Errorf("expected seed estimate <= %d, got %d", maxTokens, est)
	}

	// Pairing invariant must hold: every surviving FunctionCall has a paired
	// FunctionResponse, and vice versa.
	assertFunctionCallResponsePairing(t, got)

	// In this test, Call 1 / Response 1 exceeded share, so BOTH must have been stubbed
	// to text drop markers (neither should contain FunctionCall or FunctionResponse).
	if got[2].Parts[0].FunctionCall != nil {
		t.Errorf("turn 2 FunctionCall should have been stubbed")
	}
	if got[3].Parts[0].FunctionResponse != nil {
		t.Errorf("turn 3 FunctionResponse should have been stubbed")
	}

	// Turn 4 / Turn 5 (Call 2 / Response 2) were small enough to fit within budget and
	// must survive intact.
	if got[4].Parts[0].FunctionCall == nil || got[4].Parts[0].FunctionCall.ID != "call_small" {
		t.Errorf("turn 4 small FunctionCall should have survived intact")
	}
	if got[5].Parts[0].FunctionResponse == nil || got[5].Parts[0].FunctionResponse.ID != "call_small" {
		t.Errorf("turn 5 small FunctionResponse should have survived intact")
	}
}

// TestCapSeedTokensForCompaction_StubbingOrderPrefersPureText verifies that
// pure text history turns are stubbed before tool call/response pairs.
func TestCapSeedTokensForCompaction_StubbingOrderPrefersPureText(t *testing.T) {
	bigText := strings.Repeat("TEXT ", 20_000)
	turns := []*genai.Content{
		genai.NewContentFromText("<PERSISTENT_MEMORY>mem</PERSISTENT_MEMORY>", "user"), // 0: memory
		genai.NewContentFromText(bigText, "user"),                                      // 1: big pure text
		{ // 2: tool call
			Role: "model",
			Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "echo", Args: map[string]any{"msg": "hi"}}},
			},
		},
		{ // 3: tool response
			Role: "user",
			Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "echo", Response: map[string]any{"out": "hi"}}},
			},
		},
	}

	// Setting budget so stubbing turn 1 is enough to bring seed under budget
	maxTokens := 2000
	got := capSeedTokensForCompaction(turns, maxTokens, false)

	if est := EstimateTokens(got, false); est > maxTokens {
		t.Errorf("expected seed estimate <= %d, got %d", maxTokens, est)
	}

	// Tool call and response should survive intact because turn 1 was stubbed first
	if got[2].Parts[0].FunctionCall == nil {
		t.Errorf("tool call should have been preserved when stubbing pure text suffices")
	}
	if got[3].Parts[0].FunctionResponse == nil {
		t.Errorf("tool response should have been preserved when stubbing pure text suffices")
	}
	assertFunctionCallResponsePairing(t, got)
}

// TestCapSeedTokensForCompaction_FloorNoOp verifies behavior when maxTokens/len(turns) < 8.
func TestCapSeedTokensForCompaction_FloorNoOp(t *testing.T) {
	turns := make([]*genai.Content, 20)
	for i := range turns {
		turns[i] = genai.NewContentFromText("msg", "user")
	}
	// maxTokens=10 -> share = 10 / 20 = 0 (< 8 floor)
	got := capSeedTokensForCompaction(turns, 10, false)
	if len(got) != len(turns) {
		t.Errorf("turn count should be preserved")
	}
}
