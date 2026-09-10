package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResolveWorkspaceDir_Explicit(t *testing.T) {
	tmpDir := t.TempDir()

	// Explicit path without RootMarkerFile should fail
	_, err := ResolveWorkspaceDir(tmpDir, true)
	if err == nil {
		t.Fatalf("expected error when RootMarkerFile is missing from explicit path")
	}

	// Create RootMarkerFile
	markerPath := filepath.Join(tmpDir, RootMarkerFile)
	if err := os.WriteFile(markerPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create RootMarkerFile: %v", err)
	}

	// Explicit path with RootMarkerFile should succeed
	resolved, err := ResolveWorkspaceDir(tmpDir, true)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resolved != filepath.Clean(tmpDir) {
		t.Fatalf("expected %s, got %s", filepath.Clean(tmpDir), resolved)
	}
}

func TestResolveWorkspaceDir_WalkUp(t *testing.T) {
	wsDir := t.TempDir()
	markerPath := filepath.Join(wsDir, RootMarkerFile)
	if err := os.WriteFile(markerPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create RootMarkerFile: %v", err)
	}

	subDir := filepath.Join(wsDir, "bob", "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get wd: %v", err)
	}
	defer os.Chdir(origWd)

	if err := os.Chdir(subDir); err != nil {
		t.Fatalf("failed to chdir to subDir: %v", err)
	}

	// Unspecified --ws should walk up from subDir and find wsDir
	resolved, err := ResolveWorkspaceDir(".", false)
	if err != nil {
		t.Fatalf("expected walk-up to find workspace, got: %v", err)
	}
	if resolved != wsDir {
		t.Fatalf("expected workspace %s, got %s", wsDir, resolved)
	}
}

func TestValidateAgentTarget_CallChain(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "jax")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte("bob\nalice\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed agents: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("failed to chdir to agentDir: %v", err)
	}
	defer os.Chdir(origCwd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, "")

	origChain := os.Getenv(CallChainEnvVar)
	defer os.Setenv(CallChainEnvVar, origChain)

	os.Setenv(CallChainEnvVar, "bob,jax")

	// Target 'bob' should fail because it's already in CallChainEnvVar
	_, err = ValidateAgentTarget("bob")
	if err == nil {
		t.Fatalf("expected deadlock error for agent already in call chain")
	}
	if !strings.Contains(err.Error(), "deadlock cycle") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Target 'alice' should succeed and return updated A2AMetadata with 'alice' appended
	newMeta, err := ValidateAgentTarget("alice")
	if err != nil {
		t.Fatalf("expected success targeting alice, got: %v", err)
	}
	if strings.Join(newMeta.CallChain, ",") != "bob,jax,alice" {
		t.Fatalf("expected chain 'bob,jax,alice', got %v", newMeta.CallChain)
	}
}

func TestValidateAgentTarget_Allowlist(t *testing.T) {
	tmpDir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	origA2A := os.Getenv(Agent2AgentEnvVar)
	defer os.Setenv(Agent2AgentEnvVar, origA2A)
	os.Setenv(Agent2AgentEnvVar, "")

	origChain := os.Getenv(CallChainEnvVar)
	defer os.Setenv(CallChainEnvVar, origChain)
	os.Setenv(CallChainEnvVar, "")

	// Create an agent directory structure: <tmpDir>/bob
	bobDir := filepath.Join(tmpDir, "bob")
	os.MkdirAll(bobDir, 0755)
	os.WriteFile(filepath.Join(bobDir, "AGENTS.md"), []byte("You are Bob"), 0644)

	os.Chdir(bobDir)

	// Bob has no AllowedAgentsFile -> default deny all
	_, err := ValidateAgentTarget("alice")
	if err == nil || !strings.Contains(err.Error(), "no WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected deny-all error, got: %v", err)
	}

	// Add AllowedAgentsFile with 'alice'
	os.WriteFile(filepath.Join(bobDir, AllowedAgentsFile), []byte("# allowed\nalice\n"), 0644)

	// Targeting 'alice' should now succeed
	meta, err := ValidateAgentTarget("alice")
	if err != nil {
		t.Fatalf("expected allowed access to alice, got: %v", err)
	}
	if meta == nil || len(meta.CallChain) != 1 || meta.CallChain[0] != "alice" {
		t.Fatalf("unexpected call chain: %v", meta)
	}

	// Targeting 'charlie' should fail
	_, err = ValidateAgentTarget("charlie")
	if err == nil || !strings.Contains(err.Error(), "not in WACKYPUB_ALLOWED_AGENTS allowlist") {
		t.Fatalf("expected unauthorized error for charlie, got: %v", err)
	}
}

func TestDiscoverAgentTools(t *testing.T) {
	agentDir := t.TempDir()
	toolsDir := filepath.Join(agentDir, ToolsDirName)
	os.MkdirAll(toolsDir, 0755)

	tool1 := filepath.Join(toolsDir, "helper.sh")
	os.WriteFile(tool1, []byte("#!/bin/sh\necho hi"), 0755)

	tool2 := filepath.Join(toolsDir, "sub", "helper.sh")
	os.MkdirAll(filepath.Dir(tool2), 0755)
	os.WriteFile(tool2, []byte("#!/bin/sh\necho shadow"), 0755)

	discovered, shadowed, err := DiscoverAgentTools(agentDir)
	if err != nil {
		t.Fatalf("DiscoverAgentTools failed: %v", err)
	}

	// Unique tool names should be 1 ("helper.sh"), with 1 shadowing warning
	if len(discovered) != 1 || discovered[0] != "helper.sh" {
		t.Fatalf("unexpected discovered tools: %v", discovered)
	}
	if len(shadowed) != 1 || !strings.Contains(shadowed[0], "shadowed") {
		t.Fatalf("expected 1 shadowing warning, got: %v", shadowed)
	}
}

// deadPIDForTest returns a PID guaranteed gone, so holder liveness has a real negative
// control instead of a made-up number that might be in use.
func deadPIDForTest(t *testing.T) int {
	t.Helper()
	child := exec.Command("sleep", "0")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start child process to reap: %v", err)
	}
	pid := child.Process.Pid
	if err := child.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait for child %d: %v", pid, err)
		}
	}
	if processAlive(pid) {
		t.Skipf("pid %d still alive after being reaped", pid)
	}
	return pid
}

func TestInspectAgentLocks(t *testing.T) {
	wsDir := t.TempDir()

	writeFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	setAge := func(path string, age time.Duration) {
		t.Helper()
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
	agentDir := func(id string) string {
		t.Helper()
		dir := filepath.Join(wsDir, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		writeFile(filepath.Join(dir, "AGENTS.md"), "# "+id+"\n")
		return dir
	}

	activityOnly := agentDir("activity")
	writeFile(filepath.Join(activityOnly, "session.jsonl"), "{\"role\":\"user\"}\n")

	deadHolder := agentDir("deadholder")
	writeFile(filepath.Join(deadHolder, "session.jsonl"), "{\"role\":\"user\"}\n")
	deadPID := deadPIDForTest(t)
	writeFile(filepath.Join(deadHolder, "session.lock"), strconv.Itoa(deadPID)+"\n")
	setAge(filepath.Join(deadHolder, "session.lock"), 45*time.Minute)
	setAge(filepath.Join(deadHolder, "session.jsonl"), 40*time.Minute)

	unreadable := agentDir("unreadable")
	writeFile(filepath.Join(unreadable, "session.lock"), "not-a-pid\n")

	liveHolder := agentDir("liveholder")
	writeFile(filepath.Join(liveHolder, "session.lock"), strconv.Itoa(os.Getpid())+"\n")

	observations, err := InspectAgentLocks(wsDir)
	if err != nil {
		t.Fatalf("InspectAgentLocks: %v", err)
	}
	if len(observations) != 4 {
		t.Fatalf("InspectAgentLocks returned %d observations, want 4: %+v", len(observations), observations)
	}

	byID := map[string]AgentLockObservation{}
	for _, obs := range observations {
		byID[obs.AgentID] = obs
	}

	activity := byID["activity"]
	if activity.LockExists {
		t.Errorf("activity agent reports LockExists without a lock file")
	}
	if !activity.SessionExists {
		t.Errorf("activity agent reports SessionExists false for a session file that exists")
	}
	if time.Since(activity.LastWrite) > time.Minute {
		t.Errorf("activity agent LastWrite = %v, want recently written", activity.LastWrite)
	}

	gone := byID["deadholder"]
	if !gone.LockExists {
		t.Errorf("deadholder LockExists = false")
	}
	if !gone.HolderPIDValid || gone.HolderPID != deadPID {
		t.Errorf("deadholder HolderPID = %d (valid %v), want %d", gone.HolderPID, gone.HolderPIDValid, deadPID)
	}
	if gone.HolderAlive {
		t.Errorf("deadholder HolderAlive = true for reaped pid %d", deadPID)
	}
	if time.Since(gone.LockHeldSince) < 40*time.Minute {
		t.Errorf("deadholder LockHeldSince = %v, want the lock file mtime", gone.LockHeldSince)
	}
	if time.Since(gone.LastWrite) < 40*time.Minute {
		t.Errorf("deadholder LastWrite = %v, want the aged session mtime", gone.LastWrite)
	}

	if unreadable := byID["unreadable"]; unreadable.HolderPIDValid || !unreadable.LockExists {
		t.Errorf("unreadable LockExists = %v, HolderPIDValid = %v, want true and false", unreadable.LockExists, unreadable.HolderPIDValid)
	}

	live := byID["liveholder"]
	if !live.HolderPIDValid || !live.HolderAlive {
		t.Errorf("liveholder HolderAlive = %v (valid %v) for this process' own pid", live.HolderAlive, live.HolderPIDValid)
	}
	if _, err := os.Stat(filepath.Join(liveHolder, "session.lock")); err != nil {
		t.Errorf("lock file disappeared after inspection: %v", err)
	}

	empty, err := InspectAgentLocks(t.TempDir())
	if err != nil {
		t.Fatalf("InspectAgentLocks on an empty workspace: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("InspectAgentLocks on an empty workspace returned %d observations, want 0", len(empty))
	}
}

func TestInspectAgentLocksLeavesLockFilesUntouched(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "holder")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	lockPath := filepath.Join(agentDir, "session.lock")
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if err := os.Chtimes(lockPath, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("chtimes lock file: %v", err)
	}
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	contentsBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}

	if _, err := InspectAgentLocks(wsDir); err != nil {
		t.Fatalf("InspectAgentLocks: %v", err)
	}

	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat lock file after inspection: %v", err)
	}
	contentsAfter, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file after inspection: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("lock file mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}
	if string(contentsBefore) != string(contentsAfter) {
		t.Errorf("lock file contents changed from %q to %q", contentsBefore, contentsAfter)
	}
}

func TestShortenCommandArgsRedactsCredentialFlags(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{
			name: "space separated token value is redacted",
			argv: []string{"/usr/local/bin/wackydiscord", "--token", "MTQzOTAyNjg0NjM0NjM0NjM0Ng.abcdef"},
			want: []string{"wackydiscord", "--token", "[redacted]"},
		},
		{
			name: "equals form token value is redacted",
			argv: []string{"app", "--api-key=sk-live-supersecret"},
			want: []string{"app", "api-key=[redacted]"},
		},
		{
			name: "flag matching is case insensitive",
			argv: []string{"app", "--TOKEN", "abc"},
			want: []string{"app", "--TOKEN", "[redacted]"},
		},
		{
			name: "ordinary arguments pass through up to the limit",
			argv: []string{"wackypub", "--ws", "/home/moltbot/workspace", "workspace", "locks"},
			want: []string{"wackypub", "--ws", "/home/moltbot/workspace"},
		},
		{
			name: "a redaction does not free room for a later argument",
			argv: []string{"app", "--token", "secret-value", "--ws", "/tmp/ws"},
			want: []string{"app", "--token", "[redacted]"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shortenCommandArgs(tc.argv, 3)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("shortenCommandArgs(%v) = %v, want %v", tc.argv, got, tc.want)
			}
			joined := strings.Join(got, " ")
			if strings.Contains(joined, "supersecret") || strings.Contains(joined, "MTQzOTAy") || strings.Contains(joined, "secret-value") {
				t.Fatalf("shortenCommandArgs leaked a credential value: %v", got)
			}
		})
	}

	if got := shortenCommandArgs(nil, 3); got != nil {
		t.Errorf("shortenCommandArgs(nil) = %v, want nil", got)
	}
	if got := shortenCommandArgs([]string{"app", "extra"}, 0); len(got) != 1 || got[0] != "app" {
		t.Errorf("shortenCommandArgs with limit 0 = %v, want just the program name", got)
	}
}
