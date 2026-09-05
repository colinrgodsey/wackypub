package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const LastUsageFileName = ".last_usage.json"

type LastUsageRecord struct {
	PromptTokens     int32     `json:"prompt_tokens"`
	CandidatesTokens int32     `json:"candidates_tokens"`
	TotalTokens      int32     `json:"total_tokens"`
	Timestamp        time.Time `json:"timestamp"`
	Compacted        bool      `json:"compacted,omitempty"`
}

// WriteLastUsage writes the last turn usage atomically via tempfile + rename.
func WriteLastUsage(agentDir string, usage *LastUsageRecord) error {
	if agentDir == "" || usage == nil {
		return nil
	}
	data, err := json.MarshalIndent(usage, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal last usage: %w", err)
	}

	tmp, err := os.CreateTemp(agentDir, ".last_usage_tmp_*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for last usage: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write last usage: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	target := filepath.Join(agentDir, LastUsageFileName)
	return os.Rename(tmpName, target)
}

// ReadLastUsage reads the last usage record from <agentDir>/.last_usage.json.
// Returns nil, nil if the file does not exist.
func ReadLastUsage(agentDir string) (*LastUsageRecord, error) {
	if agentDir == "" {
		return nil, nil
	}
	target := filepath.Join(agentDir, LastUsageFileName)
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec LastUsageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", LastUsageFileName, err)
	}
	return &rec, nil
}

// InvalidateLastUsage marks .last_usage.json as compacted so cold-start emergency valves do not read stale tokens.
func InvalidateLastUsage(agentDir string) error {
	rec := &LastUsageRecord{
		Timestamp: time.Now(),
		Compacted: true,
	}
	return WriteLastUsage(agentDir, rec)
}
