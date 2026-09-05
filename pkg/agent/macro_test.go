package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandMacros(t *testing.T) {
	tempDir := t.TempDir()

	rulesContent := "Rule 1: Always be helpful.\nRule 2: Speak concisely."
	if err := os.WriteFile(filepath.Join(tempDir, "rules.md"), []byte(rulesContent), 0644); err != nil {
		t.Fatalf("failed to write rules.md: %v", err)
	}

	mainPrompt := "System Prompt:\n@rules.md\nEnd Prompt"
	expanded, err := ExpandMacros(mainPrompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected macro expansion error: %v", err)
	}

	if !strings.Contains(expanded, rulesContent) {
		t.Errorf("expected expanded prompt to contain rulesContent, got:\n%s", expanded)
	}
}

func TestExpandMacrosCircular(t *testing.T) {
	tempDir := t.TempDir()

	fileA := "@fileB.md"
	fileB := "@fileA.md"
	if err := os.WriteFile(filepath.Join(tempDir, "fileA.md"), []byte(fileA), 0644); err != nil {
		t.Fatalf("failed to write fileA.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "fileB.md"), []byte(fileB), 0644); err != nil {
		t.Fatalf("failed to write fileB.md: %v", err)
	}

	expanded, err := ExpandMacros("@fileA.md", tempDir)
	if err != nil {
		t.Fatalf("unexpected error during circular macro expansion: %v", err)
	}

	if !strings.Contains(expanded, "Circular macro import omitted") {
		t.Errorf("expected circular import omission notice, got:\n%s", expanded)
	}
}

func TestExpandMacrosEmails(t *testing.T) {
	tempDir := t.TempDir()

	// Even if agentmail.to and gmail.com exist on disk, emails must remain untouched
	if err := os.WriteFile(filepath.Join(tempDir, "agentmail.to"), []byte("AGENTMAIL CONTENT"), 0644); err != nil {
		t.Fatalf("failed to write agentmail.to: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "gmail.com"), []byte("GMAIL CONTENT"), 0644); err != nil {
		t.Fatalf("failed to write gmail.com: %v", err)
	}

	prompt := "Contact user@agentmail.to or crgodsey@gmail.com for inquiries.\nEmail: <user@agentmail.to>"
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if expanded != prompt {
		t.Errorf("expected emails to remain completely untouched, got:\n%s", expanded)
	}
	if strings.Contains(expanded, "AGENTMAIL CONTENT") || strings.Contains(expanded, "GMAIL CONTENT") {
		t.Errorf("expected email domains not to be expanded as macro files, got:\n%s", expanded)
	}
	if strings.Contains(expanded, "<!-- Error") {
		t.Errorf("expected no error comments for email addresses, got:\n%s", expanded)
	}
}

func TestExpandMacrosSocialHandles(t *testing.T) {
	tempDir := t.TempDir()

	prompt := "Thanks to @DranboF and @here for pinging @channel!"
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if expanded != prompt {
		t.Errorf("expected social handles to pass through verbatim, got:\n%s", expanded)
	}
	if strings.Contains(expanded, "<!-- Error") || strings.Contains(expanded, "<!-- Circular") {
		t.Errorf("expected no error comments for social handles, got:\n%s", expanded)
	}
}

func TestExpandMacrosTrailingPunctuation(t *testing.T) {
	tempDir := t.TempDir()

	rulesContent := "Rule 1: Be concise."
	if err := os.WriteFile(filepath.Join(tempDir, "rules.md"), []byte(rulesContent), 0644); err != nil {
		t.Fatalf("failed to write rules.md: %v", err)
	}

	prompt := "Please read @rules.md. In addition, see @rules.md, which is vital."
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Please read Rule 1: Be concise.. In addition, see Rule 1: Be concise., which is vital."
	if expanded != expected {
		t.Errorf("expected trailing punctuation preserved, got:\n%s\nexpected:\n%s", expanded, expected)
	}
}

func TestExpandMacrosParenthetical(t *testing.T) {
	tempDir := t.TempDir()

	rulesContent := "Rule 1: Safety first."
	if err := os.WriteFile(filepath.Join(tempDir, "rules.md"), []byte(rulesContent), 0644); err != nil {
		t.Fatalf("failed to write rules.md: %v", err)
	}

	prompt := "Parentheses: (@rules.md)\nBrackets: [@rules.md]\nAngle: <@rules.md>\nQuotes: \"@rules.md\" and '@rules.md'\nBackticks: `@rules.md`\nBraces: {@rules.md}"
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Parentheses: (Rule 1: Safety first.)\nBrackets: [Rule 1: Safety first.]\nAngle: <Rule 1: Safety first.>\nQuotes: \"Rule 1: Safety first.\" and 'Rule 1: Safety first.'\nBackticks: `Rule 1: Safety first.`\nBraces: {Rule 1: Safety first.}"
	if expanded != expected {
		t.Errorf("expected delimiter and parenthetical preservation, got:\n%s\nexpected:\n%s", expanded, expected)
	}
}

func TestExpandMacrosEscapes(t *testing.T) {
	tempDir := t.TempDir()

	// Create rules.md on disk - escapes must still prevent expansion
	if err := os.WriteFile(filepath.Join(tempDir, "rules.md"), []byte("SHOULD NOT EXPAND"), 0644); err != nil {
		t.Fatalf("failed to write rules.md: %v", err)
	}

	prompt := "Use \\@rules.md or @@rules.md to mention without including."
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Use @rules.md or @rules.md to mention without including."
	if expanded != expected {
		t.Errorf("expected escapes to render literally as @rules.md, got:\n%s\nexpected:\n%s", expanded, expected)
	}
	if strings.Contains(expanded, "SHOULD NOT EXPAND") {
		t.Errorf("escaped macro expanded unexpectedly: %s", expanded)
	}
}

func TestExpandMacrosCrossAgent(t *testing.T) {
	wsDir := t.TempDir()

	// Mark workspace root
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write WACKYPUB_ROOT: %v", err)
	}

	sharedDir := filepath.Join(wsDir, "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("failed to create shared dir: %v", err)
	}
	sharedRules := "Conduct: Respect workspace boundaries."
	if err := os.WriteFile(filepath.Join(sharedDir, "rules.md"), []byte(sharedRules), 0644); err != nil {
		t.Fatalf("failed to write shared rules: %v", err)
	}

	agentDir := filepath.Join(wsDir, "agents", "dranbo")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	prompt := "Shared rules: @../../shared/rules.md"
	expanded, err := ExpandMacros(prompt, agentDir)
	if err != nil {
		t.Fatalf("unexpected error in cross-agent include: %v", err)
	}

	expected := "Shared rules: Conduct: Respect workspace boundaries."
	if expanded != expected {
		t.Errorf("expected cross-agent include to expand cleanly, got:\n%s\nexpected:\n%s", expanded, expected)
	}
}

func TestExpandMacrosTraversalEscape(t *testing.T) {
	wsDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write WACKYPUB_ROOT: %v", err)
	}

	agentDir := filepath.Join(wsDir, "agentA")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// 1. Direct path traversal escape
	prompt := "Sensitive: @../../../etc/passwd"
	expanded, err := ExpandMacros(prompt, agentDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if expanded != prompt {
		t.Errorf("expected path traversal escape to pass through literally, got:\n%s", expanded)
	}

	// 2. Symlink pointing outside workspace root
	evilSymlink := filepath.Join(agentDir, "evil.txt")
	if err := os.Symlink("/etc/passwd", evilSymlink); err == nil {
		symlinkPrompt := "Symlink escape: @evil.txt"
		expandedSymlink, err := ExpandMacros(symlinkPrompt, agentDir)
		if err != nil {
			t.Fatalf("unexpected error on symlink escape: %v", err)
		}
		if expandedSymlink != symlinkPrompt {
			t.Errorf("expected symlink pointing outside workspace to pass through literally, got:\n%s", expandedSymlink)
		}
	}
}

func TestExpandMacrosDiamondAndMultiInclude(t *testing.T) {
	tempDir := t.TempDir()

	sharedContent := "Common standard."
	if err := os.WriteFile(filepath.Join(tempDir, "shared.md"), []byte(sharedContent), 0644); err != nil {
		t.Fatalf("failed to write shared.md: %v", err)
	}

	branchA := "Branch A:\n@shared.md"
	if err := os.WriteFile(filepath.Join(tempDir, "branchA.md"), []byte(branchA), 0644); err != nil {
		t.Fatalf("failed to write branchA.md: %v", err)
	}

	branchB := "Branch B:\n@shared.md"
	if err := os.WriteFile(filepath.Join(tempDir, "branchB.md"), []byte(branchB), 0644); err != nil {
		t.Fatalf("failed to write branchB.md: %v", err)
	}

	// Multi-include in same document plus diamond include
	prompt := "Multi: @shared.md and again @shared.md\n@branchA.md\n@branchB.md"
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(expanded, "Circular macro import omitted") {
		t.Errorf("unexpected circular import omission in diamond/multi include, got:\n%s", expanded)
	}
	if count := strings.Count(expanded, sharedContent); count != 4 {
		t.Errorf("expected 4 occurrences of sharedContent, got %d in:\n%s", count, expanded)
	}
}

func TestExpandMacrosDepthLimit(t *testing.T) {
	tempDir := t.TempDir()

	// Chain of 12 files exceeding depth limit of 10
	for i := 0; i < 12; i++ {
		nextFile := fmt.Sprintf("file%d.md", i+1)
		content := fmt.Sprintf("@%s", nextFile)
		if i == 11 {
			content = "Leaf content"
		}
		if err := os.WriteFile(filepath.Join(tempDir, fmt.Sprintf("file%d.md", i)), []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file%d.md: %v", i, err)
		}
	}

	_, err := ExpandMacros("@file0.md", tempDir)
	if err == nil {
		t.Fatalf("expected error exceeding depth limit of 10, got nil")
	}
	if !strings.Contains(err.Error(), "macro expansion depth exceeded limit of 10") {
		t.Errorf("expected depth limit error message, got: %v", err)
	}
}

func TestExpandMacrosDirectory(t *testing.T) {
	tempDir := t.TempDir()

	subDir := filepath.Join(tempDir, "rules")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	prompt := "Dir reference: @rules"
	expanded, err := ExpandMacros(prompt, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if expanded != prompt {
		t.Errorf("expected directory reference to pass through literally, got: %s", expanded)
	}
}
