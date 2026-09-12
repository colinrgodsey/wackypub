package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lockHolderEnv puts this test binary into "hold this lock file forever" mode. It is how
// the cross-process cancel tests get a holder that is genuinely flocking the file and is
// recognisable as this executable.
const lockHolderEnv = "WACKYPUB_TEST_LOCK_HOLDER"

// cancelTestPkgDir is the package source directory captured before any test changes the
// working directory, because the child helper below must not inherit the agent directory
// the parent chdirs into for authorization.
const lockHolderNoFlockEnv = "WACKYPUB_TEST_LOCK_HOLDER_NO_FLOCK"

var cancelTestPkgDir = func() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}()

func TestCancelLockHolderHelper(t *testing.T) {
	lockPath := os.Getenv(lockHolderEnv)
	if lockPath == "" {
		t.Skip("child helper for the cross-process cancel tests; run with " + lockHolderEnv + " set")
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	// The released-lock mode writes the PID of a live process that does not hold the
	// lock, which is what a finished turn leaves behind.
	if os.Getenv(lockHolderNoFlockEnv) == "" {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatalf("flock lock file: %v", err)
		}
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		t.Fatalf("write pid into lock file: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync lock file: %v", err)
	}
	fmt.Printf("%d\n", os.Getpid())

	select {}
}

// startLockHolder spawns the helper and waits until it reports its PID, which means the
// lock is held and the file records that PID.
// lockHolderHandle is a running child that holds an agent's session lock. Exactly one
// goroutine ever calls Wait on it, so cleanup and assertions cannot race on the
// exec.Cmd.
type lockHolderHandle struct {
	pid    int
	reaped <-chan struct{}
}

// startLockHolder spawns the helper and waits until it reports its PID, which means the
// lock is held and the lock file records that PID.
func startLockHolder(t *testing.T, lockPath string) lockHolderHandle {
	t.Helper()
	return startLockHolderMode(t, lockPath, false)
}

func startLockHolderMode(t *testing.T, lockPath string, released bool) lockHolderHandle {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCancelLockHolderHelper")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+lockPath)
	if released {
		cmd.Env = append(cmd.Env, lockHolderNoFlockEnv+"=1")
	}
	cmd.Dir = cancelTestPkgDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}

	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-reaped:
		case <-time.After(10 * time.Second):
			t.Errorf("lock holder pid %d was never reaped", cmd.Process.Pid)
		}
	})

	lines := make(chan string, 1)
	go func() {
		line, _, err := bufio.NewReader(stdout).ReadLine()
		if err != nil {
			lines <- ""
			return
		}
		lines <- string(line)
	}()

	select {
	case line := <-lines:
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("lock holder reported %q, want a PID", line)
		}
		return lockHolderHandle{pid: pid, reaped: reaped}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for lock holder to report its PID")
		return lockHolderHandle{}
	}
}

// newCancelTestWorkspace builds a workspace whose agent directory the test process is
// authorized to act on, and leaves the process inside it the way the CLI would run.
func newCancelTestWorkspace(t *testing.T, agentID string) (string, string) {
	t.Helper()
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Cancel test agent"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("write allowed agents: %v", err)
	}

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir to agent dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origCwd); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})
	return wsDir, agentDir
}

func TestCancelAgentTurn_NothingInFlight(t *testing.T) {
	wsDir, _ := newCancelTestWorkspace(t, "idleagent")

	result, err := NewSDK(wsDir).CancelAgentTurn("idleagent")
	if err == nil {
		t.Fatalf("CancelAgentTurn with no lock file returned %+v, want an error", result)
	}
	if !strings.Contains(err.Error(), `no in-flight turn for agent "idleagent"`) {
		t.Fatalf("error = %v, want the no-in-flight-turn error", err)
	}
}

// TestCancelAgentTurn_StaleLockFileIsNotSignalled is the PID-recycling safety net: a live
// but unrelated process whose PID sits in an unheld lock file must survive the call.
func TestCancelAgentTurn_StaleLockFileIsNotSignalled(t *testing.T) {
	wsDir, agentDir := newCancelTestWorkspace(t, "staleagent")

	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Skipf("cannot start bystander process: %v", err)
	}
	bystanderPID := bystander.Process.Pid
	t.Cleanup(func() {
		_ = bystander.Process.Kill()
		_, _ = bystander.Process.Wait()
	})

	lockPath := filepath.Join(agentDir, sessionLockFileName)
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(bystanderPID)+"\n"), 0644); err != nil {
		t.Fatalf("write stale lock file: %v", err)
	}

	result, err := NewSDK(wsDir).CancelAgentTurn("staleagent")
	if err == nil {
		t.Fatalf("CancelAgentTurn against a stale lock file returned %+v, want a refusal", result)
	}
	if !strings.Contains(err.Error(), "no in-flight turn") {
		t.Fatalf("error = %v, want the no-in-flight-turn error", err)
	}
	if !processIsLive(bystanderPID) {
		t.Fatalf("bystander pid %d was killed by a cancel that found an unheld lock file", bystanderPID)
	}
}

// TestCancelAgentTurn_ReleasedLockByRecognisableProcessIsNotSignalled is the PID-recycle
// case that matters: the lock file records the PID of a live, perfectly recognisable
// wackypub-shaped process, but nobody holds the lock, so there is no turn to cancel and
// nothing may be signalled.
func TestCancelAgentTurn_ReleasedLockByRecognisableProcessIsNotSignalled(t *testing.T) {
	wsDir, agentDir := newCancelTestWorkspace(t, "finishedagent")

	lockPath := filepath.Join(agentDir, sessionLockFileName)
	handle := startLockHolderMode(t, lockPath, true)

	result, err := NewSDK(wsDir).CancelAgentTurn("finishedagent")
	if err == nil {
		t.Fatalf("CancelAgentTurn against a released lock returned %+v, want a refusal", result)
	}
	if !strings.Contains(err.Error(), "no in-flight turn") {
		t.Fatalf("error = %v, want the no-in-flight-turn error", err)
	}
	if !strings.Contains(err.Error(), "leftover") {
		t.Fatalf("error = %v, want it to explain the PID is leftover metadata", err)
	}
	if !processIsLive(handle.pid) {
		t.Fatalf("pid %d was signalled even though it did not hold the lock", handle.pid)
	}
}

func TestCancelAgentTurn_ExternalHolderIsSignalled(t *testing.T) {
	wsDir, agentDir := newCancelTestWorkspace(t, "busyagent")

	lockPath := filepath.Join(agentDir, sessionLockFileName)
	holder := startLockHolder(t, lockPath)
	holderPID := holder.pid

	result, err := NewSDK(wsDir).CancelAgentTurn("busyagent")
	if err != nil {
		t.Fatalf("CancelAgentTurn: %v", err)
	}
	if result.Scope != CancelScopeExternalProcess {
		t.Fatalf("Scope = %q, want %q", result.Scope, CancelScopeExternalProcess)
	}
	if result.PID != holderPID {
		t.Fatalf("PID = %d, want the lock holder %d", result.PID, holderPID)
	}

	select {
	case <-holder.reaped:
	case <-time.After(10 * time.Second):
		t.Fatalf("lock holder pid %d is still alive after cancellation", holderPID)
	}
	if processIsLive(holderPID) {
		t.Fatalf("lock holder pid %d still exists after exiting", holderPID)
	}
}

func TestCancelAgentTurn_Unauthorized(t *testing.T) {
	wsDir, agentDir := newCancelTestWorkspace(t, "guardedagent")

	// The lock file exists and is held, so a missing authorization cannot hide behind
	// "nothing is running": the gate has to fire first.
	startLockHolder(t, filepath.Join(agentDir, sessionLockFileName))
	if err := os.Remove(filepath.Join(agentDir, AllowedAgentsFile)); err != nil {
		t.Fatalf("remove allowlist: %v", err)
	}
	if err := os.Remove(filepath.Join(wsDir, AllowedAgentsFile)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove workspace allowlist: %v", err)
	}

	_, err := NewSDK(wsDir).CancelAgentTurn("guardedagent")
	if err == nil {
		t.Fatal("CancelAgentTurn succeeded without an allowlist, want an authorization error")
	}
	if !strings.Contains(err.Error(), "not authorized") && !strings.Contains(err.Error(), AllowedAgentsFile) {
		t.Fatalf("error = %v, want an authorization error naming %s", err, AllowedAgentsFile)
	}
}

func TestCancelAgentTurn_InProcessTurn(t *testing.T) {
	wsDir, agentDir := newCancelTestWorkspace(t, "inprocagent")

	requestStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer srv.Close()

	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}

	sdk := NewSDK(wsDir)
	streamDone := make(chan error, 1)
	go func() {
		for _, err := range sdk.AddAndGenerateTurnStream(context.Background(), "inprocagent", "Hello") {
			if err != nil {
				streamDone <- err
				return
			}
		}
		streamDone <- nil
	}()

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the mock model request to start")
	}

	result, err := sdk.CancelAgentTurn("inprocagent")
	if err != nil {
		t.Fatalf("CancelAgentTurn: %v", err)
	}
	if result.Scope != CancelScopeInProcess {
		t.Fatalf("Scope = %q, want %q for a turn running in this process", result.Scope, CancelScopeInProcess)
	}

	select {
	case err := <-streamDone:
		if err == nil {
			t.Fatal("cancelled stream ended without an error")
		}
		if !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("cancelled stream error = %v, want a context cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the cancelled stream to stop")
	}
}
