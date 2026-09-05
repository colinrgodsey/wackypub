package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	goconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const DefaultWorkspaceDomain = "wackypub.local"

const DefaultWorkspaceRootGitignoreContent = `# Default WackyPub workspace root gitignore rules (D35)
# Exclude everything by default so agent folders are not tracked in the workspace root repository
*

!.gitignore
!/WACKYPUB_ROOT
!/MANIFEST.md
`

const DefaultAgentGitignoreContent = `# Default WackyPub agent gitignore rules (D35)
# Exclude everything by default so scratch/temporary files aren't automatically tracked
*

!.gitignore
!/*.md
!/runtime.json
!/.env
!/session.jsonl
!/scratchpad/
!/scratchpad/**
!/skills/
!/skills/**
!/tools/
!/tools/**
`

// EnsureWorkspaceGitignore creates <wsDir>/.gitignore if it does not already exist.
func EnsureWorkspaceGitignore(wsDir string) error {
	if wsDir == "" {
		return nil
	}
	ignorePath := filepath.Join(wsDir, ".gitignore")
	if pathExists(ignorePath) {
		return nil
	}
	return os.WriteFile(ignorePath, []byte(DefaultWorkspaceRootGitignoreContent), 0644)
}

// EnsureAgentGitignore creates <agentDir>/.gitignore if it does not already exist.
func EnsureAgentGitignore(agentDir string) error {
	if agentDir == "" {
		return nil
	}
	ignorePath := filepath.Join(agentDir, ".gitignore")
	if pathExists(ignorePath) {
		return nil
	}
	return os.WriteFile(ignorePath, []byte(DefaultAgentGitignoreContent), 0644)
}

// IsWorkspaceGitRepo returns true if <dir>/.git exists as a directory or regular file.
func IsWorkspaceGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	gitDir := filepath.Join(dir, ".git")
	st, err := os.Stat(gitDir)
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}

// ResolveGitRepoDir resolves whether the agent has its own repository (<wsDir>/<agentID>/.git)
// or (for workspace-level commands when agentID is "") uses the workspace root repository (<wsDir>/.git).
// Returns "" if git tracking is not enabled for the target.
func ResolveGitRepoDir(wsDir, agentID string) string {
	if wsDir == "" {
		return ""
	}
	if agentID != "" {
		agentDir := filepath.Join(wsDir, agentID)
		if IsWorkspaceGitRepo(agentDir) {
			return agentDir
		}
		return ""
	}
	if IsWorkspaceGitRepo(wsDir) {
		return wsDir
	}
	return ""
}

// InitWorkspaceGit initializes a new git repository at wsDir according to D35.
func InitWorkspaceGit(wsDir string) error {
	if wsDir == "" {
		return fmt.Errorf("workspace directory path cannot be empty")
	}
	if IsWorkspaceGitRepo(wsDir) {
		return EnsureWorkspaceGitignore(wsDir)
	}
	_, err := git.PlainInit(wsDir, false)
	if err != nil && err != git.ErrRepositoryAlreadyExists {
		return fmt.Errorf("failed to initialize workspace git repository at %s: %w", wsDir, err)
	}
	if err := EnsureWorkspaceGitignore(wsDir); err != nil {
		return fmt.Errorf("failed to create default workspace .gitignore: %w", err)
	}
	return nil
}

// InitAgentGit initializes an isolated per-agent git repository at <wsDir>/<agentID>/.git according to D35.
func InitAgentGit(wsDir, agentID string) error {
	if wsDir == "" || agentID == "" {
		return fmt.Errorf("workspace directory and agentID cannot be empty")
	}
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("failed to create agent directory: %w", err)
	}
	if IsWorkspaceGitRepo(agentDir) {
		return EnsureAgentGitignore(agentDir)
	}
	_, err := git.PlainInit(agentDir, false)
	if err != nil && err != git.ErrRepositoryAlreadyExists {
		return fmt.Errorf("failed to initialize agent git repository at %s: %w", agentDir, err)
	}
	return EnsureAgentGitignore(agentDir)
}

// GetWorkspaceHeadCommit returns the current HEAD commit SHA string for the target repository.
// Returns "" if the repository does not exist or has no commits yet.
func GetWorkspaceHeadCommit(repoDir string) (string, error) {
	if !IsWorkspaceGitRepo(repoDir) {
		return "", nil
	}
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return "", err
	}
	ref, err := repo.Head()
	if err != nil {
		return "", nil
	}
	return ref.Hash().String(), nil
}

// Workspace git failures are reported when they first happen and whenever the reason changes, since
// almost every caller of CommitWorkspaceEvent discards its error. Without this, a repository go-git cannot
// open, or one holding a path staging cannot handle, stops recording history forever in silence. The record
// clears on a successful commit so a repaired repository reports a later break, and it is keyed per repository
// so a workspace with several agents reports each broken one.
var (
	// gitWarnOut is swapped by tests without holding gitWarnMu, so tests in this package must not
	// use t.Parallel until that is addressed, or a redirected writer will race.
	gitWarnOut io.Writer = os.Stderr
	// Held across both the gitWarnLast check and every write to gitWarnOut, so concurrent
	// agents cannot interleave a print and a map update, and a redirected writer stays safe.
	gitWarnMu   sync.Mutex
	gitWarnLast = map[string]string{}
)

// reportGitFailure announces a failing repository, ignoring a repeat of the same error and speaking up
// again once the reason changes, which is how an operator learns that the first fix did not take.
func reportGitFailure(repoDir, op string, err error) {
	gitWarnMu.Lock()
	defer gitWarnMu.Unlock()
	key := repoDir + "\x00" + op
	if gitWarnLast[key] == err.Error() {
		return
	}
	gitWarnLast[key] = err.Error()
	fmt.Fprintf(gitWarnOut, "Warning: workspace git %s failed for %s and will keep failing until corrected, so history is not being recorded: %v\n", op, repoDir, err)
}

// clearGitFailure forgets a repository's failure, noting the recovery only if it had been failing.
func clearGitFailure(repoDir, op string) {
	gitWarnMu.Lock()
	defer gitWarnMu.Unlock()
	key := repoDir + "\x00" + op
	if _, had := gitWarnLast[key]; !had {
		return
	}
	delete(gitWarnLast, key)
	fmt.Fprintf(gitWarnOut, "Warning: workspace git %s working again for %s\n", op, repoDir)
}

// CommitWorkspaceEvent creates a new git commit for a workspace or agent state-mutating event according to D35.
// Where git tracking is not enabled this stays a silent no-op that neither reports nor clears, because absent
// versioning is a configuration choice rather than a recovery. Reporting lives here so every discarding caller
// is covered, which means a caller that handles the returned error itself should not print it as well.
func CommitWorkspaceEvent(wsDir, agentID, eventType string) error {
	repoDir := ResolveGitRepoDir(wsDir, agentID)
	if repoDir == "" {
		return nil
	}

	err := commitWorkspaceEvent(wsDir, repoDir, agentID, eventType)
	if err != nil {
		reportGitFailure(repoDir, "commit", err)
	} else {
		clearGitFailure(repoDir, "commit")
	}
	return err
}

// commitWorkspaceEvent is the D35 commit itself, error-only for callers that decide how loudly to fail.
func commitWorkspaceEvent(wsDir, repoDir, agentID, eventType string) error {
	if repoDir == wsDir {
		_ = EnsureWorkspaceGitignore(repoDir)
	} else {
		_ = EnsureAgentGitignore(repoDir)
	}

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return fmt.Errorf("failed to open git repo at %s: %w", repoDir, err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get git worktree for %s: %w", repoDir, err)
	}

	// Exclude gitlink (submodule) entries from index so staging doesn't fail on directories
	if idx, err := repo.Storer.Index(); err == nil {
		for _, e := range idx.Entries {
			if e.Mode == filemode.Submodule {
				// Gitlink names containing glob metacharacters are not escaped (rare, accepted).
				worktree.Excludes = append(worktree.Excludes,
					gitignore.ParsePattern(e.Name, nil),
					gitignore.ParsePattern(e.Name+"/**", nil),
				)
			}
		}
	}

	// Stage all workspace/agent changes
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("failed to stage workspace files: %w", err)
	}

	// Read current A2A metadata & update workspace_revision with HEAD before commit
	headSHA, _ := GetWorkspaceHeadCommit(repoDir)
	meta, err := ParseA2AMetadata()
	if err != nil || meta == nil {
		meta = &A2AMetadata{
			Metadata: make(map[string]string),
		}
	}
	if meta.Metadata == nil {
		meta.Metadata = make(map[string]string)
	}
	if headSHA != "" {
		meta.Metadata["workspace_revision"] = headSHA
	}

	a2aJSON, err := meta.Encode()
	if err != nil {
		a2aJSON = "{}"
	}

	commitMsg := fmt.Sprintf("%s\nAGENT2AGENT: %s", eventType, a2aJSON)

	if agentID == "" {
		agentID = "system"
	}
	authorEmail := fmt.Sprintf("%s@%s", agentID, DefaultWorkspaceDomain)

	sig := &object.Signature{
		Name:  agentID,
		Email: authorEmail,
		When:  time.Now(),
	}

	_, err = worktree.Commit(commitMsg, &git.CommitOptions{
		Author:            sig,
		Committer:         sig,
		AllowEmptyCommits: false,
	})
	if err != nil && err != git.ErrEmptyCommit {
		return fmt.Errorf("failed to commit workspace event: %w", err)
	}

	return nil
}

// CreateWorkspaceSnapshot creates/updates <wsDir>/MANIFEST.md with a list of all agent directories and their HEAD commit SHAs according to D35.
func CreateWorkspaceSnapshot(wsDir string) (string, error) {
	if wsDir == "" {
		return "", fmt.Errorf("workspace directory path cannot be empty")
	}

	sdk := NewSDK(wsDir)
	agentIDs, err := sdk.ListAgents()
	if err != nil {
		return "", fmt.Errorf("failed to list agents: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("# Workspace Manifest Snapshot\n\n")
	sb.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))
	sb.WriteString("| Agent ID | Commit SHA |\n")
	sb.WriteString("|---|---|\n")

	for _, agentID := range agentIDs {
		repoDir := ResolveGitRepoDir(wsDir, agentID)
		sha := "-"
		if repoDir != "" {
			if headSHA, _ := GetWorkspaceHeadCommit(repoDir); headSHA != "" {
				sha = headSHA
			}
		}
		sb.WriteString(fmt.Sprintf("| %s | %s |\n", agentID, sha))
	}

	manifestPath := filepath.Join(wsDir, "MANIFEST.md")
	if err := os.WriteFile(manifestPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("failed to write MANIFEST.md: %w", err)
	}

	if IsWorkspaceGitRepo(wsDir) {
		_ = CommitWorkspaceEvent(wsDir, "system", "snapshot")
	}

	return manifestPath, nil
}

// TagWorkspaceAndAgents creates a git tag <tagName> in the workspace root repo (if present),
// and tags each agent repository with "tag-<agent_id>" according to D35.
func TagWorkspaceAndAgents(wsDir, tagName string) error {
	if wsDir == "" || tagName == "" {
		return fmt.Errorf("workspace directory and tag name cannot be empty")
	}

	var tagErrs []string

	// 1. Tag workspace root repo if present
	if IsWorkspaceGitRepo(wsDir) {
		repo, err := git.PlainOpen(wsDir)
		if err != nil {
			tagErrs = append(tagErrs, fmt.Sprintf("failed to open workspace root repo: %v", err))
		} else {
			head, err := repo.Head()
			if err == nil {
				_, err = repo.CreateTag(tagName, head.Hash(), &git.CreateTagOptions{
					Tagger: &object.Signature{
						Name:  "system",
						Email: fmt.Sprintf("system@%s", DefaultWorkspaceDomain),
						When:  time.Now(),
					},
					Message: fmt.Sprintf("Workspace tag %s", tagName),
				})
				if err != nil && err != git.ErrTagExists {
					tagErrs = append(tagErrs, fmt.Sprintf("workspace root tag failed: %v", err))
				}
			}
		}
	}

	// 2. Tag each agent repo with "tag-<agent_id>"
	sdk := NewSDK(wsDir)
	agentIDs, err := sdk.ListAgents()
	if err != nil {
		return fmt.Errorf("failed to list agents: %w", err)
	}

	for _, agentID := range agentIDs {
		agentDir := sdk.AgentDir(agentID)
		if IsWorkspaceGitRepo(agentDir) {
			repo, err := git.PlainOpen(agentDir)
			if err != nil {
				tagErrs = append(tagErrs, fmt.Sprintf("agent %q repo open failed: %v", agentID, err))
				continue
			}
			head, err := repo.Head()
			if err != nil {
				tagErrs = append(tagErrs, fmt.Sprintf("agent %q HEAD lookup failed: %v", agentID, err))
				continue
			}
			agentTagName := fmt.Sprintf("tag-%s", agentID)
			_, err = repo.CreateTag(agentTagName, head.Hash(), &git.CreateTagOptions{
				Tagger: &object.Signature{
					Name:  agentID,
					Email: fmt.Sprintf("%s@%s", agentID, DefaultWorkspaceDomain),
					When:  time.Now(),
				},
				Message: fmt.Sprintf("Agent tag %s for %s", agentTagName, agentID),
			})
			if err != nil && err != git.ErrTagExists {
				tagErrs = append(tagErrs, fmt.Sprintf("agent %q tag %q failed: %v", agentID, agentTagName, err))
			}
		}
	}

	if len(tagErrs) > 0 {
		return fmt.Errorf("tagging encountered errors:\n  %s", strings.Join(tagErrs, "\n  "))
	}

	return nil
}

// PushWorkspaceAndAgents pushes the workspace root repo and each agent folder repo to <remoteName> according to D35.
// Reads the remote URL from the root workspace repo, and applies it to each per-agent repo pushing to branch <agent_id>.
func PushWorkspaceAndAgents(wsDir, remoteName string) error {
	if wsDir == "" || remoteName == "" {
		return fmt.Errorf("workspace directory and remote name cannot be empty")
	}

	var rootRemoteURL string
	var pushErrs []string

	// 1. Read remote URL and push workspace root repo if present
	if IsWorkspaceGitRepo(wsDir) {
		repo, err := git.PlainOpen(wsDir)
		if err != nil {
			pushErrs = append(pushErrs, fmt.Sprintf("failed to open workspace root repo: %v", err))
		} else {
			if remote, err := repo.Remote(remoteName); err == nil && len(remote.Config().URLs) > 0 {
				rootRemoteURL = remote.Config().URLs[0]
			}
			if rootRemoteURL != "" {
				err = repo.Push(&git.PushOptions{
					RemoteName: remoteName,
					RefSpecs: []goconfig.RefSpec{
						"refs/heads/*:refs/heads/*",
						"refs/tags/*:refs/tags/*",
					},
				})
				if err != nil && err != git.NoErrAlreadyUpToDate {
					pushErrs = append(pushErrs, fmt.Sprintf("workspace root push failed: %v", err))
				}
			}
		}
	}

	// Fall back to using remoteName as URL if it looks like a URL/path and root URL wasn't found
	if rootRemoteURL == "" && (strings.HasPrefix(remoteName, "http://") || strings.HasPrefix(remoteName, "https://") || strings.HasPrefix(remoteName, "git@") || strings.HasPrefix(remoteName, "ssh://") || strings.HasPrefix(remoteName, "/") || strings.HasPrefix(remoteName, ".")) {
		rootRemoteURL = remoteName
	}

	if rootRemoteURL == "" {
		return fmt.Errorf("remote %q not found in root workspace git repository and is not a valid remote URL", remoteName)
	}

	// 2. Push each agent folder repo to rootRemoteURL under branch <agent_id>
	sdk := NewSDK(wsDir)
	agentIDs, err := sdk.ListAgents()
	if err != nil {
		return fmt.Errorf("failed to list agents: %w", err)
	}

	for _, agentID := range agentIDs {
		agentDir := sdk.AgentDir(agentID)
		insp, err := sdk.InspectAgent(agentID)
		if err != nil || !insp.RuntimeJSONExists || !IsWorkspaceGitRepo(agentDir) {
			continue
		}

		repo, err := git.PlainOpen(agentDir)
		if err != nil {
			pushErrs = append(pushErrs, fmt.Sprintf("agent %q open repo failed: %v", agentID, err))
			continue
		}

		// Ensure remote exists on agent repo, dynamically adding rootRemoteURL if absent
		remote, err := repo.Remote(remoteName)
		if err != nil || len(remote.Config().URLs) == 0 {
			_ = repo.DeleteRemote(remoteName)
			remote, err = repo.CreateRemote(&goconfig.RemoteConfig{
				Name: remoteName,
				URLs: []string{rootRemoteURL},
			})
		}
		if err != nil || remote == nil {
			pushErrs = append(pushErrs, fmt.Sprintf("agent %q create remote failed: %v", agentID, err))
			continue
		}

		head, err := repo.Head()
		if err != nil {
			pushErrs = append(pushErrs, fmt.Sprintf("agent %q HEAD lookup failed: %v", agentID, err))
			continue
		}

		localRef := head.Name().String()
		remoteBranchRef := fmt.Sprintf("%s:refs/heads/%s", localRef, agentID)

		err = repo.Push(&git.PushOptions{
			RemoteName: remoteName,
			RefSpecs: []goconfig.RefSpec{
				goconfig.RefSpec(remoteBranchRef),
				goconfig.RefSpec("refs/tags/*:refs/tags/*"),
			},
		})
		if err != nil && err != git.NoErrAlreadyUpToDate {
			pushErrs = append(pushErrs, fmt.Sprintf("agent %q push failed: %v", agentID, err))
		}
	}

	if len(pushErrs) > 0 {
		return fmt.Errorf("git push encountered errors:\n  %s", strings.Join(pushErrs, "\n  "))
	}

	return nil
}
