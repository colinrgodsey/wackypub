package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// storedCompactMD plants a COMPACT.md whose every overridable field differs from both the
// built-in defaults and the zero value, so a test can tell "inherited from the agent's own
// document" apart from "silently zeroed by a sparse override".
const storedCompactMD = "---\nappend-only: true\ncompact-pct: 40\ncompact-overhead-pct: 25\ncompaction-notice: \"\"\nmajor-compact-every: 3\n---\n\nStored directive body.\n"

func writeAgentCompactMD(t *testing.T, wsDir, agentID, content string) string {
	t.Helper()
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "COMPACT.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write COMPACT.md: %v", err)
	}
	return agentDir
}

// TestBuildCompactOverrideSendsNothingWithoutFlags pins the no-op path: an invocation that
// names neither flag must send no config_override at all, so the engine keeps reading the
// agent's own COMPACT.md exactly as it does today.
func TestBuildCompactOverrideSendsNothingWithoutFlags(t *testing.T) {
	cov, err := buildCompactOverride(t.TempDir(), "assistant", "", 0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cov != nil {
		t.Fatalf("expected a nil override with no flags set, got %+v", cov)
	}
}

// TestBuildCompactOverridePctKeepsTheStoredDocument is the reason --compact-pct resolves the
// agent's configuration instead of sending a bare percentage: the engine replaces rather than
// merges, so a sparse override would drop append-only mode, the overhead reserve, the notice
// opt-out and the compaction prompt along with the percentage.
func TestBuildCompactOverridePctKeepsTheStoredDocument(t *testing.T) {
	wsDir := t.TempDir()
	writeAgentCompactMD(t, wsDir, "assistant", storedCompactMD)

	cov, err := buildCompactOverride(wsDir, "assistant", "", 100, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cov == nil {
		t.Fatal("expected an override")
	}
	if cov.GetCompactPct() != 100 {
		t.Errorf("pct must be overridden to 100, got %v", cov.GetCompactPct())
	}
	if !cov.GetAppendOnly() {
		t.Error("append-only must be inherited as true from the stored document; false means a sparse override zeroed it")
	}
	if cov.GetCompactOverheadPct() != 25 {
		t.Errorf("overhead pct must be inherited as 25, got %v", cov.GetCompactOverheadPct())
	}
	if cov.GetPrompt() != "Stored directive body." {
		t.Errorf("prompt must be inherited from the stored body, got %q", cov.GetPrompt())
	}
	if cov.GetCompactionNotice() != "" {
		t.Errorf("the stored notice opt-out must survive, got %q", cov.GetCompactionNotice())
	}
}

// TestBuildCompactOverridePctUsesEmbeddedDefaultWhenNoCompactMD covers the agent that has
// never had a COMPACT.md: the percentage must land on top of the embedded default document,
// not on top of a zeroed config with an empty prompt.
func TestBuildCompactOverridePctUsesEmbeddedDefaultWhenNoCompactMD(t *testing.T) {
	restore := adkAgent.DefaultCompactMDHybrid
	adkAgent.DefaultCompactMDHybrid = "---\nappend-only: true\ncompact-pct: 50\n---\n\nBundled directive."
	t.Cleanup(func() { adkAgent.DefaultCompactMDHybrid = restore })

	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, "assistant"), 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}

	cov, err := buildCompactOverride(wsDir, "assistant", "", 30, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cov.GetCompactPct() != 30 {
		t.Errorf("pct must be 30, got %v", cov.GetCompactPct())
	}
	if !cov.GetAppendOnly() {
		t.Error("append-only must come from the embedded default document (true)")
	}
	if cov.GetPrompt() != "Bundled directive." {
		t.Errorf("prompt must be the embedded default body, got %q", cov.GetPrompt())
	}
}

// TestBuildCompactOverrideMdFileAndPct covers the combination: the supplied document governs
// everything it can, and the flag replaces only the percentage inside it.
func TestBuildCompactOverrideMdFileAndPct(t *testing.T) {
	wsDir := t.TempDir()
	mdPath := filepath.Join(t.TempDir(), "COMPACT-alt.md")
	if err := os.WriteFile(mdPath, []byte(storedCompactMD), 0644); err != nil {
		t.Fatalf("write md: %v", err)
	}

	justFile, err := buildCompactOverride(wsDir, "assistant", mdPath, 0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if justFile.GetCompactPct() != 40 {
		t.Errorf("--md-file alone must carry its own pct (40), got %v", justFile.GetCompactPct())
	}

	both, err := buildCompactOverride(wsDir, "assistant", mdPath, 10, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if both.GetCompactPct() != 10 {
		t.Errorf("--compact-pct must win over the md-file pct, got %v", both.GetCompactPct())
	}
	if both.GetPrompt() != "Stored directive body." {
		t.Errorf("the md-file body must still supply the prompt, got %q", both.GetPrompt())
	}
	if both.GetCompactOverheadPct() != 25 {
		t.Errorf("the md-file overhead must survive, got %v", both.GetCompactOverheadPct())
	}
}

// TestBuildCompactOverrideRejectsOutOfRange pins the validation choice: an out-of-range
// percentage is an error rather than a silent fall back to the stored value, because the
// operator typed an explicit number and discarding it quietly is the worse failure.
func TestBuildCompactOverrideRejectsOutOfRange(t *testing.T) {
	wsDir := t.TempDir()
	writeAgentCompactMD(t, wsDir, "assistant", storedCompactMD)

	for _, pct := range []float64{0, -1, 100.5, 500} {
		_, err := buildCompactOverride(wsDir, "assistant", "", pct, true)
		if err == nil {
			t.Fatalf("expected an error for pct %v", pct)
		}
		if !strings.Contains(err.Error(), "--compact-pct") {
			t.Errorf("error for pct %v must name the flag, got %v", pct, err)
		}
	}

	for _, pct := range []float64{1, 50, 100} {
		cov, err := buildCompactOverride(wsDir, "assistant", "", pct, true)
		if err != nil {
			t.Fatalf("pct %v must be accepted, got %v", pct, err)
		}
		if cov.GetCompactPct() != pct {
			t.Errorf("pct %v must pass through unchanged, got %v", pct, cov.GetCompactPct())
		}
	}
}

// TestBuildCompactOverrideValidatesBeforeReadingFiles: an invalid percentage is rejected even
// when a bad --md-file path is also present, so the operator hears about the mistake they made
// on the percentage rather than about a file they did not mistype.
func TestBuildCompactOverrideValidatesBeforeReadingFiles(t *testing.T) {
	_, err := buildCompactOverride(t.TempDir(), "assistant", "/nonexistent/COMPACT.md", 0, true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--compact-pct") {
		t.Errorf("expected the range error to win, got %v", err)
	}
}
