package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	goconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestWorkspaceGitVersioning(t *testing.T) {
	wsDir := t.TempDir()

	if IsWorkspaceGitRepo(wsDir) {
		t.Fatalf("expected IsWorkspaceGitRepo to be false before init")
	}

	// 1. Create agent dir and initialize per-agent git repo
	agentID := "jax"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	if err := InitAgentGit(wsDir, agentID); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}

	if !IsWorkspaceGitRepo(agentDir) {
		t.Fatalf("expected IsWorkspaceGitRepo to be true after init")
	}

	// 2. Verify default .gitignore was created in agentDir
	if !pathExists(filepath.Join(agentDir, ".gitignore")) {
		t.Fatalf("expected default .gitignore to be created in agent directory")
	}

	// Create a temporary file that should be ignored by default .gitignore
	if err := os.WriteFile(filepath.Join(agentDir, "untracked.tmp"), []byte("temp"), 0644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "Hello Jax"); err != nil {
		t.Fatalf("failed to append session turn: %v", err)
	}

	// Set test AGENT2AGENT environment
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, `{"caller_id":"bob","call_chain":["bob"],"trace_id":"a2a-git-test"}`)

	// 3. Commit event
	if err := CommitWorkspaceEvent(wsDir, agentID, "user"); err != nil {
		t.Fatalf("CommitWorkspaceEvent failed: %v", err)
	}

	// 4. Verify HEAD commit
	headSHA, err := GetWorkspaceHeadCommit(agentDir)
	if err != nil {
		t.Fatalf("GetWorkspaceHeadCommit failed: %v", err)
	}
	if headSHA == "" {
		t.Fatalf("expected non-empty head SHA")
	}

	// Read commit details via go-git
	repo, err := git.PlainOpen(agentDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}
	commitObj, err := repo.CommitObject(plumbing.NewHash(headSHA))
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	if commitObj.Author.Name != "jax" || commitObj.Author.Email != "jax@wackypub.local" {
		t.Errorf("unexpected author: %v <%v>", commitObj.Author.Name, commitObj.Author.Email)
	}

	if !strings.HasPrefix(commitObj.Message, "user\nAGENT2AGENT:") {
		t.Errorf("unexpected commit message: %q", commitObj.Message)
	}
	if !strings.Contains(commitObj.Message, `"caller_id":"bob"`) {
		t.Errorf("expected caller_id bob in commit message A2A payload: %q", commitObj.Message)
	}

	// Verify untracked.tmp was ignored and .gitignore + session.jsonl were committed
	tree, err := commitObj.Tree()
	if err != nil {
		t.Fatalf("failed to get commit tree: %v", err)
	}
	if _, err := tree.FindEntry("untracked.tmp"); err == nil {
		t.Errorf("expected untracked.tmp to be ignored by default .gitignore, but found in commit tree")
	}
	if _, err := tree.FindEntry(".gitignore"); err != nil {
		t.Errorf("expected .gitignore to be tracked in commit tree, got error: %v", err)
	}
}

func TestPerAgentGitVersioning(t *testing.T) {
	wsDir := t.TempDir()

	// Initialize isolated git repo for agent 'clerk'
	if err := InitAgentGit(wsDir, "clerk"); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}

	clerkDir := filepath.Join(wsDir, "clerk")
	if !IsWorkspaceGitRepo(clerkDir) {
		t.Fatalf("expected clerkDir to be a git repo")
	}
	if IsWorkspaceGitRepo(wsDir) {
		t.Fatalf("expected root wsDir not to be a git repo when per-agent git is used")
	}

	if err := AppendSessionTurn(clerkDir, "user", "Hello Clerk"); err != nil {
		t.Fatalf("failed to append session turn: %v", err)
	}

	if err := CommitWorkspaceEvent(wsDir, "clerk", "user"); err != nil {
		t.Fatalf("CommitWorkspaceEvent failed for clerk: %v", err)
	}

	headSHA, err := GetWorkspaceHeadCommit(clerkDir)
	if err != nil || headSHA == "" {
		t.Fatalf("expected valid HEAD commit for clerk: %v", err)
	}

	repo, err := git.PlainOpen(clerkDir)
	if err != nil {
		t.Fatalf("failed to open clerk repo: %v", err)
	}
	commitObj, err := repo.CommitObject(plumbing.NewHash(headSHA))
	if err != nil {
		t.Fatalf("failed to get clerk commit object: %v", err)
	}

	if commitObj.Author.Name != "clerk" || commitObj.Author.Email != "clerk@wackypub.local" {
		t.Errorf("unexpected author: %v <%v>", commitObj.Author.Name, commitObj.Author.Email)
	}
}

func TestCrossAgentGitRevisionLineage(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed writing root marker: %v", err)
	}

	// Initialize isolated git repos for 'bob' and 'jax'
	if err := InitAgentGit(wsDir, "bob"); err != nil {
		t.Fatalf("InitAgentGit bob failed: %v", err)
	}
	if err := InitAgentGit(wsDir, "jax"); err != nil {
		t.Fatalf("InitAgentGit jax failed: %v", err)
	}

	bobDir := filepath.Join(wsDir, "bob")
	jaxDir := filepath.Join(wsDir, "jax")

	// Allow bob to call jax
	if err := os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("jax\n"), 0644); err != nil {
		t.Fatalf("failed writing allowed agents: %v", err)
	}

	// 1. Bob makes a turn and commits to bob's repo
	if err := AppendSessionTurn(bobDir, "user", "Message for Bob"); err != nil {
		t.Fatalf("failed to append session turn for bob: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "bob", "user"); err != nil {
		t.Fatalf("CommitWorkspaceEvent failed for bob: %v", err)
	}

	bobHeadSHA, err := GetWorkspaceHeadCommit(bobDir)
	if err != nil || bobHeadSHA == "" {
		t.Fatalf("expected valid HEAD SHA for bob: %v", err)
	}

	// 2. Validate target 'jax' from bobDir CWD
	// Clear both A2A env vars so inherited invocation context doesn't leak into
	// ValidateAgentTarget's call chain calculation (ParseA2AMetadata checks AGENT2AGENT
	// first and falls back to WACKYPUB_CALL_CHAIN, so both need isolating).
	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, "")
	origChain := os.Getenv(CallChainEnvVar)
	defer os.Setenv(CallChainEnvVar, origChain)
	os.Setenv(CallChainEnvVar, "")

	origCwd, _ := os.Getwd()
	if err := os.Chdir(bobDir); err != nil {
		t.Fatalf("failed to chdir to bobDir: %v", err)
	}
	defer os.Chdir(origCwd)
	meta, err := ValidateAgentTarget("jax")
	if err != nil {
		t.Fatalf("ValidateAgentTarget jax failed: %v", err)
	}

	if meta.Metadata["workspace_revision"] != bobHeadSHA {
		t.Fatalf("expected workspace_revision to be bob's HEAD SHA %q, got %q", bobHeadSHA, meta.Metadata["workspace_revision"])
	}

	// 3. Simulate child process receiving meta in its environment
	denseMeta, err := meta.Encode()
	if err != nil {
		t.Fatalf("failed to encode denseMeta: %v", err)
	}
	os.Setenv(Agent2AgentEnvVar, denseMeta) // origA2A already saved/deferred above

	// 4. Jax appends a turn and commits
	if err := AppendSessionTurn(jaxDir, "user", "Message for Jax"); err != nil {
		t.Fatalf("failed to append session turn for jax: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "jax", "user"); err != nil {
		t.Fatalf("CommitWorkspaceEvent failed for jax: %v", err)
	}

	jaxHeadSHA, err := GetWorkspaceHeadCommit(jaxDir)
	repo, err := git.PlainOpen(jaxDir)
	if err != nil {
		t.Fatalf("failed to open jax repo: %v", err)
	}
	commitObj, err := repo.CommitObject(plumbing.NewHash(jaxHeadSHA))
	if err != nil {
		t.Fatalf("failed to get jax commit object: %v", err)
	}

	// Verify jax's commit message contains bob's revision SHA
	if !strings.Contains(commitObj.Message, bobHeadSHA) {
		t.Errorf("expected jax commit message to embed bob's revision SHA %q, got: %q", bobHeadSHA, commitObj.Message)
	}
}

func TestWorkspaceSnapshotTagAndPush(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed writing root marker: %v", err)
	}

	if err := InitWorkspaceGit(wsDir); err != nil {
		t.Fatalf("InitWorkspaceGit failed: %v", err)
	}
	if err := InitAgentGit(wsDir, "clerk"); err != nil {
		t.Fatalf("InitAgentGit failed: %v", err)
	}

	clerkDir := filepath.Join(wsDir, "clerk")
	if err := os.WriteFile(filepath.Join(clerkDir, "runtime.json"), []byte(`{"model":"gemini-flash"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}
	if err := AppendSessionTurn(clerkDir, "user", "Test turn"); err != nil {
		t.Fatalf("failed to append session turn: %v", err)
	}
	if err := CommitWorkspaceEvent(wsDir, "clerk", "user"); err != nil {
		t.Fatalf("CommitWorkspaceEvent failed: %v", err)
	}

	// 1. Snapshot
	manifestPath, err := CreateWorkspaceSnapshot(wsDir)
	if err != nil {
		t.Fatalf("CreateWorkspaceSnapshot failed: %v", err)
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("failed reading MANIFEST.md: %v", err)
	}
	if !strings.Contains(string(content), "clerk") {
		t.Errorf("expected MANIFEST.md to contain clerk, got: %s", string(content))
	}

	// D94: Assert workspace root repo actually has a snapshot commit created
	rootHeadSHA, err := GetWorkspaceHeadCommit(wsDir)
	if err != nil || rootHeadSHA == "" {
		t.Fatalf("expected workspace root commit for snapshot, got sha=%q err=%v", rootHeadSHA, err)
	}
	rootRepo, err := git.PlainOpen(wsDir)
	if err != nil {
		t.Fatalf("failed to open root repo: %v", err)
	}
	rootCommit, err := rootRepo.CommitObject(plumbing.NewHash(rootHeadSHA))
	if err != nil {
		t.Fatalf("failed to get root commit object: %v", err)
	}
	if !strings.Contains(rootCommit.Message, "snapshot") {
		t.Errorf("expected root commit message to contain 'snapshot', got %q", rootCommit.Message)
	}
	if rootCommit.Author.Name != "system" {
		t.Errorf("expected root commit author 'system', got %q", rootCommit.Author.Name)
	}

	// 2. Tag
	if err := TagWorkspaceAndAgents(wsDir, "v1.0.0"); err != nil {
		t.Fatalf("TagWorkspaceAndAgents failed: %v", err)
	}

	clerkRepo, err := git.PlainOpen(clerkDir)
	if err != nil {
		t.Fatalf("failed to open clerk repo: %v", err)
	}
	tagRef, err := clerkRepo.Tag("tag-clerk")
	if err != nil {
		t.Fatalf("expected tag-clerk tag in clerk repo: %v", err)
	}
	if tagRef == nil {
		t.Fatalf("nil tagRef for tag-clerk")
	}

	// 3. Push to a local bare repository as remote
	remoteDir := t.TempDir()
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("failed to init bare remote repo: %v", err)
	}

	wsRepo, err := git.PlainOpen(wsDir)
	if err != nil {
		t.Fatalf("failed to open wsRepo: %v", err)
	}
	if _, err := wsRepo.CreateRemote(&goconfig.RemoteConfig{Name: "origin", URLs: []string{remoteDir}}); err != nil {
		t.Fatalf("failed to create origin remote on wsRepo: %v", err)
	}

	if err := PushWorkspaceAndAgents(wsDir, "origin"); err != nil {
		t.Fatalf("PushWorkspaceAndAgents failed: %v", err)
	}

	// Verify remote has branch 'clerk'
	remoteRepo, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatalf("failed to open remote repo: %v", err)
	}
	if _, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("clerk"), true); err != nil {
		t.Errorf("expected branch 'clerk' in remote repo, got error: %v", err)
	}
}

func TestPushWorkspaceAndAgentsErrorPropagation(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("failed writing root marker: %v", err)
	}

	if err := InitWorkspaceGit(wsDir); err != nil {
		t.Fatalf("InitWorkspaceGit failed: %v", err)
	}

	// Add remote pointing to non-existent endpoint
	wsRepo, err := git.PlainOpen(wsDir)
	if err != nil {
		t.Fatalf("failed to open wsRepo: %v", err)
	}
	if _, err := wsRepo.CreateRemote(&goconfig.RemoteConfig{Name: "origin", URLs: []string{"http://127.0.0.1:1/nonexistent.git"}}); err != nil {
		t.Fatalf("failed to create origin remote: %v", err)
	}

	err = PushWorkspaceAndAgents(wsDir, "origin")
	if err == nil {
		t.Fatalf("expected PushWorkspaceAndAgents to fail with error for invalid remote, got nil")
	}
	if !strings.Contains(err.Error(), "push failed") && !strings.Contains(err.Error(), "git push encountered errors") {
		t.Errorf("expected error message describing push failure, got: %v", err)
	}
}
