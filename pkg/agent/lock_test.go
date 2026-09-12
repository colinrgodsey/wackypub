package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

func TestTryAcquireSessionLock_Busy(t *testing.T) {
	tempDir := t.TempDir()

	lock1, err := AcquireSessionLock(tempDir)
	if err != nil {
		t.Fatalf("failed to acquire initial lock: %v", err)
	}
	defer lock1.Release()

	lock2, err := TryAcquireSessionLock(tempDir)
	if err == nil {
		lock2.Release()
		t.Fatalf("expected error from TryAcquireSessionLock when locked, got nil")
	}
	if !strings.Contains(err.Error(), "agent session is busy") {
		t.Errorf("expected busy error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("expected holder PID %d in error message, got: %v", os.Getpid(), err)
	}

	lock1.Release()

	lock3, err := TryAcquireSessionLock(tempDir)
	if err != nil {
		t.Fatalf("expected TryAcquireSessionLock to succeed after release, got: %v", err)
	}
	lock3.Release()
}

func TestAcquireGitCommitLock(t *testing.T) {
	tempDir := t.TempDir()

	lock, err := AcquireGitCommitLock(tempDir)
	if err != nil {
		t.Fatalf("failed to acquire git commit lock: %v", err)
	}

	lockFile := filepath.Join(tempDir, ".git.lock")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("expected .git.lock file to exist: %v", err)
	}

	lock.Release()
}
