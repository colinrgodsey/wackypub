package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	// ErrSessionBusy indicates that the agent session lock is held by another process.
	ErrSessionBusy = errors.New("agent session is busy: lock held by another process")
)

// SessionLock provides process-level exclusive locking for an agent session.
type SessionLock struct {
	file *os.File
}

// AcquireSessionLock acquires an exclusive POSIX lock (flock) on <agent_dir>/session.lock.
// It writes the current process PID to the lock file for diagnostic visibility.
func AcquireSessionLock(agentDir string) (*SessionLock, error) {
	return acquireLock(agentDir, "session.lock", syscall.LOCK_EX)
}

// TryAcquireSessionLock attempts to acquire an exclusive POSIX lock (flock) non-blockingly (LOCK_EX | LOCK_NB).
// If the lock is already held by another process, it returns ErrSessionBusy with holder PID if available.
func TryAcquireSessionLock(agentDir string) (*SessionLock, error) {
	return acquireLock(agentDir, "session.lock", syscall.LOCK_EX|syscall.LOCK_NB)
}

// AcquireGitCommitLock acquires an exclusive POSIX lock (flock) on <repoDir>/.git.lock to serialize
// workspace stage-and-commit operations across concurrent agents sharing a repository.
func AcquireGitCommitLock(repoDir string) (*SessionLock, error) {
	return acquireLock(repoDir, ".git.lock", syscall.LOCK_EX)
}

func acquireLock(dir string, filename string, flags int) (*SessionLock, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory for lock: %w", err)
	}

	lockPath := filepath.Join(dir, filename)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
	}

	if err := syscall.Flock(int(file.Fd()), flags); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			holderPID := ""
			if pidBytes, rErr := os.ReadFile(lockPath); rErr == nil {
				holderPID = strings.TrimSpace(string(pidBytes))
			}
			_ = file.Close()
			if holderPID != "" {
				return nil, fmt.Errorf("%w (held by PID %s)", ErrSessionBusy, holderPID)
			}
			return nil, ErrSessionBusy
		}
		_ = file.Close()
		return nil, fmt.Errorf("failed to acquire flock on %s: %w", lockPath, err)
	}

	// Write current PID to lock file
	_ = file.Truncate(0)
	_, _ = file.Seek(0, 0)
	_, _ = file.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	_ = file.Sync()

	return &SessionLock{file: file}, nil
}

// Release unlocks and closes the session lock file.
func (l *SessionLock) Release() {
	if l != nil && l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
		l.file = nil
	}
}
