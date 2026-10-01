package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ReadSessionTurns now adapts ReadPersistedTurns. These tests are the drift pin: if either path
// grows its own parser again, the tolerances diverge and the parity assertions stop meaning
// anything, so keep them pointed at one shared parse.
func TestReadSessionTurnsMatchesReadPersistedTurns(t *testing.T) {
	agentDir := t.TempDir()
	lines := strings.Join([]string{
		`{"role":"user","parts":[{"text":"one"}],"seq":7}`,
		"",
		"this line is not json",
		`{"role":"model","parts":[{"text":"two"}],"seq":8}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(agentDir, SessionFileName), []byte(lines), 0644); err != nil {
		t.Fatalf("write session log: %v", err)
	}

	contents, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	persisted, err := ReadPersistedTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadPersistedTurns: %v", err)
	}
	if len(contents) != len(persisted) {
		t.Fatalf("read paths disagree on turn count: %d vs %d", len(contents), len(persisted))
	}
	if len(contents) != 2 {
		t.Fatalf("expected the blank and corrupt lines skipped, got %d turns", len(contents))
	}
	for i := range contents {
		if contents[i] == nil {
			t.Fatalf("turn %d is nil", i)
		}
		if contents[i].Role != persisted[i].Role {
			t.Errorf("turn %d role %q vs %q", i, contents[i].Role, persisted[i].Role)
		}
		if contents[i].Parts[0].Text != persisted[i].Parts[0].Text {
			t.Errorf("turn %d text %q vs %q", i, contents[i].Parts[0].Text, persisted[i].Parts[0].Text)
		}
	}
	if persisted[0].Seq != 7 || persisted[1].Seq != 8 {
		t.Errorf("expected seq preserved on the persisted path, got %d and %d", persisted[0].Seq, persisted[1].Seq)
	}
}

// A caller mutating a returned turn must not rewrite the slice the other reader produced.
func TestReadSessionTurnsReturnsIndependentCopies(t *testing.T) {
	agentDir := t.TempDir()
	line := `{"role":"user","parts":[{"text":"original"}],"seq":1}` + "\n"
	if err := os.WriteFile(filepath.Join(agentDir, SessionFileName), []byte(line), 0644); err != nil {
		t.Fatalf("write session log: %v", err)
	}

	first, err := ReadSessionTurns(agentDir)
	if err != nil || len(first) != 1 {
		t.Fatalf("ReadSessionTurns: %d turns, err %v", len(first), err)
	}
	first[0].Parts[0].Text = "mutated"

	second, err := ReadSessionTurns(agentDir)
	if err != nil || len(second) != 1 {
		t.Fatalf("re-read: %d turns, err %v", len(second), err)
	}
	if second[0].Parts[0].Text != "original" {
		t.Errorf("re-read returned %q, so turns alias mutable state", second[0].Parts[0].Text)
	}
}

func TestReadSessionTurnsMissingFileIsNotAnError(t *testing.T) {
	contents, err := ReadSessionTurns(filepath.Join(t.TempDir(), "nobody"))
	if err != nil {
		t.Fatalf("missing session log must not error: %v", err)
	}
	if contents != nil {
		t.Errorf("expected nil turns for a missing log, got %d", len(contents))
	}
}
