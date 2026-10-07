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

// seqAllocBumpMax bounds the number of distinct agent dirs tracked in seqAllocBump.
// Steady state is self-cleaning: an entry is evicted once the log has caught up to it
// (see NextSeq/CurrentSeq), so the cap is a backstop for workloads with unbounded
// distinct dirs and never-persisted bare allocations. On overflow the map is reset to
// just the allocating dir: cleared dirs then re-derive from their logs exactly like a
// fresh process (the session lock + appended log carry cross-process monotonicity);
// the only cost is the in-process bridge for a cleared dir still holding un-persisted
// seqs.
const seqAllocBumpMax = 1024

// tailReadWindow is the initial read when seeking to the final line. The last line of a
// session.jsonl is almost always a few hundred bytes, so one window suffices for the common
// case; when the final line is bigger the read doubles (4KiB, 8KiB, 16KiB, ...) until the
// final complete line is in hand or tailMaxSeek is reached. Doubling re-reads the tail region
// from disk each step, but the page cache makes re-reads memory-speed and the total work is
// O(final line) syscalls and O(2 x final line) bytes read - the alternative (per-window
// accumulation) is O(line^2) copies, which is fatal at the 64MiB cap where a legitimate
// 53MiB image line used to cost ~500GB of memcpy per seq allocation.
const tailReadWindow = 4096

// tailMaxSeek bounds the total back-seek. The persist layer hard-caps every line it writes
// at MaxPersistTurnBytes (sanitizeContentForPersist), so seeking this far guarantees the
// final COMPLETE line on every legitimate file. The bound is derived, not independent: it
// tracks the persist cap, so a cap change cannot desynchronize the two. The back-seek is
// incremental (tailReadWindow per step) and stops at the first newline behind the final
// line, so the common small-line case is one window regardless of the bound. A final line longer than the cap cannot be
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
	scanner.Buffer(make([]byte, 1024*1024), MaxSessionLineBytes)
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
		// A line over the read path's scanner cap (MaxSessionLineBytes) means the log cannot be scanned
		// by the read path either: allocation cannot proceed safely on a session this
		// corrupt, and every subsequent turn would fail the same way. Mark it
		// UNRECOVERABLE so the process supervision (stdio-serve / ProcessDialer) restarts
		// the server from a fresh state instead of failing each turn individually.
		return 0, MarkUnrecoverable("session log cannot be scanned for sequence inference",
			fmt.Errorf("reading %s: %w", SessionFileName, err))
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

	// Back up from EOF until we hold the final complete line: read the last
	// tailReadWindow bytes, then double the window until the final newline behind the
	// final line is in hand or the read has covered tailMaxSeek. A newline inside the
	// read region means the final complete line - everything after the LAST newline -
	// is present; a region that reaches the bound with no newline cannot end a
	// legitimate line (sanitize caps every line at MaxPersistTurnBytes < tailMaxSeek),
	// so the unmarshal fallback below takes over, exactly as before.
	size := fi.Size()
	window := int64(tailReadWindow)
	if window > size {
		window = size
	}
	for {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		n, err := f.ReadAt(buf, size-window)
		if err != nil {
			if errors.Is(err, io.EOF) && n > 0 {
				buf = buf[:n]
			} else {
				return 0, fmt.Errorf("reading tail of %s: %w", SessionFileName, err)
			}
		}
		tail := buf[:n]
		trimmed := bytes.TrimRight(tail, "\n\r")
		if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 {
			// A newline in the trimmed region means we have the final complete line.
			tail = trimmed[i+1:]
			return parseTailLine(tail, agentDir)
		}
		if window >= min(tailMaxSeek, size) {
			// The whole file (or the bound) holds no complete line: either the file is
			// a single unterminated line, or the final line is over the persist bound.
			// Hand the partial region to the same unmarshal fallback.
			return parseTailLine(tail, agentDir)
		}
		window *= 2
	}
}

// parseTailLine parses the seq stamped in the tail region read by tailMaxSeq. The region
// is either the final complete line (the common path) or the last tailMaxSeek bytes of a
// file whose final line is unterminated or over the persist bound (the fallback path).
func parseTailLine(tail []byte, agentDir string) (int64, error) {
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
		// (MaxSessionLineBytes) makes the scan fail closed: a log the read path cannot even scan cannot
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
	// Evict on consume: once the log has caught up to a recorded bump, every seq it bridged
	// is in session.jsonl and the file alone bounds future allocations, so the entry is
	// redundant. Steady state (every allocation appended) self-cleans to zero entries.
	if b := seqAllocBump[key]; b <= cur {
		delete(seqAllocBump, key)
	} else {
		cur = b
	}
	next := cur + 1
	seqAllocBump[key] = next
	if len(seqAllocBump) > seqAllocBumpMax {
		for k := range seqAllocBump {
			if k != key {
				delete(seqAllocBump, k)
			}
		}
	}
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
	key := cleanLockDir(agentDir)
	// Same consume-time eviction as NextSeq: a caught-up entry is redundant.
	if b := seqAllocBump[key]; b > cur {
		cur = b
	} else {
		delete(seqAllocBump, key)
	}
	return cur, nil
}
