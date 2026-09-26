package agent

import (
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

// TestToolCallIDUniqueness pins announce/update pairing ids do not collide.
func TestToolCallIDUniqueness(t *testing.T) {
	sink := NewToolEventSink()
	id1 := sink.Announce("a", "x", false)
	id2 := sink.Announce("b", "y", false)
	if id1 == id2 {
		t.Fatalf("call ids collided: %s", id1)
	}
}
