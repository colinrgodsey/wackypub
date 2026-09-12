package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("open pipe: %v", err)
	}
	os.Stdout = w

	runErr := fn()

	closeErr := w.Close()
	os.Stdout = old
	if closeErr != nil {
		t.Fatalf("close pipe: %v", closeErr)
	}

	captured, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("read captured stdout: %v", readErr)
	}
	return string(captured), runErr
}

// newCLIDiffWorkspace lays out a throwaway workspace with two entries that differ by one line,
// and moves the test into it so authorization resolves inside the workspace instead of against
// whatever the repository checkout happens to contain.
func newCLIDiffWorkspace(t *testing.T) (wsDir, beforeID, afterID string) {
	t.Helper()

	wsDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0o644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}

	agentDir := filepath.Join(wsDir, "diffagent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	before, err := adkAgent.CreateScratchpad(agentDir, "keep\nreplace me\nkeep too", "cli_test")
	if err != nil {
		t.Fatalf("create before entry: %v", err)
	}
	after, err := adkAgent.CreateScratchpad(agentDir, "keep\nreplaced\nkeep too", "cli_test")
	if err != nil {
		t.Fatalf("create after entry: %v", err)
	}

	t.Chdir(wsDir)
	return wsDir, before.ID, after.ID
}

func TestScratchpadDiffCLIPrintsUnifiedDiff(t *testing.T) {
	wsDir, beforeID, afterID := newCLIDiffWorkspace(t)

	out, err := captureStdout(t, func() error {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "diffagent", "scratchpad", "diff", beforeID, afterID})
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("scratchpad diff: %v", err)
	}

	for _, want := range []string{"--- " + beforeID, "+++ " + afterID, "-replace me", "+replaced"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected stdout to contain %q, got:\n%s", want, out)
		}
	}
}

// Identical entries must stay silent so a caller can test for "did anything change" by looking
// for empty output instead of parsing prose.
// The canonical verb ordering (agent scratchpad diff <agent_id> ...) is a separate wiring path
// from the agent-id-first one above: it reaches the subcommand through AddCommand rather than
// the parent dispatcher's switch, so both need a test.
func TestScratchpadDiffCLICanonicalOrdering(t *testing.T) {
	wsDir, beforeID, afterID := newCLIDiffWorkspace(t)

	out, err := captureStdout(t, func() error {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "scratchpad", "diff", "diffagent", beforeID, afterID})
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("scratchpad diff in canonical ordering: %v", err)
	}
	if !strings.Contains(out, "+replaced") {
		t.Errorf("expected the diff in stdout, got %q", out)
	}
}

func TestScratchpadDiffCLIIdenticalEntriesPrintNothing(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0o644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}
	agentDir := filepath.Join(wsDir, "diffagent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	first, err := adkAgent.CreateScratchpad(agentDir, "same\ncontent", "cli_test")
	if err != nil {
		t.Fatalf("create first entry: %v", err)
	}
	second, err := adkAgent.CreateScratchpad(agentDir, "same\ncontent", "cli_test")
	if err != nil {
		t.Fatalf("create second entry: %v", err)
	}
	t.Chdir(wsDir)

	out, err := captureStdout(t, func() error {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "diffagent", "scratchpad", "diff", first.ID, second.ID})
		return RootCmd.Execute()
	})
	if err != nil {
		t.Fatalf("scratchpad diff: %v", err)
	}
	if out != "" {
		t.Errorf("identical entries should print nothing, got %q", out)
	}
}

// The typed error has to survive all the way out of RootCmd.Execute, because that is what the
// exit-2 mapping in Execute keys on.
func TestScratchpadDiffCLIUnknownEntryReportsTypedError(t *testing.T) {
	wsDir, _, afterID := newCLIDiffWorkspace(t)

	_, err := captureStdout(t, func() error {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "diffagent", "scratchpad", "diff", "zzzz", afterID})
		return RootCmd.Execute()
	})

	var notFound *adkAgent.ScratchpadEntryNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ScratchpadEntryNotFoundError out of Execute, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "zzzz") {
		t.Errorf("expected the error to name the bad ID, got %q", err.Error())
	}
}

func TestScratchpadDiffCLIRequiresEveryID(t *testing.T) {
	wsDir, beforeID, _ := newCLIDiffWorkspace(t)

	_, err := captureStdout(t, func() error {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "diffagent", "scratchpad", "diff", beforeID})
		return RootCmd.Execute()
	})
	if err == nil {
		t.Fatal("expected a usage error when after_id is missing")
	}
	if !strings.Contains(err.Error(), "before_id") {
		t.Errorf("expected the usage error to name the missing arguments, got %q", err.Error())
	}
}
