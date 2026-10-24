package agent

import (
	"testing"
	"time"
)

// TestTurnDeadline_DefaultUnset pins the single fixed default (Colin, 2026-10-24: dropped the
// local-vs-remote endpoint heuristic as brittle and unable to reliably classify an endpoint -
// one default for everyone, turnDeadlineSeconds is the only knob for anyone who wants lower or
// higher). Endpoint plays no part in the calculation at all.
func TestTurnDeadline_DefaultUnset(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
	}{
		{"empty endpoint", ""},
		{"cloud endpoint", "https://api.openai.com/v1"},
		{"self-hosted-looking endpoint", "http://llamacpp.default.k8s/v1"},
		{"loopback endpoint", "http://127.0.0.1:11434/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &RuntimeConfig{Endpoint: tc.endpoint}
			if got, want := cfg.TurnDeadline(), DefaultTurnDeadlineSeconds*time.Second; got != want {
				t.Errorf("TurnDeadline() = %v, want %v (fixed default regardless of endpoint)", got, want)
			}
		})
	}
}

// TestTurnDeadline_ExplicitOverrideWins confirms a configured TurnDeadlineSeconds always wins
// over the default, independent of Endpoint.
func TestTurnDeadline_ExplicitOverrideWins(t *testing.T) {
	cfg := &RuntimeConfig{Endpoint: "https://api.openai.com/v1", TurnDeadlineSeconds: 42}
	if got, want := cfg.TurnDeadline(), 42*time.Second; got != want {
		t.Errorf("TurnDeadline() = %v, want %v (explicit override)", got, want)
	}
}

// TestTurnDeadline_ZeroAndNegativeTreatedAsUnset confirms the existing "<= 0 means unset"
// contract (TurnDeadlineSeconds doc comment) holds: a negative or explicit zero must not leak
// through as a near-instant watchdog, it must fall back to the fixed default.
func TestTurnDeadline_ZeroAndNegativeTreatedAsUnset(t *testing.T) {
	for _, seconds := range []int{0, -1, -3600} {
		cfg := &RuntimeConfig{TurnDeadlineSeconds: seconds}
		if got, want := cfg.TurnDeadline(), DefaultTurnDeadlineSeconds*time.Second; got != want {
			t.Errorf("TurnDeadlineSeconds=%d: TurnDeadline() = %v, want default %v", seconds, got, want)
		}
	}
}
