package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAcquireAndReleaseSessionLock(t *testing.T) {
	tempDir := t.TempDir()

	lock, err := AcquireSessionLock(tempDir)
	if err != nil {
		t.Fatalf("failed to acquire session lock: %v", err)
	}

	lockFile := filepath.Join(tempDir, "session.lock")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("failed to read lock file: %v", err)
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid != os.Getpid() {
		t.Errorf("expected lock PID %d, got %s", os.Getpid(), pidStr)
	}

	lock.Release()
}

// TestAcquireSessionLockContext_CancellableContendedWait pins that a ctx-cancellable
// acquire on a CONTENDED lock abandons the wait promptly when the ctx is cancelled -
// the raw blocking flock could not do this, which broke graceful shutdown (SIGINT and SIGTERM
// become ctx cancellation via signal.NotifyContext in cmd/agent.go).
func TestAcquireSessionLockContext_CancellableContendedWait(t *testing.T) {
	tempDir := t.TempDir()

	holder, err := AcquireSessionLock(tempDir)
	if err != nil {
		t.Fatalf("holder acquire failed: %v", err)
	}
	defer holder.Release()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := AcquireSessionLockContext(ctx, tempDir)
		result <- err
	}()

	// Let the waiter hit the poll loop, then cancel.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected error on cancelled wait, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("cancel took too long: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled wait did not return - lock is not ctx-cancellable")
	}
}

// TestAcquireSessionLockContext_UncontendedImmediate pins that the uncontended acquire
// is immediate (no forced tick delay): a bare acquire on a free lock returns fast.
func TestAcquireSessionLockContext_UncontendedImmediate(t *testing.T) {
	tempDir := t.TempDir()
	start := time.Now()
	lock, err := AcquireSessionLockContext(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("uncontended acquire failed: %v", err)
	}
	defer lock.Release()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("uncontended acquire too slow: %v", elapsed)
	}
}
