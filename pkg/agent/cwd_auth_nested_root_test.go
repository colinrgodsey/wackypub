package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bypass these tests pin: authorization walked up from the working directory
// and a WACKYPUB_ROOT marker ended the walk with a grant, so a workspace root
// planted anywhere - inside another agent's subtree, or a scratch workspace aimed
// at the real one with --ws - outranked every roster. Reported in
// security-cwd-derived-agent-authorization-bypass-nested-workspace-roots with the
// matched-control setup reproduced below.

func writeNestedFixture(t *testing.T) (wsDir, victimDir, nestedDir string) {
	t.Helper()
	wsDir = t.TempDir()

	victimDir = filepath.Join(wsDir, "victim")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victimDir, "AGENTS.md"), []byte("# Victim\n"), 0o644); err != nil {
		t.Fatalf("write victim AGENTS.md: %v", err)
	}

	hostDir := filepath.Join(wsDir, "host")
	nestedDir = filepath.Join(hostDir, "scratch", "fake-ws")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0o644); err != nil {
		t.Fatalf("write outer root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, RootMarkerFile), []byte(""), 0o644); err != nil {
		t.Fatalf("write nested root: %v", err)
	}
	return wsDir, victimDir, nestedDir
}

func clearActiveWorkspace(t *testing.T) {
	t.Helper()
	SetActiveWorkspaceDir("")
	t.Cleanup(func() { SetActiveWorkspaceDir("") })
}

// Standing inside the nested root must not authorize the victim, whether or not
// the process recorded which workspace it operates on.
func TestPhoebeNestedWorkspaceRootGrantsNothing(t *testing.T) {
	wsDir, victimDir, nestedDir := writeNestedFixture(t)
	clearActiveWorkspace(t)
	t.Chdir(nestedDir)
	if err := AuthorizeAgentTarget(filepath.Base(victimDir)); err != nil {
		t.Fatalf("control: with no recorded workspace the walk should reach the operator path, got: %v", err)
	}
	SetActiveWorkspaceDir(wsDir)
	if err := AuthorizeAgentTarget(filepath.Base(victimDir)); err == nil {
		t.Fatalf("nested workspace root authorized cross-agent access while operating on workspace %q", wsDir)
	} else if !strings.Contains(err.Error(), "nested") {
		t.Errorf("expected the denial to name the nesting, got: %v", err)
	}
}

// An empty argument means "not recorded", not the current directory: filepath.Clean("")
// would otherwise leave "." behind, which reads as a recorded workspace and denies every
// caller whose root marker is an absolute path.
func TestPhoebeSetActiveWorkspaceDirTreatsEmptyAsUnset(t *testing.T) {
	clearActiveWorkspace(t)
	if got := ActiveWorkspaceDir(); got != "" {
		t.Fatalf("expected an unset active workspace, got %q", got)
	}
}

// The matched control: the same target, reached from a real agent directory, must
// still be governed by that agent's roster in both directions - listed passes,
// unlisted fails - and the marker must not change either answer.
func TestPhoebeRosterStillGovernsFromARealAgentDir(t *testing.T) {
	wsDir, victimDir, _ := writeNestedFixture(t)
	hostDir := filepath.Join(wsDir, "host")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("mkdir host: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "AGENTS.md"), []byte("# Host\n"), 0o644); err != nil {
		t.Fatalf("write host AGENTS.md: %v", err)
	}

	clearActiveWorkspace(t)
	SetActiveWorkspaceDir(wsDir)

	if err := os.WriteFile(filepath.Join(hostDir, AllowedAgentsFile), []byte("nobody\n"), 0o644); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	t.Chdir(hostDir)
	err := AuthorizeAgentTarget(filepath.Base(victimDir))
	if err == nil {
		t.Fatal("expected the roster to deny a target it does not list")
	}
	if !strings.Contains(err.Error(), "is not in") {
		t.Errorf("expected the roster denial, got: %v", err)
	}

	if err := os.WriteFile(filepath.Join(hostDir, AllowedAgentsFile), []byte(filepath.Base(victimDir)+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite roster: %v", err)
	}
	if err := AuthorizeAgentTarget(filepath.Base(victimDir)); err != nil {
		t.Errorf("a roster that lists the target must still authorize it, got: %v", err)
	}
}

// A scratch workspace that is not nested anywhere, aimed at the real workspace
// with --ws, is the other half of the bypass: its root marker must not authorize
// targets that live in the workspace being operated on.
func TestPhoebeScratchWorkspaceRootCannotReachAnotherWorkspace(t *testing.T) {
	realWs := t.TempDir()
	if err := os.WriteFile(filepath.Join(realWs, RootMarkerFile), []byte(""), 0o644); err != nil {
		t.Fatalf("write real root: %v", err)
	}
	victim := filepath.Join(realWs, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, "AGENTS.md"), []byte("# Victim\n"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, RootMarkerFile), []byte(""), 0o644); err != nil {
		t.Fatalf("write scratch root: %v", err)
	}

	clearActiveWorkspace(t)
	t.Chdir(scratch)

	SetActiveWorkspaceDir(realWs)
	if err := AuthorizeAgentTarget(filepath.Base(victim)); err == nil {
		t.Fatal("standing in an unrelated workspace root authorized a target in another workspace")
	}

	// Control: the same caller operating on its own workspace is the local
	// operator case and stays permitted.
	SetActiveWorkspaceDir(scratch)
	if err := AuthorizeAgentTarget(filepath.Base(victim)); err != nil {
		t.Errorf("operating on the workspace the caller is standing in must stay permitted, got: %v", err)
	}
}

// Behavior deliberately unchanged: a caller with no agent directory and no marker
// anywhere is still treated as a local operator. Closing this needs a harness-side
// caller identity, not a working-directory heuristic.
func TestPhoebeUnmarkedDirectoryRemainsOperator(t *testing.T) {
	dir := t.TempDir()
	clearActiveWorkspace(t)
	t.Chdir(dir)
	if err := AuthorizeAgentTarget("someone"); err != nil {
		t.Errorf("expected the unchanged operator path, got: %v", err)
	}
}

// Behavior deliberately unchanged: an agent directory with no roster at all is
// denied rather than inherited from the workspace.
func TestPhoebeAgentDirWithoutRosterStaysDenied(t *testing.T) {
	wsDir, _, _ := writeNestedFixture(t)
	lonely := filepath.Join(wsDir, "lonely")
	if err := os.MkdirAll(lonely, 0o755); err != nil {
		t.Fatalf("mkdir lonely: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lonely, "AGENTS.md"), []byte("# Lonely\n"), 0o644); err != nil {
		t.Fatalf("write lonely: %v", err)
	}

	clearActiveWorkspace(t)
	SetActiveWorkspaceDir(wsDir)
	t.Chdir(lonely)
	if err := AuthorizeAgentTarget("victim"); err == nil {
		t.Fatal("an agent directory with no roster must not inherit authorization")
	}
}

// The walk must not stop at a planted root: the roster that governs is the one further out
// on the real path. Stopping at the planted root would resolve the caller as an
// unknown local operator, which is how the bypass reached a target it was never
// granted.
func TestPhoebeWalkContinuesPastAPlantedRootToTheRealRoster(t *testing.T) {
	wsDir, victimDir, nestedDir := writeNestedFixture(t)
	hostDir := filepath.Dir(filepath.Dir(nestedDir))
	if err := os.WriteFile(filepath.Join(hostDir, "AGENTS.md"), []byte("# Host\n"), 0o644); err != nil {
		t.Fatalf("write host AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, AllowedAgentsFile), []byte("nobody\n"), 0o644); err != nil {
		t.Fatalf("write host roster: %v", err)
	}
	_ = wsDir
	clearActiveWorkspace(t)
	t.Chdir(nestedDir)
	err := AuthorizeAgentTarget(filepath.Base(victimDir))
	if err == nil {
		t.Fatal("a planted root with no roster resolved as an operator instead of falling through to the governing roster")
	}
	if !strings.Contains(err.Error(), "is not in") {
		t.Errorf("expected the governing roster to be the reason, got: %v", err)
	}
}
