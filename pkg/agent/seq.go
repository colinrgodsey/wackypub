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

// SeqFileName is the legacy session.seq watermark file. As of the D118 amendment, the
// watermark is DERIVED state whose source of truth is the last line of session.jsonl
// (every persisted turn carries its seq). The constant is retained so orphaned watermark
// files from sessions created before the amendment can still be identified and removed;
// it is never read as a source of truth.
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
// or missing file it returns 0. The read is bounded: we back up in small windows until the
// final newline is found, so cost is O(last line size), not O(file size).
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
		// The tail window did not hold a parseable turn (an over-long or corrupt line, a
		// D101 test scenario). Do NOT fall into an unbounded scan: a line over the read bound
		// is not a legitimate persisted turn, and scanning it would surface a scanner error
		// where the caller expects an allocation result. Treat it as 0 (no usable tail); the
		// turn being appended is before any recovery path in this flow.
		return 0, nil
	}
	if rec.Seq == 0 {
		// The last line is a legacy pre-#65 turn (no seq key). next-seq continuity requires
		// the legacy turns to occupy 1..N (RecoverSeq semantics), so fall back to the scan.
		// The scan is bounded by the session log being made of legitimate short lines here;
		// an over-long line would have been caught by the unmarshal error branch above.
		return RecoverSeq(agentDir)
	}
	return rec.Seq, nil
}

// NextSeq returns the next strictly monotonic sequence number for the agent in agentDir.
// It acquires the cross-process session lock flock (if not already held by the current
// process) and infers the next value from the LAST LINE of session.jsonl, which carries
// the highest seq. Callers that hold the session lock across the append (the normal turn
// path) get atomicity: the append serializes against next-allocation the same way the old
// watermark write did, with fewer moving parts. The watermark file is removed best-effort
// after a successful read so orphaned files from pre-amendment sessions do not linger.
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
	// Migration: remove an orphaned watermark file so it cannot confuse any reader that
	// still looks for it (older binaries) and so the derived state does not linger.
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

// SetSeq is retained for callers that seed a specific sequence without appending turns
// (tests). With the watermark removed it has nothing to persist: the source of truth is the
// session log, and the next allocation is inferred from the last line. It removes an
// orphaned watermark file best-effort and returns nil; callers are responsible for writing
// a turn whose seq reflects the seeded value (as tests do). Production callers should use
// NextSeq + append instead.
func SetSeq(agentDir string, seq int64) error {
	_ = os.Remove(filepath.Join(agentDir, SeqFileName))
	return nil
}

// RecoverSeq scans session.jsonl to determine the highest existing sequence number
// in agentDir. It is the fallback when the tail read yields no seq (legacy unsequenced
// lines or a fully unsequenced session), preserving pre-#65 sessions.
func RecoverSeq(agentDir string) (int64, error) {
	var maxSeq int64
	var unsequencedTurns int64

	// 1. Scan session.jsonl
	sessionPath := filepath.Join(agentDir, SessionFileName)
	if f, err := os.Open(sessionPath); err == nil {
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
			_ = f.Close()
			return 0, fmt.Errorf("reading %s: %w", SessionFileName, err)
		}
		_ = f.Close()
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("opening %s: %w", SessionFileName, err)
	}

	// If there were unsequenced turns (from prior sessions before seq stamping),
	// treat each as occupying seq 1..unsequencedTurns.
	if maxSeq < unsequencedTurns {
		maxSeq = unsequencedTurns
	}

	return maxSeq, nil
}
