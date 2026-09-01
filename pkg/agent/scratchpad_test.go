package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScratchpadCreationAndRetrieval(t *testing.T) {
	agentDir := t.TempDir()

	text := "Line 1: Hello\nLine 2: World\nLine 3: Go\nLine 4: ADK\nLine 5: Test"
	entry, err := CreateScratchpad(agentDir, text, "unit_test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	if len(entry.ID) != 4 {
		t.Errorf("expected 4-character ID, got %q (len %d)", entry.ID, len(entry.ID))
	}
	if entry.Size != len(text) {
		t.Errorf("expected Size %d, got %d", len(text), entry.Size)
	}
	if entry.CreatedBy != "unit_test" {
		t.Errorf("expected CreatedBy 'unit_test', got %q", entry.CreatedBy)
	}

	// Test full retrieval
	val, err := GetScratchpad(agentDir, entry.ID, nil, nil)
	if err != nil {
		t.Fatalf("GetScratchpad failed: %v", err)
	}
	if val != text {
		t.Errorf("expected %q, got %q", text, val)
	}

	// Test line pagination: skip 1 line, get 2 lines
	skip := 1
	num := 2
	paginated, err := GetScratchpad(agentDir, entry.ID, &skip, &num)
	if err != nil {
		t.Fatalf("GetScratchpad paginated failed: %v", err)
	}
	expected := "Line 2: World\nLine 3: Go"
	if paginated != expected {
		t.Errorf("expected paginated output %q, got %q", expected, paginated)
	}

	// Test missing ID error
	_, err = GetScratchpad(agentDir, "missing", nil, nil)
	if err == nil {
		t.Fatalf("expected error for missing ID, got nil")
	}
	if !strings.Contains(err.Error(), `scratchpad entry "missing" not found`) {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestScratchpadEvictionCap(t *testing.T) {
	agentDir := t.TempDir()

	var firstID string
	for i := 1; i <= MaxScratchpadEntries+1; i++ {
		entry, err := CreateScratchpad(agentDir, fmt.Sprintf("Entry %d", i), "test")
		if err != nil {
			t.Fatalf("CreateScratchpad failed at %d: %v", i, err)
		}
		if i == 1 {
			firstID = entry.ID
		}
		// Ensure mtime ticks forward for deterministic eviction order
		time.Sleep(1 * time.Millisecond)
	}

	items, count, capVal, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("ListScratchpads failed: %v", err)
	}

	if count != MaxScratchpadEntries {
		t.Errorf("expected %d live entries, got %d", MaxScratchpadEntries, count)
	}
	if capVal != MaxScratchpadEntries {
		t.Errorf("expected cap %d, got %d", MaxScratchpadEntries, capVal)
	}
	if len(items) != MaxScratchpadEntries {
		t.Errorf("expected len %d, got %d", MaxScratchpadEntries, len(items))
	}

	// First entry (oldest mtime) should have been evicted
	_, err = GetScratchpad(agentDir, firstID, nil, nil)
	if err == nil {
		t.Fatalf("expected evicted first ID %q to return error, got nil", firstID)
	}
}

func TestExpandScratchpadMacros(t *testing.T) {
	agentDir := t.TempDir()

	text := "Header Line\nContent Line 1\nContent Line 2\nFooter Line"
	entry, err := CreateScratchpad(agentDir, text, "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	rawArg := fmt.Sprintf("Data: <SCRATCHPAD_DATA id=%q skip_lines=\"1\" num_lines=\"2\" />", entry.ID)
	expanded, err := ExpandScratchpadMacros(agentDir, rawArg)
	if err != nil {
		t.Fatalf("ExpandScratchpadMacros failed: %v", err)
	}

	expected := "Data: Content Line 1\nContent Line 2"
	if expanded != expected {
		t.Errorf("expected %q, got %q", expected, expanded)
	}
}

func TestExecuteTool_ScratchpadMacroAndOutputRedirection(t *testing.T) {
	agentDir := t.TempDir()

	entry, err := CreateScratchpad(agentDir, "input data from scratchpad", "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	toolPath := filepath.Join(agentDir, "echo_tool.sh")
	script := "#!/bin/sh\nread input\necho \"Processed: $input\"\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	args := ExecToolArgs{
		Stdin: fmt.Sprintf("<SCRATCHPAD_DATA id=%q />", entry.ID),
	}

	output, err := executeTool(context.Background(), agentDir, "echo_tool.sh", toolPath, args, nil)
	if err != nil {
		t.Fatalf("executeTool failed: %v", err)
	}

	if !strings.Contains(output, "Processed: input data from scratchpad") || !strings.HasPrefix(output, "<STDOUT>") {
		t.Errorf("unexpected executeTool output: %s", output)
	}
}

func TestExecuteTool_LargeOutputRedirectionWithSize(t *testing.T) {
	agentDir := t.TempDir()

	// Script that outputs text larger than ScratchpadOutputThreshold (4000 bytes)
	toolPath := filepath.Join(agentDir, "large_tool.sh")
	script := "#!/bin/sh\npython3 -c \"print('A' * 5000)\"\n"
	if err := os.WriteFile(toolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write tool script: %v", err)
	}

	output, err := executeTool(context.Background(), agentDir, "large_tool.sh", toolPath, ExecToolArgs{}, nil)
	if err != nil {
		t.Fatalf("executeTool failed: %v", err)
	}

	// Output must contain <SCRATCHPAD_DATA id="..." size="5001" lines="1" /> (5000 'A's + newline)
	if !strings.Contains(output, "<SCRATCHPAD_DATA id=") || !strings.Contains(output, "size=\"5001\"") || !strings.Contains(output, "lines=\"1\"") {
		t.Fatalf("expected auto-captured tag with size and lines attributes, got: %s", output)
	}
}

func TestCreateScratchpad_ConcurrentCreations(t *testing.T) {
	agentDir := t.TempDir()

	const numGoroutines = 20
	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := CreateScratchpad(agentDir, fmt.Sprintf("payload from goroutine %d", idx), "concurrent_test")
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent CreateScratchpad failed: %v", err)
	}

	items, count, capVal, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("ListScratchpads failed after concurrent creation: %v", err)
	}

	if count != numGoroutines {
		t.Errorf("expected %d entries, got %d", numGoroutines, count)
	}
	if capVal != MaxScratchpadEntries {
		t.Errorf("expected cap %d, got %d", MaxScratchpadEntries, capVal)
	}
	if len(items) != numGoroutines {
		t.Errorf("expected %d items, got %d", numGoroutines, len(items))
	}
}

func TestSDK_ScratchpadOperations(t *testing.T) {
	wsDir := t.TempDir()
	sdk := NewSDK(wsDir)

	agentID := "test_agent"
	agentDir := sdk.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("test_agent\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	// 1. CreateScratchpad via SDK
	text := "Line 1: Hello SDK\nLine 2: Search target\nLine 3: Goodbye SDK\n"
	entry, err := sdk.CreateScratchpad(agentID, text, "cli_test")
	if err != nil {
		t.Fatalf("sdk.CreateScratchpad failed: %v", err)
	}
	if entry.CreatedBy != "cli_test" {
		t.Errorf("expected CreatedBy 'cli_test', got %q", entry.CreatedBy)
	}

	// 2. GetScratchpad via SDK
	readBack, err := sdk.GetScratchpad(agentID, entry.ID, nil, nil)
	if err != nil {
		t.Fatalf("sdk.GetScratchpad failed: %v", err)
	}
	if readBack != text {
		t.Errorf("got %q, expected %q", readBack, text)
	}

	// 3. ListScratchpads via SDK
	items, count, capVal, err := sdk.ListScratchpads(agentID)
	if err != nil {
		t.Fatalf("sdk.ListScratchpads failed: %v", err)
	}
	if count != 1 || capVal != MaxScratchpadEntries || len(items) != 1 {
		t.Errorf("unexpected list output: count %d, cap %d, len %d", count, capVal, len(items))
	}

	// 4. SearchScratchpad via SDK
	searchRes, err := sdk.SearchScratchpad(agentID, entry.ID, "target", nil, false, 10)
	if err != nil {
		t.Fatalf("sdk.SearchScratchpad failed: %v", err)
	}
	if searchRes.TotalMatches != 1 {
		t.Errorf("expected 1 match, got %d", searchRes.TotalMatches)
	}
	if len(searchRes.Matches) > 0 && searchRes.Matches[0].Line != 2 {
		t.Errorf("expected line 2, got %d", searchRes.Matches[0].Line)
	}
}

func TestCreateScratchpad_MacroCombination(t *testing.T) {
	agentDir := t.TempDir()

	e1, err := CreateScratchpad(agentDir, "Part 1 Data", "test")
	if err != nil {
		t.Fatalf("failed to create e1: %v", err)
	}

	e2, err := CreateScratchpad(agentDir, "Part 2 Data", "test")
	if err != nil {
		t.Fatalf("failed to create e2: %v", err)
	}

	combinedPayload := fmt.Sprintf("Header:\n<SCRATCHPAD_DATA id=%q />\n<SCRATCHPAD_DATA id=%q />\nFooter", e1.ID, e2.ID)
	combinedEntry, err := CreateScratchpad(agentDir, combinedPayload, "test_combine")
	if err != nil {
		t.Fatalf("failed to create combined entry: %v", err)
	}

	expectedText := "Header:\nPart 1 Data\nPart 2 Data\nFooter"
	if combinedEntry.Text != expectedText {
		t.Errorf("got combined text %q, expected %q", combinedEntry.Text, expectedText)
	}
}

func TestExpandScratchpadMacros_JsonEscape(t *testing.T) {
	agentDir := t.TempDir()

	rawText := "Line 1: \"hello\"\nLine 2: backslash \\ and tab\t"
	entry, err := CreateScratchpad(agentDir, rawText, "test_json")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	// Template JSON body expecting inner string escaping without outer quotes
	jsonTemplate := fmt.Sprintf(`{"title": "Chapter", "content": "<SCRATCHPAD_DATA id=%q json_escape=\"true\" />"}`, entry.ID)

	expanded, err := ExpandScratchpadMacros(agentDir, jsonTemplate)
	if err != nil {
		t.Fatalf("ExpandScratchpadMacros failed: %v", err)
	}

	// Verify the result is valid JSON
	var parsed struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(expanded), &parsed); err != nil {
		t.Fatalf("expanded text is not valid JSON (%v):\n%s", err, expanded)
	}

	if parsed.Title != "Chapter" {
		t.Errorf("expected Title 'Chapter', got %q", parsed.Title)
	}
	if parsed.Content != rawText {
		t.Errorf("expected Content %q, got %q", rawText, parsed.Content)
	}
}

func TestCountLines(t *testing.T) {
	tests := []struct {
		text     string
		expected int
	}{
		{"", 0},
		{"single line", 1},
		{"single line with newline\n", 1},
		{"line 1\nline 2", 2},
		{"line 1\nline 2\n", 2},
		{"line 1\nline 2\nline 3\n", 3},
	}

	for _, tt := range tests {
		got := CountLines(tt.text)
		if got != tt.expected {
			t.Errorf("CountLines(%q) = %d; want %d", tt.text, got, tt.expected)
		}
	}
}

func TestCreateScratchpad_LinesEncoding(t *testing.T) {
	agentDir := t.TempDir()

	text := "Line 1\nLine 2\nLine 3\n"
	entry, err := CreateScratchpad(agentDir, text, "test_lines")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	if entry.Lines != 3 {
		t.Errorf("expected entry.Lines == 3, got %d", entry.Lines)
	}

	items, _, _, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("ListScratchpads failed: %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	if items[0].Lines != 3 {
		t.Errorf("expected item.Lines == 3, got %d", items[0].Lines)
	}
	if items[0].CreatedBy != "test_lines" {
		t.Errorf("expected CreatedBy 'test_lines', got %q", items[0].CreatedBy)
	}
}

func TestListScratchpads_OldFormatRobustness(t *testing.T) {
	agentDir := t.TempDir()

	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		t.Fatalf("failed to create scratchpad directory: %v", err)
	}

	// Write an old D30 format file <id>-<createdBy>.txt (without lines field)
	oldPath := filepath.Join(spDir, "abcd-old_author.txt")
	if err := os.WriteFile(oldPath, []byte("old content"), 0644); err != nil {
		t.Fatalf("failed to write old-format scratchpad file: %v", err)
	}

	items, count, _, err := ListScratchpads(agentDir)
	if err != nil {
		t.Fatalf("ListScratchpads failed on old format: %v", err)
	}

	if count != 1 || len(items) != 1 {
		t.Fatalf("expected 1 item, got count=%d len=%d", count, len(items))
	}

	if items[0].ID != "abcd" {
		t.Errorf("expected ID 'abcd', got %q", items[0].ID)
	}
	if items[0].Lines != 0 {
		t.Errorf("expected old-format lines to be 0, got %d", items[0].Lines)
	}
	if items[0].CreatedBy != "old_author" {
		t.Errorf("expected CreatedBy 'old_author', got %q", items[0].CreatedBy)
	}
}

func TestGetScratchpad_ContextWindowCap(t *testing.T) {
	agentDir := t.TempDir()

	// Write runtime.json with contextWindow = 8000 (25% cap = 2000 tokens ~ 8000 chars)
	runtimeCfg := RuntimeConfig{ContextWindow: 8000}
	cfgBytes, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), cfgBytes, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	// Create 100 lines of 100 chars each = ~10,000 chars (~2500 tokens)
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("Line %03d: %s", i, strings.Repeat("x", 90))
	}
	content := strings.Join(lines, "\n")

	entry, err := CreateScratchpad(agentDir, content, "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	// Full read should fail exceeding 25% of 8000 contextWindow (2000 tokens)
	_, err = GetScratchpad(agentDir, entry.ID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the single-read limit of 2000 tokens (25% of 8000 contextWindow)") {
		t.Fatalf("expected contextWindow 25%% cap error, got: %v", err)
	}

	// Paginated read of 10 lines (~1000 chars ~ 250 tokens) should succeed
	num := 10
	paginated, err := GetScratchpad(agentDir, entry.ID, nil, &num)
	if err != nil {
		t.Fatalf("expected paginated read to succeed, got: %v", err)
	}
	if len(strings.Split(paginated, "\n")) != 10 {
		t.Errorf("expected 10 lines, got %d", len(strings.Split(paginated, "\n")))
	}

	// Macro expansion (for tool piping) should still work for the full 10,000 char content
	macroInput := fmt.Sprintf("<SCRATCHPAD_DATA id=%q />", entry.ID)
	expanded, err := ExpandScratchpadMacros(agentDir, macroInput)
	if err != nil {
		t.Fatalf("ExpandScratchpadMacros failed: %v", err)
	}
	if expanded != content {
		t.Errorf("expected macro to expand full content without cap")
	}
}

func TestGetScratchpad_FallbackCap(t *testing.T) {
	agentDir := t.TempDir()

	// When runtime.json is absent and OPENROUTER_API_KEY is not set, LoadRuntimeConfig
	// fails closed, triggering the 200KB byte-based fallback cap. If OPENROUTER_API_KEY
	// is set, D74's DefaultRuntimeJSON (contextWindow=200000) token cap triggers.
	// Both must reject an unpaginated >200KB read with a limit error.
	largeData := strings.Repeat("A", MaxScratchpadReadSizeBytes+1024)
	entry, err := CreateScratchpad(agentDir, largeData, "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	_, err = GetScratchpad(agentDir, entry.ID, nil, nil)
	if err == nil || (!strings.Contains(err.Error(), "exceeds the single-read limit") && !strings.Contains(err.Error(), "byte read limit")) {
		t.Fatalf("expected read cap error, got: %v", err)
	}
}
