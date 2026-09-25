package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const SeqFileName = "session.seq"

var seqMu sync.Mutex

func isCorruptSeqErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, strconv.ErrSyntax) || strings.Contains(err.Error(), "invalid sequence number")
}

// NextSeq returns the next strictly monotonic sequence number for the agent in agentDir.
// It acquires the cross-process session lock flock (if not already held by the current process)
// and serializes in-process calls via seqMu.
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

	cur, err := readSeqFile(agentDir)
	if err != nil {
		if os.IsNotExist(err) || isCorruptSeqErr(err) {
			cur, err = RecoverSeq(agentDir)
			if err != nil {
				return 0, err
			}
		} else {
			return 0, fmt.Errorf("reading %s: %w", SeqFileName, err)
		}
	}

	next := cur + 1
	if err := writeSeqFile(agentDir, next); err != nil {
		return 0, err
	}
	return next, nil
}

// CurrentSeq returns the current assigned sequence number for agentDir without incrementing.
func CurrentSeq(agentDir string) (int64, error) {
	seqMu.Lock()
	defer seqMu.Unlock()

	if !IsSessionLockedByCurrentProcess(agentDir) {
		lock, err := AcquireSessionLock(agentDir)
		if err != nil {
			return 0, fmt.Errorf("acquiring session lock for seq: %w", err)
		}
		defer lock.Release()
	}

	cur, err := readSeqFile(agentDir)
	if err != nil {
		if os.IsNotExist(err) || isCorruptSeqErr(err) {
			return RecoverSeq(agentDir)
		}
		return 0, err
	}
	return cur, nil
}

// SetSeq writes a specific sequence number to session.seq.
func SetSeq(agentDir string, seq int64) error {
	seqMu.Lock()
	defer seqMu.Unlock()

	if !IsSessionLockedByCurrentProcess(agentDir) {
		lock, err := AcquireSessionLock(agentDir)
		if err != nil {
			return fmt.Errorf("acquiring session lock for seq: %w", err)
		}
		defer lock.Release()
	}
	return writeSeqFile(agentDir, seq)
}

func readSeqFile(agentDir string) (int64, error) {
	p := filepath.Join(agentDir, SeqFileName)
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid sequence number in %s: %w", p, err)
	}
	return v, nil
}

func writeSeqFile(agentDir string, seq int64) error {
	p := filepath.Join(agentDir, SeqFileName)
	tmpFile, err := os.CreateTemp(agentDir, "session.seq.*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", agentDir, err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	data := []byte(fmt.Sprintf("%d\n", seq))
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, p, err)
	}
	return nil
}

// RecoverSeq scans session.jsonl and tool-journal.jsonl to determine the highest existing
// sequence number in agentDir.
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

	// 2. Scan tool-journal.jsonl
	journalPath := toolJournalPath(agentDir)
	if f, err := os.Open(journalPath); err == nil {
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
			if err := json.Unmarshal(line, &rec); err == nil && rec.Seq > maxSeq {
				maxSeq = rec.Seq
			}
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("reading tool journal: %w", err)
		}
		_ = f.Close()
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("opening tool journal: %w", err)
	}

	// If there were unsequenced turns (from prior sessions before seq stamping),
	// treat each as occupying seq 1..unsequencedTurns.
	if maxSeq < unsequencedTurns {
		maxSeq = unsequencedTurns
	}

	return maxSeq, nil
}
