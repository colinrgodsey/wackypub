package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

func TestAddMedia_GatingAndExecution(t *testing.T) {
	var err error
	wsDir := t.TempDir()
	t.Setenv("WACKYPUB_ALLOWED_AGENTS", "*")
	sdk := NewSDK(wsDir)

	agentID := "media_agent"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	// 1. Without maxImageDimension (or maxImageDimension <= 0) in runtime.json
	runtimeJSONDisabled := `{"model":"test-model","endpoint":"http://localhost:1234/v1"}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSONDisabled), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("media_agent\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents file: %v", err)
	}

	testImgData := createTestImage(800, 600, false)
	_, err = sdk.AddMedia(agentID, bytes.NewReader(testImgData))
	if err == nil {
		t.Fatal("expected error when maxImageDimension is absent/disabled, got nil")
	}

	// 2. With maxImageDimension = 400
	runtimeJSONEnabled := `{"model":"test-model","endpoint":"http://localhost:1234/v1","maxImageDimension":400}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSONEnabled), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	var content *genai.Content
	content, err = sdk.AddMedia(agentID, bytes.NewReader(testImgData))
	if err != nil {
		t.Fatalf("AddMedia failed: %v", err)
	}

	if content.Role != "user" || len(content.Parts) != 1 || content.Parts[0].InlineData == nil {
		t.Fatalf("unexpected Content structure: %+v", content)
	}

	blob := content.Parts[0].InlineData
	if blob.MIMEType != "image/jpeg" {
		t.Errorf("expected MIMEType image/jpeg, got %s", blob.MIMEType)
	}

	turns, err := sdk.ReadSession(agentID)
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn in session.jsonl, got %d", len(turns))
	}

	// Verify EstimateTokens includes image token count
	tokens := EstimateTokens(turns, false)
	if tokens <= 0 {
		t.Errorf("expected non-zero token estimate for image turn, got %d", tokens)
	}
}

func TestBinaryScratchpad_DetectionAndRejection(t *testing.T) {
	agentDir := t.TempDir()

	binaryData := []byte{0x00, 0x01, 0x02, 0x03, 'P', 'N', 'G', 0x0d, 0x0a}
	entry, err := CreateBinaryScratchpad(agentDir, binaryData, "test", "image/png")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	if !entry.IsBinary {
		t.Errorf("expected entry.IsBinary true, got false")
	}

	items, count, _, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("ListScratchpads failed: %v", err)
	}
	if count != 1 || !items[0].IsBinary {
		t.Errorf("expected 1 binary item in ListScratchpads, got %v", items)
	}

	// GetScratchpad should reject .dat entry outright
	_, err = GetScratchpad(agentDir, entry.ID, nil, nil)
	if err == nil {
		t.Fatal("expected GetScratchpad to reject binary entry, got nil error")
	}

	// SearchScratchpad should reject .dat entry outright
	_, err = SearchScratchpad(agentDir, entry.ID, "PNG", nil, false, 10)
	if err == nil {
		t.Fatal("expected SearchScratchpad to reject binary entry, got nil error")
	}

	// DeleteScratchpad should succeed
	err = DeleteScratchpad(agentDir, entry.ID)
	if err != nil {
		t.Fatalf("DeleteScratchpad failed: %v", err)
	}

	itemsAfter, countAfter, _, _ := ListScratchpads(agentDir)
	if countAfter != 0 {
		t.Errorf("expected 0 items after deletion, got %d (%v)", countAfter, itemsAfter)
	}
}

func TestExecuteTool_BinaryScratchpadPipingAndRestrictions(t *testing.T) {
	agentDir := t.TempDir()
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}

	// Script cat_stdin.sh: copies stdin to stdout
	catScript := "#!/bin/sh\ncat"
	if err := os.WriteFile(filepath.Join(toolsDir, "cat_stdin.sh"), []byte(catScript), 0755); err != nil {
		t.Fatalf("failed to write cat_stdin.sh: %v", err)
	}

	realPNGPayload := createTestImage(20, 20, true)
	// Prepend a control byte to force binary classification if PNG encoder produced text-safe byte prefix
	if !IsBinaryContent(realPNGPayload) {
		realPNGPayload = append([]byte{0x00}, realPNGPayload...)
	}

	spEntry, err := CreateBinaryScratchpad(agentDir, realPNGPayload, "test", "image/png")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	ctx := context.Background()

	// 1. Reject binary entry in command args
	badArgs := ExecToolArgs{
		Args: []string{"<SCRATCHPAD_DATA id=\"" + spEntry.ID + "\" />"},
	}
	_, _, err = executeTool(ctx, agentDir, "cat_stdin.sh", filepath.Join(toolsDir, "cat_stdin.sh"), badArgs, nil)
	if err == nil {
		t.Fatal("expected error passing binary scratchpad in args, got nil")
	}

	// 2. Reject binary entry mixed with text in stdin
	mixedStdin := ExecToolArgs{
		Stdin: "prefix <SCRATCHPAD_DATA id=\"" + spEntry.ID + "\" />",
	}
	_, _, err = executeTool(ctx, agentDir, "cat_stdin.sh", filepath.Join(toolsDir, "cat_stdin.sh"), mixedStdin, nil)
	if err == nil {
		t.Fatal("expected error mixing binary scratchpad with text in stdin, got nil")
	}

	// 3. Exact binary stdin piping -> streams file directly and output gets auto-captured to new .dat scratchpad
	exactStdin := ExecToolArgs{
		Stdin: "<SCRATCHPAD_DATA id=\"" + spEntry.ID + "\" />",
	}
	out, _, err := executeTool(ctx, agentDir, "cat_stdin.sh", filepath.Join(toolsDir, "cat_stdin.sh"), exactStdin, nil)
	if err != nil {
		t.Fatalf("exact binary stdin piping failed: %v", err)
	}

	if !strings.Contains(out, "mime=\"image/png\"") && !strings.Contains(out, "mime=\"application/octet-stream\"") {
		t.Errorf("expected stdout to auto-capture to binary scratchpad, got: %s", out)
	}

	// 4. Reject pagination attributes (skip_lines, num_lines, json_escape) on binary stdin references per D48
	skipStdin := ExecToolArgs{
		Stdin: "<SCRATCHPAD_DATA id=\"" + spEntry.ID + "\" skip_lines=\"2\" />",
	}
	_, _, err = executeTool(ctx, agentDir, "cat_stdin.sh", filepath.Join(toolsDir, "cat_stdin.sh"), skipStdin, nil)
	if err == nil {
		t.Fatal("expected error using skip_lines on binary scratchpad in stdin, got nil")
	}

	numStdin := ExecToolArgs{
		Stdin: "<SCRATCHPAD_DATA id=\"" + spEntry.ID + "\" num_lines=\"5\" />",
	}
	_, _, err = executeTool(ctx, agentDir, "cat_stdin.sh", filepath.Join(toolsDir, "cat_stdin.sh"), numStdin, nil)
	if err == nil {
		t.Fatal("expected error using num_lines on binary scratchpad in stdin, got nil")
	}

	jsonStdin := ExecToolArgs{
		Stdin: "<SCRATCHPAD_DATA id=\"" + spEntry.ID + "\" json_escape=\"true\" />",
	}
	_, _, err = executeTool(ctx, agentDir, "cat_stdin.sh", filepath.Join(toolsDir, "cat_stdin.sh"), jsonStdin, nil)
	if err == nil {
		t.Fatal("expected error using json_escape on binary scratchpad in stdin, got nil")
	}
}

func TestScratchpad_UnifiedIDNamespace(t *testing.T) {
	agentDir := t.TempDir()
	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		t.Fatalf("failed to create scratchpad dir: %v", err)
	}

	// Create 10 text and 10 binary entries, verify all 20 have unique IDs
	ids := make(map[string]bool)
	for i := 0; i < 10; i++ {
		txtEntry, err := CreateScratchpad(agentDir, "text payload", "test")
		if err != nil {
			t.Fatalf("CreateScratchpad failed: %v", err)
		}
		if ids[txtEntry.ID] {
			t.Fatalf("duplicate ID generated: %q", txtEntry.ID)
		}
		ids[txtEntry.ID] = true

		binEntry, err := CreateBinaryScratchpad(agentDir, []byte{0x00, byte(i)}, "test", "application/octet-stream")
		if err != nil {
			t.Fatalf("CreateBinaryScratchpad failed: %v", err)
		}
		if ids[binEntry.ID] {
			t.Fatalf("duplicate ID generated across text/binary namespace: %q", binEntry.ID)
		}
		ids[binEntry.ID] = true
	}

	// Simulate collision recovery: if a .txt and .dat happen to share ID "abcd", DeleteScratchpad cleans all
	const sharedID = "abcd"
	if err := os.WriteFile(filepath.Join(spDir, sharedID+"-1-agentA.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write .txt collision file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spDir, sharedID+"-0-agentB.dat"), []byte{0x00, 0x01}, 0644); err != nil {
		t.Fatalf("failed to write .dat collision file: %v", err)
	}

	if err := DeleteScratchpad(agentDir, sharedID); err != nil {
		t.Fatalf("DeleteScratchpad failed: %v", err)
	}

	// Verify both were removed and neither was orphaned
	items, _, _, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("ListScratchpads failed: %v", err)
	}
	for _, it := range items {
		if it.ID == sharedID {
			t.Fatalf("found orphaned entry with ID %q after DeleteScratchpad", sharedID)
		}
	}
}

type getScratchpadTestModel struct {
	id string
}

func (m *getScratchpadTestModel) Name() string { return "get-scratchpad-test-model" }

func (m *getScratchpadTestModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		res := &model.LLMResponse{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "get_scratchpad",
							Args: map[string]any{
								"id": m.id,
							},
						},
					},
				},
			},
		}
		yield(res, nil)
	}
}

func TestD49_DeferredImageScratchpad(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"test-model","endpoint":"http://localhost:1234/v1","maxImageDimension":400}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// 1. Create a binary PNG scratchpad
	pngData := createTestImage(100, 100, false)
	imgEntry, err := CreateBinaryScratchpad(agentDir, pngData, "test", "image/png")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	// 2. Set up FolderAgent with getScratchpadTestModel
	fa, err := LoadFolderAgent(wsDir, "bob", 5)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	fa.Model = &getScratchpadTestModel{id: imgEntry.ID}

	toolMap, _, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}

	toolsList := []tool.Tool{toolMap["get_scratchpad"]}
	fa.ADKAgent, err = BuildADKAgentWithConfig("bob", fa.SystemPrompt, 5, fa.RuntimeConfig, fa.Model, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfig failed: %v", err)
	}

	// Add initial user turn to session.jsonl
	uMsg := genai.NewContentFromText("please inspect the image in scratchpad", "user")
	if err := AppendSessionContent(agentDir, uMsg); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	// 3. GenerateTurn executes tool loop, defers image, and short-circuits
	respText, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}

	if !strings.Contains(respText, "queued") || !strings.Contains(respText, imgEntry.ID) {
		t.Errorf("expected response to mention queued image ID %q, got: %s", imgEntry.ID, respText)
	}

	// 4. Verify session.jsonl contains the deferred image turn at the very end
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	if len(turns) < 3 {
		t.Fatalf("expected at least 3 turns in session.jsonl, got %d", len(turns))
	}

	lastTurn := turns[len(turns)-1]
	if lastTurn.Role != "user" {
		t.Errorf("expected last turn in session.jsonl to be role 'user', got %q", lastTurn.Role)
	}

	if len(lastTurn.Parts) != 2 {
		t.Fatalf("expected last turn to have 2 parts (label text + image), got %d", len(lastTurn.Parts))
	}

	expectedLabel := fmt.Sprintf("<IMAGE>The following image is stored in scratchpad '%s'</IMAGE>", imgEntry.ID)
	if lastTurn.Parts[0].Text != expectedLabel {
		t.Errorf("expected part[0].Text %q, got %q", expectedLabel, lastTurn.Parts[0].Text)
	}

	if lastTurn.Parts[1].InlineData == nil || lastTurn.Parts[1].InlineData.MIMEType != "image/jpeg" {
		t.Errorf("expected part[1].InlineData with image/jpeg, got %+v", lastTurn.Parts[1].InlineData)
	}

	// 5. Test hasDeferredScratchpadResponse helper directly
	hasDef, ids := hasDeferredScratchpadResponse([]*genai.Content{
		{
			Role: "tool",
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "get_scratchpad",
						Response: map[string]any{
							"deferred":      true,
							"scratchpad_id": "xyz9",
						},
					},
				},
			},
		},
	})
	if !hasDef || len(ids) != 1 || ids[0] != "xyz9" {
		t.Errorf("hasDeferredScratchpadResponse failed: got hasDef=%v, ids=%v", hasDef, ids)
	}

	// 6. Test non-image binary rejection in get_scratchpad
	binData := []byte{0x00, 0x01, 0x02, 0x03, 0x04}
	binEntry, err := CreateBinaryScratchpad(agentDir, binData, "test", "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	fa.Model = &getScratchpadTestModel{id: binEntry.ID}
	fa.ADKAgent, err = BuildADKAgentWithConfig("bob", fa.SystemPrompt, 1, fa.RuntimeConfig, fa.Model, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfig failed: %v", err)
	}

	// Add a user turn so generate precondition holds
	_ = AppendSessionContent(agentDir, genai.NewContentFromText("check binary", "user"))
	_, _ = fa.GenerateTurn(context.Background())

	turnsAfter, _ := ReadSessionTurns(agentDir)
	// Check that tool response contains error message
	foundError := false
	for _, t := range turnsAfter {
		for _, p := range t.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "get_scratchpad" {
				respJSON, _ := json.Marshal(p.FunctionResponse.Response)
				if strings.Contains(string(respJSON), "cannot be read as text") {
					foundError = true
				}
			}
		}
	}
	if !foundError {
		t.Errorf("expected tool response error for non-image binary scratchpad")
	}

	// 7. Test that when maxImageDimension is absent/disabled, get_scratchpad on an image rejects instead of deferring
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"test-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}
	faGated, err := LoadFolderAgent(wsDir, "bob", 1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	faGated.Model = &getScratchpadTestModel{id: imgEntry.ID}
	faGated.ADKAgent, err = BuildADKAgentWithConfig("bob", faGated.SystemPrompt, 1, faGated.RuntimeConfig, faGated.Model, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfig failed: %v", err)
	}

	_ = AppendSessionContent(agentDir, genai.NewContentFromText("check gated image", "user"))
	_, _ = faGated.GenerateTurn(context.Background())

	turnsGated, _ := ReadSessionTurns(agentDir)
	foundGatedRejection := false
	for _, t := range turnsGated {
		for _, p := range t.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "get_scratchpad" {
				respJSON, _ := json.Marshal(p.FunctionResponse.Response)
				if strings.Contains(string(respJSON), "cannot be read as text") {
					foundGatedRejection = true
				}
			}
		}
	}
	if !foundGatedRejection {
		t.Errorf("expected get_scratchpad on image to reject when maxImageDimension is disabled")
	}
}

func TestDeferredImage_FailureNoticeAppended(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "corrupted_bot")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("failed writing AGENTS.md: %v", err)
	}
	// maxImageDimension=400 enabled
	runtimeJSON := `{"model":"test-model","endpoint":"http://localhost:1234/v1","maxImageDimension":400}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed writing runtime.json: %v", err)
	}

	// Create binary scratchpad with valid PNG signature prefix but truncated/corrupted payload
	corruptedPngBytes := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00}
	corruptedEntry, err := CreateBinaryScratchpad(agentDir, corruptedPngBytes, "corrupted", "image/png")
	if err != nil {
		t.Fatalf("CreateBinaryScratchpad failed: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, "corrupted_bot", 10)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	toolsMap, _, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}
	var toolsList []tool.Tool
	for _, tl := range toolsMap {
		toolsList = append(toolsList, tl)
	}

	fa.Model = &getScratchpadTestModel{id: corruptedEntry.ID}
	fa.ADKAgent, err = BuildADKAgentWithConfig("corrupted_bot", fa.SystemPrompt, 1, fa.RuntimeConfig, fa.Model, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfig failed: %v", err)
	}

	// Add initial user turn
	_ = AppendSessionContent(agentDir, genai.NewContentFromText("please inspect corrupted image", "user"))

	respText, err := fa.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}
	if !strings.Contains(respText, "queued") {
		t.Errorf("expected confirmation of queued image, got: %s", respText)
	}

	// Check session turns: must end on a user turn containing <IMAGE_ERROR>
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	if len(turns) == 0 {
		t.Fatalf("expected session turns to be present")
	}

	lastTurn := turns[len(turns)-1]
	if lastTurn.Role != "user" {
		t.Errorf("expected last turn to be role user, got: %s", lastTurn.Role)
	}

	lastText := ContentText(lastTurn)
	if !strings.Contains(lastText, "<IMAGE_ERROR>") || !strings.Contains(lastText, corruptedEntry.ID) {
		t.Errorf("expected last turn to contain <IMAGE_ERROR> with scratchpad ID %q, got: %s", corruptedEntry.ID, lastText)
	}
}
