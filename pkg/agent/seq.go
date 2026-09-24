package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const SeqFileName = "session.seq"

var seqMu sync.Mutex

// NextSeq returns the next strictly monotonic sequence number for the agent in agentDir.
// Callers must hold the agent's session lock for multi-writer safety across processes.
func NextSeq(agentDir string) (int64, error) {
	seqMu.Lock()
	defer seqMu.Unlock()

	cur, err := readSeqFile(agentDir)
	if err != nil {
		if os.IsNotExist(err) {
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

	cur, err := readSeqFile(agentDir)
	if err != nil {
		if os.IsNotExist(err) {
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
	tmp := p + ".tmp"
	data := []byte(fmt.Sprintf("%d\n", seq))
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %s to %s: %w", tmp, p, err)
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
		_ = f.Close()
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
		_ = f.Close()
	}

	// If there were unsequenced turns (from prior sessions before seq stamping),
	// treat each as occupying seq 1..unsequencedTurns.
	if maxSeq < unsequencedTurns {
		maxSeq = unsequencedTurns
	}

	return maxSeq, nil
}
