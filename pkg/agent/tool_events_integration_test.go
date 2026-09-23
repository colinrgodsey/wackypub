package agent

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	agent "google.golang.org/adk/v2/agent"
	model "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	tool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// visSequenceStubModel emits, in order: a text chunk, an echo_tool functionCall, and a
// final text chunk - the canonical native tool-call shape for the visibility tests.
type visSequenceStubModel struct {
	calls int32
}

func (m *visSequenceStubModel) Name() string { return "vis-sequence-stub" }

func (m *visSequenceStubModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		c := atomic.AddInt32(&m.calls, 1)
		if c == 1 {
			// Narration + the tool call travel in ONE response: the text chunk ships before the
			// call event, and the runner continues after the tool executes (coalescing invariant).
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{Text: "let me check"},
				{FunctionCall: &genai.FunctionCall{Name: "echo_tool", Args: map[string]any{"text": "hello"}}},
			}}}, nil)
			return
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "the result is 42"}}}}, nil)
	}
}

// visEchoTool is an executable map-returning echo tool, the shape the visibility AfterTool
// callback expects to see a completed result for.
func visEchoTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New(functiontool.Config{
		Name:        "echo_tool",
		Description: "Echoes the given text back",
	}, func(ctx agent.Context, args visEchoArgs) (map[string]any, error) {
		return map[string]any{"output": args.Text}, nil
	})
	if err != nil {
		t.Fatalf("build echo tool: %v", err)
	}
	return tl
}

// visEchoArgs is the echo tool's typed args.
type visEchoArgs struct {
	Text string `json:"text"`
}

func TestToolEventEmission_NativeTurnAndCoalescing(t *testing.T) {
	sink := NewToolEventSink()
	stub := &visSequenceStubModel{}
	echoTool := visEchoTool(t)
	ag, err := BuildADKAgentWithConfigAndTrackerWithSink("visagent", "system", DefaultMaxToolTurns, &RuntimeConfig{ContextWindow: 100000}, stub, t.TempDir(), &TurnUsageTracker{}, sink, echoTool)
	if err != nil {
		t.Fatalf("build agent: %v", err)
	}

	sess := session.InMemoryService()
	resp, err := sess.Create(context.Background(), &session.CreateRequest{AppName: "wackypub", UserID: "user", SessionID: "vis-session"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "wackypub", Agent: ag, SessionService: sess})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	// Mirror the proto-handler drain contract: after every yielded chunk take the pending
	// tool events and record the ORDER they sit in relative to text chunks.
	var textChunks []string
	var events []ToolEvent
	for event, err := range r.Run(context.Background(), "user", resp.Session.ID(), genai.NewContentFromText("go", "user"), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		if event == nil {
			continue
		}
		if text := ExtractTextFromEvent(event); text != "" {
			textChunks = append(textChunks, text)
		}
		events = append(events, sink.Drain()...)
	}
	events = append(events, sink.Drain()...)

	if len(textChunks) != 2 || textChunks[0] != "let me check" || textChunks[1] != "the result is 42" {
		t.Fatalf("unexpected text chunks: %v", textChunks)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 tool events, got %d: %+v", len(events), events)
	}
	call, update := events[0], events[1]
	if call.Status != "" || call.Denied {
		t.Errorf("expected announce event (status empty), got %+v", call)
	}
	if call.CallID != update.CallID {
		t.Errorf("announce/update call_id mismatch: %s vs %s", call.CallID, update.CallID)
	}
	if call.ToolName != "echo_tool" || update.ToolName != "echo_tool" {
		t.Errorf("tool name mismatch: %+v %+v", call, update)
	}
	if update.Status != "completed" {
		t.Errorf("expected completed update, got %q (head %q)", update.Status, update.ResultHead)
	}
	if update.ResultBytes == 0 {
		t.Errorf("expected result bytes on update, got %d", update.ResultBytes)
	}
	if !strings.Contains(call.ArgsSummary, "text=hello") {
		t.Errorf("args summary missing args: %s", call.ArgsSummary)
	}
}

func TestToolEventEmission_DeniedCompaction(t *testing.T) {
	tempDir := t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "Initial Memory"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	stub := &blockToolsStubModel{}
	runtimeCfg := &RuntimeConfig{ContextWindow: 100000}
	sink := NewToolEventSink()
	var denials int64
	ca, err := BuildADKAgentWithConfigAndTrackerForCompactionWithSink("agent", "system", DefaultMaxToolTurns, runtimeCfg, stub, tempDir, nil, &denials, sink, visEchoTool(t))
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	compacted, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, &denials)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction")
	}
	events := sink.Drain()
	if len(events) != 2 {
		t.Fatalf("expected 2 denied events, got %d: %+v", len(events), events)
	}
	call, update := events[0], events[1]
	if !call.Denied || call.Status != "denied" {
		t.Errorf("expected denied announce, got %+v", call)
	}
	if update.Status != "denied" {
		t.Errorf("expected denied update, got %+v", update)
	}
	if call.CallID != update.CallID {
		t.Errorf("announce/update call_id mismatch")
	}
	if denials != 1 {
		t.Errorf("expected 1 denial count, got %d", denials)
	}
}

// TestToolEventJournal_SurvivesCompaction pins workshop flag (a): result/store references
// must survive compaction. The journal is a per-agent append-only JSONL sidecar that
// compaction never rewrites - entries written before a compaction run remain readable and
// the file itself is not deleted.
func TestToolEventJournal_SurvivesCompaction(t *testing.T) {
	tempDir := t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "Initial Memory"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// Seed a journal entry BEFORE compaction (simulating an earlier turn's tool call whose
	// result ref must survive the archive pass).
	journalPath := filepath.Join(tempDir, "tool-journal.jsonl")
	if err := os.WriteFile(journalPath, []byte(`{"call_id":"pre-1","tool_name":"create_scratchpad","status":"completed","result_ref":"ref-pre-1"}`+string(rune(10))), 0644); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	stub := &blockToolsStubModel{}
	runtimeCfg := &RuntimeConfig{ContextWindow: 100000}
	sink := NewToolEventSinkWithJournal(journalPath)
	var denials int64
	ca, err := BuildADKAgentWithConfigAndTrackerForCompactionWithSink("agent", "system", DefaultMaxToolTurns, runtimeCfg, stub, tempDir, nil, &denials, sink, visEchoTool(t))
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	if _, err := CheckAndCompactSession(context.Background(), tempDir, runtimeCfg, ca, true, nil, &denials); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// The denied event was journaled (append) and the pre-seeded line is still intact.
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal after compaction: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "ref-pre-1") {
		t.Errorf("pre-compaction journal entry lost after compaction: %s", content)
	}
	if !strings.Contains(content, "denied") {
		t.Errorf("denied event not journaled: %s", content)
	}
}
