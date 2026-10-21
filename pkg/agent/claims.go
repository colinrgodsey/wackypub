package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Claim line grammar. A dedicated "## Claims" section in MEMORY.md carries
// machine-parseable lines, one per claim, each with an exact identifier and an
// as-of date. Kinds and shapes (deliberately boring - a line grammar, not a
// database):
//
//   - card:<kb-slug> as-of YYYY-MM-DD
//   - branch:<name>@<repo-path> as-of YYYY-MM-DD
//   - pr:<N>@<repo-path> as-of YYYY-MM-DD
//   - worktree:<path> as-of YYYY-MM-DD
//
// Narrative stays free-form and unconstrained; only lines inside the Claims
// section are parsed. branch/pr values split on the FIRST "@" (branch names
// containing "@" are not supported; repo paths must not contain spaces).
//
// The reconciler (ReconcileMemoryClaims) checks each claim against its
// authority: card claims against the KB document's status, branch/pr claims
// against the named local git repo (local refs only - zero network), worktree
// claims against the filesystem.
const ClaimsSectionHeading = "## Claims"

// ResolvedSectionHeading is where the reconciler moves stale claim lines, each
// annotated with the detection date and evidence.
const ResolvedSectionHeading = "## Resolved"

type claimKind string

const (
	claimKindCard     claimKind = "card"
	claimKindBranch   claimKind = "branch"
	claimKindPR       claimKind = "pr"
	claimKindWorktree claimKind = "worktree"
)

var claimLinePatterns = []struct {
	kind claimKind
	re   *regexp.Regexp
}{
	{claimKindCard, regexp.MustCompile(`^-[ \t]+card:([^\s]+)[ \t]+as-of[ \t]+(\d{4}-\d{2}-\d{2})[ \t]*$`)},
	{claimKindBranch, regexp.MustCompile(`^-[ \t]+branch:(.+?)@(\S+)[ \t]+as-of[ \t]+(\d{4}-\d{2}-\d{2})[ \t]*$`)},
	{claimKindPR, regexp.MustCompile(`^-[ \t]+pr:(\d+)@(\S+)[ \t]+as-of[ \t]+(\d{4}-\d{2}-\d{2})[ \t]*$`)},
	{claimKindWorktree, regexp.MustCompile(`^-[ \t]+worktree:(\S+)[ \t]+as-of[ \t]+(\d{4}-\d{2}-\d{2})[ \t]*$`)},
}

// Claim is one parsed claim line.
type Claim struct {
	Kind  claimKind
	Value string // card slug | branch name | pr number | worktree path
	Repo  string // branch/pr only: repo path
	AsOf  string // YYYY-MM-DD as written
	Line  string // the original line, verbatim
}

type ClaimState int

const (
	ClaimVerified ClaimState = iota
	ClaimStale
	ClaimUnverifiable
)

func (s ClaimState) String() string {
	switch s {
	case ClaimVerified:
		return "verified"
	case ClaimStale:
		return "stale"
	default:
		return "unverifiable"
	}
}

// ClaimResult is the reconciled state of one claim.
type ClaimResult struct {
	Claim    Claim
	State    ClaimState
	Evidence string
}

// ReconcileReport summarizes one reconciliation pass.
type ReconcileReport struct {
	ClaimCount    int
	Verified      int
	Stale         int
	Unverifiable  int
	Unparseable   []string // claim-shaped lines left untouched
	StaleEvidence []string // "<kind>:<value> -> <evidence>"
}

// ReconcileMemoryClaims deterministically reconciles MEMORY.md's "## Claims"
// section against each claim's authority. Zero LLM tokens, zero network: card
// claims are checked against the KB document's frontmatter status, branch/pr
// claims against the named local git repo (local and remote-tracking refs
// only), worktree claims against the filesystem.
//
// Three states, and only a positive closed answer annotates:
//   - verified     -> the line is left unchanged
//   - stale        -> the line is moved to the "## Resolved" section, annotated
//     [STALE <date>: <evidence>]
//   - unverifiable -> the line is left unchanged; the authority was
//     unreachable or its answer is inconclusive, and that is never rendered as
//     stale
//
// It never deletes or rewrites narrative: only stale claim lines move. Lines
// that look like claims but do not parse are left untouched and reported,
// never guessed at. The returned document is byte-identical to the input when
// nothing is stale, so callers can detect a no-op cheaply.
// wsDir is the workspace root (the parent of agent directories) - it locates
// the KB under <wsDir>/.kb/workspaces/main. now is injected for determinism.
func ReconcileMemoryClaims(memoryContent, wsDir string, now time.Time) (string, ReconcileReport, error) {
	var rep ReconcileReport

	lines := strings.Split(memoryContent, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == ClaimsSectionHeading {
			start = i
			break
		}
	}
	if start == -1 {
		return memoryContent, rep, nil
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if isHeadingLine(lines[i]) {
			end = i
			break
		}
	}

	var kept []string
	var stale []ClaimResult
	for _, l := range lines[start+1 : end] {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || !strings.HasPrefix(trimmed, "- ") {
			kept = append(kept, l)
			continue
		}
		claim, ok := parseClaimLine(trimmed)
		if !ok {
			rep.Unparseable = append(rep.Unparseable, trimmed)
			kept = append(kept, l)
			continue
		}
		rep.ClaimCount++
		res := reconcileOne(claim, wsDir)
		switch res.State {
		case ClaimStale:
			rep.Stale++
			rep.StaleEvidence = append(rep.StaleEvidence, fmt.Sprintf("%s:%s -> %s", res.Claim.Kind, res.Claim.Value, res.Evidence))
			stale = append(stale, res)
		default:
			if res.State == ClaimVerified {
				rep.Verified++
			} else {
				rep.Unverifiable++
			}
			kept = append(kept, l)
		}
	}

	if len(stale) == 0 {
		return memoryContent, rep, nil
	}

	out := make([]string, 0, len(lines)+len(stale)+2)
	out = append(out, lines[:start+1]...)
	out = append(out, kept...)
	out = append(out, lines[end:]...)
	if resIdx, ok := findSectionStart(out, ResolvedSectionHeading); ok {
		resEnd := sectionEnd(out, resIdx)
		for _, res := range stale {
			out = insertBefore(out, resEnd, annotateClaim(res, now))
			resEnd++
		}
	} else {
		out = appendBlankLine(out)
		out = append(out, ResolvedSectionHeading)
		for _, res := range stale {
			out = append(out, annotateClaim(res, now))
		}
	}
	return strings.Join(out, "\n"), rep, nil
}

// parseClaimLine parses one claim-shaped line against the line grammar.
func parseClaimLine(line string) (Claim, bool) {
	for _, p := range claimLinePatterns {
		m := p.re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		c := Claim{Kind: p.kind, Line: line}
		switch p.kind {
		case claimKindCard:
			c.Value, c.AsOf = m[1], m[2]
		case claimKindBranch:
			c.Value, c.Repo, c.AsOf = m[1], m[2], m[3]
		case claimKindPR:
			c.Value, c.Repo, c.AsOf = m[1], m[2], m[3]
		case claimKindWorktree:
			c.Value, c.AsOf = m[1], m[2]
		}
		return c, true
	}
	return Claim{}, false
}

// reconcileOne checks one claim against its authority.
func reconcileOne(c Claim, wsDir string) ClaimResult {
	var state ClaimState
	var evidence string
	switch c.Kind {
	case claimKindCard:
		state, evidence = checkCardClaim(c.Value, wsDir)
	case claimKindBranch:
		state, evidence = checkBranchClaim(c.Value, c.Repo)
	case claimKindPR:
		state, evidence = checkPRClaim(c.Value, c.Repo)
	case claimKindWorktree:
		state, evidence = checkWorktreeClaim(c.Value)
	}
	return ClaimResult{Claim: c, State: state, Evidence: evidence}
}

// checkCardClaim resolves a card claim against the KB document at
// <wsDir>/.kb/workspaces/main/<slug>.md. A missing document is a positive
// closed answer (the card is gone); a present document decides by its
// frontmatter status.
func checkCardClaim(slug, wsDir string) (ClaimState, string) {
	kbPath := filepath.Join(wsDir, ".kb", "workspaces", "main", slug+".md")
	data, err := os.ReadFile(kbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ClaimStale, "card not found in KB"
		}
		return ClaimUnverifiable, "KB document unreadable"
	}
	status, ok := kbDocumentStatus(string(data))
	if !ok {
		return ClaimUnverifiable, "KB document has no parseable status"
	}
	switch status {
	case "completed", "cancelled", "closed", "done", "archived":
		return ClaimStale, "KB status=" + status
	default:
		return ClaimVerified, "KB status=" + status
	}
}

// kbDocumentStatus extracts the frontmatter status field from a KB document.
func kbDocumentStatus(content string) (string, bool) {
	if !strings.HasPrefix(content, "---") {
		return "", false
	}
	rest := content[3:]
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return "", false
	}
	fm := rest[:i]
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "status:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "status:")), "\"'` "), true
		}
	}
	return "", false
}

// checkBranchClaim resolves a branch claim against the named repo. Local refs
// only - a branch absent locally is never rendered stale, because the remote
// is the authority and this pass makes no network calls.
func checkBranchClaim(name, repoPath string) (ClaimState, string) {
	ref, err := gitRefPresent(repoPath, "refs/heads/"+name)
	if err != nil {
		return ClaimUnverifiable, "git unavailable: " + err.Error()
	}
	if ref {
		return ClaimVerified, "local branch present"
	}
	refs, err := gitListRefs(repoPath, "refs/remotes/")
	if err != nil {
		return ClaimUnverifiable, "git unavailable: " + err.Error()
	}
	want := "/" + name
	for _, r := range refs {
		if strings.HasSuffix(r, want) {
			return ClaimVerified, "remote-tracking ref present"
		}
	}
	return ClaimUnverifiable, "not in local or remote-tracking refs (remote is the authority; zero-network pass)"
}

// checkPRClaim resolves a PR claim against locally fetched PR refs
// (refs/pull/<N>/head). An absent ref is inconclusive - the PR may be merged
// or simply never fetched - so it stays unverifiable, never stale.
func checkPRClaim(num, repoPath string) (ClaimState, string) {
	ref, err := gitRefPresent(repoPath, "refs/pull/"+num+"/head")
	if err != nil {
		return ClaimUnverifiable, "git unavailable: " + err.Error()
	}
	if ref {
		return ClaimVerified, "PR ref present locally"
	}
	return ClaimUnverifiable, "PR ref not present locally (merged or never fetched)"
}

// checkWorktreeClaim resolves a worktree claim against the filesystem, which
// is the authority for path existence: absence is a positive closed answer.
func checkWorktreeClaim(path string) (ClaimState, string) {
	st, err := os.Stat(path)
	if err == nil {
		if st.IsDir() {
			return ClaimVerified, "path exists (directory)"
		}
		return ClaimVerified, "path exists"
	}
	if os.IsNotExist(err) {
		return ClaimStale, "path absent"
	}
	return ClaimUnverifiable, "stat failed"
}

func annotateClaim(res ClaimResult, now time.Time) string {
	return res.Claim.Line + " [STALE " + now.UTC().Format("2006-01-02") + ": " + res.Evidence + "]"
}

func isHeadingLine(l string) bool {
	t := strings.TrimSpace(l)
	return strings.HasPrefix(t, "#")
}

func findSectionStart(lines []string, heading string) (int, bool) {
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			return i, true
		}
	}
	return 0, false
}

func sectionEnd(lines []string, start int) int {
	for i := start + 1; i < len(lines); i++ {
		if isHeadingLine(lines[i]) {
			return i
		}
	}
	return len(lines)
}

func insertBefore(lines []string, idx int, line string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, line)
	out = append(out, lines[idx:]...)
	return out
}

func appendBlankLine(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	return lines
}

// gitRefPresent reports whether ref exists in the repo at repoPath.
func gitRefPresent(repoPath, ref string) (bool, error) {
	cmd := gitCommand(repoPath, "show-ref", "--verify", "--quiet", ref)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false, fmt.Errorf("%v", err)
	}
	if exitErr.ExitCode() == 1 {
		return false, nil // ref does not exist
	}
	return false, fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
}

// gitListRefs lists refs under pattern in the repo at repoPath.
func gitListRefs(repoPath, pattern string) ([]string, error) {
	cmd := gitCommand(repoPath, "for-each-ref", "--format=%(refname)", pattern)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("%v", err)
	}
	var refs []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			refs = append(refs, l)
		}
	}
	return refs, nil
}

func gitCommand(repoPath string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	// Cancel runs when the process exits (or the deadline hits), not when this
	// function returns - a deferred cancel would kill the context before git
	// has even started.
	cmd.Cancel = func() error {
		cancel()
		return nil
	}
	return cmd
}
