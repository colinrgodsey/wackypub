package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SeqFileName is the dead session.seq watermark file. It existed for one release and no
// consumer depends on it; a stray file from that window is deleted on sight but never read.
const SeqFileName = "session.seq"

var seqMu sync.Mutex

// seqAllocBump is a per-process record of the highest seq handed out by NextSeq, so that
// bare (non-appending) allocations - tool-event ephemerals, compaction summary before it
// is written, tests - are strictly monotonic within the process even though the log only
// advances when a turn is appended. Cross-process monotonicity still comes from the
// session lock + the appended log (AppendSessionContentGetSeq holds the lock across both).
var seqAllocBump = map[string]int64{}

// tailReadWindow is the increment we back up per read when seeking to the final line. The
// last line of a session.jsonl is almost always a few hundred bytes, so one small window
// suffices; a D101-capped line fits within this bound.
const tailReadWindow = 4096

// tailMaxSeq returns the highest sequence number stamped in session.jsonl by seeking to
// the last line rather than scanning the whole file. The session log is append-only and
// every stamped turn carries its seq, so the LAST line holds the highest seq. On an empty
// / missing file or a corrupt tail it returns 0.
func tailMaxSeq(agentDir string) (int64, error) {
	sessionPath := filepath.Join(agentDir, SessionFileName)
	f, err := os.Open(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("opening %s: %w", SessionFileName, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", SessionFileName, err)
	}
	if fi.Size() == 0 {
		return 0, nil
	}

	// Back up from EOF in tailReadWindow chunks until we hold the final complete line.
	pos := fi.Size()
	var tail []byte
	for len(tail) < tailReadWindow*2 && pos > 0 {
		window := int64(tailReadWindow)
		if pos < window {
			window = pos
		}
		buf := make([]byte, window)
		n, err := f.ReadAt(buf, pos-window)
		if err != nil {
			if errors.Is(err, io.EOF) && n > 0 {
				buf = buf[:n]
			} else {
				return 0, fmt.Errorf("reading tail of %s: %w", SessionFileName, err)
			}
		}
		tail = append(buf[:n], tail...)
		pos -= window
		trimmed := bytes.TrimRight(tail, "\n\r")
		if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 {
			// A newline in the trimmed tail means we have the final complete line:
			// everything after the LAST newline. Stop backing up - this is O(last line).
			tail = trimmed[i+1:]
			break
		}
	}

	s := strings.TrimSpace(string(tail))
	if s == "" {
		return 0, nil
	}

	var rec struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(s), &rec); err != nil {
		// The tail window did not hold a parseable turn (an over-long or corrupt line).
		// Do NOT scan the file looking for a usable line: a line over the read bound is not
		// a legitimate persisted turn. Returning 0 lets the caller allocate fresh; the D101
		// over-long-line test asserts the post-turn read surfaces the corruption instead.
		return 0, nil
	}
	return rec.Seq, nil
}

// NextSeq returns the next strictly monotonic sequence number for the agent in agentDir.
// It acquires the cross-process session lock flock (if not already held by the current
// process) and infers the next value from the LAST LINE of session.jsonl, which carries
// the highest seq. Callers that hold the session lock across the append (the normal turn
// path) get atomicity: the append serializes against next-allocation the same way the old
// watermark write did, with fewer moving parts. A stray session.seq from the brief window
// it existed is deleted on sight; it is never a source of truth.
func NextSeq(agentDir string) (int64, error) {
	seqMu.Lock()
	defer seqMu.Unlock()

	if !IsSessionLockedByCurrentProcess(agentDir) {
		lock, err := AcquireSessionLock(agentDir)
		if err != nil {
			return 0, fmt.Errorf("acquiring session lock for seq: %w", err)
		}
		defer lock.Release()
	}

	key := cleanLockDir(agentDir)
	cur, err := tailMaxSeq(agentDir)
	if err != nil {
		return 0, err
	}
	if b := seqAllocBump[key]; b > cur {
		cur = b
	}
	next := cur + 1
	seqAllocBump[key] = next
	_ = os.Remove(filepath.Join(agentDir, SeqFileName))
	return next, nil
}

// CurrentSeq returns the current assigned sequence number for agentDir without incrementing.
// It is a LOCK-FREE read: the source of truth is the last appended line of session.jsonl, and
// appends are single atomic writes under O_APPEND, so a tail read is never torn and never
// needs the cross-process flock. Taking the flock here would deadlock the read-only replay
// path (SubscribeSession calls CurrentSeq while a concurrent writer holds the lock for the
// whole append). Callers that need a strictly-synchronized read already hold the session
// lock and get it from the writer side.
func CurrentSeq(agentDir string) (int64, error) {
	seqMu.Lock()
	defer seqMu.Unlock()
	cur, err := tailMaxSeq(agentDir)
	if err != nil {
		return 0, err
	}
	if b := seqAllocBump[cleanLockDir(agentDir)]; b > cur {
		cur = b
	}
	return cur, nil
}
