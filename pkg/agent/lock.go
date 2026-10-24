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

	// heldLockSince records when each in-process session lock was first acquired;
	// heldLockAlarmNext records when the hold-time alarm last spoke for it. The alarm is
	// the observability for stdio-serve-holds-session-lock-while-idle: a lock held
	// without an active turn must be visible within minutes, not discovered days later
	// by a dispatcher blocked on it.
	heldLockSince     = make(map[string]time.Time)
	heldLockAlarmNext = make(map[string]time.Time)
)

// Lock-hold observability knobs (vars, not consts, so tests can shorten them).
var (
	// lockHoldCheckInterval is how often the monitor inspects the held locks.
	lockHoldCheckInterval = 30 * time.Second
	// lockHoldAlarmAfter is how long a lock may be held before the alarm speaks. It sits
	// well under the default turn-watchdog bound (3600s) so a wedged turn is reported
	// while it is still cheap to diagnose.
	lockHoldAlarmAfter = 30 * time.Minute
	// lockHoldAlarmRepeat re-speaks a still-holding alarm at most this often, so a wedged
	// hold stays visible without a line per check tick.
	lockHoldAlarmRepeat = 5 * time.Minute
)

var (
	lockHoldMonitorOnce sync.Once
	lockHoldWake        = make(chan struct{}, 1)

	// lockHoldAlarmSink emits a hold-time alarm line. A var (not a direct os.Stderr write) so
	// tests capture the line without swapping the process-wide os.Stderr pointer while the
	// background monitor goroutine is active. Read and write under heldLocksMu.
	lockHoldAlarmSink = func(line string) { fmt.Fprintln(os.Stderr, line) }
)

// startLockHoldMonitor starts the process-wide hold monitor on the first acquisition.
// Acquisitions wake it (lockHoldWake) so a newly wedged hold is checked on the next
// interval rather than whenever the ticker happens to fire.
func startLockHoldMonitor() {
	lockHoldMonitorOnce.Do(func() {
		go func() {
			for {
				heldLocksMu.Lock()
				idle := len(heldLocks) == 0
				interval := lockHoldCheckInterval
				heldLocksMu.Unlock()
				if idle {
					// Park: no locks held, so block on the wake channel with no armed timer; a new
					// acquisition wakes us via lockHoldWake. Zero-idle-wake discipline, #118.
					<-lockHoldWake
					continue
				}
				timer := time.NewTimer(interval)
				select {
				case <-timer.C:
				case <-lockHoldWake:
					timer.Stop()
				}
				checkLockHolds()
			}
		}()
	})
}

func wakeLockHoldMonitor() {
	select {
	case lockHoldWake <- struct{}{}:
	default:
	}
}

// checkLockHolds inspects every lock this process holds and speaks for the ones held
// past lockHoldAlarmAfter. It is exported to the tests only through the package; the
// monitor calls it on its tick.
func checkLockHolds() {
	now := time.Now()
	heldLocksMu.Lock()
	type heldInfo struct {
		dir   string
		since time.Time
		next  time.Time
	}
	var held []heldInfo
	alarmAfter := lockHoldAlarmAfter
	for dir, n := range heldLocks {
		if n > 0 {
			since := heldLockSince[dir]
			if since.IsZero() {
				since = now
			}
			held = append(held, heldInfo{dir: dir, since: since, next: heldLockAlarmNext[dir]})
		}
	}
	heldLocksMu.Unlock()

	for _, h := range held {
		heldFor := now.Sub(h.since)
		if heldFor < alarmAfter {
			continue
		}
		if !h.next.IsZero() && now.Before(h.next) {
			continue
		}
		heldLocksMu.Lock()
		sink := lockHoldAlarmSink
		if _, still := heldLocks[h.dir]; still {
			heldLockAlarmNext[h.dir] = now.Add(lockHoldAlarmRepeat)
		}
		heldLocksMu.Unlock()
		sink(lockHoldAlarmLine(h.dir, h.since, now))
	}
}

// lockHoldAlarmLine renders the alarm. A lock held WITHOUT an active turn is the incident
// signature (held while idle) and says so in plain terms; a lock held WITH a registered
// turn is reported with the turn's age - that is where a wedged turn announces itself
// before the turn watchdog's bound.
func lockHoldAlarmLine(dir string, since, now time.Time) string {
	lockPath := filepath.Join(dir, "session.lock")
	agentID := filepath.Base(dir)
	if active, turnAge := describeInFlightTurn(agentID); active {
		return fmt.Sprintf("session.lock on %s held for %s with an active turn (in flight %s): expected until the turn ends, its context is cancelled, or the turn watchdog fires",
			lockPath, formatHolderAge(now.Sub(since)), formatHolderAge(turnAge))
	}
	return fmt.Sprintf("session.lock on %s held for %s WITHOUT an active turn: the session lock is scoped to turn duration, so a hold like this means a wedged turn or a missing release (holding pid %d)",
		lockPath, formatHolderAge(now.Sub(since)), os.Getpid())
}

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
		// the process owns this context for its whole life, so cancel is never needed early.
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
	var (
		namedHolder string
		holderDue   time.Time
	)
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
			holderDue = time.Now().Add(lockWaitGrace)
		}

		// Only a wait that is already stuck pays for a holder lookup, and then at most once per
		// refresh interval: the hot path stays one LOCK_NB attempt plus a tick.
		if !holderDue.IsZero() && !time.Now().Before(holderDue) {
			holderDue = time.Now().Add(lockWaitRefreshInterval)
			if h, ok := resolveLockWaitHolder(lockPath); ok {
				if desc := h.describe(); desc != "" && desc != namedHolder {
					namedHolder = desc
					fmt.Fprintf(os.Stderr, "waiting for session.lock on %s (%s)\n", lockPath, desc)
				}
			}
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
	// advisory PID hint only: the lock itself is held by the flock, not this file.
	_, _ = file.Seek(0, 0)
	// advisory PID hint only: the lock itself is held by the flock, not this file.
	_, _ = file.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	_ = file.Sync()

	clean := cleanLockDir(agentDir)
	heldLocksMu.Lock()
	if heldLocks[clean] == 0 {
		heldLockSince[clean] = time.Now()
	}
	heldLocks[clean]++
	heldLocksMu.Unlock()
	startLockHoldMonitor()
	wakeLockHoldMonitor()

	return &SessionLock{file: file, dir: agentDir}, nil
}

// Release unlocks and closes the session lock file.
func (l *SessionLock) Release() {
	if l != nil && l.file != nil {
		clean := cleanLockDir(l.dir)
		heldLocksMu.Lock()
		wasLast := false
		if heldLocks[clean] > 1 {
			heldLocks[clean]--
		} else {
			wasLast = true
			delete(heldLocks, clean)
			delete(heldLockSince, clean)
			delete(heldLockAlarmNext, clean)
		}
		heldLocksMu.Unlock()

		if wasLast {
			// Truncate the PID hint before unlocking so an idle long-lived holder (stdio-serve,
			// tcp-serve) never leaves its live PID in session.lock for a waiter's holder-naming
			// to misread in the race window between this release and the next acquire.
			_ = l.file.Truncate(0)
			_, _ = l.file.Seek(0, 0)
		}
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
		l.file = nil
	}
}
