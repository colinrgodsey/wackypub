package agent

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// blockToolsStubModel emits a functionCall on its first invocation (the risky compaction
// case), then a plain-text summary on every later call once the denial response is fed back.
// It records whether the tool declaration reached the request payload (acceptance 2) and
// whether it ever saw the denial text.
type blockToolsStubModel struct {
	calls            int32
	declaredEchoTool bool
	sawDenial        bool
}

func (m *blockToolsStubModel) Name() string { return "block-tools-stub" }

func (m *blockToolsStubModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&m.calls, 1)

		reqText := ""
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p == nil {
					continue
				}
				reqText += p.Text
				if p.FunctionResponse != nil {
					// FunctionResponse payload carries the denial result map; flatten it so the
					// stub can verify the model actually received the denial on the next call.
					if v, ok := p.FunctionResponse.Response["result"].(string); ok {
						reqText += " " + v
					}
				}
			}
		}
		if strings.Contains(reqText, "echo_tool") {
			m.declaredEchoTool = true
		}
		// Tool declarations travel in req.Config.Tools, not the content parts - this is the
		// cache-prefix identity D45 chose to preserve.
		if req.Config != nil {
			for _, td := range req.Config.Tools {
				for _, fd := range td.FunctionDeclarations {
					if fd != nil && fd.Name == "echo_tool" {
						m.declaredEchoTool = true
					}
				}
			}
		}
		if strings.Contains(reqText, "denied: tools are unavailable during compaction") {
			m.sawDenial = true
		}

		if atomic.LoadInt32(&m.calls) == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{FunctionCall: &genai.FunctionCall{Name: "echo_tool", Args: map[string]any{"text": "hello"}}},
					},
				},
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: "Compaction summary complete."}},
			},
		}, nil)
	}
}

func TestCompactionBlockTools_ToolInvocationDeniedAndSummaryCompletes(t *testing.T) {
	tempDir := t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
		genai.NewContentFromText("user two", "user"),
		genai.NewContentFromText("model two", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "Initial Memory"); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	var toolRuns int32
	echoTool, err := functiontool.New(functiontool.Config{
		Name: "echo_tool",
	}, func(ctx agent.Context, args d45EchoArgs) (map[string]any, error) {
		atomic.AddInt32(&toolRuns, 1)
		return map[string]any{"output": args.Text}, nil
	})
	if err != nil {
		t.Fatalf("build echo tool: %v", err)
	}

	stub := &blockToolsStubModel{}
	// Raise the window far above the tool-loop context check (which would stop the turn
	// early on the second call and never feed the denial back); compaction is forced below.
	runtimeCfg := &RuntimeConfig{ContextWindow: 100000}

	compacted, denials, err := compactionRunWithToolDenied(t, tempDir, runtimeCfg, stub, echoTool)
	if err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to occur")
	}

	// Acceptance 1: the emitted functionCall was NOT executed; exactly one denial counted;
	// the model saw the denial and completed the summary.
	if got := atomic.LoadInt32(&toolRuns); got != 0 {
		t.Fatalf("echo_tool executed %d times during compaction, want 0", got)
	}
	if denials != 1 {
		t.Fatalf("expected exactly 1 tool denial, got %d", denials)
	}
	if !stub.sawDenial {
		t.Fatal("stub model never saw the denial response text")
	}

	// Acceptance 2: the tool declaration was present in the request payload (cache prefix
	// preserved - the model saw echo_tool even though it could not run it).
	if !stub.declaredEchoTool {
		t.Fatal("tool declaration echo_tool missing from compaction request payload")
	}

	// Acceptance 1 tail: the completed summary landed in MEMORY.md.
	mem, err := ReadMemoryFile(tempDir)
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if !strings.Contains(mem, "Compaction summary complete.") {
		t.Fatalf("summary missing from MEMORY.md: %q", mem)
	}
}

// compactionRunWithToolDenied builds the compaction-scoped agent (same tools/declarations as
// a normal build, but tool invocation denied by the BeforeToolCallback) exactly as
// loadFolderAgentFromRuntime does, runs CheckAndCompactSession, and returns the run's denial
// count from the same pointer the callback increments.
func compactionRunWithToolDenied(t *testing.T, agentDir string, runtimeCfg *RuntimeConfig, stub model.LLM, tools ...tool.Tool) (bool, int64, error) {
	t.Helper()
	var denials int64
	ca, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, runtimeCfg, stub, agentDir, nil, &denials, tools...)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	compacted, err := CheckAndCompactSession(context.Background(), agentDir, runtimeCfg, ca, true, nil, &denials)
	return compacted, denials, err
}

// TestCompactionBlockTools_NormalGenerationStillExecutesTools pins acceptance 3: the deny
// callback is scoped to the compaction agent only. A normal agent built from the same model
// and tool must still execute the functionCall the stub emits on its first call.
func TestCompactionBlockTools_NormalGenerationStillExecutesTools(t *testing.T) {
	tempDir := t.TempDir()
	turns := []*genai.Content{genai.NewContentFromText("go", "user")}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}

	var toolRuns int32
	echoTool, err := functiontool.New(functiontool.Config{
		Name: "echo_tool",
	}, func(ctx agent.Context, args d45EchoArgs) (map[string]any, error) {
		atomic.AddInt32(&toolRuns, 1)
		return map[string]any{"output": args.Text}, nil
	})
	if err != nil {
		t.Fatalf("build echo tool: %v", err)
	}

	stub := &blockToolsStubModel{}
	runtimeCfg := &RuntimeConfig{ContextWindow: 100000}
	// Normal build - no deny callback.
	normalAgent, err := BuildADKAgentWithConfig("agent", "system", DefaultMaxToolTurns, runtimeCfg, stub, echoTool)
	if err != nil {
		t.Fatalf("build normal agent: %v", err)
	}

	sessionSvc := session.InMemoryService()
	createResp, err := sessionSvc.Create(context.Background(), &session.CreateRequest{
		AppName:   "wackypub",
		UserID:    "user",
		SessionID: "sess-normal",
	})
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName:        "wackypub",
		Agent:          normalAgent,
		SessionService: sessionSvc,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	prompt := genai.NewContentFromText("Hello", "user")
	for event, err := range r.Run(context.Background(), "user", createResp.Session.ID(), prompt, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		_ = event
	}

	if got := atomic.LoadInt32(&toolRuns); got != 1 {
		t.Fatalf("echo_tool executed %d times in normal generation, want 1", got)
	}
}

// TestCompactionBlockTools_MultiPartFunctionCallsDenied is the #50 fast-follow: a model can
// emit MULTIPLE functionCalls in a single response (batch tool calls). Each must be denied;
// the tool must never execute; the denial count must equal the batch size; and the summary
// still completes once the denials are fed back.
func TestCompactionBlockTools_MultiPartFunctionCallsDenied(t *testing.T) {
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

	var toolRuns int32
	echoTool, err := functiontool.New(functiontool.Config{
		Name: "echo_tool",
	}, func(ctx agent.Context, args d45EchoArgs) (map[string]any, error) {
		atomic.AddInt32(&toolRuns, 1)
		return map[string]any{"output": args.Text}, nil
	})
	if err != nil {
		t.Fatalf("build echo tool: %v", err)
	}

	// Stub that emits two functionCalls on call 1, then a plain summary on call 2. Records
	// whether the tool DECLARATIONS reached the payload.
	var declaredCnt int32
	stub := &multiCallStubModel{declarations: &declaredCnt, sawDenial: false}
	runtimeCfg := &RuntimeConfig{ContextWindow: 100000}

	// Register both batch-called tools under their real names so each functionCall resolves
	// to a registered tool and hits the deny callback (a not-found call would take the
	// onError path and not count as a denial).
	getTool, err := functiontool.New(functiontool.Config{
		Name: "get_scratchpad",
	}, func(ctx agent.Context, args multiCallGetArgs) (map[string]any, error) {
		return map[string]any{"output": "never reached"}, nil
	})
	if err != nil {
		t.Fatalf("build get_scratchpad stub: %v", err)
	}

	compacted, denials, err := compactionRunWithToolDenied2(t, tempDir, runtimeCfg, stub, echoTool, getTool)
	if err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to occur")
	}
	if got := atomic.LoadInt32(&toolRuns); got != 0 {
		t.Fatalf("echo_tool executed %d times, want 0", got)
	}
	if denials != 2 {
		t.Fatalf("expected 2 tool denials (batch), got %d", denials)
	}
	if atomic.LoadInt32(&declaredCnt) == 0 {
		t.Fatal("tool declarations missing from compaction request payload")
	}
	mem, err := ReadMemoryFile(tempDir)
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if !strings.Contains(mem, "Batch summary complete.") {
		t.Fatalf("summary missing from MEMORY.md: %q", mem)
	}
}

// multiCallGetArgs is the minimal argument shape for the get_scratchpad stub tool.
type multiCallGetArgs struct {
	ID string `json:"id"`
}

// multiCallStubModel emits two functionCalls on the first invocation and a text summary on
// every later invocation.
type multiCallStubModel struct {
	calls        int32
	declarations *int32
	sawDenial    bool
}

func (m *multiCallStubModel) Name() string { return "multi-call-stub" }

func (m *multiCallStubModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&m.calls, 1)

		// Declarations live in req.Config.Tools.
		if req.Config != nil {
			for _, td := range req.Config.Tools {
				for _, fd := range td.FunctionDeclarations {
					if fd != nil {
						atomic.StoreInt32(m.declarations, 1)
					}
				}
			}
		}
		// Check the denial responses made it into the request history.
		reqText := ""
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p == nil {
					continue
				}
				reqText += p.Text
				if p.FunctionResponse != nil {
					if v, ok := p.FunctionResponse.Response["result"].(string); ok {
						reqText += " " + v
					}
				}
			}
		}
		if strings.Contains(reqText, "denied: tools are unavailable during compaction") {
			m.sawDenial = true
		}

		if atomic.LoadInt32(&m.calls) == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{FunctionCall: &genai.FunctionCall{Name: "echo_tool", Args: map[string]any{"text": "a"}}},
						{FunctionCall: &genai.FunctionCall{Name: "get_scratchpad", Args: map[string]any{"id": "abcd"}}},
					},
				},
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: "Batch summary complete."}},
			},
		}, nil)
	}
}

// compactionRunWithToolDenied2 mirrors compactionRunWithToolDenied but exposes the stub's
// declaration counter through two runnable tools (echo_tool + get_scratchpad are both part
// of the standard folder toolset, so the batch calls resolve to real denials).
func compactionRunWithToolDenied2(t *testing.T, agentDir string, runtimeCfg *RuntimeConfig, stub model.LLM, tools ...tool.Tool) (bool, int64, error) {
	t.Helper()
	var denials int64
	// get_scratchpad is a real built-in with a different signature; provide a stand-in tool
	// implementation registered under that name so the batch denial path resolves both.
	ca, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, runtimeCfg, stub, agentDir, nil, &denials, tools...)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}
	compacted, err := CheckAndCompactSession(context.Background(), agentDir, runtimeCfg, ca, true, nil)
	return compacted, denials, err
}
