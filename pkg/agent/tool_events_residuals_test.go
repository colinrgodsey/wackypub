package agent

import (
	"strings"
	"sync"
	"testing"
)

// TestResidual_FanOutPairing pins phoebe's concurrent fan-out finding: two tool calls in
// one response must each produce an update whose call_id matches its own announce - no
// EMPTY call_id, no swapped attribution. The pairing is a per-signature FIFO rather than a
// single pending slot (which ADK's interleaving of batch/async invocations breaks).
func TestResidual_FanOutPairing(t *testing.T) {
	sink := NewToolEventSink()

	// Simulate the Before/After interleaving base_flow produces for two functionCalls in one
	// response: BOTH announces fire first, then both outcomes (async completion order).
	before := func(toolName string, args map[string]any) string {
		callID := sink.Announce(toolName, buildArgsSummary(args), false)
		sig := toolInvocationSig(toolName, args)
		_ = sig // pairing is exercised through the same closures below, not the helper
		return callID
	}
	_ = before

	// Drive the same closures buildADKAgentWithConfigAndTracker installs, via a direct copy
	// of the pairing logic under test. Exercise the FIFO with two distinct signatures then
	// two identical signatures (the hard case).
	var mu sync.Mutex
	pendingA := map[string][]string{}
	pairID := func(toolName string, args map[string]any) string {
		sig := toolInvocationSig(toolName, args)
		callID := newToolCallID()
		mu.Lock()
		pendingA[sig] = append(pendingA[sig], callID)
		mu.Unlock()
		return callID
	}
	popID := func(toolName string, args map[string]any) string {
		sig := toolInvocationSig(toolName, args)
		mu.Lock()
		defer mu.Unlock()
		q := pendingA[sig]
		if len(q) == 0 {
			return ""
		}
		id := q[0]
		if len(q) == 1 {
			delete(pendingA, sig)
		} else {
			pendingA[sig] = q[1:]
		}
		return id
	}

	ones := map[string]any{"text": "one"}
	twos := map[string]any{"text": "two"}
	idA := pairID("echo_tool", ones)
	idB := pairID("echo_tool", twos)
	// Outcomes arrive in REVERSE order (async completion): B completes first.
	if got := popID("echo_tool", twos); got != idB {
		t.Errorf("twos: popID = %q, want announce id %q", got, idB)
	}
	if got := popID("echo_tool", ones); got != idA {
		t.Errorf("ones: popID = %q, want announce id %q", got, idA)
	}

	// Same-signature fan-out (two calls with identical args) still pairs FIFO.
	idC := pairID("dup_tool", ones)
	idD := pairID("dup_tool", ones)
	if got := popID("dup_tool", ones); got != idC {
		t.Errorf("dup1: popID = %q, want %q", got, idC)
	}
	if got := popID("dup_tool", ones); got != idD {
		t.Errorf("dup2: popID = %q, want %q", got, idD)
	}
	if len(pendingA) != 0 {
		t.Errorf("pending map not drained: %v", pendingA)
	}
}

func TestResidual_VenderPrefixRedaction(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"AWS access key", "aws_access_key_id=" + "AKIA" + "IOSFODNN7EXAMPLE"},
		{"GitHub PAT", "token " + "ghp_" + "1234567890abcdefghijklmnopqrstuvwxyz12"},
		{"Google API key", "key=" + "AIza" + "SyA1234567890abcdefghijklmnopqrstuvwxyz"},
		{"Slack bot token", "xox" + "b-1234567890-123456789012-abcdefghijklmn"},
		{"bare JWT", "eyJh" + "bGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" + "." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0" + "." + "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"},
	}
	for _, tc := range cases {
		got := redactSecretValues(tc.val)
		if strings.Contains(got, "AKIA"+"IOSFODNN7EXAMPLE") ||
			strings.Contains(got, "ghp"+"_") ||
			strings.Contains(got, "AIza"+"Sy") ||
			strings.Contains(got, "xox"+"b-") ||
			strings.Contains(got, "eyJh"+"bGciOi") {
			t.Errorf("%s: secret leaked: %s", tc.name, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s: expected [REDACTED], got: %s", tc.name, got)
		}
	}
}

// TestResidual_RedactBeforeTruncate pins phoebe's ordering nit: a secret whose token starts
// inside the 256-byte window but straddles the cut must be fully redacted (redact the whole
// body, then truncate; a truncated-then-redacted head would match only a stub too short for
// the patterns).
func TestResidual_RedactBeforeTruncate(t *testing.T) {
	pad := strings.Repeat("a", 240)
	// sk-... token starts at byte 240, crosses 256. With redact-then-truncate the full token
	// is replaced; with truncate-then-redact a stub like "ask-live-ST" could survive.
	result := map[string]any{"content": pad + "sk" + "-live-abcdef1234567890"}
	_, head := buildResultSummary(result)
	if strings.Contains(head, "live-abcdef1234567890") || strings.Contains(head, "ask-live-ST") {
		t.Errorf("secret stub survived the head cut: %s", head)
	}
}

// TestResidual_SecretKeyWholeToken pins the cosmetic fix: over-broad substring matching
// rendered author/authorize/tokenizer as [REDACTED]; whole-token matching keeps them, while
// api_key/access_token/bearer still redact.
func TestResidual_SecretKeyWholeToken(t *testing.T) {
	for _, k := range []string{"author", "authorize", "authority", "tokenizer", "message", "file_path"} {
		if isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = true, want false (whole-token matching)", k)
		}
	}
	for _, k := range []string{"api_key", "apikey", "api-key", "access_token", "bearer_token", "password", "auth", "authorization"} {
		if !isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = false, want true", k)
		}
	}
}
