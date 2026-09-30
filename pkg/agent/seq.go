package agent

import (
	"bufio"
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

// session.seq never existed as far as this code is concerned (Colin 2026-09-29): no
// read, write, delete, or check. The source of truth is session.jsonl alone.
var seqMu sync.Mutex

// seqAllocBump is a per-process record of the highest seq handed out by NextSeq, so that
// bare (non-appending) allocations - tool-event ephemerals, compaction summary before it
// is written, tests - are strictly monotonic within the process even though the log only
// advances when a turn is appended. Cross-process monotonicity still comes from the
// session lock + the appended log (AppendSessionContentGetSeq holds the lock across both).
var seqAllocBump = map[string]int64{}

// tailReadWindow is the increment we back up per read when seeking to the final line. The
// last line of a session.jsonl is almost always a few hundred bytes, so one window suffices
// for the common case; the seek keeps going, window at a time, until tailMaxSeek.
const tailReadWindow = 4096

// tailMaxSeek bounds the total back-seek. The persist layer hard-caps every line it writes
// at MaxPersistTurnBytes (sanitizeContentForPersist), so seeking this far guarantees the
// final COMPLETE line on every legitimate file. A final line longer than the cap cannot be
// a legitimately persisted turn - the unmarshal fallback below recovers from it.
const tailMaxSeek = MaxPersistTurnBytes + 4096

// recoverMaxSeqFromLog scans session.jsonl for the highest sequence number, treating each
// unsequenced (pre-#65) row as occupying 1..N. This is NOT a compatibility shim for the
// dead session.seq file - it is correctness for the log format: a session whose rows have
// no seq key is a legitimate log, and its next allocation must not collide with the turns
// those rows conceptually occupy. Called only when the tail line carries no seq.
func recoverMaxSeqFromLog(agentDir string) (int64, error) {
	var maxSeq int64
	var unsequencedTurns int64

	sessionPath := filepath.Join(agentDir, SessionFileName)
	f, err := os.Open(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("opening %s: %w", SessionFileName, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &rec); err == nil {
			if rec.Seq > 0 {
				if rec.Seq > maxSeq {
					maxSeq = rec.Seq
				}
			} else {
				unsequencedTurns++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("reading %s: %w", SessionFileName, err)
	}

	if maxSeq < unsequencedTurns {
		maxSeq = unsequencedTurns
	}
	return maxSeq, nil
}

// tailMaxSeq returns the highest sequence number stamped in session.jsonl by seeking to
// the last line rather than scanning the whole file. The session log is append-only and
// every stamped turn carries its seq, so the LAST line holds the highest seq. On an empty
// / missing file it returns 0. If the final line is corrupt or over the persist bound it
// falls back to a full scan of the recoverable lines, so a damaged tail can never reset
// the allocator to 0.
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
	for len(tail) < tailMaxSeek && pos > 0 {
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
		// The final line is not a parseable turn. With the seek sized to MaxPersistTurnBytes,
		// every legitimately persisted line is found and parsed here, so reaching this branch
		// means the final line is corrupt or over the persist bound. Recover the max over every
		// parseable line in the log: the read path skips unparseable lines too, so the
		// allocator and the reader agree on which lines are real. A line over the scanner cap
		// (16MiB) makes the scan fail closed: a log the read path cannot even scan cannot
		// be safely allocated from, so the append reports the scanner error. The old behavior here
		// (return 0, nil) was fail-open: a fresh process inferred max=0 and re-allocated seq
		// 1 into a non-empty log, duplicating stamps (Sept-29 audit finding F1).
		return recoverMaxSeqFromLog(agentDir)
	}
	if rec.Seq == 0 {
		// The last line is a pre-#65 row with no seq key. Infer the max from the whole log
		// (unsequenced rows occupy 1..N) - log-format correctness, not file back-compat.
		return recoverMaxSeqFromLog(agentDir)
	}
	return rec.Seq, nil
}

// NextSeq returns the next strictly monotonic sequence number for the agent in agentDir.
// It acquires the cross-process session lock flock (if not already held by the current
// process) and infers the next value from the LAST LINE of session.jsonl, which carries
// the highest seq. Callers that hold the session lock across the append (the normal turn
// path) get atomicity: the append serializes against next-allocation.
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
