package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Naming the holder of a contended session lock. During the Oct 11 forensics the generic
// "held by another process" line was the only breadcrumb, and it named nobody.
var (
	// lockWaitGrace is how long a contended wait must run before the message is upgraded to
	// name the holder, so a fast acquisition never pays for these lookups. A var, not a const,
	// so tests can shorten it.
	lockWaitGrace = 30 * time.Second

	// lockWaitRefreshInterval bounds how often a long wait re-resolves, so a stuck waiter costs
	// at most one lookup per interval instead of one per 25ms tick.
	lockWaitRefreshInterval = 60 * time.Second
)

// clockTicksPerSecond is the USER_HZ that /proc/<pid>/stat reports start times in. It is 100 on
// every Linux architecture this project runs on; if that changed, only the reported age drifts.
const clockTicksPerSecond = 100

// lockWaitHolder is the wait-path view of who owns the flock. cancel.go's sessionLockHolder
// answers "is it held and by what recorded PID" for the CancelTurn path; this one exists to
// describe an owner while we are blocked, where a probe would only ever report EWOULDBLOCK.
type lockWaitHolder struct {
	pid int
	cmd string
	age time.Duration
}

// describe renders the holder clause. Empty means "could not tell", and callers must then leave
// the original message standing rather than guess at an innocent process.
func (h lockWaitHolder) describe() string {
	if h.pid <= 0 {
		return ""
	}
	out := "held by pid " + strconv.Itoa(h.pid)
	if h.cmd != "" {
		out += ", " + h.cmd
	}
	if h.age > 0 {
		out += ", " + formatHolderAge(h.age)
	}
	return out
}

func formatHolderAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// resolveLockWaitHolder names the process holding the flock on lockPath. Everything here reads
// regular files or /proc: it never touches the lock itself, so it cannot add contention, and it
// returns ok=false wherever /proc is unavailable so non-Linux keeps the old wording.
//
// Order matters. /proc/locks is kernel truth and comes first. The PID recorded in the lock file
// is only a hint written at acquisition, so it is trusted solely when the process is live, and
// it is what disambiguates the case where /proc/locks lists several foreign pids because other
// waiters are queued on the same inode. A /proc/*/fd scan is the last resort.
func resolveLockWaitHolder(lockPath string) (lockWaitHolder, bool) {
	hint := lockFilePidHint(lockPath)
	if fromKernel := holderPIDFromProcLocks(lockPath); fromKernel > 0 &&
		(hint == 0 || fromKernel == hint || !processIsLive(fromKernel)) {
		return describePID(fromKernel), true
	}
	if hint > 0 && hint != os.Getpid() && processIsLive(hint) {
		return describePID(hint), true
	}
	if scanned := holderPIDFromOpenFd(lockPath); scanned > 0 {
		return describePID(scanned), true
	}
	if fromKernel := holderPIDFromProcLocks(lockPath); fromKernel > 0 {
		return describePID(fromKernel), true
	}
	return lockWaitHolder{}, false
}

func describePID(pid int) lockWaitHolder {
	return lockWaitHolder{pid: pid, cmd: holderCommandLine(pid), age: processAge(pid)}
}

// lockFilePidHint reads the advisory PID the holder wrote at acquisition. Absent, empty or
// unparseable content means no hint, which is normal for a lock file created but never written.
func lockFilePidHint(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// holderPIDFromProcLocks finds an advisory lock on lockPath's inode owned by another process.
// Blocked waiters appear in /proc/locks too, so the result is a candidate, not a proof; the
// caller weighs it against the PID hint.
func holderPIDFromProcLocks(lockPath string) int {
	info, err := os.Stat(lockPath)
	if err != nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	want := fmt.Sprintf("%02x:%02x:%d", (uint64(st.Dev)>>8)&0xfff, uint64(st.Dev)&0xff, uint64(st.Ino))
	return holderPIDFromProcLocksText(readProcFile("/proc/locks"), want, os.Getpid())
}

// holderPIDFromProcLocksText parses /proc/locks content for an advisory lock on the want token
// (major:minor:inode) owned by a process other than self.
func holderPIDFromProcLocksText(procLocks, want string, self int) int {
	for _, line := range strings.Split(procLocks, "\n") {
		// Row shape, 8 whitespace-separated fields:
		// "1: FLOCK  ADVISORY  WRITE 4123 08:01:2949123 0 EOF"
		// fields[4] is the pid and fields[5] is major:minor:inode.
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[2] != "ADVISORY" || fields[5] != want {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid == self {
			continue
		}
		return pid
	}
	return 0
}

// holderPIDFromOpenFd is the fuser-style fallback for kernels or namespaces where /proc/locks
// does not list the inode.
func holderPIDFromOpenFd(lockPath string) int {
	target, err := filepath.EvalSymlinks(lockPath)
	if err != nil {
		target = lockPath
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() || !e.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name()))
			if err == nil && strings.TrimSuffix(link, " (deleted)") == target {
				return pid
			}
		}
	}
	return 0
}

// holderCommandLine is the full invocation, because "wackypub" alone does not say which agent or
// which command is holding the lock. Deliberately not processProgramName, which is the exe base
// and is what CancelTurn reports.
func holderCommandLine(pid int) string {
	raw := readProcFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if raw == "" {
		return strings.TrimSpace(readProcFile(fmt.Sprintf("/proc/%d/comm", pid)))
	}
	cmd := strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return r == 0 }), " ")
	if len(cmd) > 120 {
		cmd = cmd[:120] + "..."
	}
	return cmd
}

func processAge(pid int) time.Duration {
	stat := readProcFile(fmt.Sprintf("/proc/%d/stat", pid))
	// comm can contain spaces and parentheses, so count fields from the last ')' rather than
	// splitting the line.
	end := strings.LastIndex(stat, ")")
	if end < 0 || end+2 >= len(stat) {
		return 0
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) < 20 {
		return 0
	}
	startTicks, err := strconv.ParseFloat(fields[19], 64) // starttime, field 22 of stat
	if err != nil {
		return 0
	}
	uptimeFields := strings.Fields(readProcFile("/proc/uptime"))
	if len(uptimeFields) == 0 {
		return 0
	}
	uptime, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return 0
	}
	if age := uptime - startTicks/clockTicksPerSecond; age > 0 {
		return time.Duration(age * float64(time.Second))
	}
	return 0
}

func readProcFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
