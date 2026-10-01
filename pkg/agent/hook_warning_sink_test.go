package agent

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Hook warnings only reach a caller that asks for them. Before this, a stream started with no
// onWarning callback dropped every hook failure silently: the warnings were computed and then
// discarded, so a broken on-user-message hook looked identical to no hook at all.
func TestHookWarningsLoggedToStderrWhenNoSink(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "sinkless"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	// A2A authorization walks up from CWD and stops at a workspace root marker without consulting
	// any allowlist, so the fixture owns its own root instead of the package directory.
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	t.Chdir(wsDir)
	writeHookScript(t, agentDir, "00-bad", "#!/bin/sh\necho not json\n")

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(prev)

	sdk := NewSDK(wsDir)
	for _, err := range sdk.addAndGenerateTurnStreamImpl(context.Background(), agentID, "hello") {
		_ = err
	}

	out := logged.String()
	if !strings.Contains(out, "hook warning for agent") {
		t.Fatalf("expected the hook warning on stderr, got %q", out)
	}
	if !strings.Contains(out, "00-bad") {
		t.Errorf("expected the warning to name the hook, got %q", out)
	}
}

// A caller that did supply a callback must not also get stderr output, or every warning prints
// twice, once for the operator and once for the client.
func TestHookWarningsNotDoubleLoggedWhenSinkPresent(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "sunk"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	// A2A authorization walks up from CWD and stops at a workspace root marker without consulting
	// any allowlist, so the fixture owns its own root instead of the package directory.
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	t.Chdir(wsDir)
	writeHookScript(t, agentDir, "00-bad", "#!/bin/sh\necho not json\n")

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(prev)

	var got []string
	sdk := NewSDK(wsDir)
	for _, err := range sdk.addAndGenerateTurnStreamImpl(context.Background(), agentID, "hello", func(w string) {
		got = append(got, w)
	}) {
		_ = err
	}

	if len(got) == 0 {
		t.Fatal("expected the callback to receive the hook warning")
	}
	if out := logged.String(); strings.Contains(out, "hook warning for agent") {
		t.Errorf("warning was logged to stderr as well as delivered to the sink: %q", out)
	}
}
