package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Cancellation scopes reported by CancelAgentTurn.
const (
	// CancelScopeInProcess means the turn was running in this process and its
	// registered context cancel function was called, which is D85's mechanism.
	CancelScopeInProcess = "in-process"
	// CancelScopeExternalProcess means the turn was running in another process, which
	// received SIGTERM on the strength of holding the agent's session lock.
	CancelScopeExternalProcess = "external-process"
)

// sessionLockFileName is the lock file AcquireSessionLock creates; it is also where a
// running turn records the holder PID for exactly this diagnostic.
const sessionLockFileName = "session.lock"

// TurnCancellation reports which turn a cancellation request reached.
type TurnCancellation struct {
	AgentID string
	// Scope is CancelScopeInProcess or CancelScopeExternalProcess.
	Scope string
	// PID is the process that was signalled, set only for an external turn.
	PID int
	// Program is the holder's program name, for the acknowledgement line. Only the
	// executable name is reported, never the full argv, because wackypub invocations
	// carry whole prompts and sometimes credentials in their arguments.
	Program string
}

// CancelAgentTurn requests cancellation of the turn now running for agentID, in this
// process or in another one, and reports which it reached.
//
// Authorization is ValidateAgentTarget, the same gate prompt, add, and generate use.
// Cancelling ends another agent's work, so it is a mutating call and not exempt the way
// read-only diagnostics (InspectAgentDir, D16) are.
//
// An in-process turn is cancelled through the D85 registry via CancelTurn. A turn in
// another process cannot be reached through that map, and nothing else identifies it:
// what an outside process can act on is the session lock, which every turn holds for its
// whole duration and which records the holder PID. So the external path asks whether the
// lock is genuinely held and, only then, sends SIGTERM to the holder. SIGTERM is what the
// CLI already treats as a stop request (cmd/agent.go's signal context traps SIGINT and
// SIGTERM), so a cancelled remote turn unwinds through exactly the same context
// cancellation a operator gets from Ctrl-C in that terminal.
//
// Trusting the PID written in the lock file on its own would be unsafe: releasing a lock
// leaves the file in place, so the number in it is often a long-exited process, and any
// PID can be recycled onto an unrelated victim. Two independent facts must agree before
// anything is signalled: flock is currently held, so a live holder exists, and that
// holder is a wackypub process or this same executable. When they disagree the call
// refuses and says what it saw.
//
// Lock state is probed with LOCK_EX|LOCK_NB, which never queues: if the probe succeeds,
// nobody holds the lock and it is closed again immediately. That probe briefly takes the
// very short path a waiting turn might be blocked on, which is the whole cost of it.
//
// What a cancelled turn leaves behind: cancellation is observed at the existing
// context.Err() checkpoints in the turn loop, all of which are before the assistant
// turn-boundary commit, so the partial assistant text is not committed. The user message
// that started the turn stays in session.jsonl, because AddAndGenerateTurnStream appends
// it before generating, under the same lock. The lock is released by the deferred Release,
// so the session is immediately usable again instead of wedged.
func (s *AgentSDK) CancelAgentTurn(agentID string) (TurnCancellation, error) {
	if agentID == "" {
		return TurnCancellation{}, fmt.Errorf("agentID cannot be empty")
	}
	if _, err := ValidateAgentTarget(agentID); err != nil {
		return TurnCancellation{}, err
	}

	if err := s.CancelTurn(agentID); err == nil {
		return TurnCancellation{AgentID: agentID, Scope: CancelScopeInProcess}, nil
	}

	holder, err := sessionLockHolder(s.AgentDir(agentID))
	if err != nil {
		return TurnCancellation{}, err
	}
	if !holder.held {
		return TurnCancellation{AgentID: agentID}, fmt.Errorf("no in-flight turn for agent %q (%s)", agentID, holder.detail())
	}
	if !holder.recognized {
		return TurnCancellation{AgentID: agentID}, fmt.Errorf("refusing to cancel agent %q: its session lock is held by pid %d (%s), which is not a wackypub process - inspect it with `wackypub workspace locks` before signalling it by hand",
			agentID, holder.pid, holder.program)
	}
	if err := syscall.Kill(holder.pid, syscall.SIGTERM); err != nil {
		return TurnCancellation{AgentID: agentID}, fmt.Errorf("failed to send SIGTERM to pid %d holding agent %q's session lock: %w", holder.pid, agentID, err)
	}
	return TurnCancellation{AgentID: agentID, Scope: CancelScopeExternalProcess, PID: holder.pid, Program: holder.program}, nil
}

// lockHolder is who currently holds an agent's session lock.
type lockHolder struct {
	held       bool
	pid        int
	pidKnown   bool
	program    string
	recognized bool
	lockAbsent bool
}

func (h lockHolder) detail() string {
	if h.lockAbsent {
		return "no session.lock file, so nothing is holding the session"
	}
	if !h.pidKnown {
		return "session.lock is not held and records no usable PID"
	}
	return fmt.Sprintf("session.lock is not held; the PID %d recorded in it is leftover from a finished turn", h.pid)
}

// sessionLockHolder reports whether agentDir's session lock is currently held and, if so,
// who holds it. It reads the lock file without creating, truncating, or writing it.
func sessionLockHolder(agentDir string) (lockHolder, error) {
	var holder lockHolder
	f, err := os.OpenFile(filepath.Join(agentDir, sessionLockFileName), os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			holder.lockAbsent = true
			return holder, nil
		}
		return holder, fmt.Errorf("failed to open session lock in %s: %w", agentDir, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close probed session lock in %s: %v\n", agentDir, err)
		}
	}()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			holder.held = true
		} else {
			return holder, fmt.Errorf("failed to probe session lock in %s: %w", agentDir, err)
		}
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return holder, fmt.Errorf("failed to read session lock in %s: %w", agentDir, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return holder, nil
	}
	holder.pid, holder.pidKnown = pid, true
	holder.program = processProgramName(pid)
	if holder.held {
		holder.recognized = processIsCancelTarget(pid)
	}
	return holder, nil
}

// processProgramName returns the holder's executable name, or a marker when /proc has
// nothing to say about it. Deliberately not the full command line.
func processProgramName(pid int) string {
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return filepath.Base(exe)
	}
	if pid > 0 && processIsLive(pid) {
		return "unknown program"
	}
	return "process gone"
}

// processIsCancelTarget reports whether pid is a process this build would recognise as a
// legitimate cancel target: a wackypub binary, or the same executable that is asking.
// The second clause is what lets a test binary cancel a child it spawned, and what makes
// a `go run` build cancelable too.
func processIsCancelTarget(pid int) bool {
	if pid <= 0 {
		return false
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	if name := filepath.Base(exe); name == "wackypub" {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	if exe == self {
		return true
	}
	selfResolved, errSelf := filepath.EvalSymlinks(self)
	exeResolved, errExe := filepath.EvalSymlinks(exe)
	if errSelf != nil || errExe != nil {
		return false
	}
	return exeResolved == selfResolved
}

// processIsLive reports whether a process exists. Signal 0 runs the existence and
// permission checks without delivering anything, so EPERM also means alive.
func processIsLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
