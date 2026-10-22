package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"google.golang.org/genai"
)

// --- frontmatter helpers ---

func TestSplitMemoryFrontmatter(t *testing.T) {
	t.Run("no frontmatter", func(t *testing.T) {
		mapping, body, present := splitMemoryFrontmatter("# Task Overview\n\nbody")
		if present || mapping != nil || body != "# Task Overview\n\nbody" {
			t.Fatalf("unexpected split: present=%v mapping=%v body=%q", present, mapping, body)
		}
	})
	t.Run("managed frontmatter", func(t *testing.T) {
		mapping, body, present := splitMemoryFrontmatter("---\ncompact-cycle: 3\n---\nbody line")
		if !present || mapping == nil || body != "body line" {
			t.Fatalf("unexpected split: present=%v mapping=%v body=%q", present, mapping, body)
		}
		c, ok := memoryIntFromNode(mapping, memoryCompactCycleKey)
		if !ok || c != 3 {
			t.Fatalf("expected compact-cycle 3, got %d ok=%v", c, ok)
		}
	})
	t.Run("unparseable block leaves content intact", func(t *testing.T) {
		content := "---\nnot: [valid yaml\n---\nbody"
		_, body, present := splitMemoryFrontmatter(content)
		if present || body != content {
			t.Fatalf("unexpected split: present=%v body=%q", present, body)
		}
	})
	t.Run("non-mapping document leaves content intact", func(t *testing.T) {
		content := "---\n- just\n- a list\n---\nbody"
		_, body, present := splitMemoryFrontmatter(content)
		if present || body != content {
			t.Fatalf("unexpected split: present=%v body=%q", present, body)
		}
	})
}

func TestMemoryCycleOf(t *testing.T) {
	if c := memoryCycleOf("# legacy doc\n\nno frontmatter"); c != 0 {
		t.Fatalf("legacy doc should read as cycle 0, got %d", c)
	}
	if c := memoryCycleOf("---\ncompact-cycle: 7\n---\nbody"); c != 7 {
		t.Fatalf("expected cycle 7, got %d", c)
	}
	if c := memoryCycleOf("---\ncompact-cycle: not-a-number\n---\nbody"); c != 0 {
		t.Fatalf("non-numeric cycle should read as 0, got %d", c)
	}
}

// --- writers ---

func TestApplyMinorAppend(t *testing.T) {
	t.Run("creates ledger section on legacy doc", func(t *testing.T) {
		in := "# Task Overview\n\nSome narrative."
		out := applyMinorAppend(in, "* bullet one\n* bullet two", 1)
		for _, want := range []string{"compact-cycle: 1", "# Task Overview", "Some narrative.", "## Ledger Addendum", "* bullet one", "* bullet two"} {
			if !strings.Contains(out, want) {
				t.Errorf("minor append missing %q, got:\n%s", want, out)
			}
		}
	})
	t.Run("appends under existing ledger and bumps cycle", func(t *testing.T) {
		in := "---\ncompact-cycle: 4\n---\n# Task Overview\n\ntext\n\n## Ledger Addendum\n* old bullet"
		out := applyMinorAppend(in, "* new bullet", 5)
		if !strings.Contains(out, "* old bullet\n* new bullet") {
			t.Errorf("expected new bullet under existing ledger, got:\n%s", out)
		}
		if strings.Contains(out, "compact-cycle: 4") {
			t.Errorf("old cycle counter should be replaced, got:\n%s", out)
		}
		if !strings.Contains(out, "compact-cycle: 5") {
			t.Errorf("expected cycle 5, got:\n%s", out)
		}
		if strings.Contains(out, "last-major-cycle") {
			t.Errorf("minor pass must not set last-major-cycle, got:\n%s", out)
		}
	})
}

func TestApplyMajorRewrite(t *testing.T) {
	modelOut := "# Task Overview\n\nFinal state.\n\n## Current State\n\n- done"
	out := applyMajorRewrite(modelOut, 3)
	for _, want := range []string{"compact-cycle: 3", "last-major-cycle: 3", "Final state.", "- done"} {
		if !strings.Contains(out, want) {
			t.Errorf("major rewrite missing %q, got:\n%s", want, out)
		}
	}
	trimmed := strings.TrimRight(out, "\n")
	if !strings.HasSuffix(trimmed, ledgerAddendumHeading) {
		t.Errorf("expected trailing empty Ledger Addendum, got tail: %q", trimmed[max(0, len(trimmed)-80):])
	}
}

func TestApplyMajorRewritePreservesCompliantTrailer(t *testing.T) {
	modelOut := "# Task Overview\n\nFinal state.\n\n## Ledger Addendum"
	out := applyMajorRewrite(modelOut, 4)
	if n := strings.Count(out, ledgerAddendumHeading); n != 1 {
		t.Fatalf("expected exactly one Ledger Addendum heading, got %d:\n%s", n, out)
	}
	trimmed := strings.TrimRight(out, "\n")
	if !strings.HasSuffix(trimmed, ledgerAddendumHeading) {
		t.Errorf("trailing heading missing, got tail: %q", trimmed[len(trimmed)-80:])
	}
}

// --- pass decision ---

func TestDecideMemoryPass(t *testing.T) {
	runtimeCfg := &RuntimeConfig{ContextWindow: 1000}
	bigDoc := strings.Repeat("x ", 2000) // comfortably past 50% of a 1000-token window

	cases := []struct {
		name      string
		existing  string
		every     int
		pct       float64
		runtime   *RuntimeConfig
		wantMajor bool
	}{
		{"bootstrap: missing file is always minor", "", 1, 0, runtimeCfg, false},
		{"legacy doc, every-N not yet", "# t", 3, 0, runtimeCfg, false},
		{"every-N hit on cycle 3", "---\ncompact-cycle: 2\n---\n# t", 3, 0, runtimeCfg, true},
		{"memory-pct hit", bigDoc, 0, 50, runtimeCfg, true},
		{"memory-pct miss on small doc", "# small", 0, 50, runtimeCfg, false},
		{"both triggers off", bigDoc, 0, 0, runtimeCfg, false},
		{"reset: pass after a major is minor again", "---\ncompact-cycle: 3\nlast-major-cycle: 3\n---\n# t", 1, 0, runtimeCfg, false},
		{"every-N still fires after reset", "---\ncompact-cycle: 2\nlast-major-cycle: 1\n---\n# t", 3, 0, runtimeCfg, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &CompactConfig{MajorCompactEvery: tc.every, MajorCompactMemoryPct: tc.pct}
			if got := decideMemoryPass(tc.existing, cfg, tc.runtime); got != tc.wantMajor {
				t.Fatalf("decideMemoryPass = %v, want %v", got, tc.wantMajor)
			}
		})
	}
}

// --- config parsing ---

func TestParseCompactConfig_MajorKnobs(t *testing.T) {
	t.Run("values parse", func(t *testing.T) {
		content := "---\nappend-only: true\nmajor-compact-every: 3\nmajor-compact-memory-pct: 60\nmajor-compact-overhead-pct: 40\n---\nBase directive."
		cfg, err := ParseCompactConfig(content)
		if err != nil {
			t.Fatalf("ParseCompactConfig failed: %v", err)
		}
		if cfg.MajorCompactEvery != 3 || cfg.MajorCompactMemoryPct != 60 || cfg.MajorCompactOverheadPct != 40 {
			t.Fatalf("unexpected knobs: every=%d pct=%v overhead=%v", cfg.MajorCompactEvery, cfg.MajorCompactMemoryPct, cfg.MajorCompactOverheadPct)
		}
		if cfg.Prompt != "Base directive." {
			t.Fatalf("prompt body lost: %q", cfg.Prompt)
		}
	})
	t.Run("out-of-range values ignored", func(t *testing.T) {
		content := "---\nmajor-compact-every: 0\nmajor-compact-memory-pct: 150\nmajor-compact-overhead-pct: 120\n---\nBase."
		cfg, err := ParseCompactConfig(content)
		if err != nil {
			t.Fatalf("ParseCompactConfig failed: %v", err)
		}
		if cfg.MajorCompactEvery != 0 || cfg.MajorCompactMemoryPct != 0 {
			t.Fatalf("invalid every/pct should stay 0: every=%d pct=%v", cfg.MajorCompactEvery, cfg.MajorCompactMemoryPct)
		}
		if cfg.MajorCompactOverheadPct != DefaultMajorCompactOverheadPct {
			t.Fatalf("invalid overhead should fall back to default, got %v", cfg.MajorCompactOverheadPct)
		}
	})
	t.Run("defaults without keys", func(t *testing.T) {
		cfg, err := ParseCompactConfig("---\nappend-only: true\n---\nBase.")
		if err != nil {
			t.Fatalf("ParseCompactConfig failed: %v", err)
		}
		if cfg.MajorCompactEvery != 0 || cfg.MajorCompactMemoryPct != 0 || cfg.MajorCompactOverheadPct != DefaultMajorCompactOverheadPct {
			t.Fatalf("unexpected defaults: every=%d pct=%v overhead=%v", cfg.MajorCompactEvery, cfg.MajorCompactMemoryPct, cfg.MajorCompactOverheadPct)
		}
	})
}

// --- directive rendering ---

func TestRenderCompactionDirective(t *testing.T) {
	minor, err := renderCompactionDirective("Base prompt.", false)
	if err != nil {
		t.Fatalf("render minor failed: %v", err)
	}
	if !strings.HasPrefix(minor, "Base prompt.") {
		t.Errorf("base directive must lead the rendered prompt, got:\n%s", minor)
	}
	if !strings.Contains(minor, "PASS MODE: MINOR - APPEND") {
		t.Errorf("minor marker missing, got:\n%s", minor)
	}
	major, err := renderCompactionDirective("Base prompt.", true)
	if err != nil {
		t.Fatalf("render major failed: %v", err)
	}
	if !strings.Contains(major, "PASS MODE: MAJOR - FULL CONSOLIDATION REWRITE") {
		t.Errorf("major marker missing, got:\n%s", major)
	}
	if strings.Contains(major, "PASS MODE: MINOR") {
		t.Errorf("major directive must not carry the minor mode, got:\n%s", major)
	}
}

// --- end-to-end flow ---

// runHybridCompaction builds a temp workspace with the given COMPACT.md and
// optional existing MEMORY.md, runs a forced compaction against a canned
// OpenAI response, and returns the agent dir plus the raw HTTP request body
// (for prompt assertions). Mirrors the TestCompactionGitTwoCommitsD73 harness.
func runHybridCompaction(t *testing.T, compactMD, existingMemory, summary string) (string, string) {
	t.Helper()
	wsDir := t.TempDir()
	agentID := "hybrid"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := InitAgentGit(wsDir, agentID); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "COMPACT.md"), []byte(compactMD), 0644); err != nil {
		t.Fatalf("failed to write COMPACT.md: %v", err)
	}
	if existingMemory != "" {
		if err := WriteMemoryFile(agentDir, existingMemory); err != nil {
			t.Fatalf("failed to write MEMORY.md: %v", err)
		}
	}
	turns := []*genai.Content{
		genai.NewContentFromText("Turn 1 user", "user"),
		genai.NewContentFromText("Turn 1 assistant", "model"),
		genai.NewContentFromText("Turn 2 user", "user"),
		genai.NewContentFromText("Turn 2 assistant", "model"),
	}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("failed to write session turns: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err != nil {
		t.Fatalf("initial CommitWorkspaceEvent failed: %v", err)
	}

	content, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("failed to marshal summary: %v", err)
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`, content)
	}))
	t.Cleanup(srv.Close)

	runtimeCfg := &RuntimeConfig{Provider: "openai", Model: "test-model", Endpoint: srv.URL, ContextWindow: 100}
	adkAgent := mustBuildTestADKAgent(t, agentDir, "Prompt", runtimeCfg, NewOpenAIModel(runtimeCfg))

	compacted, err := CheckAndCompactSession(context.Background(), agentDir, runtimeCfg, adkAgent, true, nil, nil)
	if err != nil {
		t.Fatalf("CheckAndCompactSession failed: %v", err)
	}
	if !compacted {
		t.Fatalf("expected compaction to occur")
	}
	return agentDir, got
}

// headCommitMessages returns the most recent n commit messages, newest first.
func headCommitMessages(t *testing.T, agentDir string, n int) []string {
	t.Helper()
	repo, err := git.PlainOpen(agentDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	headRef, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	iter, err := repo.Log(&git.LogOptions{From: headRef.Hash()})
	if err != nil {
		t.Fatalf("failed to get commit log: %v", err)
	}
	var msgs []string
	err = iter.ForEach(func(c *object.Commit) error {
		msgs = append(msgs, c.Message)
		return nil
	})
	if err != nil {
		t.Fatalf("failed iterating commits: %v", err)
	}
	if len(msgs) > n {
		msgs = msgs[:n]
	}
	return msgs
}

func TestCompactionHybrid_MinorAppendCycle(t *testing.T) {
	compactMD := "---\nappend-only: true\nmajor-compact-every: 5\nmajor-compact-memory-pct: 90\n---\nBase directive text."
	existing := "# Task Overview\n\nSome narrative."
	agentDir, req := runHybridCompaction(t, compactMD, existing, "* did a thing\n* learned a fact")

	if !strings.Contains(req, "Base directive text.") || !strings.Contains(req, "PASS MODE: MINOR - APPEND") {
		t.Errorf("expected minor-mode directive, got: %s", req)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("failed to read MEMORY.md: %v", err)
	}
	for _, want := range []string{"compact-cycle: 1", "# Task Overview", "Some narrative.", "## Ledger Addendum", "* did a thing", "* learned a fact"} {
		if !strings.Contains(mem, want) {
			t.Errorf("MEMORY.md missing %q, got:\n%s", want, mem)
		}
	}
	if strings.Contains(mem, "last-major-cycle") {
		t.Errorf("minor pass must not set last-major-cycle, got:\n%s", mem)
	}

	msgs := headCommitMessages(t, agentDir, 2)
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 commits, got %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[1], "compact (memory) [minor cycle 1]") {
		t.Errorf("expected minor audit label, got: %q", msgs[1])
	}
}

func TestCompactionHybrid_MajorRewriteCycle(t *testing.T) {
	compactMD := "---\nappend-only: true\nmajor-compact-every: 3\n---\nBase directive text."
	existing := "---\ncompact-cycle: 2\n---\n# Task Overview\n\nOld narrative to be dropped."
	summary := "# Task Overview\n\nFinal state.\n\n## Current State\n\n- done\n\n## Key Decisions & Discoveries\n\n- none\n\n## Next Steps\n\n- none\n\n## Context to Preserve\n\n- none\n"
	agentDir, req := runHybridCompaction(t, compactMD, existing, summary)

	if !strings.Contains(req, "PASS MODE: MAJOR - FULL CONSOLIDATION REWRITE") {
		t.Errorf("expected major-mode directive, got: %s", req)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("failed to read MEMORY.md: %v", err)
	}
	for _, want := range []string{"compact-cycle: 3", "last-major-cycle: 3", "Final state.", "- done"} {
		if !strings.Contains(mem, want) {
			t.Errorf("MEMORY.md missing %q, got:\n%s", want, mem)
		}
	}
	if strings.Contains(mem, "Old narrative to be dropped.") {
		t.Errorf("major pass should replace the old document, got:\n%s", mem)
	}
	trimmed := strings.TrimRight(mem, "\n")
	if !strings.HasSuffix(trimmed, ledgerAddendumHeading) {
		t.Errorf("expected trailing empty Ledger Addendum, got tail: %q", trimmed[len(trimmed)-80:])
	}

	msgs := headCommitMessages(t, agentDir, 2)
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 commits, got %d", len(msgs))
	}
	if !strings.Contains(msgs[1], "[major cycle 3, full rewrite: ") || !strings.Contains(msgs[1], " bytes]") {
		t.Errorf("expected major audit label with byte sizes, got: %q", msgs[1])
	}
}

func TestCompactionHybrid_BootstrapAlwaysMinor(t *testing.T) {
	// every:1 would major-rewrite every cycle if the bootstrap rule did not
	// hold: the first compaction on a nonexistent MEMORY.md is always minor.
	compactMD := "---\nappend-only: true\nmajor-compact-every: 1\n---\nBase directive text."
	agentDir, req := runHybridCompaction(t, compactMD, "", "* boot bullet")

	if !strings.Contains(req, "PASS MODE: MINOR - APPEND") {
		t.Errorf("expected minor-mode directive on bootstrap, got: %s", req)
	}

	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		t.Fatalf("failed to read MEMORY.md: %v", err)
	}
	for _, want := range []string{"compact-cycle: 1", "## Ledger Addendum", "* boot bullet"} {
		if !strings.Contains(mem, want) {
			t.Errorf("MEMORY.md missing %q, got:\n%s", want, mem)
		}
	}
	if strings.Contains(mem, "last-major-cycle") {
		t.Errorf("bootstrap must not set last-major-cycle, got:\n%s", mem)
	}

	msgs := headCommitMessages(t, agentDir, 2)
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 commits, got %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[1], "compact (memory) [minor cycle 1]") {
		t.Errorf("expected minor audit label, got: %q", msgs[1])
	}
}
