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
	"syscall"
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

// agy's blocking finding: a stale PID hint must never mask a live kernel holder. The case is a
// holder that was killed, leaving its PID in session.lock, while a different process now holds the
// flock. Naming the dead PID sends an operator to a process that died an hour ago.
func TestChooseLockHolderPID(t *testing.T) {
	cases := []struct {
		name     string
		kernel   int
		hint     int
		hintLive bool
		want     int
	}{
		{"stale hint never masks a live kernel holder", 4123, 9999, false, 4123},
		{"agreeing candidates", 4123, 4123, true, 4123},
		{"no hint, kernel only", 4123, 0, false, 4123},
		{"live hint disagreeing with the kernel row wins", 4123, 7777, true, 7777},
		{"no kernel row, live hint", 0, 7777, true, 7777},
		{"nothing known", 0, 0, false, 0},
		{"kernel missing and hint dead names nobody", 0, 9999, false, 0},
	}
	for _, tc := range cases {
		if got := chooseLockHolderPID(tc.kernel, tc.hint, tc.hintLive); got != tc.want {
			t.Errorf("%s: chooseLockHolderPID(%d, %d, %v) = %d, want %d", tc.name, tc.kernel, tc.hint, tc.hintLive, got, tc.want)
		}
	}
}

func TestHolderPIDFromProcLocksTextSkipsNonFlockRows(t *testing.T) {
	const inode = "08:01:2949123"
	self := os.Getpid()
	// A POSIX (fcntl) lock and an OFD lock on the same inode do not block flock(2), so their
	// owners must not be named as the blocker. Only the FLOCK row counts.
	fixture := strings.Join([]string{
		"1: POSIX  ADVISORY  WRITE 2222 " + inode + " 0 EOF",
		"2: OFDLCK ADVISORY  WRITE 3333 " + inode + " 0 EOF",
		"3: FLOCK  ADVISORY  WRITE 4444 " + inode + " 0 EOF",
	}, "\n")
	if got := holderPIDFromProcLocksText(fixture, inode, self); got != 4444 {
		t.Errorf("want the flock owner 4444, got %d", got)
	}

	onlyPosix := "1: POSIX  ADVISORY  WRITE 2222 " + inode + " 0 EOF"
	if got := holderPIDFromProcLocksText(onlyPosix, inode, self); got != 0 {
		t.Errorf("a POSIX-only inode must name nobody, got %d", got)
	}
	onlyOfd := "1: OFDLCK ADVISORY  WRITE 3333 " + inode + " 0 EOF"
	if got := holderPIDFromProcLocksText(onlyOfd, inode, self); got != 0 {
		t.Errorf("an OFD-only inode must name nobody, got %d", got)
	}
}

func TestFdInfoReportsLock(t *testing.T) {
	dir := t.TempDir()

	// Real kernel shapes, sampled on this host: an unlocked fd reports only pos/flags/mnt_id/ino.
	unlocked := filepath.Join(dir, "fdinfo-unlocked")
	if err := os.WriteFile(unlocked, []byte("pos:\t0\nflags:\t02402002\nmnt_id:\t44\nino:\t132794\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "fdinfo-locked")
	if err := os.WriteFile(locked, []byte("pos:\t0\nflags:\t02402002\nmnt_id:\t44\nino:\t132794\nlock:\t1: FLOCK  ADVISORY  WRITE 904706 b3:02:132794 0 EOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	posixOnly := filepath.Join(dir, "fdinfo-posix")
	if err := os.WriteFile(posixOnly, []byte("pos:\t0\nflags:\t02\nmnt_id:\t44\nino:\t1\nlock:\t2: POSIX  ADVISORY  WRITE 5 b3:02:1 0 EOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if fdInfoReportsLock(unlocked) {
		t.Error("an fd with no lock row must not be reported as a holder")
	}
	if !fdInfoReportsLock(locked) {
		t.Error("an fd whose fdinfo names a FLOCK row must be reported as a holder")
	}
	if fdInfoReportsLock(posixOnly) {
		t.Error("a POSIX lock row must not satisfy the flock check")
	}

	// And against the live kernel: a file this test actually flocks must read back as held.
	lockPath := filepath.Join(dir, "session.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fdInfoReportsLock(fmt.Sprintf("/proc/self/fdinfo/%d", f.Fd())) {
		t.Error("the fd is not locked yet, so it must read as unlocked")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Skipf("flock unavailable here: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if !fdInfoReportsLock(fmt.Sprintf("/proc/self/fdinfo/%d", f.Fd())) {
		t.Error("a fd this test holds an exclusive flock on must read as a holder")
	}
}

func TestHolderCommandLineStaysSingleLineAndRedactsSecrets(t *testing.T) {
	argv := []string{
		"/usr/local/bin/wackypub", "agent", "phineas", "prompt",
		"--token", "s3crret-do-not-print",
		"multi\nline\tprompt body",
	}
	got := singleLine(strings.Join(shortenCommandArgs(argv, holderArgvTokenLimit), " "))
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("the holder notice must be one line, got %q", got)
	}
	if strings.Contains(got, "s3crret-do-not-print") {
		t.Errorf("a credential flag value must never be printed, got %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("a redacted credential flag must be visible as such, got %q", got)
	}
	if !strings.Contains(got, "wackypub") || !strings.Contains(got, "phineas") {
		t.Errorf("the notice must still identify the command, got %q", got)
	}
	if !strings.Contains(got, "multi line prompt body") {
		t.Errorf("embedded whitespace must collapse to single spaces, got %q", got)
	}

	// Same guarantees against a real /proc read: a live child whose argv carries both a newline and
	// a credential flag.
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30\n", "holder", "--token", "s3crret-real")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// A child that has not finished exec reports an empty cmdline for a moment, which correctly
	// falls back to comm, so wait for the real argv rather than racing it. A holder we name in
	// production has already held the lock for the whole grace period, so this is a fixture
	// concern only.
	var line string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line = holderCommandLine(cmd.Process.Pid)
		if strings.Contains(line, "sleep") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(line, "sleep") {
		t.Fatalf("never read the child's real argv, last saw %q", line)
	}
	if strings.ContainsAny(line, "\n\r") {
		t.Errorf("a real argv containing a newline must still render one line, got %q", line)
	}
	if strings.Contains(line, "s3crret-real") {
		t.Errorf("real argv must be redacted before it is printed, got %q", line)
	}
	if !strings.Contains(line, "sh") || !strings.Contains(line, "[redacted]") {
		t.Errorf("expected the program name and a redaction marker, got %q", line)
	}
}
