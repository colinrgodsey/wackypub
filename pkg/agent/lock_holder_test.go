package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHolderPIDFromProcLocksText(t *testing.T) {
	const inode = "08:01:2949123"
	self := os.Getpid()
	fixture := strings.Join([]string{
		"1: FLOCK  ADVISORY  WRITE 4123 08:01:111 0 EOF",
		"2: FLOCK  ADVISORY  WRITE 5555 " + inode + " 0 EOF",
		"3: FLOCK  ADVISORY  READ  " + strconv.Itoa(self) + " " + inode + " 0 EOF",
		"4: POSIX  ADVISORY  WRITE 6666 " + inode + " 0 EOF",
	}, "\n")

	if got := holderPIDFromProcLocksText(fixture, inode, self); got != 5555 {
		t.Errorf("want the foreign flock owner 5555, got %d", got)
	}
	if got := holderPIDFromProcLocksText(fixture, "ff:ff:1", self); got != 0 {
		t.Errorf("unrelated inode must resolve to nobody, got %d", got)
	}
	// A row that is only ourselves must not name us as our own blocker.
	onlySelf := "1: FLOCK  ADVISORY  WRITE " + strconv.Itoa(self) + " " + inode + " 0 EOF"
	if got := holderPIDFromProcLocksText(onlySelf, inode, self); got != 0 {
		t.Errorf("self must never be reported as the holder, got %d", got)
	}
	if got := holderPIDFromProcLocksText("", inode, self); got != 0 {
		t.Errorf("empty /proc/locks must resolve to nobody, got %d", got)
	}
}

func TestFormatHolderAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{38 * time.Second, "38s"},
		{45 * time.Minute, "45m"},
		{2*time.Hour + 13*time.Minute, "2h13m"},
	}
	for _, tc := range cases {
		if got := formatHolderAge(tc.in); got != tc.want {
			t.Errorf("formatHolderAge(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if (lockWaitHolder{}).describe() != "" {
		t.Error("an unresolved holder must describe as nothing, so the old message stands")
	}
	if got := (lockWaitHolder{pid: 12345, cmd: "wackypub agent phineas prompt", age: 45 * time.Minute}).describe(); got != "held by pid 12345, wackypub agent phineas prompt, 45m" {
		t.Errorf("unexpected rendering: %q", got)
	}
}

// captureLockStderr redirects os.Stderr to a temp file for the duration of fn and returns it.
func captureLockStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("creating stderr capture: %v", err)
	}
	saved := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = saved }()
	fn()
	if err := f.Sync(); err != nil {
		t.Fatalf("syncing stderr capture: %v", err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("reading stderr capture: %v", err)
	}
	return string(data)
}

// holdLockElsewhere starts a separate process that flocks <dir>/session.lock and reports its
// pid once the flock is held. Returns the pid and a stop function.
func holdLockElsewhere(t *testing.T, dir string, holdFor time.Duration) (int, func()) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available to hold a lock in another process")
	}
	lockPath := filepath.Join(dir, sessionLockFileName)
	script := `import fcntl, os, sys, time
f = open(sys.argv[1], "a+")
fcntl.flock(f, fcntl.LOCK_EX)
print(os.getpid(), flush=True)
time.sleep(float(sys.argv[2]))
`
	cmd := exec.Command("python3", "-c", script, lockPath, fmt.Sprintf("%.1f", holdFor.Seconds()))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting lock holder: %v", err)
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(stop)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("holder never reported its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parsing holder pid from %q: %v", line, err)
	}
	return pid, stop
}

func shortLockWaitTimings(t *testing.T) {
	t.Helper()
	graceSave, refreshSave := lockWaitGrace, lockWaitRefreshInterval
	lockWaitGrace = 100 * time.Millisecond
	lockWaitRefreshInterval = 200 * time.Millisecond
	t.Cleanup(func() { lockWaitGrace, lockWaitRefreshInterval = graceSave, refreshSave })
}

// Acceptance: a stuck wait says who is in the way, naming the pid and the invocation, because
// "held by another process" was the only breadcrumb available during an incident.
func TestLockWaitNamesHolderAfterGrace(t *testing.T) {
	shortLockWaitTimings(t)
	dir := t.TempDir()
	pid, stop := holdLockElsewhere(t, dir, 2*time.Second)

	out := captureLockStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lock, err := AcquireSessionLockContext(ctx, dir)
		if err != nil {
			t.Errorf("acquiring after the holder released: %v", err)
			return
		}
		lock.Release()
		stop()
	})

	if !strings.Contains(out, "waiting for session.lock on") {
		t.Fatalf("a contended wait must still say it is waiting, got %q", out)
	}
	if !strings.Contains(out, "held by pid "+strconv.Itoa(pid)) {
		t.Errorf("the wait must name the holder pid %d, got %q", pid, out)
	}
	if !strings.Contains(out, "python3") {
		t.Errorf("the wait must name the holder command, got %q", out)
	}
}

// Acceptance: the grace timer must not fire on acquisitions that resolve quickly, so the common
// contended-but-brief case keeps its original single line and pays no /proc lookups.
func TestLockWaitStaysQuietBeforeGrace(t *testing.T) {
	graceSave, refreshSave := lockWaitGrace, lockWaitRefreshInterval
	lockWaitGrace = 30 * time.Second
	lockWaitRefreshInterval = 50 * time.Millisecond
	t.Cleanup(func() { lockWaitGrace, lockWaitRefreshInterval = graceSave, refreshSave })

	dir := t.TempDir()
	_, stop := holdLockElsewhere(t, dir, 30*time.Second)
	go func() {
		time.Sleep(150 * time.Millisecond)
		stop()
	}()

	out := captureLockStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lock, err := AcquireSessionLockContext(ctx, dir)
		if err != nil {
			t.Errorf("acquiring after the holder released: %v", err)
			return
		}
		lock.Release()
	})

	if strings.Contains(out, "held by pid") {
		t.Errorf("a wait shorter than the grace period must not name a holder, got %q", out)
	}
	if !strings.Contains(out, "waiting for session.lock on") {
		t.Errorf("the original contended line must still print, got %q", out)
	}
}

func TestFastUncontendedAcquisitionIsSilent(t *testing.T) {
	graceSave := lockWaitGrace
	lockWaitGrace = 0
	t.Cleanup(func() { lockWaitGrace = graceSave })
	dir := t.TempDir()
	out := captureLockStderr(t, func() {
		lock, err := AcquireSessionLockContext(context.Background(), dir)
		if err != nil {
			t.Fatalf("uncontended acquire: %v", err)
		}
		lock.Release()
	})
	if out != "" {
		t.Errorf("an uncontended acquisition must print nothing, got %q", out)
	}
}
