package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// TestD88_MidTurnBailTriggersCompactionAndContinuation verifies that a mid-turn bail
// due to tool context exceeding compaction threshold immediately triggers post-turn
// compaction, appends the continuation sentinel, and runs continuation without user intervention.
func TestD88_MidTurnBailTriggersCompactionAndContinuation(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d88-bail-bot"
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

	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 1 {
			// Call 1: Tool call with prompt_tokens = 90 (>= threshold 80)
			toolCallJSON := `{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, toolCallJSON)
		} else if c == 2 {
			// Call 2: Compaction summarizer call
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- Compaction summary of prior work."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		} else {
			// Call 3: Continuation turn response
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"Task finished after compaction."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "Start task"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100, // 20% overhead -> threshold is 80. Real prompt_tokens is 90 >= 80
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

	if !strings.Contains(resp, "stopping turn early to allow session compaction") {
		t.Errorf("expected mid-turn short-circuit message, got: %q", resp)
	}
	if !strings.Contains(resp, "Task finished after compaction.") {
		t.Errorf("expected continuation turn response, got: %q", resp)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()
	if count != 3 {
		t.Errorf("expected server callCount to be 3 (tool call, compaction, continuation), got: %d", count)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile failed: %v", err)
	}
	if !strings.Contains(mem, "Compaction summary of prior work.") {
		t.Errorf("expected MEMORY.md to contain compaction summary, got: %q", mem)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	var foundSentinel bool
	for _, trn := range turns {
		if strings.Contains(ContentText(trn), `<CONTINUATION reason="post-compaction">`) {
			foundSentinel = true
			break
		}
	}
	if !foundSentinel {
		t.Errorf("expected session.jsonl to contain sentinel continuation turn")
	}

	lastTurn := turns[len(turns)-1]
	if lastTurn.Role != "model" || !strings.Contains(ContentText(lastTurn), "Task finished after compaction.") {
		t.Errorf("expected last turn to be model response after continuation, got %+v", lastTurn)
	}
}

// TestD88_DeferredImageQueueTriggersContinuation verifies that when an agent retrieves an image
// from a scratchpad via get_scratchpad, the harness queues the <IMAGE> user turn and automatically
// triggers a follow-up continuation turn where the image is analyzed.
func TestD88_DeferredImageQueueTriggersContinuation(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d88-img-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	pngBytes := createTestImage(100, 100, false)
	imgEntry, err := CreateBinaryScratchpad(agentDir, pngBytes, "test", "image/png")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 1 {
			// Call 1: Model calls get_scratchpad tool
			toolCallJSON := fmt.Sprintf(`{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_get_sp","type":"function","function":{"name":"get_scratchpad","arguments":"{\"id\":\"%s\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
			}`, imgEntry.ID)
			io.WriteString(w, toolCallJSON)
		} else {
			// Call 2: Continuation turn model response (receives the <IMAGE> user turn)
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"I now see the image: it is a 100x100 PNG test image."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	runtimeCfg := &RuntimeConfig{
		Provider:          "openai",
		Model:             "test-model",
		Endpoint:          srv.URL,
		MaxImageDimension: 400,
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "Please inspect the image in scratchpad"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	resp, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}

	if !strings.Contains(resp, "has been queued") {
		t.Errorf("expected initial turn output in response, got: %q", resp)
	}
	if !strings.Contains(resp, "I now see the image: it is a 100x100 PNG test image.") {
		t.Errorf("expected continuation turn output in response, got: %q", resp)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()
	if count != 2 {
		t.Errorf("expected callCount to be 2 (tool call, continuation turn), got: %d", count)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	var foundImageTurn bool
	for _, trn := range turns {
		if trn.Role == "user" && len(trn.Parts) == 2 && trn.Parts[1].InlineData != nil {
			foundImageTurn = true
			if !strings.Contains(trn.Parts[0].Text, fmt.Sprintf("scratchpad '%s'", imgEntry.ID)) {
				t.Errorf("unexpected image label: %s", trn.Parts[0].Text)
			}
			break
		}
	}
	if !foundImageTurn {
		t.Errorf("expected session.jsonl to contain deferred image user turn")
	}

	lastTurn := turns[len(turns)-1]
	if lastTurn.Role != "model" || !strings.Contains(ContentText(lastTurn), "I now see the image") {
		t.Errorf("expected last turn to be continuation model turn, got %+v", lastTurn)
	}
}

// TestD88_CoincidenceOrderingImageAndBail verifies that when both image deferral and compaction bail
// occur in one turn, compaction runs first, image turn is appended second, and exactly one continuation
// turn triggers without a redundant sentinel turn.
func TestD88_CoincidenceOrderingImageAndBail(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d88-coincidence-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	pngBytes := createTestImage(100, 100, false)
	imgEntry, err := CreateBinaryScratchpad(agentDir, pngBytes, "test", "image/png")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	// Seed session with prior turns so compaction has turns to trim
	priorTurns := []*genai.Content{
		genai.NewContentFromText("Prior user message 1", "user"),
		genai.NewContentFromText("Prior model response 1", "model"),
		genai.NewContentFromText("Prior user message 2", "user"),
		genai.NewContentFromText("Prior model response 2", "model"),
		genai.NewContentFromText("Please load image and continue", "user"),
	}
	if err := WriteSessionTurns(agentDir, priorTurns); err != nil {
		t.Fatalf("WriteSessionTurns failed: %v", err)
	}

	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 1 {
			// Call 1: Model calls get_scratchpad tool with prompt_tokens: 90 (>= threshold 80)
			toolCallJSON := fmt.Sprintf(`{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_get_sp","type":"function","function":{"name":"get_scratchpad","arguments":"{\"id\":\"%s\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`, imgEntry.ID)
			io.WriteString(w, toolCallJSON)
		} else if c == 2 {
			// Call 2: Compaction summarizer LLM call
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- Coincidence summary of prior exchanges."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		} else {
			// Call 3: Single continuation turn response
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"Processed image successfully after coincidence compaction."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":40,"completion_tokens":15,"total_tokens":55}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	runtimeCfg := &RuntimeConfig{
		Provider:          "openai",
		Model:             "test-model",
		Endpoint:          srv.URL,
		ContextWindow:     100, // threshold = 80
		MaxImageDimension: 400,
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

	if !strings.Contains(resp, "stopping turn early to allow session compaction") {
		t.Errorf("expected mid-turn bail message, got: %q", resp)
	}
	if !strings.Contains(resp, "Processed image successfully after coincidence compaction.") {
		t.Errorf("expected continuation response, got: %q", resp)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()
	if count != 3 {
		t.Errorf("expected callCount to be 3 (tool call, compaction, single continuation), got: %d", count)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile failed: %v", err)
	}
	if !strings.Contains(mem, "Coincidence summary of prior exchanges.") {
		t.Errorf("expected MEMORY.md to contain coincidence summary, got: %q", mem)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	// Verify: Exactly one continuation turn was triggered driven by the image;
	// no redundant sentinel was appended!
	for _, trn := range turns {
		if strings.Contains(ContentText(trn), `<CONTINUATION reason="post-compaction">`) {
			t.Errorf("expected NO post-compaction sentinel turn during coincidence continuation, but found one")
		}
	}

	var foundImageTurn bool
	for _, trn := range turns {
		if trn.Role == "user" && len(trn.Parts) == 2 && trn.Parts[1].InlineData != nil {
			foundImageTurn = true
			break
		}
	}
	if !foundImageTurn {
		t.Errorf("expected session.jsonl to contain deferred image user turn")
	}

	lastTurn := turns[len(turns)-1]
	if lastTurn.Role != "model" || !strings.Contains(ContentText(lastTurn), "Processed image successfully after coincidence compaction.") {
		t.Errorf("expected last turn to be continuation model turn, got %+v", lastTurn)
	}
}

// TestD88_ColdStartPreTurnEmergencyValve verifies that an uncompacted cold-start session
// with EstimateTokens >= threshold forcefully compacts before call 1 of generation.
func TestD88_ColdStartPreTurnEmergencyValve(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d88-coldstart-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// Seed session with large turns so EstimateTokens(turns) >= 80
	// 4 turns of 100 characters each will estimate ~100 tokens (> 80)
	longText := strings.Repeat("test token text ", 15) // ~240 characters
	seedTurns := []*genai.Content{
		genai.NewContentFromText("Initial user message: "+longText, "user"),
		genai.NewContentFromText("Initial model reply: "+longText, "model"),
		genai.NewContentFromText("Followup user question: "+longText, "user"),
		genai.NewContentFromText("Followup model reply: "+longText, "model"),
		genai.NewContentFromText("Please proceed with next step.", "user"),
	}
	if err := WriteSessionTurns(agentDir, seedTurns); err != nil {
		t.Fatalf("WriteSessionTurns failed: %v", err)
	}

	initialTokenEstimate := EstimateTokens(seedTurns, false)
	if initialTokenEstimate < 80 {
		t.Fatalf("expected initialTokenEstimate >= 80, got %d", initialTokenEstimate)
	}

	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 1 {
			// Call 1: Emergency pre-turn compaction call before call 1 of generation!
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- Emergency cold start summary of prior history."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":40,"completion_tokens":10,"total_tokens":50}
			}`
			io.WriteString(w, respJSON)
		} else {
			// Call 2: Generation turn 1 response
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"Ready on cold start after emergency compaction."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100, // threshold is 80
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

	if !strings.Contains(resp, "Ready on cold start after emergency compaction.") {
		t.Errorf("expected response from generation, got: %q", resp)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()
	if count != 2 {
		t.Errorf("expected callCount to be 2 (compaction before call 1, then generation), got: %d", count)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("ReadMemoryFile failed: %v", err)
	}
	if !strings.Contains(mem, "Emergency cold start summary of prior history.") {
		t.Errorf("expected MEMORY.md to contain cold start summary, got: %q", mem)
	}

	finalTurns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(finalTurns) >= len(seedTurns) {
		t.Errorf("expected session turns to be compacted (less than %d), got %d", len(seedTurns), len(finalTurns))
	}
}

// TestD88_MaxAutoContinuationBudgetCap verifies that runaway loops hit the budget guard
// (MaxAutoContinuations = 4 standard, 2 A2A) and emit an explicit incomplete status response.
func TestD88_MaxAutoContinuationBudgetCap(t *testing.T) {
	t.Run("Standard_Cap4", func(t *testing.T) {
		wsDir := t.TempDir()
		agentID := "d88-cap2-bot"
		agentDir := filepath.Join(wsDir, agentID)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("failed creating agent dir: %v", err)
		}

		toolsDir := filepath.Join(agentDir, "tools")
		if err := os.MkdirAll(toolsDir, 0755); err != nil {
			t.Fatalf("failed creating tools dir: %v", err)
		}
		toolScript := "#!/bin/sh\necho 'done'\n"
		if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
			t.Fatalf("failed creating tool script: %v", err)
		}

		if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
			t.Fatalf("failed to write AGENTS.md: %v", err)
		}

		// Seed session with multiple turns so compactions succeed with reduction
		seedTurns := []*genai.Content{
			genai.NewContentFromText("u1", "user"),
			genai.NewContentFromText("m1", "model"),
			genai.NewContentFromText("u2", "user"),
			genai.NewContentFromText("m2", "model"),
			genai.NewContentFromText("u3", "user"),
			genai.NewContentFromText("m3", "model"),
			genai.NewContentFromText("u4", "user"),
			genai.NewContentFromText("m4", "model"),
			genai.NewContentFromText("u5", "user"),
		}
		if err := WriteSessionTurns(agentDir, seedTurns); err != nil {
			t.Fatalf("WriteSessionTurns failed: %v", err)
		}

		var mu sync.Mutex
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			callCount++
			c := callCount
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if c%2 == 0 {
				// Compaction calls
				respJSON := `{
					"choices":[{"message":{"role":"assistant","content":"- Compacted memory step."},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
				}`
				io.WriteString(w, respJSON)
			} else {
				// Tool call that trips compaction threshold (prompt_tokens: 90 >= 80)
				toolCallJSON := fmt.Sprintf(`{
					"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
					"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
				}`, c)
				io.WriteString(w, toolCallJSON)
			}
		}))
		defer srv.Close()

		runtimeCfg := &RuntimeConfig{
			Provider:      "openai",
			Model:         "test-model",
			Endpoint:      srv.URL,
			ContextWindow: 100, // threshold = 80
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

		for _, want := range []string{"4 of 4", "incomplete status", "maxAutoContinuations", "runtime.json"} {
			if !strings.Contains(resp, want) {
				t.Errorf("expected response to contain %q, got: %q", want, resp)
			}
		}
	})

	t.Run("A2A_Cap2", func(t *testing.T) {
		wsDir := t.TempDir()
		agentID := "d88-cap1-a2a-bot"
		agentDir := filepath.Join(wsDir, agentID)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("failed creating agent dir: %v", err)
		}

		toolsDir := filepath.Join(agentDir, "tools")
		if err := os.MkdirAll(toolsDir, 0755); err != nil {
			t.Fatalf("failed creating tools dir: %v", err)
		}
		toolScript := "#!/bin/sh\necho 'done'\n"
		if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
			t.Fatalf("failed creating tool script: %v", err)
		}

		if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
			t.Fatalf("failed to write AGENTS.md: %v", err)
		}

		seedTurns := []*genai.Content{
			genai.NewContentFromText("u1", "user"),
			genai.NewContentFromText("m1", "model"),
			genai.NewContentFromText("u2", "user"),
			genai.NewContentFromText("m2", "model"),
			genai.NewContentFromText("u3", "user"),
		}
		if err := WriteSessionTurns(agentDir, seedTurns); err != nil {
			t.Fatalf("WriteSessionTurns failed: %v", err)
		}

		var mu sync.Mutex
		callCount := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			callCount++
			c := callCount
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if c%2 == 0 {
				// Compaction call
				respJSON := `{
					"choices":[{"message":{"role":"assistant","content":"- Compacted memory step."},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
				}`
				io.WriteString(w, respJSON)
			} else {
				toolCallJSON := fmt.Sprintf(`{
					"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
					"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
				}`, c)
				io.WriteString(w, toolCallJSON)
			}
		}))
		defer srv.Close()

		runtimeCfg := &RuntimeConfig{
			Provider:      "openai",
			Model:         "test-model",
			Endpoint:      srv.URL,
			ContextWindow: 100, // threshold = 80
		}
		runtimeData, _ := json.Marshal(runtimeCfg)
		if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
			t.Fatalf("failed to write runtime.json: %v", err)
		}

		a2aMeta := &A2AMetadata{
			CallerID: "peer-bot",
		}
		fa, err := LoadFolderAgentWithHookEnv(wsDir, agentID, a2aMeta, nil, DefaultMaxToolTurns)
		if err != nil {
			t.Fatalf("LoadFolderAgentWithHookEnv failed: %v", err)
		}

		resp, err := fa.GenerateTurn(context.Background())
		if err != nil {
			t.Fatalf("GenerateTurn failed: %v", err)
		}

		for _, want := range []string{"2 of 2", "incomplete status", "maxAutoContinuations", "runtime.json"} {
			if !strings.Contains(resp, want) {
				t.Errorf("expected response to contain %q, got: %q", want, resp)
			}
		}
	})
}

// TestD88_ContextCancellationStopsContinuation verifies that sdk.CancelTurn
// cleanly stops an in-flight continuation turn.
func TestD88_ContextCancellationStopsContinuation(t *testing.T) {
	wsDir := t.TempDir()
	t.Setenv("WACKYPUB_ALLOWED_AGENTS", "*")
	agentID := "d88-cancel-bot"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	toolScript := "#!/bin/sh\necho 'done'\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents file: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	continuationStarted := make(chan struct{}, 1)
	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 1 {
			// Call 1: Mid-turn bail tool call
			toolCallJSON := `{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, toolCallJSON)
		} else if c == 2 {
			// Call 2: Compaction summarizer LLM call
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"- Compaction summary."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
			}`
			io.WriteString(w, respJSON)
		} else {
			// Call 3: Continuation turn model call!
			_, _ = io.ReadAll(r.Body)
			select {
			case continuationStarted <- struct{}{}:
			default:
			}
			// Block until canceled by client context
			<-r.Context().Done()
		}
	}))
	defer srv.CloseClientConnections()
	defer srv.Close()

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100, // threshold = 80
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	sdk := NewSDK(wsDir)
	if _, err := sdk.AddUserTurn(context.Background(), &agentv1.AddUserTurnRequest{
		AgentId: agentID,
		Message: "Start cancelable task",
	}); err != nil {
		t.Fatalf("AddUserTurn failed: %v", err)
	}

	streamDone := make(chan error, 1)
	go func() {
		var streamErr error
		for _, err := range sdk.generateTurnStreamImpl(context.Background(), agentID) {
			if err != nil {
				streamErr = err
				break
			}
		}
		streamDone <- streamErr
	}()

	select {
	case <-continuationStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for continuation turn to start")
	}

	// Cancel the turn during continuation turn execution
	if _, err := sdk.CancelTurn(context.Background(), &agentv1.CancelTurnRequest{AgentId: agentID}); err != nil {
		t.Fatalf("CancelTurn failed: %v", err)
	}

	select {
	case err := <-streamDone:
		if err == nil {
			t.Fatal("expected non-nil error when continuation turn is canceled, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("expected context.Canceled error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled continuation stream to terminate")
	}
}

func captureStderr(f func()) string {
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		f()
		return ""
	}
	os.Stderr = w

	outChan := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outChan <- buf.String()
	}()

	f()

	w.Close()
	os.Stderr = oldStderr
	out := <-outChan
	r.Close()
	return out
}

func TestD101_ErrorTransparency_CompactionError(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d101-compaction-err"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	toolScript := "#!/bin/sh\necho 'done'\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		if c == 1 {
			// Call 1: Tool call triggering mid-turn context bail
			w.Header().Set("Content-Type", "application/json")
			toolCallJSON := `{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, toolCallJSON)
		} else {
			// Call 2: Compaction summarizer fails with HTTP 500
			http.Error(w, "internal compaction failure", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "Start task"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100, // 20% overhead -> threshold is 80. Real prompt_tokens is 90 >= 80
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	var resp string
	stderrOut := captureStderr(func() {
		resp, err = fa.GenerateTurn(context.Background())
	})
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}

	if !strings.Contains(stderrOut, "Warning: auto-continuation compaction error:") {
		t.Errorf("expected stderr to contain 'Warning: auto-continuation compaction error:', got: %q", stderrOut)
	}
	if !strings.Contains(resp, "[Auto-continuation aborted: session compaction error:") {
		t.Errorf("expected response to contain '[Auto-continuation aborted: session compaction error:', got: %q", resp)
	}
}

func TestD101_ErrorTransparency_InsufficientReduction(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d101-no-reduction"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	toolScript := "#!/bin/sh\necho 'done'\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "test_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		c := callCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if c == 1 {
			// Call 1: Tool call triggering mid-turn context bail on next check
			toolCallJSON := `{
				"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"test_tool.sh","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
			}`
			io.WriteString(w, toolCallJSON)
		} else {
			// Call 2: Compaction summarizer call
			respJSON := `{
				"choices":[{"message":{"role":"assistant","content":"Compacted summary."},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
			}`
			io.WriteString(w, respJSON)
		}
	}))
	defer srv.Close()

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	// COMPACT.md with compact-pct: 1 compacts only the initial model turn
	compactConfig := "---\ncompact-pct: 1\ncompaction-notice: \"Session compacted.\"\n---\nSummarize prior turns.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "COMPACT.md"), []byte(compactConfig), 0644); err != nil {
		t.Fatalf("failed to write COMPACT.md: %v", err)
	}

	// Starts with model turn, then user turn:
	// Turn 0: model "a"
	// Turn 1: user "Start task"
	turns := []*genai.Content{
		genai.NewContentFromText("a", "model"),
		genai.NewContentFromText("Start task", "user"),
	}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("WriteSessionTurns failed: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100,
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	var resp string
	stderrOut := captureStderr(func() {
		resp, err = fa.GenerateTurn(context.Background())
	})
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}

	// This scenario does shrink the session, just nowhere near enough to fit the
	// budget. Under one shared ceiling that is reported as insufficiency instead of
	// the generic no-reduction text, which reserved that wording for when nothing
	// came off at all.
	if !strings.Contains(stderrOut, "auto-continuation compaction left the session at") {
		t.Errorf("expected the insufficient-compaction warning, got: %q", stderrOut)
	}
	if !strings.Contains(resp, "compaction insufficient - session still at") {
		t.Errorf("expected the insufficient status, got: %q", resp)
	}
	if strings.Contains(resp, "produced no reduction") {
		t.Errorf("this scenario does reduce, so no-reduction is the wrong diagnosis: %q", resp)
	}
}

func TestD101_ErrorTransparency_ReadSessionError(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d101-read-err"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	// Tool script writes a 17MB line to session.jsonl to trigger scanner buffer overflow on post-turn read
	toolScript := fmt.Sprintf("#!/bin/sh\npython3 -c 'print(\"x\" * 17000000)' >> %s/session.jsonl\necho 'done'\n", agentDir)
	if err := os.WriteFile(filepath.Join(toolsDir, "corrupt_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		toolCallJSON := `{
			"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"corrupt_tool.sh\",\"args\":[]}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
		}`
		io.WriteString(w, toolCallJSON)
	}))
	defer srv.Close()

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "Start task"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100,
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	// A line over the scanner cap cannot be safely inferred, so the post-turn append
	// fails CLOSED and surfaces the scanner error at allocation time, instead of the
	// old fail-open (allocate a duplicate seq; surface the corruption only later, at
	// the continuation read). The corruption is transparent either way, per D101.
	_, err = fa.GenerateTurn(context.Background())
	if err == nil {
		t.Fatalf("GenerateTurn should fail when session.jsonl holds an over-cap line")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("expected token-too-long in error, got: %v", err)
	}
}

// TestD88_DefaultAutoContinuationCaps pins the D88 budget guard defaults. These are
// runaway-loop guards, not feature limits: image-queue and compaction continuations
// consume from the same per-turn budget, so the cap must leave room for a
// multi-image turn plus its follow-up work.
func TestD88_DefaultAutoContinuationCaps(t *testing.T) {
	if DefaultMaxAutoContinuations != 4 {
		t.Errorf("DefaultMaxAutoContinuations = %d, want 4", DefaultMaxAutoContinuations)
	}
	if DefaultMaxAutoContinuationsA2A != 2 {
		t.Errorf("DefaultMaxAutoContinuationsA2A = %d, want 2", DefaultMaxAutoContinuationsA2A)
	}
}

// TestD88_BudgetExhaustedStopIsLoud verifies the cap-exhausted stop names the count,
// the cap, and the runtime.json knob, so an operator can raise the budget on purpose
// instead of wondering why a multi-image turn stalled.
// TestD88_BudgetExhaustedStopIsLoud unit-drives the D88 budget gate at exhaustion for
// both budget classes: the stop must be emitted (never silent) and must name the
// exhausted budget, the count and cap, and its runtime.json knob.
func TestD88_BudgetExhaustedStopIsLoud(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bail    bool
		images  []string
		maxGen  int
		maxImg  int
		wantCap string
		knob    string
	}{
		{"general budget standard", true, nil, 4, 300, "4 of 4", "maxAutoContinuations"},
		{"general budget a2a", true, nil, 2, 300, "2 of 2", "maxAutoContinuations"},
		{"image budget", false, []string{"img1"}, 4, 4, "4 of 4", "maxAutoContinuationsImages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := &FolderAgent{RuntimeConfig: &RuntimeConfig{MaxImageDimension: 1024}}
			if tc.bail {
				fa.UsageTracker = &TurnUsageTracker{StoppedEarlyForCompaction: true}
			}
			genCount := tc.maxGen
			imgCount := tc.maxImg
			reason := ContinuationNone
			var out []string
			yield := func(chunk string, err error) bool {
				if err != nil {
					t.Fatalf("unexpected yield error: %v", err)
				}
				out = append(out, chunk)
				return true
			}

			ok := fa.handleContinuationOrCompaction(context.Background(), t.TempDir(), tc.images, &reason, &genCount, &imgCount, tc.maxGen, tc.maxImg, yield)
			if ok {
				t.Fatal("expected the budget gate to stop the continuation")
			}
			msg := strings.Join(out, "")
			for _, want := range []string{tc.wantCap, "incomplete status", tc.knob, "runtime.json"} {
				if !strings.Contains(msg, want) {
					t.Errorf("budget-exhausted stop missing %q, got %s", want, msg)
				}
			}
			if tc.bail {
				if strings.Contains(msg, "(images)") {
					t.Errorf("general budget stop wrongly labels (images), got %s", msg)
				}
			} else if !strings.Contains(msg, "(images)") {
				t.Errorf("image budget stop missing (images) label, got %s", msg)
			}
		})
	}
}

// TestD88_AutoContinuationBudgets pins the D88 budget resolution rules: general
// defaults (4 standard / 2 A2A), the image budget defaulting to the agent maxToolTurns
// (so every action a turn can take can queue an image), explicit runtime.json knobs,
// and DisableAutoContinuation zeroing both.
func TestD88_AutoContinuationBudgets(t *testing.T) {
	pi := func(v int) *int { return &v }
	for _, tc := range []struct {
		name string
		fa   *FolderAgent
		gen  int
		img  int
	}{
		{"standard defaults", &FolderAgent{MaxToolTurns: 300}, 4, 300},
		{"a2a general default", &FolderAgent{A2AMeta: &A2AMetadata{}, MaxToolTurns: 300}, 2, 300},
		{"general knob", &FolderAgent{MaxAutoContinuations: pi(7), MaxToolTurns: 300}, 7, 300},
		{"image knob", &FolderAgent{MaxAutoContinuationsImages: pi(105), MaxToolTurns: 300}, 4, 105},
		{"image default ties to maxToolTurns", &FolderAgent{MaxToolTurns: 120}, 4, 120},
		{"disable zeroes both", &FolderAgent{DisableAutoContinuation: true, MaxAutoContinuations: pi(7), MaxAutoContinuationsImages: pi(105), MaxToolTurns: 300}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, i := tc.fa.autoContinuationBudgets()
			if g != tc.gen || i != tc.img {
				t.Errorf("autoContinuationBudgets = (%d, %d), want (%d, %d)", g, i, tc.gen, tc.img)
			}
		})
	}
}

// newImageBudgetAgent builds a FolderAgent whose deferred-image path works against a
// temp workspace, plus the png bytes shared by all test images.
func newImageBudgetAgent(t *testing.T) (*FolderAgent, string, []byte) {
	t.Helper()
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "img-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	fa := &FolderAgent{AgentDir: agentDir, AgentID: "img-agent", RuntimeConfig: &RuntimeConfig{MaxImageDimension: 400}, MaxToolTurns: 300}
	pngBytes := createTestImage(100, 100, false)
	return fa, wsDir, pngBytes
}

func makeImageIDs(t *testing.T, fa *FolderAgent, pngBytes []byte, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		entry, err := CreateBinaryScratchpad(fa.AgentDir, pngBytes, "test", "image/png")
		if err != nil {
			t.Fatalf("create scratchpad image %d: %v", i, err)
		}
		ids[i] = entry.ID
	}
	return ids
}

// TestD88_ImageBudgetSeparateFromGeneral proves the two D88 budgets are independent:
// 110 image-driven continuations burn only the image budget (the general counter stays
// at 0), and the general budget still exhausts at its own cap regardless of how many
// image continuations preceded it.
func TestD88_ImageBudgetSeparateFromGeneral(t *testing.T) {
	fa, wsDir, pngBytes := newImageBudgetAgent(t)
	genCap, imgCap := fa.autoContinuationBudgets()
	if genCap != 4 || imgCap != 300 {
		t.Fatalf("unexpected default budgets: general=%d image=%d", genCap, imgCap)
	}
	const rounds = 110
	ids := makeImageIDs(t, fa, pngBytes, rounds)
	reason := ContinuationNone
	gen, img := 0, 0
	for i := range ids {
		ok := fa.handleContinuationOrCompaction(context.Background(), wsDir, []string{ids[i]}, &reason, &gen, &img, genCap, imgCap, yieldNoop)
		if !ok {
			t.Fatalf("round %d: expected image continuation to fire under the image budget", i)
		}
		if gen != 0 {
			t.Fatalf("round %d: image continuation burned the general budget (gen=%d)", i, gen)
		}
	}
	if img != rounds {
		t.Fatalf("image counter = %d, want %d", img, rounds)
	}
	// General exhaustion point is unchanged by the image volume: general budget at its
	// cap, 110 image continuations already on the clock, fresh reason (not an image
	// re-inflation turn). A bail-only turn must be denied with the general message.
	fa.UsageTracker = &TurnUsageTracker{StoppedEarlyForCompaction: true}
	reason2 := ContinuationNone
	gen = genCap
	var out []string
	yieldMsg := func(chunk string, err error) bool {
		out = append(out, chunk)
		return true
	}
	ok := fa.handleContinuationOrCompaction(context.Background(), wsDir, nil, &reason2, &gen, &img, genCap, imgCap, yieldMsg)
	if ok {
		t.Fatal("expected general budget to exhaust at its cap despite image volume")
	}
	msg := strings.Join(out, "")
	for _, want := range []string{"4 of 4", "incomplete status", "maxAutoContinuations"} {
		if !strings.Contains(msg, want) {
			t.Errorf("general budget stop missing %q, got %s", want, msg)
		}
	}
	if strings.Contains(msg, "(images)") {
		t.Errorf("general budget stop wrongly labels (images), got %s", msg)
	}
}

// TestD88_ImageBudgetCapWithManyImageLoads is the acceptance for the separate image
// budget: 100+ image-driven continuations in one session all fire under the image
// budget (knob set to 105), and the cap denial is loud with the image knob named.
func TestD88_ImageBudgetCapWithManyImageLoads(t *testing.T) {
	fa, wsDir, pngBytes := newImageBudgetAgent(t)
	fa.MaxAutoContinuationsImages = pi105()
	genCap, imgCap := fa.autoContinuationBudgets()
	if genCap != 4 || imgCap != 105 {
		t.Fatalf("unexpected budgets with knob: general=%d image=%d", genCap, imgCap)
	}
	const rounds = 106
	ids := makeImageIDs(t, fa, pngBytes, rounds)
	reason := ContinuationNone
	gen, img := 0, 0
	for i := 0; i < 105; i++ {
		ok := fa.handleContinuationOrCompaction(context.Background(), wsDir, []string{ids[i]}, &reason, &gen, &img, genCap, imgCap, yieldNoop)
		if !ok {
			t.Fatalf("round %d: expected image continuation to fire under the image budget", i)
		}
	}
	if gen != 0 {
		t.Fatalf("image continuations burned the general budget (gen=%d)", gen)
	}
	var out []string
	yieldMsg := func(chunk string, err error) bool {
		out = append(out, chunk)
		return true
	}
	ok := fa.handleContinuationOrCompaction(context.Background(), wsDir, []string{ids[105]}, &reason, &gen, &img, genCap, imgCap, yieldMsg)
	if ok {
		t.Fatal("expected the image budget to exhaust at its cap")
	}
	msg := strings.Join(out, "")
	for _, want := range []string{"105 of 105", "(images)", "incomplete status", "maxAutoContinuationsImages"} {
		if !strings.Contains(msg, want) {
			t.Errorf("image budget stop missing %q, got %s", want, msg)
		}
	}
}

func pi105() *int {
	v := 105
	return &v
}

// TestD88_ImageBudgetDefaultsToMaxToolTurns proves the default image budget is the
// agent maxToolTurns itself (not the 300 constant): with MaxToolTurns 3 and no knob,
// exactly 3 image continuations fire and the 4th is the loud image-budget stop.
func TestD88_ImageBudgetDefaultsToMaxToolTurns(t *testing.T) {
	fa, wsDir, pngBytes := newImageBudgetAgent(t)
	fa.MaxToolTurns = 3
	genCap, imgCap := fa.autoContinuationBudgets()
	if genCap != 4 || imgCap != 3 {
		t.Fatalf("unexpected budgets tied to maxToolTurns: general=%d image=%d", genCap, imgCap)
	}
	ids := makeImageIDs(t, fa, pngBytes, 4)
	reason := ContinuationNone
	gen, img := 0, 0
	for i := 0; i < 3; i++ {
		ok := fa.handleContinuationOrCompaction(context.Background(), wsDir, []string{ids[i]}, &reason, &gen, &img, genCap, imgCap, yieldNoop)
		if !ok {
			t.Fatalf("round %d: expected image continuation to fire under the default budget", i)
		}
	}
	var out []string
	yieldMsg := func(chunk string, err error) bool {
		out = append(out, chunk)
		return true
	}
	ok := fa.handleContinuationOrCompaction(context.Background(), wsDir, []string{ids[3]}, &reason, &gen, &img, genCap, imgCap, yieldMsg)
	if ok {
		t.Fatal("expected the default image budget to exhaust at maxToolTurns")
	}
	msg := strings.Join(out, "")
	for _, want := range []string{"3 of 3", "(images)", "incomplete status", "maxAutoContinuationsImages"} {
		if !strings.Contains(msg, want) {
			t.Errorf("image budget stop missing %q, got %s", want, msg)
		}
	}
}
func yieldNoop(chunk string, err error) bool {
	return true
}
