package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHookScriptForEvent is writeHookScript generalized to an arbitrary event
// directory, for tests that need more than one event.
func writeHookScriptForEvent(t *testing.T, agentDir, event, filename, content string) string {
	t.Helper()
	dir := filepath.Join(agentDir, "hooks", event)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	hookPath := filepath.Join(dir, filename)
	if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write hook script %s: %v", filename, err)
	}
	return hookPath
}

func TestInspectAgentHooks_NoAgentDir(t *testing.T) {
	wsDir := t.TempDir()

	obs, err := InspectAgentHooks(wsDir, "ghost")
	if err != nil {
		t.Fatalf("expected no error for missing agent dir, got: %v", err)
	}
	if obs != nil {
		t.Fatalf("expected nil observations for missing agent dir, got: %v", obs)
	}
}

func TestInspectAgentHooks_NoHooksDir(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	obs, err := InspectAgentHooks(wsDir, "bob")
	if err != nil {
		t.Fatalf("expected no error for missing hooks dir, got: %v", err)
	}
	if obs != nil {
		t.Fatalf("expected nil observations for missing hooks dir, got: %v", obs)
	}
}

func TestInspectAgentHooks_EmptyAgentID(t *testing.T) {
	wsDir := t.TempDir()
	if _, err := InspectAgentHooks(wsDir, ""); err == nil {
		t.Fatalf("expected error for empty agentID")
	}
}

func TestInspectAgentHooks_OrderingAndFields(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")

	writeHookScriptForEvent(t, agentDir, EventOnUserMessage, "10-announce", "#!/bin/sh\necho second\n")
	writeHookScriptForEvent(t, agentDir, EventOnUserMessage, "00-date", "#!/bin/sh\necho first\n")

	obs, err := InspectAgentHooks(wsDir, "bob")
	if err != nil {
		t.Fatalf("InspectAgentHooks failed: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("expected 2 hooks, got %d: %+v", len(obs), obs)
	}

	if obs[0].Name != "00-date" || obs[1].Name != "10-announce" {
		t.Fatalf("expected ascending numeric order 00-date, 10-announce; got %s, %s", obs[0].Name, obs[1].Name)
	}

	first := obs[0]
	if first.Event != EventOnUserMessage {
		t.Errorf("expected event %q, got %q", EventOnUserMessage, first.Event)
	}
	wantPath := filepath.Join(agentDir, "hooks", EventOnUserMessage, "00-date")
	if first.Path != wantPath {
		t.Errorf("expected path %q, got %q", wantPath, first.Path)
	}
	if first.Content != "#!/bin/sh\necho first" {
		t.Errorf("unexpected content: %q", first.Content)
	}
	if first.TotalLines != 2 {
		t.Errorf("expected TotalLines 2, got %d", first.TotalLines)
	}
	if first.Truncated {
		t.Errorf("expected Truncated false for a short script")
	}
}

func TestInspectAgentHooks_MultipleEventsSorted(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")

	writeHookScriptForEvent(t, agentDir, "zzz-event", "00-a", "#!/bin/sh\ntrue\n")
	writeHookScriptForEvent(t, agentDir, EventOnUserMessage, "00-b", "#!/bin/sh\ntrue\n")

	obs, err := InspectAgentHooks(wsDir, "bob")
	if err != nil {
		t.Fatalf("InspectAgentHooks failed: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(obs))
	}
	if obs[0].Event != EventOnUserMessage || obs[1].Event != "zzz-event" {
		t.Fatalf("expected events sorted alphabetically (%q then %q), got %q then %q",
			EventOnUserMessage, "zzz-event", obs[0].Event, obs[1].Event)
	}
}

func TestInspectAgentHooks_SkipsNonExecutable(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")

	writeHookScriptForEvent(t, agentDir, EventOnUserMessage, "00-real", "#!/bin/sh\ntrue\n")

	// A non-executable stray file in the same directory (e.g. a README or a
	// disabled hook) must not appear - it would never actually run.
	nonExecPath := filepath.Join(agentDir, "hooks", EventOnUserMessage, "README.md")
	if err := os.WriteFile(nonExecPath, []byte("notes"), 0644); err != nil {
		t.Fatalf("failed to write non-executable file: %v", err)
	}

	obs, err := InspectAgentHooks(wsDir, "bob")
	if err != nil {
		t.Fatalf("InspectAgentHooks failed: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 hook (non-executable file skipped), got %d: %+v", len(obs), obs)
	}
	if obs[0].Name != "00-real" {
		t.Fatalf("expected only 00-real to be reported, got %q", obs[0].Name)
	}
}

func TestInspectAgentHooks_Truncation(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	totalLines := HookContentLineLimit + 50
	for i := 1; i < totalLines; i++ {
		fmt.Fprintf(&b, "echo line-%d\n", i)
	}

	writeHookScriptForEvent(t, agentDir, EventOnUserMessage, "00-long", b.String())

	obs, err := InspectAgentHooks(wsDir, "bob")
	if err != nil {
		t.Fatalf("InspectAgentHooks failed: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(obs))
	}

	got := obs[0]
	if got.TotalLines != totalLines {
		t.Errorf("expected TotalLines %d, got %d", totalLines, got.TotalLines)
	}
	if !got.Truncated {
		t.Errorf("expected Truncated true for a %d-line script", totalLines)
	}
	gotLines := strings.Split(got.Content, "\n")
	if len(gotLines) != HookContentLineLimit {
		t.Errorf("expected Content to hold exactly %d lines, got %d", HookContentLineLimit, len(gotLines))
	}
	if gotLines[0] != "#!/bin/sh" {
		t.Errorf("expected first captured line to be the shebang, got %q", gotLines[0])
	}
}
