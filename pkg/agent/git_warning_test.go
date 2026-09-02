package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newAgentRepo(t *testing.T) (string, string, string) {
	t.Helper()
	wsDir := t.TempDir()
	agentID := "hooper"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := InitAgentGit(wsDir, agentID); err != nil {
		t.Fatalf("InitAgentGit: %v", err)
	}
	return wsDir, agentID, agentDir
}

// capture redirects failure output for the duration of the test.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := gitWarnOut
	gitWarnOut = buf
	t.Cleanup(func() { gitWarnOut = prev })
	return buf
}

func poisonConfig(t *testing.T, agentDir, branch string) {
	t.Helper()
	cfgPath := filepath.Join(agentDir, ".git", "config")
	good, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// Left behind by `gh pr checkout`, and enough for go-git to refuse the whole repository.
	bad := append(good, []byte("\n[branch \""+branch+"\"]\n\tremote = https://example.invalid/repo.git\n\tmerge = refs/pull/1/head\n")...)
	if err := os.WriteFile(cfgPath, bad, 0644); err != nil {
		t.Fatalf("write poisoned config: %v", err)
	}
	t.Cleanup(func() { os.WriteFile(cfgPath, good, 0644) })
}

// A repository go-git cannot open has to become visible, because harness callers discard the error.
func TestCommitWorkspaceEvent_ReportsSilentFailureOnce(t *testing.T) {
	wsDir, agentID, agentDir := newAgentRepo(t)
	poisonConfig(t, agentDir, "broken")

	out := capture(t)

	for i := 1; i <= 3; i++ {
		if err := CommitWorkspaceEvent(wsDir, agentID, "tool call (bash)"); err == nil {
			t.Fatalf("attempt %d: expected failure against an unopenable repo", i)
		}
	}
	msg := out.String()
	if n := strings.Count(msg, "Warning: workspace git commit failed"); n != 1 {
		t.Fatalf("expected exactly 1 warning across 3 failures, got %d:\n%s", n, msg)
	}
	if !strings.Contains(msg, "history is not being recorded") {
		t.Fatalf("warning does not name the consequence:\n%s", msg)
	}
	// The message has to name the repository that failed, not the workspace root beside it.
	if !strings.Contains(msg, agentDir) {
		t.Fatalf("warning should name the failing repo %q:\n%s", agentDir, msg)
	}
}

// Removing a repository is a configuration change, not a repair, so it must not be read as recovery.
func TestCommitWorkspaceEvent_NoFalseRecoveryWhenRepoRemoved(t *testing.T) {
	wsDir, agentID, agentDir := newAgentRepo(t)
	poisonConfig(t, agentDir, "broken")

	out := capture(t)

	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err == nil {
		t.Fatal("expected failure against an unopenable repo")
	}
	if err := os.RemoveAll(filepath.Join(agentDir, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err != nil {
		t.Fatalf("without a repo this should be a no-op: %v", err)
	}
	if strings.Contains(out.String(), "working again") {
		t.Fatalf("claimed recovery for a removed repo:\n%s", out.String())
	}
}

// Fixing one cause while another remains has to speak up again instead of repeating stale text.
func TestCommitWorkspaceEvent_ReportsWhenReasonChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	wsDir, agentID, agentDir := newAgentRepo(t)
	cfgPath := filepath.Join(agentDir, ".git", "config")
	good, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte("hi"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, agentDir, "add", "-f", "notes.txt")
	runGit(t, agentDir, "commit", "-m", "base")

	out := capture(t)

	// First cause: go-git refuses to open the repository.
	if err := os.WriteFile(cfgPath, append(append([]byte{}, good...), []byte("\n[branch \"broken\"]\n\tremote = https://example.invalid/repo.git\n\tmerge = refs/pull/1/head\n")...), 0644); err != nil {
		t.Fatalf("poison config: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err == nil {
		t.Fatal("expected the poisoned config to fail")
	}

	// Second cause, same repository: invalid config syntax so error text changes.
	bad2 := append(append([]byte{}, good...), []byte("\n[broken section\n\tfoo = bar\n")...)
	if err := os.WriteFile(cfgPath, bad2, 0644); err != nil {
		t.Fatalf("poison config 2: %v", err)
	}

	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err == nil {
		t.Fatal("expected second poisoned config to fail")
	}

	if n := strings.Count(out.String(), "Warning: workspace git commit failed"); n != 2 {
		t.Fatalf("expected a warning per distinct cause, got %d:\n%s", n, out.String())
	}
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err == nil {
		t.Fatal("expected second poisoned config to keep failing")
	}
	if n := strings.Count(out.String(), "Warning: workspace git commit failed"); n != 2 {
		t.Fatalf("repeat of the same cause should stay quiet, got %d:\n%s", n, out.String())
	}
}

// Gitlinks in the index (from submodules or nested clones) must not block staging or commit.
func TestCommitWorkspaceEvent_GitlinkSurvivesStaging(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	wsDir, agentID, agentDir := newAgentRepo(t)

	if err := os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte("hi"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, agentDir, "add", "-f", "notes.txt")
	runGit(t, agentDir, "commit", "-m", "base")

	// An accidental `git add` of a clone records mode 160000 with no .gitmodules.
	embedded := filepath.Join(agentDir, "embedded")
	if err := os.MkdirAll(embedded, 0755); err != nil {
		t.Fatalf("embedded dir: %v", err)
	}
	runGit(t, embedded, "init")
	if err := os.WriteFile(filepath.Join(embedded, "inner.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("inner file: %v", err)
	}
	runGit(t, embedded, "add", "inner.txt")
	runGit(t, embedded, "commit", "-m", "inner")
	runGit(t, agentDir, "add", "-f", "embedded") // force past the D35 exclude-everything gitignore
	// so the repository is left holding a gitlink in its index, the state that matters.

	// Write an updated file to be committed
	if err := os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte("hi updated"), 0644); err != nil {
		t.Fatalf("update file: %v", err)
	}

	out := capture(t)

	for i := 1; i <= 2; i++ {
		if err := CommitWorkspaceEvent(wsDir, agentID, "tool call (bash)"); err != nil {
			t.Fatalf("attempt %d: expected staging to succeed with gitlink, got: %v", i, err)
		}
	}

	if out.Len() != 0 {
		t.Fatalf("expected no warnings on success, got:\n%s", out.String())
	}

	// Verify commit succeeded and does not include the gitlink as content
	repo, err := git.PlainOpen(agentDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("lookup HEAD: %v", err)
	}
	cObj, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	tree, err := cObj.Tree()
	if err != nil {
		t.Fatalf("commit tree: %v", err)
	}

	// Embedded directory content must not be present in the tree
	if _, err := tree.File("embedded/inner.txt"); err == nil {
		t.Fatalf("commit tree must not contain gitlink content 'embedded/inner.txt'")
	}

	// Any entry named embedded in the tree must have submodule mode
	for _, entry := range tree.Entries {
		if entry.Name == "embedded" && entry.Mode != filemode.Submodule {
			t.Fatalf("expected embedded entry to have submodule mode, got: %v", entry.Mode)
		}
	}

	// notes.txt must be committed with updated content
	notesFile, err := tree.File("notes.txt")
	if err != nil {
		t.Fatalf("missing notes.txt in commit tree: %v", err)
	}
	content, err := notesFile.Contents()
	if err != nil {
		t.Fatalf("read notes.txt contents: %v", err)
	}
	if content != "hi updated" {
		t.Fatalf("expected 'hi updated', got: %q", content)
	}
}

// Recovery is reported once, and a healthy repository stays quiet afterwards.
func TestCommitWorkspaceEvent_ReportsRecoveryOnce(t *testing.T) {
	wsDir, agentID, agentDir := newAgentRepo(t)
	cfgPath := filepath.Join(agentDir, ".git", "config")
	good, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	poisonConfig(t, agentDir, "broken")

	out := capture(t)

	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err == nil {
		t.Fatal("expected failure against an unopenable repo")
	}
	if err := os.WriteFile(cfgPath, good, 0644); err != nil {
		t.Fatalf("restore config: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err != nil {
		t.Fatalf("after repair: %v", err)
	}
	if n := strings.Count(out.String(), "working again"); n != 1 {
		t.Fatalf("expected 1 recovery notice, got %d:\n%s", n, out.String())
	}

	quiet := out.String()
	if err := CommitWorkspaceEvent(wsDir, agentID, "assistant"); err != nil {
		t.Fatalf("healthy commit: %v", err)
	}
	if out.String() != quiet {
		t.Fatalf("healthy repo added output:\n%s", out.String()[len(quiet):])
	}
}

// With no repository at all, versioning is simply off, which is not worth reporting.
func TestCommitWorkspaceEvent_SilentWithoutRepo(t *testing.T) {
	wsDir := t.TempDir()
	out := capture(t)

	if err := CommitWorkspaceEvent(wsDir, "hooper", "user"); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected silence without a repo, got: %s", out.String())
	}
}

// TraceAgentCommit reports a warning when PlainOpen fails on a repository that exists.
func TestTrace_ReportsPlainOpenFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	wsDir, agentID, agentDir := newAgentRepo(t)
	if err := os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte("hi"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, agentDir, "add", "-f", "notes.txt")
	runGit(t, agentDir, "commit", "-m", "base")

	repo, err := git.PlainOpen(agentDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("lookup HEAD: %v", err)
	}
	commitSHA := head.Hash().String()

	poisonConfig(t, agentDir, "broken")

	out := capture(t)

	res, err := TraceAgentCommit(wsDir, agentID, commitSHA, TraceOptions{})
	if err != nil {
		t.Fatalf("expected TraceAgentCommit to return partial result, got err: %v", err)
	}
	if len(res.Steps) != 0 {
		t.Fatalf("expected 0 steps because PlainOpen failed, got %d", len(res.Steps))
	}

	msg := out.String()
	if !strings.Contains(msg, "Warning: workspace git trace failed for "+agentDir) {
		t.Fatalf("expected trace warning for %s, got:\n%s", agentDir, msg)
	}

	// Un-poison config: successful trace must clear the failure latch
	cfgPath := filepath.Join(agentDir, ".git", "config")
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cleaned := bytes.ReplaceAll(cfgBytes, []byte("\n[branch \"broken\"]\n\tremote = https://example.invalid/repo.git\n\tmerge = refs/pull/1/head\n"), nil)
	if err := os.WriteFile(cfgPath, cleaned, 0644); err != nil {
		t.Fatalf("restore config: %v", err)
	}

	res, err = TraceAgentCommit(wsDir, agentID, commitSHA, TraceOptions{})
	if err != nil {
		t.Fatalf("expected successful trace after repair, got err: %v", err)
	}
	if len(res.Steps) == 0 {
		t.Fatal("expected trace steps after repair")
	}
	if !strings.Contains(out.String(), "Warning: workspace git trace working again for "+agentDir) {
		t.Fatalf("expected recovery message for %s, got:\n%s", agentDir, out.String())
	}
}

// TraceByTraceID reports a warning when PlainOpen fails while searching across agent repositories.
func TestTrace_ReportsSearchPlainOpenFailure(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}

	// 1. Agent "alice" has a good repo with a commit containing a trace_id
	if err := InitAgentGit(wsDir, "alice"); err != nil {
		t.Fatalf("init alice: %v", err)
	}
	aliceDir := filepath.Join(wsDir, "alice")
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"user","trace_id":"trace-xyz"}`)
	if err := AppendSessionTurn(aliceDir, "user", "prompt"); err != nil {
		t.Fatalf("append turn: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "alice", "user"); err != nil {
		t.Fatalf("commit alice: %v", err)
	}

	// 2. Agent "bob" has a repo that exists but cannot be opened
	if err := InitAgentGit(wsDir, "bob"); err != nil {
		t.Fatalf("init bob: %v", err)
	}
	bobDir := filepath.Join(wsDir, "bob")
	if err := os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("# bob\n"), 0644); err != nil {
		t.Fatalf("write bob AGENTS.md: %v", err)
	}
	poisonConfig(t, bobDir, "broken")

	out := capture(t)

	// TraceByTraceID will search both alice and bob
	res, err := TraceByTraceID(wsDir, "trace-xyz", TraceOptions{})
	if err != nil {
		t.Fatalf("TraceByTraceID failed: %v", err)
	}
	if len(res.Steps) == 0 {
		t.Fatal("expected steps from alice")
	}

	msg := out.String()
	if !strings.Contains(msg, "Warning: workspace git trace-search failed for "+bobDir) {
		t.Fatalf("expected trace-search warning for %s, got:\n%s", bobDir, msg)
	}

	// Un-poison bob's config: next search must clear the failure latch
	bobCfgPath := filepath.Join(bobDir, ".git", "config")
	cfgBytes, err := os.ReadFile(bobCfgPath)
	if err != nil {
		t.Fatalf("read bob config: %v", err)
	}
	cleaned := bytes.ReplaceAll(cfgBytes, []byte("\n[branch \"broken\"]\n\tremote = https://example.invalid/repo.git\n\tmerge = refs/pull/1/head\n"), nil)
	if err := os.WriteFile(bobCfgPath, cleaned, 0644); err != nil {
		t.Fatalf("restore bob config: %v", err)
	}

	res, err = TraceByTraceID(wsDir, "trace-xyz", TraceOptions{})
	if err != nil {
		t.Fatalf("TraceByTraceID failed after repair: %v", err)
	}
	if !strings.Contains(out.String(), "Warning: workspace git trace-search working again for "+bobDir) {
		t.Fatalf("expected trace-search recovery message for %s, got:\n%s", bobDir, out.String())
	}
}

// File-backed .git repositories (.git as regular file) are detected, protected during init, and reported on error.
func TestIsWorkspaceGitRepo_FileBacked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	tempDir := t.TempDir()
	repoDir := filepath.Join(tempDir, "agent")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	if IsWorkspaceGitRepo(repoDir) {
		t.Fatal("expected false before .git creation")
	}

	asideDir := filepath.Join(tempDir, "gitdir_aside")
	runGit(t, tempDir, "init", "--bare", asideDir)

	gitFilePath := filepath.Join(repoDir, ".git")
	if err := os.WriteFile(gitFilePath, []byte("gitdir: "+asideDir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if !IsWorkspaceGitRepo(repoDir) {
		t.Fatal("expected true for file-backed .git repository")
	}

	if err := InitAgentGit(tempDir, "agent"); err != nil {
		t.Fatalf("InitAgentGit failed on existing file-backed repo: %v", err)
	}
	fi, err := os.Stat(gitFilePath)
	if err != nil || fi.IsDir() {
		t.Fatalf(".git was converted or removed: %v", err)
	}

	brokenDir := filepath.Join(tempDir, "broken_agent")
	if err := os.MkdirAll(brokenDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, ".git"), []byte("gitdir: /nonexistent/gitdir\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !IsWorkspaceGitRepo(brokenDir) {
		t.Fatal("expected true for broken file-backed repo")
	}

	out := capture(t)
	if err := CommitWorkspaceEvent(tempDir, "broken_agent", "user"); err == nil {
		t.Fatal("expected commit failure on broken file-backed repo")
	}
	if !strings.Contains(out.String(), "Warning: workspace git commit failed for "+brokenDir) {
		t.Fatalf("expected warning for broken file-backed repo, got:\n%s", out.String())
	}
}
