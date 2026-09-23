package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedactSecretShapedValues_RealisticSecrets pins the workshop red flag: redaction MUST
// remove realistic secret-shaped values (api keys, bearer tokens, passwords) from the args
// summary BEFORE any client renders it.
func TestRedactSecretShapedValues_RealisticSecrets(t *testing.T) {
	args := map[string]any{
		"file_path":    "agent1/MEMORY.md",
		"api_key":      "sk-live-abcdef1234567890",
		"bearer_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"password":     "hunter2-secret",
		"auth":         "Basic dXNlcjpwYXNz",
		"message":      "hello",
	}
	red := redactSecretShapedValues(args)
	if red["api_key"] != "[REDACTED]" {
		t.Errorf("api_key not redacted: %v", red["api_key"])
	}
	if red["bearer_token"] != "[REDACTED]" {
		t.Errorf("bearer_token not redacted: %v", red["bearer_token"])
	}
	if red["password"] != "[REDACTED]" {
		t.Errorf("password not redacted: %v", red["password"])
	}
	if red["auth"] != "[REDACTED]" {
		t.Errorf("auth not redacted: %v", red["auth"])
	}
	if red["message"] != "hello" {
		t.Errorf("non-secret message mutated: %v", red["message"])
	}

	// The full summary must not contain ANY of the secret-shaped values.
	summary := buildArgsSummary(args)
	for _, secret := range []string{"sk-live-abcdef1234567890", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "hunter2-secret", "dXNlcjpwYXNz"} {
		if strings.Contains(summary, secret) {
			t.Errorf("summary leaked secret %q: %s", secret, summary)
		}
	}
	if !strings.Contains(summary, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in summary, got: %s", summary)
	}
	if len(summary) > maxArgsSummaryLen {
		t.Errorf("summary exceeds %d chars: %d", maxArgsSummaryLen, len(summary))
	}
}

// TestBuildArgsSummary_TruncatesLongArgs pins the hard truncation at maxArgsSummaryLen.
func TestBuildArgsSummary_TruncatesLongArgs(t *testing.T) {
	long := strings.Repeat("x", 1000)
	args := map[string]any{"data": long}
	summary := buildArgsSummary(args)
	if len(summary) > maxArgsSummaryLen {
		t.Errorf("summary not truncated: len=%d", len(summary))
	}
	if !strings.HasPrefix(summary, "data=") {
		t.Errorf("unexpected summary: %s", summary)
	}
}

// TestToolEventSink_JournalAppends pins the evidence-trail persistence: completed events are
// appended to the JSONL journal in addition to being stream-drained.
func TestToolEventSink_JournalAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool-journal.jsonl")
	sink := NewToolEventSinkWithJournal(path)
	callID := sink.Announce("create_scratchpad", "text=hello", false)
	sink.Update(callID, "create_scratchpad", "completed", 123, "hello", "result-ref-1")

	// Drain returns both (stream side).
	evts := sink.Drain()
	if len(evts) != 2 {
		t.Fatalf("expected 2 drained events, got %d", len(evts))
	}

	// Journal has both lines (evidence side).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 journal lines, got %d", len(lines))
	}
	var ev ToolEvent
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatalf("journal line 2 unmarshal: %v", err)
	}
	if ev.Status != "completed" || ev.ResultRef != "result-ref-1" {
		t.Errorf("unexpected journaled event: %+v", ev)
	}
}

// TestToolCallIDUniqueness pins announce/update pairing ids do not collide.
func TestToolCallIDUniqueness(t *testing.T) {
	sink := NewToolEventSink()
	id1 := sink.Announce("a", "x", false)
	id2 := sink.Announce("b", "y", false)
	if id1 == id2 {
		t.Fatalf("call ids collided: %s", id1)
	}
}
