package agent

import (
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
// (MaxAutoContinuations = 2 standard, 1 A2A) and emit an explicit incomplete status response.
func TestD88_MaxAutoContinuationBudgetCap(t *testing.T) {
	t.Run("Standard_Cap2", func(t *testing.T) {
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
			if c == 2 || c == 4 {
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

		expectedCapMsg := "[Reached maximum auto-continuations (2) - stopping with incomplete status.]"
		if !strings.Contains(resp, expectedCapMsg) {
			t.Errorf("expected response to contain %q, got: %q", expectedCapMsg, resp)
		}
	})

	t.Run("A2A_Cap1", func(t *testing.T) {
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
			if c == 2 {
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

		expectedCapMsg := "[Reached maximum auto-continuations (1) - stopping with incomplete status.]"
		if !strings.Contains(resp, expectedCapMsg) {
			t.Errorf("expected response to contain %q, got: %q", expectedCapMsg, resp)
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
	if _, err := sdk.AddUserTurn(agentID, "Start cancelable task"); err != nil {
		t.Fatalf("AddUserTurn failed: %v", err)
	}

	streamDone := make(chan error, 1)
	go func() {
		var streamErr error
		for _, err := range sdk.GenerateTurnStream(context.Background(), agentID) {
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
	if err := sdk.CancelTurn(agentID); err != nil {
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
