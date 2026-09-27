package agent

import (
	"errors"
	"testing"
)

// TestIsQualifyingFallbackError_UrlPortDoesNotMisclassify pins the merge-blocking flake
// family root cause: httptest's ephemeral port (or a net/http dial address) contains the
// digits of another status code, and the classifier must not let those digits flip the
// verdict. Forms covered: quoted URLs, unquoted URLs, and the BARE ADDRESS inside a
// net/http dial error ("dial tcp 127.0.0.1:40173") which is not inside any URL.
func TestIsQualifyingFallbackError_UrlPortDoesNotMisclassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "429 with port 44017 (401-digits)", err: errors.New(`runner execution error: POST "http://127.0.0.1:44017/chat/completions": 429 Too Many Requests`), want: true},
		{name: "503 with port 40173", err: errors.New(`runner execution error: POST "http://127.0.0.1:40173/chat/completions": 503 Service Unavailable`), want: true},
		{name: "503 unquoted URL with 401-digit port", err: errors.New(`runner execution error: POST http://127.0.0.1:43401/chat/completions: 503 Service Unavailable`), want: true},
		{name: "dial-refused bare address with 401 digits", err: errors.New(`Post "http://127.0.0.1:36931/chat/completions": dial tcp 127.0.0.1:40173: connect: connection refused`), want: true},
		{name: "dial-refused bare IPv4 with 403 digits", err: errors.New(`dial tcp 10.0.0.5:40312: connect: connection refused`), want: true},
		{name: "dial-refused bracketed IPv6 with 401 digits", err: errors.New(`dial tcp [::1]:40173: connect: connection refused`), want: true},
		{name: "dial-refused bracketed IPv6 full addr with 403 digits", err: errors.New(`dial tcp [2001:db8::1]:40312: connect: connection refused`), want: true},
		{name: "dial-refused localhost.localdomain with 403 digits", err: errors.New(`dial tcp localhost.localdomain:40312: connect: connection refused`), want: true},
		{name: "403 with port 40300 stays non-qualifying", err: errors.New(`runner execution error: POST "http://127.0.0.1:40300/chat/completions": 403 Forbidden`), want: false},
		{name: "401 with port 42900 stays non-qualifying", err: errors.New(`runner execution error: POST "http://127.0.0.1:42900/chat/completions": 401 Unauthorized`), want: false},
		{name: "401 with 503-digit port stays non-qualifying", err: errors.New(`POST "http://127.0.0.1:50321/chat/completions": 401 Unauthorized`), want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsQualifyingFallbackError(c.err); got != c.want {
				t.Fatalf("IsQualifyingFallbackError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
