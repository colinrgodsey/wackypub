package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func now2026() time.Time {
	return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
}

// mustInitGitRepo creates a git repo at path with one real commit. Real
// commits matter: git show-ref --verify resolves the ref to an object, so a
// dangling ref (which go-git happily writes) reads as a git error, not as a
// missing ref.
func mustInitGitRepo(t *testing.T, path string) (*git.Repository, plumbing.Hash) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("failed to create repo dir: %v", err)
	}
	repo, err := git.PlainInit(path, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	if _, err := w.Add("f.txt"); err != nil {
		t.Fatalf("failed to stage file: %v", err)
	}
	hash, err := w.Commit("init", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
	return repo, hash
}

func mustSetRef(t *testing.T, repo *git.Repository, name string, hash plumbing.Hash) {
	t.Helper()
	if err := repo.Storer.SetReference(plumbing.NewReferenceFromStrings(name, hash.String())); err != nil {
		t.Fatalf("failed to set ref %s: %v", name, err)
	}
}

func mustWriteKB(t *testing.T, wsDir, slug, status string) {
	t.Helper()
	dir := filepath.Join(wsDir, ".kb", "workspaces", "main")
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(slug)), 0755); err != nil {
		t.Fatalf("failed to create KB dir: %v", err)
	}
	doc := "---\nid: test\nslug: " + slug + "\nstatus: " + status + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(doc), 0644); err != nil {
		t.Fatalf("failed to write KB doc: %v", err)
	}
}

const claimDoc = "# Task Overview\n\nsome narrative line\n\n## Claims\n%s\n\n## Notes\nmore narrative\n"

func TestParseClaimLine(t *testing.T) {
	cases := []struct {
		in     string
		kind   claimKind
		value  string
		repo   string
		asOf   string
		parsed bool
	}{
		{"- card:tasks/wackypub/foo as-of 2026-10-21", claimKindCard, "tasks/wackypub/foo", "", "2026-10-21", true},
		{"- branch:phineas/foo@/home/u/repo as-of 2026-10-21", claimKindBranch, "phineas/foo", "/home/u/repo", "2026-10-21", true},
		{"- branch:a/b/c@/r/p as-of 2026-01-02", claimKindBranch, "a/b/c", "/r/p", "2026-01-02", true},
		{"- pr:119@/home/u/repo as-of 2026-10-21", claimKindPR, "119", "/home/u/repo", "2026-10-21", true},
		{"- worktree:/tmp/wpflock as-of 2026-10-21", claimKindWorktree, "/tmp/wpflock", "", "2026-10-21", true},
		{"- card:foo as-of 2026-10-21 trailing", claimKindCard, "", "", "", false},
		{"- card:foo", claimKindCard, "", "", "", false},
		{"- pr:abc@/r as-of 2026-10-21", claimKindPR, "", "", "", false},
		{"- todo:fix the thing as-of 2026-10-21", claimKindCard, "", "", "", false},
		{"- worktree:/tmp/lock as-of 26-10-21", claimKindWorktree, "", "", "", false},
	}
	for _, c := range cases {
		claim, ok := parseClaimLine(c.in)
		if c.kind == claimKindCard && c.repo == "" && c.value == "" && c.parsed {
			t.Fatalf("test case malformed: %s", c.in)
		}
		if ok != c.parsed {
			t.Errorf("parseClaimLine(%q) ok = %v, want %v", c.in, ok, c.parsed)
			continue
		}
		if !ok {
			continue
		}
		if claim.Kind != c.kind || claim.Value != c.value || claim.Repo != c.repo || claim.AsOf != c.asOf {
			t.Errorf("parseClaimLine(%q) = %+v, want kind=%s value=%s repo=%s asOf=%s", c.in, claim, c.kind, c.value, c.repo, c.asOf)
		}
	}
}

func TestReconcile_CardClaim(t *testing.T) {
	wsDir := t.TempDir()
	mustWriteKB(t, wsDir, "tasks/open-card", "active")
	mustWriteKB(t, wsDir, "tasks/closed-card", "completed")

	mem := fmt.Sprintf(claimDoc, "- card:tasks/open-card as-of 2026-10-20\n- card:tasks/closed-card as-of 2026-10-20\n- card:tasks/gone-card as-of 2026-10-20")
	out, rep, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	if rep.ClaimCount != 3 || rep.Verified != 1 || rep.Stale != 2 {
		t.Fatalf("report = %+v, want 3 claims, 1 verified, 2 stale", rep)
	}
	if !strings.Contains(out, "- card:tasks/open-card as-of 2026-10-20") {
		t.Errorf("verified claim should stay in place, got:\n%s", out)
	}
	if strings.Contains(out, "\n- card:tasks/closed-card as-of 2026-10-20\n") && !strings.Contains(out, "[STALE 2026-10-21: KB status=completed]") {
		t.Errorf("closed card should be annotated stale, got:\n%s", out)
	}
	if !strings.Contains(out, "[STALE 2026-10-21: KB status=completed]") {
		t.Errorf("missing closed-card stale annotation, got:\n%s", out)
	}
	if !strings.Contains(out, "[STALE 2026-10-21: card not found in KB]") {
		t.Errorf("missing gone-card stale annotation, got:\n%s", out)
	}
	// stale lines move to Resolved, out of the Claims section
	claimsPart := out[strings.Index(out, "## Claims"):strings.Index(out, "## Notes")]
	if strings.Contains(claimsPart, "closed-card") || strings.Contains(claimsPart, "gone-card") {
		t.Errorf("stale claims should leave the Claims section, got:\n%s", claimsPart)
	}
	if !strings.Contains(out, "## Resolved") {
		t.Errorf("expected a Resolved section to be created, got:\n%s", out)
	}
}

func TestReconcile_BranchClaim(t *testing.T) {
	wsDir := t.TempDir()
	repoDir := filepath.Join(wsDir, "repo")
	repo, h := mustInitGitRepo(t, repoDir)
	mustSetRef(t, repo, "refs/heads/feature-x", h)
	mustSetRef(t, repo, "refs/remotes/origin/remote-only-branch", h)

	mem := fmt.Sprintf(claimDoc, "- branch:feature-x@"+repoDir+" as-of 2026-10-20\n- branch:remote-only-branch@"+repoDir+" as-of 2026-10-20\n- branch:absent-branch@"+repoDir+" as-of 2026-10-20\n- branch:feature-x@/no/such/repo as-of 2026-10-20")
	out, rep, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	// local branch + remote-tracking branch verify; absent branch and broken repo
	// are unverifiable, NEVER stale (the remote is the authority).
	if rep.ClaimCount != 4 || rep.Verified != 2 || rep.Unverifiable != 2 || rep.Stale != 0 {
		t.Fatalf("report = %+v, want 4 claims, 2 verified, 2 unverifiable, 0 stale", rep)
	}
	if out != mem {
		t.Errorf("no stale claims means the document must be byte-identical\n--- got:\n%s\n--- want:\n%s", out, mem)
	}
}

func TestReconcile_PRClaim(t *testing.T) {
	wsDir := t.TempDir()
	repoDir := filepath.Join(wsDir, "repo")
	repo, h := mustInitGitRepo(t, repoDir)
	mustSetRef(t, repo, "refs/pull/7/head", h)

	mem := fmt.Sprintf(claimDoc, "- pr:7@"+repoDir+" as-of 2026-10-20\n- pr:8@"+repoDir+" as-of 2026-10-20")
	out, rep, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	if rep.ClaimCount != 2 || rep.Verified != 1 || rep.Unverifiable != 1 || rep.Stale != 0 {
		t.Fatalf("report = %+v, want 2 claims, 1 verified, 1 unverifiable, 0 stale", rep)
	}
	if out != mem {
		t.Errorf("no stale claims means the document must be byte-identical")
	}
}

func TestReconcile_WorktreeClaim(t *testing.T) {
	wsDir := t.TempDir()
	live := filepath.Join(wsDir, "live-worktree")
	if err := os.MkdirAll(live, 0755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	mem := fmt.Sprintf(claimDoc, "- worktree:"+live+" as-of 2026-10-20\n- worktree:/no/such/worktree as-of 2026-10-20")
	out, rep, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	if rep.ClaimCount != 2 || rep.Verified != 1 || rep.Stale != 1 {
		t.Fatalf("report = %+v, want 2 claims, 1 verified, 1 stale", rep)
	}
	if !strings.Contains(out, "[STALE 2026-10-21: path absent]") {
		t.Errorf("missing path-absent annotation, got:\n%s", out)
	}
}

func TestReconcile_IdempotentAndExistingResolved(t *testing.T) {
	wsDir := t.TempDir()
	mustWriteKB(t, wsDir, "tasks/closed-card", "completed")
	mem := "# Task Overview\n\n## Claims\n- card:tasks/closed-card as-of 2026-10-20\n\n## Resolved\n- old resolved line\n"
	out1, rep1, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	if rep1.Stale != 1 {
		t.Fatalf("expected 1 stale, got %+v", rep1)
	}
	// existing Resolved section gets the stale line appended inside it, before
	// whatever follows the section
	out2, rep2, err := ReconcileMemoryClaims(out1, wsDir, now2026())
	if err != nil {
		t.Fatalf("second pass failed: %v", err)
	}
	if rep2.ClaimCount != 0 || out2 != out1 {
		t.Errorf("second pass must be a no-op on the reconciled document\n--- pass1:\n%s\n--- pass2:\n%s", out1, out2)
	}
	resolvedIdx := strings.Index(out1, "## Resolved")
	if resolvedIdx < 0 {
		t.Fatalf("missing Resolved section:\n%s", out1)
	}
	resolved := out1[resolvedIdx:]
	if !strings.Contains(resolved, "- old resolved line") || !strings.Contains(resolved, "[STALE 2026-10-21: KB status=completed]") {
		t.Errorf("Resolved section should hold both the old line and the new annotation, got:\n%s", resolved)
	}
	if strings.Index(out1, "- old resolved line") > strings.Index(out1, "[STALE 2026-10-21: KB status=completed]") {
		t.Errorf("new stale line should append after existing Resolved content, got:\n%s", out1)
	}
}

func TestReconcile_NoClaimsSectionNoop(t *testing.T) {
	wsDir := t.TempDir()
	mem := "# Task Overview\n\nnothing to reconcile here\n"
	out, rep, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	if out != mem || rep.ClaimCount != 0 {
		t.Errorf("no-Claims document must pass through byte-identical, got:\n%s", out)
	}
}

func TestReconcile_UnparseableUntouched(t *testing.T) {
	wsDir := t.TempDir()
	liveDir := filepath.Join(wsDir, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	mem := fmt.Sprintf(claimDoc, "- card:as-of-missing-line\n- worktree:"+liveDir+" as-of 2026-10-20\n- todo:free-form bullet as-of 2026-10-20")
	out, rep, err := ReconcileMemoryClaims(mem, wsDir, now2026())
	if err != nil {
		t.Fatalf("ReconcileMemoryClaims failed: %v", err)
	}
	if rep.ClaimCount != 1 || len(rep.Unparseable) != 2 {
		t.Fatalf("report = %+v, want 1 claim, 2 unparseable", rep)
	}
	if out != mem {
		t.Errorf("unparseable lines must stay put and nothing is stale, so the document must be byte-identical")
	}
}
