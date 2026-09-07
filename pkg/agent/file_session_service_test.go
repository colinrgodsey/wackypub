package agent

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type dummyModel struct {
	response string
}

func (m *dummyModel) Name() string { return "dummy-model" }

func (m *dummyModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		res := &model.LLMResponse{
			Content: genai.NewContentFromText(m.response, "model"),
		}
		yield(res, nil)
	}
}

func TestFileSessionService(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// Write AGENTS.md
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt for Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Append user turn
	uMsg := genai.NewContentFromText("Hello Bob", "user")
	if err := AppendSessionContent(agentDir, uMsg); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	svc := NewFileSessionService(wsDir)

	mockModel := &dummyModel{response: "Hello Traveler"}
	ag, err := BuildADKAgent("bob", "System prompt for Bob", 10, mockModel)
	if err != nil {
		t.Fatalf("BuildADKAgent failed: %v", err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "wackypub",
		Agent:             ag,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New failed: %v", err)
	}

	var events []*genai.Content
	for event, err := range r.Run(context.Background(), "user", "bob", nil, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("r.Run failed: %v", err)
		}
		if event != nil && event.Content != nil {
			events = append(events, event.Content)
		}
	}

	if len(events) == 0 {
		t.Fatalf("expected events, got none")
	}

	// Read session turns from disk
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	if len(turns) < 2 {
		t.Fatalf("expected at least 2 turns in session.jsonl (user + model), got %d", len(turns))
	}

	lastTurn := turns[len(turns)-1]
	if lastTurn.Role != "model" {
		t.Errorf("expected last turn role model, got %s", lastTurn.Role)
	}
}

func TestNoUserTurnDuplication(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"dummy-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// Add single user turn
	uMsg := genai.NewContentFromText("unique user query", "user")
	if err := AppendSessionContent(agentDir, uMsg); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, "bob", 10)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	fa.Model = &dummyModel{response: "Bob answer"}
	fa.ADKAgent, err = BuildADKAgent("bob", fa.SystemPrompt, 10, fa.Model)
	if err != nil {
		t.Fatalf("BuildADKAgent failed: %v", err)
	}

	resp, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}
	if resp != "Bob answer" {
		t.Errorf("expected 'Bob answer', got %q", resp)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	// Verify exact turn sequence: 1 user turn, 1 model turn (no duplicate user turn!)
	if len(turns) != 2 {
		t.Fatalf("expected exactly 2 turns in session.jsonl, got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Parts[0].Text != "unique user query" {
		t.Errorf("turn 0 mismatch: %v", turns[0])
	}
	if turns[1].Role != "model" || turns[1].Parts[0].Text != "Bob answer" {
		t.Errorf("turn 1 mismatch: %v", turns[1])
	}
}

type loopingToolModel struct {
	toolName string
}

func (m *loopingToolModel) Name() string { return "looping-model" }

func (m *loopingToolModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		res := &model.LLMResponse{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: m.toolName,
							Args: map[string]any{"text": "loop"},
						},
					},
				},
			},
		}
		yield(res, nil)
	}
}

func TestMaxToolTurnsLimit(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"dummy-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	uMsg := genai.NewContentFromText("trigger loop", "user")
	if err := AppendSessionContent(agentDir, uMsg); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	maxTurns := 3
	fa, err := LoadFolderAgent(wsDir, "bob", maxTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	fa.Model = &loopingToolModel{toolName: "create_scratchpad"}

	toolsMap, _, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}
	var tools []tool.Tool
	for _, tr := range toolsMap {
		tools = append(tools, tr)
	}

	// Re-bind ADKAgent with mock model and configured maxTurns from LoadFolderAgent
	fa.ADKAgent, err = BuildADKAgent("bob", fa.SystemPrompt, maxTurns, fa.Model, tools...)
	if err != nil {
		t.Fatalf("BuildADKAgent failed: %v", err)
	}

	// Hitting the cap should stop the turn gracefully (a warning on stderr,
	// not an error) rather than failing the whole generation - see
	// BuildADKAgent's BeforeModelCallbacks.
	resp, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("expected no error when the max tool turns cap is hit, got: %v", err)
	}
	if !strings.Contains(resp, "maximum of") || !strings.Contains(resp, "continue") {
		t.Fatalf("expected a response hinting at the tool turn cap and how to continue, got: %q", resp)
	}
}

func TestLoadFolderAgentMaxToolTurns(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"dummy-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, "bob", 3)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	if fa.MaxToolTurns != 3 {
		t.Errorf("expected MaxToolTurns 3, got %d", fa.MaxToolTurns)
	}
}
