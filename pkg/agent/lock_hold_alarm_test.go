package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortLockHoldTimings shrinks the hold-alarm knobs so the alarm path is exercised in
// milliseconds instead of thirty minutes.
func shortLockHoldTimings(t *testing.T) {
	t.Helper()
	heldLocksMu.Lock()
	checkSave, alarmSave, repeatSave := lockHoldCheckInterval, lockHoldAlarmAfter, lockHoldAlarmRepeat
	lockHoldCheckInterval, lockHoldAlarmAfter, lockHoldAlarmRepeat = 20*time.Millisecond, 50*time.Millisecond, time.Hour
	heldLocksMu.Unlock()
	t.Cleanup(func() {
		heldLocksMu.Lock()
		lockHoldCheckInterval, lockHoldAlarmAfter, lockHoldAlarmRepeat = checkSave, alarmSave, repeatSave
		heldLocksMu.Unlock()
	})
}

// lockHoldCapture accumulates alarm lines emitted by the background monitor so a test can read
// them without racing the monitor goroutine that writes them.
type lockHoldCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *lockHoldCapture) write(line string) {
	c.mu.Lock()
	c.buf.WriteString(line + "\n")
	c.mu.Unlock()
}

func (c *lockHoldCapture) string() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// captureLockHoldAlarms routes the hold-alarm sink to a buffer for the duration of fn and returns
// it, without swapping the process-wide os.Stderr pointer while the background monitor goroutine
// is active.
func captureLockHoldAlarms(t *testing.T, fn func()) *lockHoldCapture {
	t.Helper()
	lc := &lockHoldCapture{}
	heldLocksMu.Lock()
	saved := lockHoldAlarmSink
	lockHoldAlarmSink = lc.write
	heldLocksMu.Unlock()
	t.Cleanup(func() {
		heldLocksMu.Lock()
		lockHoldAlarmSink = saved
		heldLocksMu.Unlock()
	})
	fn()
	time.Sleep(30 * time.Millisecond)
	return lc
}

// Acceptance: a session lock held past the threshold WITHOUT an active turn is the exact
// signature of the stdio-serve incident, so the alarm must name it in plain terms.
func TestLockHoldAlarm_FiresWithoutActiveTurn(t *testing.T) {
	shortLockHoldTimings(t)
	agentDir := filepath.Join(t.TempDir(), "alarmagent")
	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()

	lc := captureLockHoldAlarms(t, func() {
		time.Sleep(100 * time.Millisecond)
		checkLockHolds()
	})
	out := lc.string()
	want := "session.lock on " + filepath.Join(agentDir, "session.lock") + " held for"
	if !strings.Contains(out, want) {
		t.Fatalf("the alarm must fire for a long hold, got %q", out)
	}
	if !strings.Contains(out, "WITHOUT an active turn") {
		t.Errorf("a hold with no active turn must be named as such, got %q", out)
	}
}

// Acceptance: the same hold WITH a registered in-flight turn is reported as an active-turn
// hold (a wedged turn announcing itself before the watchdog bound), not as the no-turn alarm.
func TestLockHoldAlarm_NamesActiveTurn(t *testing.T) {
	shortLockHoldTimings(t)
	agentDir := filepath.Join(t.TempDir(), "turnagent")
	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()

	_, cancel := context.WithCancel(context.Background())
	dereg := registerInFlightTurn("turnagent", cancel)
	defer dereg()

	lc := captureLockHoldAlarms(t, func() {
		time.Sleep(100 * time.Millisecond)
		checkLockHolds()
	})
	out := lc.string()
	if !strings.Contains(out, "with an active turn") {
		t.Errorf("a hold with a registered turn must say so, got %q", out)
	}
	if strings.Contains(out, "WITHOUT an active turn") {
		t.Errorf("a registered turn must not raise the no-turn alarm, got %q", out)
	}
}

// Acceptance: releasing the lock stops the alarm - the registry entry is gone, so nothing
// keeps speaking for a lock nobody holds.
func TestLockHoldAlarm_StopsAfterRelease(t *testing.T) {
	shortLockHoldTimings(t)
	agentDir := filepath.Join(t.TempDir(), "releaseagent")
	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	lc := captureLockHoldAlarms(t, func() {
		time.Sleep(100 * time.Millisecond)
		checkLockHolds()
	})
	out := lc.string()
	if !strings.Contains(out, "WITHOUT an active turn") {
		t.Fatalf("expected an alarm while the lock was held, got %q", out)
	}

	lock.Release()
	out2 := captureLockHoldAlarms(t, func() {
		checkLockHolds()
	}).string()
	if strings.Contains(out2, "session.lock") {
		t.Errorf("the alarm must stop once the lock is released, got %q", out2)
	}
}

// The monitor loop itself: a hold that outlasts the (shortened) threshold is spoken for by
// the background monitor without any explicit check, and acquisitions wake the loop.
func TestLockHoldMonitor_SpeaksForLongHold(t *testing.T) {
	shortLockHoldTimings(t)
	agentDir := filepath.Join(t.TempDir(), "monitoragent")
	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()

	lc := captureLockHoldAlarms(t, func() {
		time.Sleep(300 * time.Millisecond)
	})
	out := lc.string()
	if !strings.Contains(out, "WITHOUT an active turn") {
		t.Fatalf("the background monitor must speak for a long hold, got %q", out)
	}
}
