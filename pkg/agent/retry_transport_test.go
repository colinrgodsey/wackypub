package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// rateStub answers with a scripted status per request, so a test can name the exact
// number of attempts a policy is allowed to make.
type rateStub struct {
	mu         sync.Mutex
	requests   int
	bodies     []string
	statuses   []int
	retryAfter string
	serverURL  string
}

func (s *rateStub) statusFor(n int) int {
	if n <= len(s.statuses) {
		return s.statuses[n-1]
	}
	return s.statuses[len(s.statuses)-1]
}

func (s *rateStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("stub could not read body: %v", err)
		}
		s.mu.Lock()
		s.requests++
		n := s.requests
		s.bodies = append(s.bodies, string(raw))
		status := s.statusFor(n)
		retryAfter := s.retryAfter
		s.mu.Unlock()

		if status != http.StatusOK {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"Rate limit exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
}

func (s *rateStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *rateStub) bodyAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		return ""
	}
	return s.bodies[i]
}

func newStubServer(t *testing.T, statuses []int, retryAfter string) *rateStub {
	t.Helper()
	stub := &rateStub{statuses: statuses, retryAfter: retryAfter}
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)
	stub.serverURL = srv.URL
	return stub
}

func transportRequest(t *testing.T, ctx context.Context, url string, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

type retryTransportRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *retryTransportRecorder) add(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, msg)
}

func (r *retryTransportRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func policyWith(maxRetries int, ceiling time.Duration) retryPolicy {
	return retryPolicy{maxRetries: maxRetries, baseDelay: time.Millisecond, maxRetryDelay: ceiling}
}

// TestRetryTransportExhaustionSpendsBudgetAndKeepsTheError asserts the whole point of
// the layer: exactly maxRetries+1 requests reach the provider, the throttled response
// is handed on with its body intact so the caller can report it, the notice fires, and
// the SDK is told not to retry again on top of us.
func TestRetryTransportExhaustionSpendsBudgetAndKeepsTheError(t *testing.T) {
	for _, maxRetries := range []int{0, 1, 3} {
		stub := newStubServer(t, []int{http.StatusTooManyRequests}, "")
		notice := &retryTransportRecorder{}
		tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(maxRetries, time.Second), notice: notice.add}

		res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		if want := maxRetries + 1; stub.count() != want {
			t.Errorf("maxRetries=%d: got %d requests, want exactly %d", maxRetries, stub.count(), want)
		}
		if res.StatusCode != http.StatusTooManyRequests {
			t.Errorf("maxRetries=%d: final status %s, want the 429 surfaced", maxRetries, res.Status)
		}
		payload, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil || !strings.Contains(string(payload), "Rate limit exceeded") {
			t.Errorf("maxRetries=%d: throttled body must survive for the caller, got %q err %v", maxRetries, payload, readErr)
		}
		if got := res.Header.Get(shouldRetryHeader); got != "false" {
			t.Errorf("maxRetries=%d: response must carry %s: false so the SDK does not add attempts, got %q",
				maxRetries, shouldRetryHeader, got)
		}
		// A throttled answer is worth a notice whether the budget ran out or was
		// switched off: either way the turn is ending on a rate limit.
		lines := notice.snapshot()
		if len(lines) != 1 || !strings.Contains(lines[0], "retry budget") {
			t.Errorf("maxRetries=%d: expected one exhaustion notice, got %v", maxRetries, lines)
		}
	}
}

func TestRetryTransportRecoversAfterRateLimit(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK}, "0")
	notice := &retryTransportRecorder{}
	tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(2, time.Second), notice: notice.add}

	res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a rate limit that clears must yield the completion, got %s", res.Status)
	}
	if stub.count() != 3 {
		t.Errorf("expected 2 throttled attempts then success, got %d requests", stub.count())
	}
	if lines := notice.snapshot(); len(lines) != 0 {
		t.Errorf("a recovered request should not warn about exhaustion, got %v", lines)
	}
}

// TestRetryTransportCeilsServerRequestedWait is the finding the whole change exists
// for, checked as a pure function so no clock is involved: a provider asking for a
// minute gets the default ceiling, not a minute.
func TestRetryTransportCeilsServerRequestedWait(t *testing.T) {
	tr := &retryTransport{base: http.DefaultTransport, policy: defaultRetryPolicy()}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "60")

	if got := tr.delayFor(resp, 0); got != DefaultModelRetryDelayCeiling {
		t.Errorf("Retry-After: 60 under the default policy gave %v, want the %v ceiling", got, DefaultModelRetryDelayCeiling)
	}
	resp.Header.Set("Retry-After", "3")
	if got := tr.delayFor(resp, 0); got != 3*time.Second {
		t.Errorf("a Retry-After inside the ceiling must be honoured exactly, got %v", got)
	}
	resp.Header.Set("Retry-After", "0")
	if got := tr.delayFor(resp, 0); got != 0 {
		t.Errorf("Retry-After: 0 must not sleep, got %v", got)
	}
}

func TestRetryTransportComputedBackoffDoublesUnderTheCeiling(t *testing.T) {
	tr := &retryTransport{base: http.DefaultTransport, policy: retryPolicy{maxRetries: 5, baseDelay: 100 * time.Millisecond, maxRetryDelay: 400 * time.Millisecond}}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}

	for attempt, wantMax := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond} {
		got := tr.delayFor(resp, attempt)
		if got <= 0 || got > wantMax {
			t.Errorf("attempt %d: jittered backoff %v outside (0, %v]", attempt, got, wantMax)
		}
	}
	// The shift must not overflow into a negative sleep for an absurd retry count.
	if got := tr.delayFor(resp, 80); got <= 0 || got > 400*time.Millisecond {
		t.Errorf("attempt 80 overflowed the backoff shift: %v", got)
	}
}

// TestRetryTransportDoesNotSleepPastTheTurn closes the wedge path: when the next wait
// cannot fit the remaining deadline, the throttled response comes back immediately with
// an explanation instead of parking the turn.
func TestRetryTransportDoesNotSleepPastTheTurn(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests}, "30")
	notice := &retryTransportRecorder{}
	tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(3, time.Minute), notice: notice.add}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := tr.RoundTrip(transportRequest(t, ctx, stub.serverURL, `{"model":"m"}`))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer res.Body.Close()

	if elapsed > time.Second {
		t.Errorf("a wait that cannot fit must not be taken; slept %v", elapsed)
	}
	if stub.count() != 1 {
		t.Errorf("expected a single attempt when the wait cannot fit, got %d", stub.count())
	}
	lines := notice.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], "will not fit") {
		t.Errorf("expected a notice explaining the deadline refusal, got %v", lines)
	}
}

func TestRetryTransportLeavesNonRetryableAlone(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		stub := newStubServer(t, []int{status}, "")
		tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(3, time.Second), notice: (&retryTransportRecorder{}).add}

		res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
		if err != nil {
			t.Fatalf("round trip %d: %v", status, err)
		}
		res.Body.Close()
		if stub.count() != 1 {
			t.Errorf("status %d must be surfaced after one attempt, got %d", status, stub.count())
		}
	}
}

// TestRetryPolicyFromConfig pins the two knobs and the safe reading of a typo: unset
// keeps what agents have today, negative clamps to the harmless end and says so.
func TestRetryPolicyFromConfig(t *testing.T) {
	def := retryPolicyFromConfig(nil, nil)
	if def.maxRetries != DefaultModelMaxRetries || def.maxRetryDelay != DefaultModelRetryDelayCeiling || def.baseDelay != defaultRetryBaseDelay {
		t.Errorf("unset config changed today's defaults: %+v", def)
	}

	zero, big := 0, 90
	got := retryPolicyFromConfig(&zero, &big)
	if got.maxRetries != 0 || got.maxRetryDelay != 90*time.Second {
		t.Errorf("explicit values ignored: %+v", got)
	}

	negRetries, negDelay := -1, -5
	warned := captureStderr(func() {
		clamped := retryPolicyFromConfig(&negRetries, &negDelay)
		if clamped.maxRetries != 0 || clamped.maxRetryDelay != 0 {
			t.Errorf("negative values must clamp to 0, got %+v", clamped)
		}
	})
	if !strings.Contains(warned, "maxRetries -1") || !strings.Contains(warned, "maxRetryDelaySeconds -5") {
		t.Errorf("clamping must be announced on stderr, got %q", warned)
	}
}

// TestRetryTransportReservesTheSameBodyEveryAttempt guards the quiet disaster: a request
// body is a one-shot stream, so without buffering, retry two would send an empty prompt
// and the model would answer a question nobody asked.
func TestRetryTransportReservesTheSameBodyEveryAttempt(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK}, "0")
	tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(2, time.Second), notice: (&retryTransportRecorder{}).add}

	body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, body))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res.Body.Close()

	if stub.count() != 3 {
		t.Fatalf("expected 3 attempts, got %d", stub.count())
	}
	for i := 0; i < 3; i++ {
		if got := stub.bodyAt(i); got != body {
			t.Errorf("attempt %d body differs from the original:\n got %q\nwant %q", i+1, got, body)
		}
	}
}

func TestRetryTransportSurvivesAnEmptyBody(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests, http.StatusOK}, "0")
	tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(1, time.Second), notice: (&retryTransportRecorder{}).add}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, stub.serverURL+"/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res.Body.Close()
	if stub.count() != 2 {
		t.Errorf("a bodyless request must still retry, got %d attempts", stub.count())
	}
}

func TestRetryTransportRespectsContextCancellationMidWait(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests}, "30")
	tr := &retryTransport{base: http.DefaultTransport, policy: policyWith(3, time.Minute), notice: (&retryTransportRecorder{}).add}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	_, err := tr.RoundTrip(transportRequest(t, ctx, stub.serverURL, `{"model":"m"}`))
	if err == nil {
		t.Fatal("a cancelled wait must surface the cancellation, not a response")
	}
	if !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("expected the context error, got %v", err)
	}
}

func TestSleepContext(t *testing.T) {
	if !sleepContext(context.Background(), time.Millisecond) {
		t.Error("a normal sleep must report success")
	}
	if !sleepContext(context.Background(), 0) {
		t.Error("a zero sleep on a live context must report success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepContext(ctx, time.Second) {
		t.Error("a cancelled context must abort the wait")
	}
}
