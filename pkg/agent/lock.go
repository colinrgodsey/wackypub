package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// SessionLock provides process-level exclusive locking for an agent session.
type SessionLock struct {
	file *os.File
	dir  string
}

var (
	heldLocksMu sync.Mutex
	heldLocks   = make(map[string]int)
)

func cleanLockDir(agentDir string) string {
	abs, err := filepath.Abs(agentDir)
	if err == nil {
		return abs
	}
	return filepath.Clean(agentDir)
}

// IsSessionLockedByCurrentProcess reports whether the current process holds a SessionLock on agentDir.
func IsSessionLockedByCurrentProcess(agentDir string) bool {
	heldLocksMu.Lock()
	defer heldLocksMu.Unlock()
	return heldLocks[cleanLockDir(agentDir)] > 0
}

// shutdownSignalContext is a process-wide context cancelled by SIGINT/SIGTERM, created
// once. Bare lock acquisitions routed through it stay abortable on Ctrl+C even though
// they carry no caller ctx (bugs/wackypub/watch-takes-session-lock-and-ignores-sigint:
// lock acquisition must be context-cancellable everywhere; the old background context
// made a contended wait uninterruptible).
var (
	shutdownOnce sync.Once
	shutdownCtx  context.Context
)

func shutdownSignalContext() context.Context {
	shutdownOnce.Do(func() {
		shutdownCtx, _ = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	})
	return shutdownCtx
}

// AcquireSessionLock acquires an exclusive POSIX lock (flock) on <agent_dir>/session.lock
// with no caller ctx. It is a thin wrapper over AcquireSessionLockContext using a
// process-wide signal-cancelled context (SIGINT/SIGTERM abort the wait); call sites that
// already hold a ctx (all SDK turn paths) should use the ctx variant so a contended lock
// does not hang shutdown or wait on signals they already converted to cancellation.
func AcquireSessionLock(agentDir string) (*SessionLock, error) {
	return AcquireSessionLockContext(shutdownSignalContext(), agentDir)
}

// AcquireSessionLockContext acquires an exclusive POSIX lock (flock) on <agent_dir>/session.lock,
// polling with LOCK_NB so the wait is cancellable. It mirrors wackyacp's D117 pattern
// (internal/session/session.go AcquireLock): a raw blocking flock would never wake when the
// ctx is cancelled, and wackypub converts SIGINT/SIGTERM into ctx cancellation via
// signal.NotifyContext - so on a contended lock a goroutine must be able to abandon the wait
// to make graceful shutdown possible. flock auto-releases on process death, but clean
// shutdown is broken if the wait is uninterruptible. The uncontended first attempt is still
// immediate (first LOCK_NB succeeds without a tick).
func AcquireSessionLockContext(ctx context.Context, agentDir string) (*SessionLock, error) {
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agent directory for lock: %w", err)
	}

	lockPath := filepath.Join(agentDir, "session.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
	}

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	// contended is set on the first EWOULDBLOCK so the wait-visibility line prints ONCE
	// instead of every 25ms tick.
	contended := false
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if contended {
				fmt.Fprintf(os.Stderr, "session.lock: acquired lock on %s after waiting\n", lockPath)
			}
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("failed to acquire flock on %s: %w", lockPath, err)
		}

		if !contended {
			contended = true
			fmt.Fprintf(os.Stderr, "waiting for session.lock on %s (held by another process)\n", lockPath)
		}

		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("acquiring lock on %s: %w", lockPath, ctx.Err())
		case <-ticker.C:
		}
	}

	// Write current PID to lock file
	_ = file.Truncate(0)
	_, _ = file.Seek(0, 0)
	_, _ = file.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	_ = file.Sync()

	clean := cleanLockDir(agentDir)
	heldLocksMu.Lock()
	heldLocks[clean]++
	heldLocksMu.Unlock()

	return &SessionLock{file: file, dir: agentDir}, nil
}

// Release unlocks and closes the session lock file.
func (l *SessionLock) Release() {
	if l != nil && l.file != nil {
		clean := cleanLockDir(l.dir)
		heldLocksMu.Lock()
		if heldLocks[clean] > 1 {
			heldLocks[clean]--
		} else {
			delete(heldLocks, clean)
		}
		heldLocksMu.Unlock()

		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
		l.file = nil
	}
}
