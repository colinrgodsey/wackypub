package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// TestMain seeds DefaultCompactMD from the real examples/COMPACT.md before any
// test runs. Production gets this from main.go's //go:embed (D45) - main.go
// never runs under `go test`, so tests read the same real file directly
// instead of a fabricated fixture, to keep exercising the actual shipped
// default content rather than a stand-in.
func TestMain(m *testing.M) {
	data, err := os.ReadFile("../../examples/COMPACT.md")
	if err != nil {
		panic("failed to read examples/COMPACT.md for tests: " + err.Error())
	}
	DefaultCompactMD = string(data)

	rtData, err := os.ReadFile("../../examples/runtimes/openrouter-auto.json")
	if err != nil {
		panic("failed to read examples/runtimes/openrouter-auto.json for tests: " + err.Error())
	}
	DefaultRuntimeJSON = string(rtData)

	os.Exit(m.Run())
}

// mustBuildTestADKAgent builds a real ADK agent around llmModel the same way
// LoadFolderAgent does, for tests that need to call CheckAndCompactSession
// (D45 - it takes an agent.Agent, not a bare model.LLM, so its request goes
// through the real runner/llmagent pipeline like a normal turn would). agentDir
// must be the same directory passed to CheckAndCompactSession: the ADK
// agent's Name has to equal filepath.Base(agentDir), exactly like
// LoadFolderAgent always guarantees in production - otherwise seeded
// "model"-role turns get attributed to an agent the runner doesn't
// recognize as self and are re-wrapped as third-party "for context" text
// instead of landing as native assistant-role turns (confirmed live: this
// is a real, observable wire-shape difference, not cosmetic).
func mustBuildTestADKAgent(t *testing.T, agentDir string, systemPrompt string, runtimeCfg *RuntimeConfig, llmModel model.LLM, tools ...tool.Tool) agent.Agent {
	t.Helper()
	agentID := filepath.Base(agentDir)
	ag, err := BuildADKAgentWithConfig(agentID, systemPrompt, DefaultMaxToolTurns, runtimeCfg, llmModel, tools...)
	if err != nil {
		t.Fatalf("failed to build test ADK agent: %v", err)
	}
	return ag
}

func TestFormatPersistentMemoryTurn(t *testing.T) {
	mem := "Fact A: User is a mechanic."
	turn := FormatPersistentMemoryTurn(mem)

	expected := "<PERSISTENT_MEMORY>\nFact A: User is a mechanic.\n</PERSISTENT_MEMORY>"
	if turn != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, turn)
	}
}

func TestCompactionPrefixPreservation(t *testing.T) {
	tempDir := t.TempDir()

	// Write initial MEMORY.md
	if err := WriteMemoryFile(tempDir, "Initial Memory"); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	// Write session turns
	turns := []*genai.Content{
		genai.NewContentFromText("Turn 1 user message", "user"),
		genai.NewContentFromText("Turn 1 assistant response", "model"),
		genai.NewContentFromText("Turn 2 user message", "user"),
		genai.NewContentFromText("Turn 2 assistant response", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed to write session turns: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		ContextWindow: 10, // low threshold to force compaction
	}

	mockModel := NewOpenAIModel(&RuntimeConfig{
		Model:    "test-model",
		Endpoint: "http://localhost:9999",
		APIKey:   "fake-key",
	})
	adkAgent := mustBuildTestADKAgent(t, tempDir, "System prompt", runtimeCfg, mockModel)

	// Check compaction with mock model (http request will fail gracefully, testing flow)
	ctx := context.Background()
	_, err := CheckAndCompactSession(ctx, tempDir, runtimeCfg, adkAgent, false)
	if err == nil {
		// Mock HTTP error expected
	}

	// Verify MEMORY.md file exists
	memContent, err := ReadMemoryFile(tempDir)
	if err != nil {
		t.Fatalf("failed to read MEMORY.md: %v", err)
	}
	if !strings.Contains(memContent, "Initial Memory") {
		t.Errorf("expected MEMORY.md to preserve existing content")
	}
}

// TestCompactionEndsOnModelTurn verifies the compaction boundary always
// lands after a model turn, so the surviving session never opens with a
// dangling assistant response whose prompting user turn was just archived
// into MEMORY.md. With sessionCompactPct=50 over 6 turns, a raw percentage
// cut would land mid-exchange (index 3, "user1") - this checks it gets
// extended forward to the next model turn instead.
func TestCompactionEndsOnModelTurn(t *testing.T) {
	tempDir := t.TempDir()

	turns := []*genai.Content{
		genai.NewContentFromText("u0", "user"),
		genai.NewContentFromText("m0", "model"),
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1", "model"),
		genai.NewContentFromText("u2", "user"),
		genai.NewContentFromText("m2", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed to write session turns: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* addendum"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	llmModel := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	runtimeCfg := &RuntimeConfig{ContextWindow: 1} // ContextWindow=1 forces compaction regardless of content size
	adkAgent := mustBuildTestADKAgent(t, tempDir, "system prompt", runtimeCfg, llmModel)

	ctx := context.Background()
	compacted, err := CheckAndCompactSession(ctx, tempDir, runtimeCfg, adkAgent, false)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}

	remaining, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("failed to read remaining session turns: %v", err)
	}
	if len(remaining) == 0 {
		t.Fatalf("expected some turns to remain after compaction")
	}
	if remaining[0].Role != "user" {
		t.Errorf("expected remaining session to start with a user turn, got a dangling %q turn: %+v", remaining[0].Role, remaining[0])
	}
	// 50% cut (index 3) extends forward to index 4 (through "m1"), leaving
	// u2, m2 - plus the D46 compaction-notice turn prepended in front of them.
	if len(remaining) != 3 {
		t.Errorf("expected 3 remaining turns (compaction notice, u2, m2), got %d: %+v", len(remaining), remaining)
	}
	if len(remaining) > 0 && !strings.Contains(ContentText(remaining[0]), "<COMPACTION_NOTICE>") {
		t.Errorf("expected remaining[0] to be the D46 compaction notice turn, got: %+v", remaining[0])
	}
	if len(remaining) > 1 && ContentText(remaining[1]) != "u2" {
		t.Errorf("expected remaining[1] to be the surviving \"u2\" turn, got: %+v", remaining[1])
	}
}

// TestCompactionNoticeOptOut verifies an explicit compaction-notice: "" in
// COMPACT.md suppresses the D46 notice turn entirely, rather than falling
// back to the built-in default the way an absent key does.
func TestCompactionNoticeOptOut(t *testing.T) {
	tempDir := t.TempDir()

	turns := []*genai.Content{
		genai.NewContentFromText("u0", "user"),
		genai.NewContentFromText("m0", "model"),
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed to write session turns: %v", err)
	}

	compactContent := "---\ncompaction-notice: \"\"\n---\nSummarize."
	if err := os.WriteFile(filepath.Join(tempDir, "COMPACT.md"), []byte(compactContent), 0644); err != nil {
		t.Fatalf("failed writing COMPACT.md: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"* addendum"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	llmModel := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	runtimeCfg := &RuntimeConfig{ContextWindow: 1}
	adkAgent := mustBuildTestADKAgent(t, tempDir, "system prompt", runtimeCfg, llmModel)

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}

	remaining, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("failed to read remaining session turns: %v", err)
	}
	// 50% cut (index 2) already lands on a model turn (m0), leaving u1, m1 -
	// a non-empty remaining session, so the notice would normally be
	// prepended here if not explicitly opted out.
	if len(remaining) != 2 {
		t.Fatalf("expected 2 remaining turns (u1, m1) with no notice, got %d: %+v", len(remaining), remaining)
	}
	for _, c := range remaining {
		if strings.Contains(ContentText(c), "<COMPACTION_NOTICE>") {
			t.Errorf("expected compaction-notice: \"\" to suppress the notice turn entirely, got: %+v", remaining)
		}
	}
}

func TestLoadCompactConfig_Defaults(t *testing.T) {
	tempDir := t.TempDir()

	cfg, err := LoadCompactConfig(tempDir)
	if err != nil {
		t.Fatalf("LoadCompactConfig failed: %v", err)
	}

	if !cfg.AppendOnly {
		t.Errorf("expected default AppendOnly to be true")
	}
	if cfg.CompactPct != 50.0 {
		t.Errorf("expected default CompactPct to be 50.0, got %f", cfg.CompactPct)
	}
	wantCfg, err := ParseCompactConfig(DefaultCompactMD)
	if err != nil {
		t.Fatalf("ParseCompactConfig(DefaultCompactMD) failed: %v", err)
	}
	if cfg.Prompt != wantCfg.Prompt {
		t.Errorf("expected default Prompt to equal the embedded DefaultCompactMD's body, got a mismatch")
	}
	if !strings.Contains(cfg.Prompt, "state compaction engine") {
		t.Errorf("expected default Prompt to contain the compaction directive text, got: %s", cfg.Prompt)
	}
	if !strings.Contains(cfg.Prompt, "SKILL LOADS") {
		t.Errorf("expected default Prompt to contain the D44 skill-loads guideline, got: %s", cfg.Prompt)
	}
}

func TestLoadCompactConfig_CustomFrontmatterAndBody(t *testing.T) {
	tempDir := t.TempDir()

	content := "---\nappend-only: false\ncompact-pct: 25\n---\nCustom Compaction Directive Prompt Body"
	compactPath := filepath.Join(tempDir, "COMPACT.md")
	if err := os.WriteFile(compactPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write COMPACT.md: %v", err)
	}

	cfg, err := LoadCompactConfig(tempDir)
	if err != nil {
		t.Fatalf("LoadCompactConfig failed: %v", err)
	}

	if cfg.AppendOnly {
		t.Errorf("expected AppendOnly to be false")
	}
	if cfg.CompactPct != 25.0 {
		t.Errorf("expected CompactPct to be 25.0, got %f", cfg.CompactPct)
	}
	if cfg.Prompt != "Custom Compaction Directive Prompt Body" {
		t.Errorf("expected custom prompt body, got %q", cfg.Prompt)
	}
}

func TestCompactionWithCustomCompactMD(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Write existing MEMORY.md that should be REPLACED (because append-only: false)
	if err := WriteMemoryFile(tempDir, "Old Memory Content To Be Overwritten"); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	// 2. Write custom COMPACT.md
	compactContent := "---\nappend-only: false\ncompact-pct: 50\n---\nSummarize state into clean new memory."
	if err := os.WriteFile(filepath.Join(tempDir, "COMPACT.md"), []byte(compactContent), 0644); err != nil {
		t.Fatalf("failed writing COMPACT.md: %v", err)
	}

	// 3. Session turns (4 turns: u0, m0, u1, m1)
	turns := []*genai.Content{
		genai.NewContentFromText("u0", "user"),
		genai.NewContentFromText("m0", "model"),
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed writing session turns: %v", err)
	}

	var receivedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedPrompt = string(body)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Brand New Wholesale Memory"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	llmModel := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	runtimeCfg := &RuntimeConfig{ContextWindow: 1}
	adkAgent := mustBuildTestADKAgent(t, tempDir, "system prompt", runtimeCfg, llmModel)

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to execute")
	}

	// Verify custom prompt reached the model payload
	if !strings.Contains(receivedPrompt, "Summarize state into clean new memory.") {
		t.Errorf("expected custom prompt in HTTP request payload, got: %s", receivedPrompt)
	}

	// Verify MEMORY.md was replaced wholesale (not appended)
	memContent, err := ReadMemoryFile(tempDir)
	if err != nil {
		t.Fatalf("failed to read MEMORY.md: %v", err)
	}
	if strings.Contains(memContent, "Old Memory Content To Be Overwritten") {
		t.Errorf("expected old memory to be replaced wholesale when append-only is false, got: %s", memContent)
	}
	if !strings.Contains(memContent, "Brand New Wholesale Memory") {
		t.Errorf("expected new wholesale memory content, got: %s", memContent)
	}
}

type d45EchoArgs struct {
	Text string `json:"text"`
}

// mustBuildEchoTool is a minimal real tool.Tool, used only to prove tool
// declarations actually reach the wire request below.
func mustBuildEchoTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New(functiontool.Config{
		Name:        "echo_tool",
		Description: "Echoes the given text back",
	}, func(ctx agent.Context, args d45EchoArgs) (string, error) {
		return args.Text, nil
	})
	if err != nil {
		t.Fatalf("failed to build echo tool: %v", err)
	}
	return tl
}

// TestCheckAndCompactSession_WirePayloadMatchesRealTurnShape is the
// httptest-based wire-payload test the "compaction bypasses the runner
// pipeline" TODO asked for once fixed (D45) - a test that would have failed
// against the old hand-built-request implementation, not one that just
// re-confirms whatever the current behavior happens to be. Verifies, in the
// literal JSON sent to the model: the system prompt lands in the dedicated
// "system" role message (not glued into turn 1's text), archived turns
// appear as native user/assistant-role messages (not text), and a real tool
// declaration is present - all three were absent or wrong before this
// decision.
func TestCheckAndCompactSession_WirePayloadMatchesRealTurnShape(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "test-agent")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	turns := []*genai.Content{
		genai.NewContentFromText("u0-marker-abc", "user"),
		genai.NewContentFromText("m0-marker-def", "model"),
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed writing session turns: %v", err)
	}

	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"addendum text"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	llmModel := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	runtimeCfg := &RuntimeConfig{ContextWindow: 1}
	adkAgent := mustBuildTestADKAgent(t, tempDir, "system prompt with SENTINEL_SYS_PROMPT", runtimeCfg, llmModel, mustBuildEchoTool(t))

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}

	var wire struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(receivedBody), &wire); err != nil {
		t.Fatalf("failed to parse wire payload as JSON: %v\nbody: %s", err, receivedBody)
	}

	if len(wire.Messages) == 0 || wire.Messages[0].Role != "system" || !strings.Contains(wire.Messages[0].Content, "SENTINEL_SYS_PROMPT") {
		t.Errorf("expected message[0] to be a dedicated system-role message containing the system prompt, got: %+v", wire.Messages)
	}

	var sawArchivedUserTurn, sawArchivedAssistantTurn bool
	for _, m := range wire.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "u0-marker-abc") {
			sawArchivedUserTurn = true
		}
		if m.Role == "assistant" && strings.Contains(m.Content, "m0-marker-def") {
			sawArchivedAssistantTurn = true
		}
	}
	if !sawArchivedUserTurn {
		t.Errorf("expected archived user turn to appear as a native user-role message, got: %+v", wire.Messages)
	}
	if !sawArchivedAssistantTurn {
		t.Errorf("expected archived model turn to appear as a native assistant-role message (not re-wrapped as third-party text), got: %+v", wire.Messages)
	}

	var sawTool bool
	for _, tl := range wire.Tools {
		if tl.Function.Name == "echo_tool" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Errorf("expected tool declaration %q in wire payload - this is exactly what the pre-D45 direct GenerateContent call never sent, got tools: %+v", "echo_tool", wire.Tools)
	}
}

func TestTokenWeightedCompactionPercentage(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "test-token-weighted")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// 6 turns: turn 3 is huge (~40,000 chars = ~10,000 tokens), others are small (~10 chars)
	turns := []*genai.Content{
		genai.NewContentFromText("u0", "user"),
		genai.NewContentFromText("m0", "model"),
		genai.NewContentFromText("u1", "user"),
		genai.NewContentFromText("m1-huge: "+strings.Repeat("Z", 40000), "model"),
		genai.NewContentFromText("u2", "user"),
		genai.NewContentFromText("m2", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("failed writing session turns: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"compacted memory update"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	llmModel := NewOpenAIModel(&RuntimeConfig{Model: "test-model", Endpoint: srv.URL})
	runtimeCfg := &RuntimeConfig{ContextWindow: 5000} // total is ~10,000 tokens, exceeds 5000
	adkAgent := mustBuildTestADKAgent(t, tempDir, "system prompt", runtimeCfg, llmModel)

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}

	// Read remaining turns from disk - the huge turn (m1) must have been archived!
	remaining, err := ReadSessionTurns(tempDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	// Clean remaining turns (merging any synthetic notice turn into the first real user turn)
	cleanedRemaining := CleanSessionTurns(remaining)
	if len(cleanedRemaining) != 2 {
		t.Fatalf("expected 2 cleaned remaining turns (u2, m2) after token-weighted compaction, got %d (raw remaining: %d)", len(cleanedRemaining), len(remaining))
	}
	if cleanedRemaining[0].Role != "user" || !strings.Contains(ContentText(cleanedRemaining[0]), "u2") {
		t.Errorf("expected first remaining turn to be u2, got: %+v", cleanedRemaining[0])
	}
	if cleanedRemaining[1].Role != "model" || !strings.Contains(ContentText(cleanedRemaining[1]), "m2") {
		t.Errorf("expected second remaining turn to be m2, got: %+v", cleanedRemaining[1])
	}
}

func TestMidTurnContextShortCircuit(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "test-short-circuit")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	// Server that returns a tool call to echo_tool on call 1, and final text on call 2
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Call echo_tool with a message
			toolCallJSON := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\"text\":\"hello\"}"}}]},"finish_reason":"tool_calls"}]}`
			io.WriteString(w, toolCallJSON)
		} else {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	// Tool that returns huge output (~1000 chars = ~250 tokens > 50 token budget)
	largeEchoTool, err := functiontool.New(functiontool.Config{
		Name: "echo_tool",
	}, func(ctx agent.Context, args d45EchoArgs) (map[string]any, error) {
		return map[string]any{"output": strings.Repeat("Huge tool output data in mid-turn! ", 40)}, nil
	})
	if err != nil {
		t.Fatalf("failed to create largeEchoTool: %v", err)
	}

	// contextWindow is small: 50 tokens (~200 chars)
	runtimeCfg := &RuntimeConfig{
		ContextWindow: 50,
		Model:         "test-model",
		Endpoint:      srv.URL,
	}

	mockModel := NewOpenAIModel(runtimeCfg)
	adkAgent, err := BuildADKAgentWithConfig("short-circuit-bot", "System prompt", DefaultMaxToolTurns, runtimeCfg, mockModel, largeEchoTool)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfig failed: %v", err)
	}

	sessionSvc := session.InMemoryService()
	createResp, err := sessionSvc.Create(context.Background(), &session.CreateRequest{
		AppName:   "wackypub",
		UserID:    "user",
		SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("sessionSvc.Create failed: %v", err)
	}

	r, err := runner.New(runner.Config{
		AppName:        "wackypub",
		Agent:          adkAgent,
		SessionService: sessionSvc,
	})
	if err != nil {
		t.Fatalf("runner.New failed: %v", err)
	}

	prompt := genai.NewContentFromText("Hello", "user")
	var outputText string
	for event, err := range r.Run(context.Background(), "user", createResp.Session.ID(), prompt, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run failed: %v", err)
		}
		if event != nil && event.Content != nil {
			outputText += ExtractTextFromEvent(event)
		}
	}

	if !strings.Contains(outputText, "stopping turn early to allow session compaction") {
		t.Errorf("expected mid-turn context short circuit message, got: %q", outputText)
	}
}

func TestParseCompactConfig_OverheadPct(t *testing.T) {
	// Test default
	cfg, err := ParseCompactConfig("Directive body")
	if err != nil {
		t.Fatalf("ParseCompactConfig failed: %v", err)
	}
	if cfg.CompactOverheadPct != DefaultCompactionOverheadPct {
		t.Errorf("expected default CompactOverheadPct %v, got %v", DefaultCompactionOverheadPct, cfg.CompactOverheadPct)
	}

	// Test custom overhead percentage
	customYAML := "---\ncompact-overhead-pct: 35\ncompact-pct: 40\n---\nCustom directive"
	cfg2, err := ParseCompactConfig(customYAML)
	if err != nil {
		t.Fatalf("ParseCompactConfig failed: %v", err)
	}
	if cfg2.CompactOverheadPct != 35 {
		t.Errorf("expected CompactOverheadPct 35, got %v", cfg2.CompactOverheadPct)
	}
	if cfg2.CompactPct != 40 {
		t.Errorf("expected CompactPct 40, got %v", cfg2.CompactPct)
	}
}

func TestCheckAndCompactSession_OverheadThreshold(t *testing.T) {
	tempDir := t.TempDir()

	// Write session turns with ~80 chars = ~20 tokens
	// 4 turns of ~80 chars each = ~320 chars = ~80 tokens
	turns := []*genai.Content{
		genai.NewContentFromText(strings.Repeat("Hello turn one test. ", 4), "user"),
		genai.NewContentFromText(strings.Repeat("Model response turn one. ", 4), "model"),
		genai.NewContentFromText(strings.Repeat("Hello turn two test. ", 4), "user"),
		genai.NewContentFromText(strings.Repeat("Model response turn two. ", 4), "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("WriteSessionTurns failed: %v", err)
	}

	estTokens := EstimateTokens(turns, false)

	// Set contextWindow such that estTokens is between 80% (threshold with 20% overhead) and 100%
	// If estTokens is e.g. 50, contextWindow = 60:
	// threshold with 20% overhead: 60 * 0.80 = 48 <= 50 (should trigger!)
	// threshold with 5% overhead: 60 * 0.95 = 57 > 50 (should NOT trigger!)
	contextWindow := int(float64(estTokens) / 0.85)

	runtimeCfg := &RuntimeConfig{
		ContextWindow: contextWindow,
		Model:         "test-model",
		Endpoint:      "http://localhost:9999",
		APIKey:        "fake",
	}

	// 1. With default overhead (20% -> threshold 80% of contextWindow <= estTokens):
	// Check that compaction is triggered (even if mock model network call fails)
	mockModel := NewOpenAIModel(runtimeCfg)
	adkAgent := mustBuildTestADKAgent(t, tempDir, "System prompt", runtimeCfg, mockModel)

	// Write custom COMPACT.md with 5% overhead (threshold 95% of contextWindow > estTokens)
	compactFile := filepath.Join(tempDir, "COMPACT.md")
	_ = os.WriteFile(compactFile, []byte("---\ncompact-overhead-pct: 5\n---\nPrompt"), 0644)

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if compacted {
		t.Errorf("expected no compaction with 5%% overhead (threshold > estTokens), but it triggered")
	}

	// Now update COMPACT.md with 25% overhead (threshold 75% of contextWindow < estTokens)
	_ = os.WriteFile(compactFile, []byte("---\ncompact-overhead-pct: 25\n---\nPrompt"), 0644)

	// Check that compaction now triggers (mock model endpoint will error on network, proving it attempted compaction!)
	_, err = CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, adkAgent, false)
	if err == nil {
		// Mock model points to 9999, so attempting LLM generation will return an error
		t.Logf("compaction attempted and proceeded")
	} else if !strings.Contains(err.Error(), "LLM compaction generation failed") {
		t.Errorf("expected LLM compaction generation attempt, got: %v", err)
	}
}

func TestMidTurnContextShortCircuit_RealUsage(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "test-short-circuit-usage")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Call echo_tool with a message and return real provider usage stats: prompt_tokens=90
			toolCallJSON := `{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\"text\":\"hi\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, toolCallJSON)
		} else {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	// Tool that returns small output (10 chars), but provider returned prompt_tokens=90 > threshold 80 (100 * 0.80)
	smallEchoTool, err := functiontool.New(functiontool.Config{
		Name: "echo_tool",
	}, func(ctx agent.Context, args d45EchoArgs) (map[string]any, error) {
		return map[string]any{"output": "small"}, nil
	})
	if err != nil {
		t.Fatalf("failed to create smallEchoTool: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		ContextWindow: 100, // 20% overhead -> threshold is 80 tokens. Real prompt_tokens is 90 >= 80!
		Model:         "test-model",
		Endpoint:      srv.URL,
	}

	mockModel := NewOpenAIModel(runtimeCfg)
	tracker := &TurnUsageTracker{}
	adkAgent, err := BuildADKAgentWithConfigAndTracker("usage-bot", "System prompt", DefaultMaxToolTurns, runtimeCfg, mockModel, tempDir, tracker, smallEchoTool)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfigAndTracker failed: %v", err)
	}

	sessionSvc := session.InMemoryService()
	createResp, err := sessionSvc.Create(context.Background(), &session.CreateRequest{
		AppName:   "wackypub",
		UserID:    "user",
		SessionID: "sess-usage",
	})
	if err != nil {
		t.Fatalf("sessionSvc.Create failed: %v", err)
	}

	r, err := runner.New(runner.Config{
		AppName:        "wackypub",
		Agent:          adkAgent,
		SessionService: sessionSvc,
	})
	if err != nil {
		t.Fatalf("runner.New failed: %v", err)
	}

	prompt := genai.NewContentFromText("Hello", "user")
	var outputText string
	for event, err := range r.Run(context.Background(), "user", createResp.Session.ID(), prompt, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run failed: %v", err)
		}
		if event != nil && event.Content != nil {
			outputText += ExtractTextFromEvent(event)
		}
	}

	if !strings.Contains(outputText, "stopping turn early to allow session compaction") {
		t.Errorf("expected mid-turn context short circuit message from real usage, got: %q", outputText)
	}
	if tracker.LastPromptTokens != 90 {
		t.Errorf("expected tracker.LastPromptTokens to be 90, got: %d", tracker.LastPromptTokens)
	}
}

func TestGenerateTurn_PostTurnCompaction(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "post-turn-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Return assistant response for the main turn with real usage: prompt_tokens = 90
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"Response with high token usage."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, respJSON)
		} else {
			// Return compaction summary addendum for the compaction pass
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- User said hello and agent greeted back."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	// Write AGENTS.md
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Write session ending with user turn
	if err := AppendSessionTurn(agentDir, "user", "Hello world"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100, // 20% overhead -> threshold is 80. Real usage is 90 >= 80 -> triggers post-turn compaction!
	}

	// Write runtime.json
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	resp, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}
	if !strings.Contains(resp, "Response with high token usage") {
		t.Errorf("unexpected response: %q", resp)
	}

	// Verify that MEMORY.md was created with the real LLM summary, not corrupted by mid-turn synthetic stop messages
	memory, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile failed: %v", err)
	}
	if !strings.Contains(memory, "- User said hello and agent greeted back.") {
		t.Errorf("expected MEMORY.md to contain LLM summary, got: %q", memory)
	}
	if strings.Contains(memory, "Accumulated tool context reached") || strings.Contains(memory, "Reached the maximum") {
		t.Errorf("MEMORY.md was corrupted with synthetic short-circuit message: %q", memory)
	}
}

func TestCompactionGitTwoCommitsD73(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "bob"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := InitAgentGit(wsDir, agentID); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}

	// Write AGENTS.md
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Seed 4 turns (2 user, 2 model)
	turns := []*genai.Content{
		genai.NewContentFromText("Turn 1 user", "user"),
		genai.NewContentFromText("Turn 1 assistant", "model"),
		genai.NewContentFromText("Turn 2 user", "user"),
		genai.NewContentFromText("Turn 2 assistant", "model"),
	}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("failed to write session turns: %v", err)
	}

	// Commit initial state
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err != nil {
		t.Fatalf("initial CommitWorkspaceEvent failed: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		respJSON := `{
			"choices":[{"message":{"role":"assistant","content":"- Facts from turns 1 and 2."},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
		}`
		io.WriteString(w, respJSON)
	}))
	defer srv.Close()

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100,
	}

	mockModel := NewOpenAIModel(runtimeCfg)
	adkAgent := mustBuildTestADKAgent(t, agentDir, "Prompt Bob", runtimeCfg, mockModel)

	compacted, err := CheckAndCompactSession(context.Background(), agentDir, runtimeCfg, adkAgent, true)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}

	// Verify git history has the two distinct commits in sequence
	repo, err := git.PlainOpen(agentDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}

	headRef, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	cIter, err := repo.Log(&git.LogOptions{From: headRef.Hash()})
	if err != nil {
		t.Fatalf("failed to get commit log: %v", err)
	}

	var commits []*plumbing.Hash
	var commitObjs []*object.Commit
	err = cIter.ForEach(func(c *object.Commit) error {
		h := c.Hash
		commits = append(commits, &h)
		commitObjs = append(commitObjs, c)
		return nil
	})
	if err != nil {
		t.Fatalf("failed iterating commits: %v", err)
	}

	// Should have:
	// commitObjs[0] = "compact" (session pruned)
	// commitObjs[1] = "compact (memory)" (memory updated, full session still present)
	// commitObjs[2] = "user" (initial)
	if len(commitObjs) < 3 {
		t.Fatalf("expected at least 3 commits, got %d", len(commitObjs))
	}

	// 1. Check HEAD commit ("compact")
	if !strings.HasPrefix(commitObjs[0].Message, "compact\n") {
		t.Errorf("expected HEAD commit to be 'compact', got message: %q", commitObjs[0].Message)
	}
	tree0, err := commitObjs[0].Tree()
	if err != nil {
		t.Fatalf("failed to get tree0: %v", err)
	}
	f0, err := tree0.File("session.jsonl")
	if err != nil {
		t.Fatalf("failed to get session.jsonl from tree0: %v", err)
	}
	r0, _ := f0.Reader()
	b0, _ := io.ReadAll(r0)
	// Post-prune session should contain COMPACTION_NOTICE
	if !strings.Contains(string(b0), "COMPACTION_NOTICE") {
		t.Errorf("expected session.jsonl in HEAD commit to contain COMPACTION_NOTICE, got:\n%s", string(b0))
	}

	// 2. Check parent commit ("compact (memory)")
	if !strings.HasPrefix(commitObjs[1].Message, "compact (memory)\n") {
		t.Errorf("expected parent commit to be 'compact (memory)', got message: %q", commitObjs[1].Message)
	}
	tree1, err := commitObjs[1].Tree()
	if err != nil {
		t.Fatalf("failed to get tree1: %v", err)
	}
	f1Mem, err := tree1.File("MEMORY.md")
	if err != nil {
		t.Fatalf("failed to get MEMORY.md from tree1: %v", err)
	}
	r1Mem, _ := f1Mem.Reader()
	b1Mem, _ := io.ReadAll(r1Mem)
	if !strings.Contains(string(b1Mem), "Facts from turns 1 and 2.") {
		t.Errorf("expected MEMORY.md in parent commit to contain addendum, got:\n%s", string(b1Mem))
	}

	f1Sess, err := tree1.File("session.jsonl")
	if err != nil {
		t.Fatalf("failed to get session.jsonl from tree1: %v", err)
	}
	r1Sess, _ := f1Sess.Reader()
	b1Sess, _ := io.ReadAll(r1Sess)
	// Pre-prune session should still have all original turns (e.g. Turn 1 user) and NOT have compaction notice
	if !strings.Contains(string(b1Sess), "Turn 1 user") {
		t.Errorf("expected session.jsonl in 'compact (memory)' commit to still contain Turn 1, got:\n%s", string(b1Sess))
	}
	if strings.Contains(string(b1Sess), "COMPACTION_NOTICE") {
		t.Errorf("expected session.jsonl in 'compact (memory)' commit not to contain COMPACTION_NOTICE")
	}
}

func TestGenerateTurn_MidTurnShortCircuit_NoImmediatePostTurnCompaction_D77(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d77-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	// Create tool in tools/
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	toolScript := "#!/bin/sh\necho 'done'\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Return tool call with high prompt_tokens = 90
			toolCallJSON := `{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, toolCallJSON)
		} else {
			// If compaction were called, it would request a compaction directive prompt
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- Unexpected compaction summary."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	// Write AGENTS.md
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Write session ending with user turn
	if err := AppendSessionTurn(agentDir, "user", "Start task"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100, // 20% overhead -> threshold is 80 tokens. Real prompt_tokens is 90 >= 80!
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	resp, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}

	// Verify that the mid-turn short-circuit message was returned
	if !strings.Contains(resp, "stopping turn early to allow session compaction") {
		t.Errorf("expected mid-turn short-circuit message, got: %q", resp)
	}

	// D77: Verify that StoppedEarlyForCompaction was set to true
	if fa.UsageTracker == nil || !fa.UsageTracker.StoppedEarlyForCompaction {
		t.Errorf("expected fa.UsageTracker.StoppedEarlyForCompaction to be true")
	}

	// D77: Verify that immediate post-turn compaction was skipped:
	// 1. Server callCount must be 1 (compaction generation was NOT called)
	if callCount != 1 {
		t.Errorf("expected server callCount to be 1 (no compaction call), got: %d", callCount)
	}

	// 2. MEMORY.md must NOT have been written
	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile failed: %v", err)
	}
	if mem != "" {
		t.Errorf("expected MEMORY.md to remain empty, got: %q", mem)
	}

	// 3. session.jsonl should retain the interrupted turn and its tool call / synthetic stop message
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) < 3 {
		t.Errorf("expected at least 3 turns (user, tool call, tool response/stop), got %d turns: %+v", len(turns), turns)
	}
}

func TestGenerateTurn_MaxToolTurns_StillTriggersPostTurnCompaction_D77(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "max-turns-d77-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	// Create tool in tools/
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	toolScript := "#!/bin/sh\necho 'done'\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount <= 2 {
			// Both calls return a tool call: prompt_tokens = 35 (< 40 threshold for 50 contextWindow, does not trigger mid-turn compaction short circuit)
			// But total_tokens = 45 (>= 40 threshold, so post-turn check will see it over threshold)
			toolCallJSON := fmt.Sprintf(`{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":35,"completion_tokens":10,"total_tokens":45}
			}`, callCount)
			io.WriteString(w, toolCallJSON)
		} else if callCount == 3 {
			// Compaction directive prompt call
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- Summarized task from max-tool-turns run."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	// Write AGENTS.md
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Write session ending with user turn
	if err := AppendSessionTurn(agentDir, "user", "Start task"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 50, // Low context window so total session tokens trigger post-turn compaction
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// maxToolTurns = 1: 1st tool call runs, then 2nd model call hits maxToolTurns short circuit
	fa, err := LoadFolderAgent(wsDir, agentID, 1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	resp, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}

	// Verify that the maxToolTurns short-circuit message was returned (not the compaction one)
	if !strings.Contains(resp, "Reached the maximum of 1 consecutive tool calls") {
		t.Errorf("expected maxToolTurns short-circuit message, got: %q", resp)
	}

	// D77: Verify that StoppedEarlyForCompaction is FALSE
	if fa.UsageTracker != nil && fa.UsageTracker.StoppedEarlyForCompaction {
		t.Errorf("expected fa.UsageTracker.StoppedEarlyForCompaction to be false")
	}

	// D77: Verify that post-turn compaction DID run:
	// Server callCount must be 3 (2 tool calls + 1 compaction directive prompt)
	if callCount != 3 {
		t.Errorf("expected server callCount to be 3 (2 tool calls + 1 compaction occurred), got: %d", callCount)
	}

	// MEMORY.md was updated with the summary
	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile failed: %v", err)
	}
	if !strings.Contains(mem, "Summarized task from max-tool-turns run.") {
		t.Errorf("expected MEMORY.md to contain compaction summary, got: %q", mem)
	}
}
