package cmd

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestCLIWatchWhileSessionLocked verifies symptom 1:
// wackypub agent watch runs completely lock-free, streaming events even while
// an active turn/writer holds the exclusive session lock (D118 read-only reader).
func TestCLIWatchWhileSessionLocked(t *testing.T) {
	wsDir, agentID, agentDir := setupTestWatchAgent(t)

	_ = adkAgent.AppendSessionTurn(agentDir, "user", "turn while locked")

	// Acquire exclusive session lock, simulating an active in-flight turn
	holder, err := adkAgent.AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("failed to acquire session lock: %v", err)
	}
	defer holder.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "watch", agentID, "--raw"})
	out, err := captureStdout(t, func() error {
		return RootCmd.ExecuteContext(ctx)
	})
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	if !strings.Contains(out, "turn while locked") {
		t.Errorf("expected watch output to contain turn while session lock is held, got:\n%s", out)
	}
}

// TestCLISIGINTDuringContendedLockWait verifies symptom 2:
// When a CLI command waits on a contended session lock, sending SIGINT
// cancels the wait and terminates the command within a bounded duration.
func TestCLISIGINTDuringContendedLockWait(t *testing.T) {
	wsDir, agentID, agentDir := setupTestWatchAgent(t)

	// Hold the session lock so any subsequent lock attempt blocks
	holder, err := adkAgent.AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("failed to acquire session lock: %v", err)
	}
	defer holder.Release()

	errCh := make(chan error, 1)
	go func() {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", agentID, "add", "blocked message"})
		errCh <- RootCmd.Execute()
	}()

	// Give the command enough time to attempt the lock and enter the contended polling loop
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	// Send SIGINT to current process; signalCtx(cmd) traps it and cancels the context
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("failed to send SIGINT: %v", err)
	}

	select {
	case err := <-errCh:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected error on contended lock wait interrupted by SIGINT, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context canceled error, got: %v", err)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("SIGINT abort took %v, expected bounded termination under 500ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contended lock wait did not abort on SIGINT within 2s")
	}
}

// TestCLIContextCancelDuringContendedLockWait verifies that passing a cancellable
// context to ExecuteContext aborts a contended lock wait within a bounded duration.
func TestCLIContextCancelDuringContendedLockWait(t *testing.T) {
	wsDir, agentID, agentDir := setupTestWatchAgent(t)

	// Hold the session lock so any subsequent lock attempt blocks
	holder, err := adkAgent.AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("failed to acquire session lock: %v", err)
	}
	defer holder.Release()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		RootCmd.SetArgs([]string{"--ws", wsDir, "agent", agentID, "add", "blocked message"})
		errCh <- RootCmd.ExecuteContext(ctx)
	}()

	// Wait for the command to hit the lock wait loop
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()

	select {
	case err := <-errCh:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected error on cancelled lock wait, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context canceled error, got: %v", err)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("cancel abort took %v, expected bounded termination under 500ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contended lock wait did not abort on context cancel within 2s")
	}
}
