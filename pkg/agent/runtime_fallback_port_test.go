package agent

import (
	"errors"
	"testing"
)

// TestIsQualifyingFallbackError_UrlPortDoesNotMisclassify pins the bug where
// httptest's ephemeral port contains the digits of another status code: a
// qualifying 429/503 behind http://127.0.0.1:44017 (contains "401") must NOT be
// rejected as an auth error, and a genuine 401 behind :42900 must NOT be
// treated as a rate limit.
func TestIsQualifyingFallbackError_UrlPortDoesNotMisclassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "429 with port 44017 (401-digits)", err: errors.New(`runner execution error: POST "http://127.0.0.1:44017/chat/completions": 429 Too Many Requests`), want: true},
		{name: "503 with port 40173", err: errors.New(`runner execution error: POST "http://127.0.0.1:40173/chat/completions": 503 Service Unavailable`), want: true},
		{name: "403 with port 40300 stays non-qualifying", err: errors.New(`runner execution error: POST "http://127.0.0.1:40300/chat/completions": 403 Forbidden`), want: false},
		{name: "401 with port 42900 stays non-qualifying", err: errors.New(`runner execution error: POST "http://127.0.0.1:42900/chat/completions": 401 Unauthorized`), want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsQualifyingFallbackError(c.err); got != c.want {
				t.Fatalf("IsQualifyingFallbackError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
