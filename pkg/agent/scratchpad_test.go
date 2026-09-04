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

	"google.golang.org/genai"
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

	// Test missing ID error (conforming 4-char ID)
	_, err = GetScratchpad(agentDir, "zzzz", nil, nil)
	if err == nil {
		t.Fatalf("expected error for missing ID, got nil")
	}
	if !strings.Contains(err.Error(), `scratchpad entry "zzzz" not found`) {
		t.Errorf("unexpected error message: %v", err)
	}

	// Test malformed ID error (D81)
	_, err = GetScratchpad(agentDir, "missing", nil, nil)
	if err == nil {
		t.Fatalf("expected error for malformed ID, got nil")
	}
	if !strings.Contains(err.Error(), `invalid scratchpad entry ID "missing"`) {
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
	expanded, warnings, err := ExpandScratchpadMacros(agentDir, rawArg)
	if err != nil {
		t.Fatalf("ExpandScratchpadMacros failed: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
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

	output, _, err := executeTool(context.Background(), agentDir, "echo_tool.sh", toolPath, args, nil)
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

	output, _, err := executeTool(context.Background(), agentDir, "large_tool.sh", toolPath, ExecToolArgs{}, nil)
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

	expanded, warnings, err := ExpandScratchpadMacros(agentDir, jsonTemplate)
	if err != nil {
		t.Fatalf("ExpandScratchpadMacros failed: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
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
	expanded, warnings, err := ExpandScratchpadMacros(agentDir, macroInput)
	if err != nil {
		t.Fatalf("ExpandScratchpadMacros failed: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
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

func TestScratchpad_IDValidation_Read(t *testing.T) {
	wsDir := t.TempDir()
	agentA := filepath.Join(wsDir, "agentA")
	agentB := filepath.Join(wsDir, "agentB")

	entryA, err := CreateScratchpad(agentA, "secret data in agentA", "agentA")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	// 1. Malformed IDs rejected with no filesystem access attempted
	malformedIDs := []string{"", "a", "ab", "abc", "abcde", "missing", "TOOLONG", "ABCD", "ab-d", "ab_d"}
	for _, id := range malformedIDs {
		_, err := GetScratchpad(agentA, id, nil, nil)
		if err == nil {
			t.Errorf("GetScratchpad(%q) expected error for malformed ID, got nil", id)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("invalid scratchpad entry ID %q", id)) || !strings.Contains(err.Error(), "must be exactly 4 lowercase alphanumeric characters") {
			t.Errorf("GetScratchpad(%q) unexpected error message: %v", id, err)
		}

		_, err = readScratchpadRaw(agentA, id, nil, nil)
		if err == nil {
			t.Errorf("readScratchpadRaw(%q) expected error for malformed ID, got nil", id)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("invalid scratchpad entry ID %q", id)) {
			t.Errorf("readScratchpadRaw(%q) unexpected error message: %v", id, err)
		}
	}

	// 2. Parent-directory traversal cannot resolve an entry in sibling agent's scratchpad
	// Escaping variant: genuinely escapes agentB/scratchpad into sibling agentA/scratchpad/
	escapingTraversalID := "../../agentA/scratchpad/" + entryA.ID
	_, err = GetScratchpad(agentB, escapingTraversalID, nil, nil)
	if err == nil {
		t.Errorf("GetScratchpad escaping traversal ID %q expected error, got nil", escapingTraversalID)
	} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
		t.Errorf("GetScratchpad escaping traversal ID %q expected invalid ID error, got: %v", escapingTraversalID, err)
	}

	// Control case: single '..' only cancels 'scratchpad' subfolder, resolving to agentB/agentA/scratchpad/ (non-reaching)
	controlTraversalID := "../agentA/scratchpad/" + entryA.ID
	_, err = GetScratchpad(agentB, controlTraversalID, nil, nil)
	if err == nil {
		t.Errorf("GetScratchpad control traversal ID %q expected error, got nil", controlTraversalID)
	} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
		t.Errorf("GetScratchpad control traversal ID %q expected invalid ID error, got: %v", controlTraversalID, err)
	}

	// 3. Trailing-wildcard id resolves nothing
	wildcardIDs := []string{"abc*", "*", entryA.ID[:3] + "*"}
	for _, id := range wildcardIDs {
		_, err := GetScratchpad(agentA, id, nil, nil)
		if err == nil {
			t.Errorf("GetScratchpad wildcard ID %q expected error, got nil", id)
		} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
			t.Errorf("GetScratchpad wildcard ID %q expected invalid ID error, got: %v", id, err)
		}
	}

	// 4. Wildcard combined with traversal: reaches sibling agent's directory without prior knowledge of any id
	traversalWildcardID := "../../agentA/scratchpad/*"
	val, err := GetScratchpad(agentB, traversalWildcardID, nil, nil)
	if err == nil {
		t.Errorf("GetScratchpad traversal wildcard ID %q unexpectedly succeeded: read %q", traversalWildcardID, val)
	} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
		t.Errorf("GetScratchpad traversal wildcard ID %q expected invalid ID error, got: %v", traversalWildcardID, err)
	}

	// 5. Ordinary four-character ID still works as before
	val, err = GetScratchpad(agentA, entryA.ID, nil, nil)
	if err != nil {
		t.Fatalf("GetScratchpad with valid ID failed: %v", err)
	}
	if val != "secret data in agentA" {
		t.Errorf("expected %q, got %q", "secret data in agentA", val)
	}

	// Non-existent four-character ID returns not found (not invalid ID shape)
	_, err = GetScratchpad(agentA, "zzzz", nil, nil)
	if err == nil {
		t.Fatalf("expected error for non-existent 4-char ID, got nil")
	}
	if !strings.Contains(err.Error(), `scratchpad entry "zzzz" not found`) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestScratchpad_IDValidation_Delete(t *testing.T) {
	wsDir := t.TempDir()
	agentA := filepath.Join(wsDir, "agentA")
	agentB := filepath.Join(wsDir, "agentB")

	entryA, err := CreateScratchpad(agentA, "secret data in agentA", "agentA")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	// 1. Malformed IDs rejected with no filesystem access attempted
	malformedIDs := []string{"", "a", "abc", "abcde", "missing", "ABCD"}
	for _, id := range malformedIDs {
		err := DeleteScratchpad(agentA, id)
		if err == nil {
			t.Errorf("DeleteScratchpad(%q) expected error for malformed ID, got nil", id)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("invalid scratchpad entry ID %q", id)) || !strings.Contains(err.Error(), "must be exactly 4 lowercase alphanumeric characters") {
			t.Errorf("DeleteScratchpad(%q) unexpected error message: %v", id, err)
		}
	}

	// 2. Parent-directory traversal cannot delete an entry in sibling agent's scratchpad
	// Escaping variant: genuinely escapes agentB/scratchpad into sibling agentA/scratchpad/
	escapingTraversalID := "../../agentA/scratchpad/" + entryA.ID
	err = DeleteScratchpad(agentB, escapingTraversalID)
	if err == nil {
		t.Errorf("DeleteScratchpad escaping traversal ID %q expected error, got nil", escapingTraversalID)
	} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
		t.Errorf("DeleteScratchpad escaping traversal ID %q expected invalid ID error, got: %v", escapingTraversalID, err)
	}

	// Control case: single '..' only cancels 'scratchpad' subfolder, resolving to agentB/agentA/scratchpad/ (non-reaching)
	controlTraversalID := "../agentA/scratchpad/" + entryA.ID
	err = DeleteScratchpad(agentB, controlTraversalID)
	if err == nil {
		t.Errorf("DeleteScratchpad control traversal ID %q expected error, got nil", controlTraversalID)
	} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
		t.Errorf("DeleteScratchpad control traversal ID %q expected invalid ID error, got: %v", controlTraversalID, err)
	}

	// Verify agentA's entry is untouched
	val, err := GetScratchpad(agentA, entryA.ID, nil, nil)
	if err != nil || val != "secret data in agentA" {
		t.Fatalf("entry in agentA was affected by traversal delete attempt: val=%q, err=%v", val, err)
	}

	// 3. Trailing-wildcard id resolves nothing
	wildcardIDs := []string{"abc*", "*", entryA.ID[:3] + "*"}
	for _, id := range wildcardIDs {
		err := DeleteScratchpad(agentA, id)
		if err == nil {
			t.Errorf("DeleteScratchpad wildcard ID %q expected error, got nil", id)
		} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
			t.Errorf("DeleteScratchpad wildcard ID %q expected invalid ID error, got: %v", id, err)
		}
	}

	// 4. Wildcard combined with traversal: attempts to delete sibling agent's entries without knowing id
	traversalWildcardID := "../../agentA/scratchpad/*"
	err = DeleteScratchpad(agentB, traversalWildcardID)
	if err == nil {
		t.Errorf("DeleteScratchpad traversal wildcard ID %q unexpectedly succeeded", traversalWildcardID)
	} else if !strings.Contains(err.Error(), "invalid scratchpad entry ID") {
		t.Errorf("DeleteScratchpad traversal wildcard ID %q expected invalid ID error, got: %v", traversalWildcardID, err)
	}

	// Verify agentA's entry is still untouched
	val, err = GetScratchpad(agentA, entryA.ID, nil, nil)
	if err != nil || val != "secret data in agentA" {
		t.Fatalf("entry in agentA was affected by wildcard traversal delete attempt: val=%q, err=%v", val, err)
	}

	// 5. Ordinary four-character ID still deletes as before
	entryToDelete, err := CreateScratchpad(agentA, "to be deleted", "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}
	if err := DeleteScratchpad(agentA, entryToDelete.ID); err != nil {
		t.Fatalf("DeleteScratchpad with valid ID failed: %v", err)
	}
	// Verify it's gone
	_, err = GetScratchpad(agentA, entryToDelete.ID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("scratchpad entry %q not found", entryToDelete.ID)) {
		t.Errorf("expected entry to be deleted, got: %v", err)
	}

	// Non-existent four-character ID returns not found (not invalid ID shape)
	err = DeleteScratchpad(agentA, "zzzz")
	if err == nil {
		t.Fatalf("expected error for non-existent 4-char ID, got nil")
	}
	if !strings.Contains(err.Error(), `scratchpad entry "zzzz" not found`) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestD90_ExpandScratchpadMacros_ExistenceGatedAndEscape(t *testing.T) {
	agentDir := t.TempDir()

	realEntry, err := CreateScratchpad(agentDir, "line one\nline two\nline three", "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	t.Run("well-formed but nonexistent id passes through literally with warning", func(t *testing.T) {
		input := `prefix <SCRATCHPAD_DATA id="zz99" skip_lines="1" num_lines="1" json_escape="true" /> suffix`
		expanded, warnings, err := ExpandScratchpadMacros(agentDir, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if expanded != input {
			t.Errorf("expected text to pass through literally, got %q", expanded)
		}
		if len(warnings) != 1 {
			t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], `scratchpad entry "zz99" not found; macro passed through literally`) {
			t.Errorf("unexpected warning text: %q", warnings[0])
		}
	})

	t.Run("malformed-shape id passes through literally with warning", func(t *testing.T) {
		input := `Example: <SCRATCHPAD_DATA id="EXAMPLE_ID" /> and <SCRATCHPAD_DATA id="../traversal" />`
		expanded, warnings, err := ExpandScratchpadMacros(agentDir, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if expanded != input {
			t.Errorf("expected text to pass through literally, got %q", expanded)
		}
		if len(warnings) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], `scratchpad entry "EXAMPLE_ID" not found; macro passed through literally`) {
			t.Errorf("unexpected warning[0]: %q", warnings[0])
		}
		if !strings.Contains(warnings[1], `scratchpad entry "../traversal" not found; macro passed through literally`) {
			t.Errorf("unexpected warning[1]: %q", warnings[1])
		}
	})

	t.Run("genuine expansion works for existing entry without warning", func(t *testing.T) {
		input := fmt.Sprintf("Data: <SCRATCHPAD_DATA id=%q skip_lines=\"1\" num_lines=\"1\" />", realEntry.ID)
		expanded, warnings, err := ExpandScratchpadMacros(agentDir, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if expanded != "Data: line two" {
			t.Errorf("expected 'Data: line two', got %q", expanded)
		}
		if len(warnings) != 0 {
			t.Errorf("expected 0 warnings on genuine expansion, got: %v", warnings)
		}
	})

	t.Run("backslash-escaped macro renders literally even when id exists, without warning", func(t *testing.T) {
		input := fmt.Sprintf(`Escaped: \<SCRATCHPAD_DATA id=%q skip_lines="1" />`, realEntry.ID)
		expanded, warnings, err := ExpandScratchpadMacros(agentDir, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := fmt.Sprintf(`Escaped: <SCRATCHPAD_DATA id=%q skip_lines="1" />`, realEntry.ID)
		if expanded != expected {
			t.Errorf("expected %q, got %q", expected, expanded)
		}
		if len(warnings) != 0 {
			t.Errorf("expected 0 warnings on escaped macro, got: %v", warnings)
		}
	})

	t.Run("backslash-escaped macro with nonexistent id renders literally without warning", func(t *testing.T) {
		input := `Escaped: \<SCRATCHPAD_DATA id="zz88" />`
		expanded, warnings, err := ExpandScratchpadMacros(agentDir, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := `Escaped: <SCRATCHPAD_DATA id="zz88" />`
		if expanded != expected {
			t.Errorf("expected %q, got %q", expected, expanded)
		}
		if len(warnings) != 0 {
			t.Errorf("expected 0 warnings on escaped macro, got: %v", warnings)
		}
	})
}

func TestD90_ExecuteTool_ArgsAndStdin_PassthroughAndWarnings(t *testing.T) {
	agentDir := t.TempDir()

	realEntry, err := CreateScratchpad(agentDir, "real payload content", "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	echoToolPath := filepath.Join(agentDir, "echo_tool.sh")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"ARG: $a\"; done\nif [ ! -t 0 ]; then\n  STDIN=$(cat)\n  if [ -n \"$STDIN\" ]; then echo \"STDIN: $STDIN\"; fi\nfi\n"
	if err := os.WriteFile(echoToolPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write echo_tool.sh: %v", err)
	}

	failToolPath := filepath.Join(agentDir, "fail_tool.sh")
	failScript := "#!/bin/sh\necho \"failing tool stdout\"\nexit 1\n"
	if err := os.WriteFile(failToolPath, []byte(failScript), 0755); err != nil {
		t.Fatalf("failed to write fail_tool.sh: %v", err)
	}

	t.Run("well-formed nonexistent id in args and stdin passes through with warnings", func(t *testing.T) {
		args := ExecToolArgs{
			Args:  []string{"<SCRATCHPAD_DATA id=\"no01\" />"},
			Stdin: "<SCRATCHPAD_DATA id=\"no02\" />",
		}
		out, warnings, err := executeTool(context.Background(), agentDir, "echo_tool.sh", echoToolPath, args, nil)
		if err != nil {
			t.Fatalf("executeTool failed: %v", err)
		}
		if strings.Contains(out, "<WARNING>") {
			t.Fatalf("expected no <WARNING> block in clean output, got:\n%s", out)
		}
		if len(warnings) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
		}
		if warnings[0] != `warning: scratchpad entry "no01" not found; macro passed through literally` {
			t.Errorf("expected warning for no01, got:\n%s", warnings[0])
		}
		if warnings[1] != `warning: scratchpad entry "no02" not found; macro passed through literally` {
			t.Errorf("expected warning for no02, got:\n%s", warnings[1])
		}
		if !strings.Contains(out, `ARG: <SCRATCHPAD_DATA id="no01" />`) {
			t.Errorf("expected arg to pass through literally, got:\n%s", out)
		}
		if !strings.Contains(out, `STDIN: <SCRATCHPAD_DATA id="no02" />`) {
			t.Errorf("expected stdin to pass through literally, got:\n%s", out)
		}
	})

	t.Run("malformed-shape id in args passes through with warning", func(t *testing.T) {
		args := ExecToolArgs{
			Args: []string{"<SCRATCHPAD_DATA id=\"EXAMPLE_ID\" />"},
		}
		out, warnings, err := executeTool(context.Background(), agentDir, "echo_tool.sh", echoToolPath, args, nil)
		if err != nil {
			t.Fatalf("executeTool failed: %v", err)
		}
		if strings.Contains(out, "<WARNING>") {
			t.Fatalf("expected no <WARNING> block in clean output, got:\n%s", out)
		}
		if len(warnings) != 1 || warnings[0] != `warning: scratchpad entry "EXAMPLE_ID" not found; macro passed through literally` {
			t.Errorf("expected warning for EXAMPLE_ID, got: %v", warnings)
		}
		if !strings.Contains(out, `ARG: <SCRATCHPAD_DATA id="EXAMPLE_ID" />`) {
			t.Errorf("expected arg literal passthrough, got:\n%s", out)
		}
	})

	t.Run("genuine expansion in args and stdin works without warning", func(t *testing.T) {
		args := ExecToolArgs{
			Args:  []string{fmt.Sprintf("<SCRATCHPAD_DATA id=%q />", realEntry.ID)},
			Stdin: fmt.Sprintf("<SCRATCHPAD_DATA id=%q />", realEntry.ID),
		}
		out, warnings, err := executeTool(context.Background(), agentDir, "echo_tool.sh", echoToolPath, args, nil)
		if err != nil {
			t.Fatalf("executeTool failed: %v", err)
		}
		if len(warnings) != 0 {
			t.Errorf("expected 0 warnings, got %d: %v", len(warnings), warnings)
		}
		if strings.Contains(out, "<WARNING>") {
			t.Errorf("unexpected <WARNING> block in genuine expansion output:\n%s", out)
		}
		if !strings.Contains(out, "ARG: real payload content") {
			t.Errorf("expected expanded arg content, got:\n%s", out)
		}
		if !strings.Contains(out, "STDIN: real payload content") {
			t.Errorf("expected expanded stdin content, got:\n%s", out)
		}
	})

	t.Run("backslash-escaped macro in args and stdin renders literally without warning", func(t *testing.T) {
		args := ExecToolArgs{
			Args:  []string{fmt.Sprintf(`\<SCRATCHPAD_DATA id=%q />`, realEntry.ID)},
			Stdin: fmt.Sprintf(`\<SCRATCHPAD_DATA id=%q />`, realEntry.ID),
		}
		out, warnings, err := executeTool(context.Background(), agentDir, "echo_tool.sh", echoToolPath, args, nil)
		if err != nil {
			t.Fatalf("executeTool failed: %v", err)
		}
		if len(warnings) != 0 {
			t.Errorf("expected 0 warnings on escaped macro, got: %v", warnings)
		}
		if strings.Contains(out, "<WARNING>") {
			t.Errorf("unexpected <WARNING> block on escaped macro:\n%s", out)
		}
		expectedTag := fmt.Sprintf(`ARG: <SCRATCHPAD_DATA id=%q />`, realEntry.ID)
		if !strings.Contains(out, expectedTag) {
			t.Errorf("expected literal macro tag %q in args, got:\n%s", expectedTag, out)
		}
		expectedStdinTag := fmt.Sprintf(`STDIN: <SCRATCHPAD_DATA id=%q />`, realEntry.ID)
		if !strings.Contains(out, expectedStdinTag) {
			t.Errorf("expected literal macro tag %q in stdin, got:\n%s", expectedStdinTag, out)
		}
	})

	t.Run("warning rides on error when tool fails", func(t *testing.T) {
		args := ExecToolArgs{
			Args: []string{"<SCRATCHPAD_DATA id=\"no99\" />"},
		}
		_, warnings, err := executeTool(context.Background(), agentDir, "fail_tool.sh", failToolPath, args, nil)
		if err == nil {
			t.Fatalf("expected failure from fail_tool.sh, got nil")
		}
		if len(warnings) != 1 {
			t.Errorf("expected 1 warning, got %d: %v", len(warnings), warnings)
		}
		if !strings.Contains(err.Error(), "<WARNING>") {
			t.Errorf("expected error message to contain <WARNING> block, got:\n%s", err.Error())
		}
		if !strings.Contains(err.Error(), `warning: scratchpad entry "no99" not found; macro passed through literally`) {
			t.Errorf("expected error message to contain missing id warning, got:\n%s", err.Error())
		}
	})
}

func TestD90_InProcessCreateScratchpad_WarningSurface(t *testing.T) {
	agentDir := t.TempDir()

	realEntry, err := CreateScratchpad(agentDir, "original content", "test")
	if err != nil {
		t.Fatalf("CreateScratchpad failed: %v", err)
	}

	t.Run("missing id emits warning in entry struct", func(t *testing.T) {
		entry, err := CreateScratchpad(agentDir, `Ref: <SCRATCHPAD_DATA id="no01" />`, "test")
		if err != nil {
			t.Fatalf("CreateScratchpad failed: %v", err)
		}
		if !strings.Contains(entry.Text, `<SCRATCHPAD_DATA id="no01" />`) {
			t.Errorf("expected text to retain literal macro, got: %s", entry.Text)
		}
		if len(entry.Warnings) != 1 {
			t.Fatalf("expected 1 warning, got %d: %v", len(entry.Warnings), entry.Warnings)
		}
		if !strings.Contains(entry.Warning, `scratchpad entry "no01" not found; macro passed through literally`) {
			t.Errorf("unexpected entry.Warning: %q", entry.Warning)
		}
	})

	t.Run("escaped macro creates literal content without warning", func(t *testing.T) {
		input := fmt.Sprintf(`Ref: \<SCRATCHPAD_DATA id=%q />`, realEntry.ID)
		entry, err := CreateScratchpad(agentDir, input, "test")
		if err != nil {
			t.Fatalf("CreateScratchpad failed: %v", err)
		}
		expectedText := fmt.Sprintf(`Ref: <SCRATCHPAD_DATA id=%q />`, realEntry.ID)
		if entry.Text != expectedText {
			t.Errorf("expected %q, got %q", expectedText, entry.Text)
		}
		if len(entry.Warnings) != 0 || entry.Warning != "" {
			t.Errorf("expected no warnings on escaped macro, got: %v / %q", entry.Warnings, entry.Warning)
		}
	})
}

func TestD90_ToolResultLayer_MissingEntryWarningsNotDuplicatedInOutput(t *testing.T) {
	agentDir := t.TempDir()
	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed to create tools dir: %v", err)
	}

	echoScript := filepath.Join(toolsDir, "echo_tool.sh")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"ARG: $a\"; done\nif [ ! -t 0 ]; then\n  STDIN=$(cat)\n  if [ -n \"$STDIN\" ]; then echo \"STDIN: $STDIN\"; fi\nfi\n"
	if err := os.WriteFile(echoScript, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write echo_tool.sh: %v", err)
	}

	discoveredMap := map[string]string{
		"echo_tool.sh": echoScript,
	}

	t.Run("missing-id macro in args and stdin populates Warning and Warnings without duplicating into Output", func(t *testing.T) {
		args := RunCommandArgs{
			Command: "echo_tool.sh",
			Args:    []string{"<SCRATCHPAD_DATA id=\"no01\" />"},
			Stdin:   "<SCRATCHPAD_DATA id=\"no02\" />",
		}

		res, err := executeRunCommand(context.Background(), agentDir, discoveredMap, nil, 0, args)
		if err != nil {
			t.Fatalf("executeRunCommand failed: %v", err)
		}

		expectedWarn1 := `warning: scratchpad entry "no01" not found; macro passed through literally`
		expectedWarn2 := `warning: scratchpad entry "no02" not found; macro passed through literally`

		// 1. result.Warnings contains the expected warning strings
		if len(res.Warnings) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %v", len(res.Warnings), res.Warnings)
		}
		if res.Warnings[0] != expectedWarn1 {
			t.Errorf("expected warning[0] = %q, got %q", expectedWarn1, res.Warnings[0])
		}
		if res.Warnings[1] != expectedWarn2 {
			t.Errorf("expected warning[1] = %q, got %q", expectedWarn2, res.Warnings[1])
		}

		// 2. result.Warning equals that string (joined with newline)
		expectedJoined := expectedWarn1 + "\n" + expectedWarn2
		if res.Warning != expectedJoined {
			t.Errorf("expected Warning = %q, got %q", expectedJoined, res.Warning)
		}

		// 3. result.Output contains ONLY stdout/stderr without <WARNING> tags or warning text
		if strings.Contains(res.Output, "<WARNING>") || strings.Contains(res.Output, "</WARNING>") {
			t.Errorf("res.Output must not contain <WARNING> tags, got:\n%s", res.Output)
		}
		if strings.Contains(res.Output, "warning: scratchpad entry") {
			t.Errorf("res.Output must not contain warning text, got:\n%s", res.Output)
		}
		if !strings.Contains(res.Output, `ARG: <SCRATCHPAD_DATA id="no01" />`) {
			t.Errorf("expected literal arg macro in output, got:\n%s", res.Output)
		}
		if !strings.Contains(res.Output, `STDIN: <SCRATCHPAD_DATA id="no02" />`) {
			t.Errorf("expected literal stdin macro in output, got:\n%s", res.Output)
		}
	})

	t.Run("end-to-end FolderAgent GenerateTurn FunctionResponse contains clean output and structured warning", func(t *testing.T) {
		wsDir := t.TempDir()
		bobDir := filepath.Join(wsDir, "bob")
		bobToolsDir := filepath.Join(bobDir, "tools")
		if err := os.MkdirAll(bobToolsDir, 0755); err != nil {
			t.Fatalf("failed to create bob tools dir: %v", err)
		}
		bobEcho := filepath.Join(bobToolsDir, "echo.sh")
		if err := os.WriteFile(bobEcho, []byte(script), 0755); err != nil {
			t.Fatalf("failed to write bob echo.sh: %v", err)
		}

		fa, err := LoadFolderAgent(wsDir, "bob", 1)
		if err != nil {
			t.Fatalf("LoadFolderAgent failed: %v", err)
		}
		fa.Model = &runCmdTestModel{
			command: "echo.sh",
			args:    []string{"<SCRATCHPAD_DATA id=\"no99\" />"},
		}

		toolMap, _, err := BuildFolderAgentToolsWithA2A(bobDir, nil)
		if err != nil {
			t.Fatalf("BuildFolderAgentToolsWithA2A failed: %v", err)
		}
		fa.ADKAgent, err = BuildADKAgent("bob", fa.SystemPrompt, 1, fa.Model, toolMap["run_command"])
		if err != nil {
			t.Fatalf("BuildADKAgent failed: %v", err)
		}

		uMsg := genai.NewContentFromText("run echo with missing macro", "user")
		if err := AppendSessionContent(bobDir, uMsg); err != nil {
			t.Fatalf("AppendSessionContent failed: %v", err)
		}

		_, _ = fa.GenerateTurn(context.Background())

		turns, err := ReadSessionTurns(bobDir)
		if err != nil {
			t.Fatalf("ReadSessionTurns failed: %v", err)
		}

		foundFunctionResponse := false
		for _, turn := range turns {
			if turn.Role == "user" {
				for _, part := range turn.Parts {
					if part.FunctionResponse != nil {
						foundFunctionResponse = true
						respMap := part.FunctionResponse.Response
						outStr, _ := respMap["output"].(string)
						warnStr, _ := respMap["warning"].(string)

						if strings.Contains(outStr, "<WARNING>") {
							t.Errorf("FunctionResponse output contains <WARNING>: %q", outStr)
						}
						if strings.Contains(outStr, "warning:") {
							t.Errorf("FunctionResponse output contains warning text: %q", outStr)
						}
						if !strings.Contains(outStr, `ARG: <SCRATCHPAD_DATA id="no99" />`) {
							t.Errorf("FunctionResponse output missing literal arg: %q", outStr)
						}
						expectedWarn := `warning: scratchpad entry "no99" not found; macro passed through literally`
						if warnStr != expectedWarn {
							t.Errorf("expected FunctionResponse warning %q, got %q", expectedWarn, warnStr)
						}
					}
				}
			}
		}
		if !foundFunctionResponse {
			t.Fatalf("expected to find FunctionResponse in session turns")
		}
	})
}
