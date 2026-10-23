package agent

import (
	"testing"
	"time"
)

// TestTurnDeadline_DefaultByEndpointLocality pins the local-vs-remote default policy: an
// unset TurnDeadlineSeconds resolves to the long local bound for a loopback endpoint and the
// short remote bound for everything else, including an empty Endpoint (native Gemini).
func TestTurnDeadline_DefaultByEndpointLocality(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     time.Duration
	}{
		{"empty endpoint (native Gemini) defaults remote", "", DefaultTurnDeadlineSecondsRemote * time.Second},
		{"localhost hostname", "http://localhost:11434/v1", DefaultTurnDeadlineSecondsLocal * time.Second},
		{"loopback IPv4", "http://127.0.0.1:8080/v1", DefaultTurnDeadlineSecondsLocal * time.Second},
		{"loopback IPv6", "http://[::1]:8080/v1", DefaultTurnDeadlineSecondsLocal * time.Second},
		{"cloud https endpoint", "https://api.openai.com/v1", DefaultTurnDeadlineSecondsRemote * time.Second},
		{"non-loopback LAN IP is remote", "http://192.168.1.50:8080/v1", DefaultTurnDeadlineSecondsRemote * time.Second},
		{"unparseable endpoint defaults remote (safer/tighter)", "not a url: :::", DefaultTurnDeadlineSecondsRemote * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &RuntimeConfig{Endpoint: tc.endpoint}
			if got := cfg.TurnDeadline(); got != tc.want {
				t.Errorf("TurnDeadline() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTurnDeadline_ExplicitOverrideWins confirms a configured TurnDeadlineSeconds always wins
// over the locality-based default, for both local and remote endpoints - the existing
// per-runtime configurability (already present before this change) must survive untouched.
func TestTurnDeadline_ExplicitOverrideWins(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
	}{
		{"override on local endpoint", "http://localhost:11434/v1"},
		{"override on remote endpoint", "https://api.openai.com/v1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &RuntimeConfig{Endpoint: tc.endpoint, TurnDeadlineSeconds: 42}
			if got, want := cfg.TurnDeadline(), 42*time.Second; got != want {
				t.Errorf("TurnDeadline() = %v, want %v (explicit override)", got, want)
			}
		})
	}
}

// TestTurnDeadline_ZeroAndNegativeTreatedAsUnset confirms the existing "<= 0 means unset"
// contract (TurnDeadlineSeconds doc comment) still holds now that "unset" resolves to one of
// two defaults instead of one - a negative or explicit zero must not leak through as a
// near-instant watchdog.
func TestTurnDeadline_ZeroAndNegativeTreatedAsUnset(t *testing.T) {
	for _, seconds := range []int{0, -1, -3600} {
		cfg := &RuntimeConfig{Endpoint: "http://localhost:11434/v1", TurnDeadlineSeconds: seconds}
		if got, want := cfg.TurnDeadline(), DefaultTurnDeadlineSecondsLocal*time.Second; got != want {
			t.Errorf("TurnDeadlineSeconds=%d: TurnDeadline() = %v, want default %v", seconds, got, want)
		}
	}
}
