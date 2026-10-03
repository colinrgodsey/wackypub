package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestCLI_SIGINTAbortsContendedAddLock is the watch-lock card regression for symptom 2:
// a CLI command that takes the session lock (add via AddUserTurn) must abort on
// SIGINT even when the lock is contended, instead of force-kill. The CLI wires
// signalCtx into the lock-taking SDK call (was cmdCtx = background, so SIGINT
// never cancelled the LOCK_NB+25ms poll).
func TestCLI_SIGINTAbortsContendedAddLock(t *testing.T) {
	bin := getWackypubBin(t)
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte("*"), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "lockagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// allowlist: self-targeting requires the agent OWN ID in its own allowlist (the
	// child runs with Dir=agentDir, so the walk finds it first).
	if err := os.WriteFile(filepath.Join(agentDir, "WACKYPUB_ALLOWED_AGENTS"), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	// hold the session lock so the add waits contended
	holder, err := adkAgent.AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer holder.Release()

	cmd := exec.Command(bin, "--ws", wsDir, "agent", agentID, "add", "hello")
	cmd.Dir = agentDir
	cmd.Env = append(os.Environ(), "WACKYPUB_ALLOWED_AGENTS=*")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Let it reach the contended lock wait, then SIGINT.
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Errorf("SIGINT took %v to abort contended add lock (must be bounded)", elapsed)
		}
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("SIGINT did not abort the contended add lock - CLI ctx not signal-cancellable")
	}
	// Assert GRACEFUL exit, not signal death: with signalCtx the lock wait returns
	// context.Canceled and the process exits 1; without it (cmdCtx background) SIGINT
	// default-kills the process (signal: interrupt, ExitCode -1). Both terminate, but
	// only signalCtx makes the cancellation flow through the lock path as intended.
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ee.ExitCode() == -1 {
				t.Errorf("add died by signal (exit -1) instead of cancelling the lock wait: CLI ctx not wired to signalCtx")
			}
		}
	}
}
