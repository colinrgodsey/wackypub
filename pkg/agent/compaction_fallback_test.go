package agent

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	adkAgent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// fallbackStubModel is a stub LLM whose behavior is fixed per instance: the primary level
// returns a qualifying error on EVERY call (simulating an exhausted/degraded backend),
// the fallback level returns a summary. No HTTP, no retries - deterministic by
// construction, unlike the 503-with-retry family in runtime_fallback_test.go.
type fallbackStubModel struct {
	name   string
	fail   bool
	answer string
	calls  int32
}

func (m *fallbackStubModel) Name() string { return m.name }

func (m *fallbackStubModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		atomic.AddInt32(&m.calls, 1)
		if m.fail {
			yield(nil, fmt.Errorf("runner execution error: POST http://127.0.0.1:5000/chat/completions: 503 Service Unavailable"))
			return
		}
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText(m.answer, "model"),
		}, nil)
	}
}

func compactionFallbackFixture(t *testing.T, primary, fallback *fallbackStubModel) (string, bool, int32, int32, error) {
	t.Helper()
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

	primaryCfg := &RuntimeConfig{
		ContextWindow: 100000,
		Endpoint:      "http://primary.test",
		Model:         "primary-model",
		Fallback: &RuntimeConfig{
			ContextWindow: 100000,
			Endpoint:      "http://fallback.test",
			Model:         "fallback-model",
		},
	}

	var primaryDenials, fallbackDenials int64
	_ = fallbackDenials
	primaryAgent, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, primaryCfg, primary, tempDir, nil, &primaryDenials)
	if err != nil {
		t.Fatalf("build primary compaction agent: %v", err)
	}

	loader := func(cfg *RuntimeConfig) (adkAgent.Agent, *int64, error) {
		if cfg == primaryCfg {
			return primaryAgent, &primaryDenials, nil
		}
		var d int64
		ag, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, fallback, tempDir, nil, &d)
		if err != nil {
			return nil, nil, err
		}
		return ag, &d, nil
	}

	compacted, err := CheckAndCompactSessionWithFallback(context.Background(), tempDir, primaryCfg, loader, true, nil)
	mem, _ := ReadMemoryFile(tempDir)
	return mem, compacted, atomic.LoadInt32(&primary.calls), atomic.LoadInt32(&fallback.calls), err
}

// TestCompactionFallback_FailForwardToHealthyFallback pins the core bug: a primary that
// 503s for the whole attempt + a healthy fallback => compaction SUCCEEDS with the summary
// from the fallback. This is the compaction-side analog of
// TestRuntimeFallback_FailForwardPerTurn, written deterministically (stub models, no
// openai-go retry timing).
func TestCompactionFallback_FailForwardToHealthyFallback(t *testing.T) {
	primary := &fallbackStubModel{name: "primary", fail: true}
	fallback := &fallbackStubModel{name: "fallback", answer: "compacted by fallback"}

	mem, compacted, primaryCalls, fallbackCalls, err := compactionFallbackFixture(t, primary, fallback)
	if err != nil {
		t.Fatalf("compaction with fallback errored: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction to succeed (fallback engaged)")
	}
	if primaryCalls == 0 {
		t.Fatal("primary should have been attempted first")
	}
	if fallbackCalls == 0 {
		t.Fatal("fallback should have been attempted after primary's qualifying error")
	}
	// Default COMPACT.md is append-only: the fallback summary must be appended after the
	// existing memory (this is what the pre-fallback path would have written too).
	if !strings.Contains(mem, "compacted by fallback") {
		t.Fatalf("memory must contain the fallback summary, got %q", mem)
	}
}

// TestCompactionFallback_AllLevelsFail pins the exhausted-chain behavior: when every
// level qualifies-fails, compaction returns an error naming the last failure and the
// memory file is untouched.
func TestCompactionFallback_AllLevelsFail(t *testing.T) {
	primary := &fallbackStubModel{name: "primary", fail: true}
	fallback := &fallbackStubModel{name: "fallback", fail: true}

	mem, compacted, _, _, err := compactionFallbackFixture(t, primary, fallback)
	if err == nil {
		t.Fatal("expected compaction to fail when every backend 503s")
	}
	if compacted {
		t.Fatal("compaction must not report success when every backend failed")
	}
	if !strings.Contains(mem, "Initial Memory") {
		t.Fatalf("failed compaction must not touch MEMORY.md, got %q", mem)
	}
}

// TestCompactionFallback_NoFallbackKeepsOldBehavior pins that a single-backend config
// with a qualifying failure still errors exactly as the pre-fallback path did.
func TestCompactionFallback_NoFallbackKeepsOldBehavior(t *testing.T) {
	primary := &fallbackStubModel{name: "primary", fail: true}
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

	primaryCfg := &RuntimeConfig{
		ContextWindow: 100000,
		Endpoint:      "http://primary.test",
		Model:         "primary-model",
	}
	var denials int64
	ag, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, primaryCfg, primary, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build agent: %v", err)
	}
	loader := func(cfg *RuntimeConfig) (adkAgent.Agent, *int64, error) { return ag, &denials, nil }

	_, err = CheckAndCompactSessionWithFallback(context.Background(), tempDir, primaryCfg, loader, true, nil)
	if err == nil {
		t.Fatal("expected compaction to fail when the only backend qualifies-fails")
	}
}
