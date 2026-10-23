package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortLockHoldTimings shrinks the hold-alarm knobs so the alarm path is exercised in
// milliseconds instead of thirty minutes.
func shortLockHoldTimings(t *testing.T) {
	t.Helper()
	checkSave, alarmSave, repeatSave := lockHoldCheckInterval, lockHoldAlarmAfter, lockHoldAlarmRepeat
	lockHoldCheckInterval, lockHoldAlarmAfter, lockHoldAlarmRepeat = 20*time.Millisecond, 50*time.Millisecond, time.Hour
	t.Cleanup(func() {
		lockHoldCheckInterval, lockHoldAlarmAfter, lockHoldAlarmRepeat = checkSave, alarmSave, repeatSave
	})
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

	out := captureLockStderr(t, func() {
		time.Sleep(100 * time.Millisecond)
		checkLockHolds()
	})
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

	out := captureLockStderr(t, func() {
		time.Sleep(100 * time.Millisecond)
		checkLockHolds()
	})
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

	out := captureLockStderr(t, func() {
		time.Sleep(100 * time.Millisecond)
		checkLockHolds()
	})
	if !strings.Contains(out, "WITHOUT an active turn") {
		t.Fatalf("expected an alarm while the lock was held, got %q", out)
	}

	lock.Release()
	out2 := captureLockStderr(t, func() {
		checkLockHolds()
	})
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

	out := captureLockStderr(t, func() {
		time.Sleep(300 * time.Millisecond)
	})
	if !strings.Contains(out, "WITHOUT an active turn") {
		t.Fatalf("the background monitor must speak for a long hold, got %q", out)
	}
}
