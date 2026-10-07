package agent

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

// TestWarnWorkspaceEventCommitSurfacesFailure pins the reporting half of the fix: the
// callers deliberately do not fail a durable turn over a lost trace commit, so the log
// line is the only signal that the trace diverged.
func TestWarnWorkspaceEventCommitSurfacesFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	warnWorkspaceEventCommit("bob", traceEventUser, nil)
	if buf.Len() != 0 {
		t.Errorf("a successful commit must log nothing, got %q", buf.String())
	}

	warnWorkspaceEventCommit("bob", traceEventUser, errors.New("git exploded"))
	got := buf.String()
	if !strings.Contains(got, "bob") || !strings.Contains(got, "git exploded") {
		t.Errorf("the failure must name the agent and the cause, got %q", got)
	}
	if !strings.Contains(got, traceEventUser) {
		t.Errorf("the failure must name the event type, got %q", got)
	}
}

func TestParseCommitMessageCorruptA2AMetadataIsNotSilent(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	eventType, meta := parseCommitMessage("tool call (bash)\nAGENT2AGENT:not-json{")
	if meta != nil {
		t.Fatalf("expected no metadata out of a corrupt payload, got %+v", meta)
	}
	if eventType != "tool call (bash)" {
		t.Fatalf("the event type must still be classified, got %q", eventType)
	}
	if !strings.Contains(buf.String(), "A2A metadata") {
		t.Errorf("a corrupt attribution payload must be reported, got %q", buf.String())
	}
}

func TestParseCommitMessageKeepsGoodA2AMetadataQuiet(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	eventType, meta := parseCommitMessage(
		"user\nAGENT2AGENT:{\"caller_id\":\"dranbo\",\"call_chain\":[\"dranbo\",\"bob\"]}")
	if meta == nil || meta.CallerID != "dranbo" {
		t.Fatalf("expected attribution to parse, got %+v", meta)
	}
	if eventType != "user" {
		t.Fatalf("unexpected event type %q", eventType)
	}
	if buf.Len() != 0 {
		t.Errorf("a well-formed payload must not warn, got %q", buf.String())
	}
}
