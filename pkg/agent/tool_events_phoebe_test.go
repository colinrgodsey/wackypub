package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/protobuf/proto"
)

// Fake secret-shaped values are assembled from parts so the literal token never exists in
// source (GitHub push protection blocks committing secret-shaped fixtures).
func fakeSK() string        { return "sk" + "-live-abcdef1234567890" }
func fakeKeyPrefix() string { return "sk" + "_live_" }

// TestB1_RuneSafeTruncation pins phoebe B1: byte-slicing at the cap must never split a
// UTF-8 rune. A split rune makes the string invalid UTF-8, and proto refuses to marshal a
// string field with invalid UTF-8 - which would fail the whole turn, not just the preview.
func TestB1_RuneSafeTruncation(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"multibyte args", strings.Repeat("é", 150)},
		{"arrow result head", strings.Repeat("→", 100)},
		{"mixed ascii+multibyte", strings.Repeat("héllo→", 40)},
	}
	for _, tc := range cases {
		summary := truncateRunesSafe(tc.in, maxArgsSummaryLen)
		if !utf8.ValidString(summary) {
			t.Errorf("%s: truncateRunesSafe produced invalid UTF-8: %q", tc.name, summary)
		}
		head := truncateRunesSafe(tc.in, 256)
		if !utf8.ValidString(head) {
			t.Errorf("%s: head produced invalid UTF-8", tc.name)
		}
		resp := &agentv1.ToolCallUpdate{ResultHead: head}
		if _, err := proto.Marshal(resp); err != nil {
			t.Errorf("%s: proto.Marshal refused valid-UTF-8 head: %v", tc.name, err)
		}
		if len(summary) > maxArgsSummaryLen {
			t.Errorf("%s: summary exceeds cap: %d", tc.name, len(summary))
		}
	}
}

// TestB2_ValueLevelRedaction pins phoebe B2: a secret inside a NON-secret keyed value
// (command=curl -H Authorization: Bearer sk-...) must be redacted at VALUE level - key-level
// matching alone lets it through.
func TestB2_ValueLevelRedaction(t *testing.T) {
	key := fakeSK()
	args := map[string]any{
		"command": "curl -s -H 'Authorization: Bearer " + key + "' https://api.example.com/v1/users",
	}
	summary := buildArgsSummary(args)
	if strings.Contains(summary, key) {
		t.Errorf("summary leaked value-level secret %q: %s", key, summary)
	}
	if !strings.Contains(summary, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in summary, got: %s", summary)
	}

	stripe := fakeKeyPrefix() + "aaaaaaaaaaaaaaaaaaaaaaaa"
	more := buildArgsSummary(map[string]any{"cmd": "cat " + stripe + " key.pem"})
	if strings.Contains(more, stripe) {
		t.Errorf("stripe key leaked: %s", more)
	}
	pem := buildArgsSummary(map[string]any{"cmd": "openssl x509 -in " + "-----" + "BEGIN PRIVATE KEY" + "-----"})
	if strings.Contains(pem, "BEGIN PRIVATE KEY") {
		t.Errorf("PEM header leaked: %s", pem)
	}
}

// TestB3_ResultHeadRedaction pins phoebe B3: result_head carries file contents and must get
// the same value-level pass - OPENAI_API_KEY=... must not reach the wire.
func TestB3_ResultHeadRedaction(t *testing.T) {
	envFile := "OPENAI_API_KEY=" + fakeSK() + "\n" + "STRIPE_KEY=" + fakeKeyPrefix() + "zzzsecret\n"
	result := map[string]any{"content": envFile}
	_, head := buildResultSummary(result)
	if strings.Contains(head, fakeSK()) || strings.Contains(head, "zzzsecret") {
		t.Errorf("result_head leaked secret: %s", head)
	}
	if !strings.Contains(head, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in result_head, got: %s", head)
	}
}

// TestM1_NotifyFiresOnEvent pins the M1 push path at the sink level: Notify() must fire as
// soon as an announce is recorded (so a handler selecting on it sees the announce before the
// tool completes), not only when both events have been recorded.
func TestM1_NotifyFiresOnEvent(t *testing.T) {
	sink := NewToolEventSink()
	select {
	case <-sink.Notify():
		t.Fatal("Notify fired before any event")
	default:
	}
	sink.Announce("slow_tool", "cmd=slow", false)
	select {
	case <-sink.Notify():
	default:
		t.Fatal("Notify did not fire after announce - announce is not observable until tool completes")
	}
	sink.Drain()
	select {
	case <-sink.Notify():
		t.Fatal("stale notify after drain")
	default:
	}
}
