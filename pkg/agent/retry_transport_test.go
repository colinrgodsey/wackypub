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
	errorBody  string
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
			body := s.errorBody
			if body == "" {
				body = `{"error":{"message":"Rate limit exceeded"}}`
			}
			fmt.Fprint(w, body)
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
	return newStubServerWithBody(t, statuses, retryAfter, "")
}

func newStubServerWithBody(t *testing.T, statuses []int, retryAfter, errorBody string) *rateStub {
	t.Helper()
	stub := &rateStub{statuses: statuses, retryAfter: retryAfter, errorBody: errorBody}
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

// failoverPolicy adds the magnitude threshold on top, since a hand-built policy leaves
// it zero, meaning disabled.
func failoverPolicy(maxRetries int, ceiling, threshold time.Duration) retryPolicy {
	p := policyWith(maxRetries, ceiling)
	p.failoverAfter = threshold
	return p
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
	def := retryPolicyFromConfig(nil, nil, nil)
	if def.maxRetries != DefaultModelMaxRetries || def.maxRetryDelay != DefaultModelRetryDelayCeiling || def.baseDelay != defaultRetryBaseDelay {
		t.Errorf("unset config changed today's defaults: %+v", def)
	}

	zero, big := 0, 90
	got := retryPolicyFromConfig(&zero, &big, nil)
	if got.maxRetries != 0 || got.maxRetryDelay != 90*time.Second {
		t.Errorf("explicit values ignored: %+v", got)
	}

	negRetries, negDelay := -1, -5
	warned := captureStderr(func() {
		clamped := retryPolicyFromConfig(&negRetries, &negDelay, nil)
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

// TestRetryTransportFailsOverOnHugeRetryAfter is the subscription-usage case: the
// provider says come back in five hours, so waiting at this endpoint cannot save the
// turn and the fallback chain must get it. One request, no sleep, an explanation.
func TestRetryTransportFailsOverOnHugeRetryAfter(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests}, "18000")
	notice := &retryTransportRecorder{}
	tr := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(2, 15*time.Second, time.Minute), notice: notice.add}

	start := time.Now()
	res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer res.Body.Close()

	if stub.count() != 1 {
		t.Errorf("a five-hour wait must not be retried at this level, got %d requests", stub.count())
	}
	if elapsed > time.Second {
		t.Errorf("fail-over must be immediate, slept %v", elapsed)
	}
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the throttled response must still be surfaced, got %s", res.Status)
	}
	lines := notice.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], "failing over") {
		t.Errorf("expected a fail-over notice so the operator knows why the level gave up, got %v", lines)
	}
}

// TestRetryTransportComparesTheRequestedWaitNotTheClampedWait pins the subtlety that
// makes the threshold work: the decision looks at what the provider asked for, never at
// what this transport would sleep after the ceiling. A five-hour ask clamped to fifteen
// seconds still fails over, and a thirty-second ask clamped to one millisecond still
// retries, because thirty seconds is under the threshold.
func TestRetryTransportComparesTheRequestedWaitNotTheClampedWait(t *testing.T) {
	clampedButHuge := newStubServer(t, []int{http.StatusTooManyRequests}, "18000")
	tight := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(2, 15*time.Millisecond, time.Minute), notice: (&retryTransportRecorder{}).add}
	res, err := tight.RoundTrip(transportRequest(t, context.Background(), clampedButHuge.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res.Body.Close()
	if clampedButHuge.count() != 1 {
		t.Errorf("18000s clamped to a 15ms ceiling must still fail over, got %d requests", clampedButHuge.count())
	}

	shortButClamped := newStubServer(t, []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK}, "30")
	loose := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(2, time.Millisecond, time.Minute), notice: (&retryTransportRecorder{}).add}
	res2, err := loose.RoundTrip(transportRequest(t, context.Background(), shortButClamped.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res2.Body.Close()
	if shortButClamped.count() != 3 {
		t.Errorf("a 30s wait is under the 60s threshold and must retry to success, got %d requests", shortButClamped.count())
	}
}

// TestRetryTransportFailoverThresholdBoundary fixes the comparison as strictly greater
// than, so a provider that asks for exactly the threshold is still given a retry.
func TestRetryTransportFailoverThresholdBoundary(t *testing.T) {
	atThreshold := newStubServer(t, []int{http.StatusTooManyRequests, http.StatusOK}, "60")
	tr := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(1, time.Millisecond, time.Minute), notice: (&retryTransportRecorder{}).add}
	res, err := tr.RoundTrip(transportRequest(t, context.Background(), atThreshold.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res.Body.Close()
	if atThreshold.count() != 2 {
		t.Errorf("a wait equal to the threshold should retry once, got %d requests", atThreshold.count())
	}

	justOver := newStubServer(t, []int{http.StatusTooManyRequests}, "61")
	tr2 := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(1, time.Millisecond, time.Minute), notice: (&retryTransportRecorder{}).add}
	res2, err := tr2.RoundTrip(transportRequest(t, context.Background(), justOver.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res2.Body.Close()
	if justOver.count() != 1 {
		t.Errorf("a wait one second over the threshold must fail over, got %d requests", justOver.count())
	}
}

// TestRetryTransportFailsOverOnQuotaWithoutRetryAfter covers the case magnitude cannot
// describe: a spent account that gives no wait hint at all. It also proves the body peek
// restores the response, since the caller has to report the provider's own message.
func TestRetryTransportFailsOverOnQuotaWithoutRetryAfter(t *testing.T) {
	quota := `{"error":{"message":"You exceeded your current quota, please check your plan and billing details","type":"insufficient_quota"}}`
	stub := newStubServerWithBody(t, []int{http.StatusTooManyRequests}, "", quota)
	notice := &retryTransportRecorder{}
	tr := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(3, time.Minute, time.Minute), notice: notice.add}

	res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer res.Body.Close()

	if stub.count() != 1 {
		t.Errorf("a spent quota must not be retried at this level, got %d requests", stub.count())
	}
	payload, readErr := io.ReadAll(res.Body)
	if readErr != nil || string(payload) != quota {
		t.Errorf("peeking the body must leave it exactly as it arrived, got %q err %v", payload, readErr)
	}
	lines := notice.snapshot()
	if len(lines) != 1 || !(strings.Contains(lines[0], "exceeded your current quota") || strings.Contains(lines[0], "insufficient_quota")) {
		t.Errorf("the notice should name the marker that ended the level, got %v", lines)
	}
}

// TestRetryTransportLeavesAnUnexplainedRateLimitToTheBudget states the conservative
// default out loud: no wait hint and no quota marker means retry, not guess a fail-over
// from a bare status code.
func TestRetryTransportLeavesAnUnexplainedRateLimitToTheBudget(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests}, "")
	tr := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(2, time.Millisecond, time.Minute), notice: (&retryTransportRecorder{}).add}

	res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	res.Body.Close()
	if stub.count() != 3 {
		t.Errorf("a 429 with no wait and no quota marker should spend the retry budget, got %d requests", stub.count())
	}
}

func TestRetryPolicyFromConfigFailoverThreshold(t *testing.T) {
	if got := retryPolicyFromConfig(nil, nil, nil).failoverAfter; got != DefaultRetryAfterFailover {
		t.Errorf("unset threshold should keep the default, got %v", got)
	}

	halfMinute := 30
	if got := retryPolicyFromConfig(nil, nil, &halfMinute).failoverAfter; got != 30*time.Second {
		t.Errorf("explicit threshold ignored, got %v", got)
	}

	off := 0
	if got := retryPolicyFromConfig(nil, nil, &off).failoverAfter; got != 0 {
		t.Errorf("0 must disable magnitude fail-over, got %v", got)
	}

	broken := -1
	warned := captureStderr(func() {
		if got := retryPolicyFromConfig(nil, nil, &broken).failoverAfter; got != 0 {
			t.Errorf("a negative threshold must disable rather than fail over on everything, got %v", got)
		}
	})
	if !strings.Contains(warned, "retryAfterFailoverSeconds -1") {
		t.Errorf("a nonsense threshold must announce itself, got %q", warned)
	}
}

// TestFailOverSurfacesAnErrorTheFallbackChainQualifies bridges the two layers this
// finding is about: giving up on the level has to produce an error the chain accepts, or
// a fast fail-over would just be a fast failure.
func TestFailOverSurfacesAnErrorTheFallbackChainQualifies(t *testing.T) {
	stub := newStubServer(t, []int{http.StatusTooManyRequests}, "18000")
	tr := &retryTransport{base: http.DefaultTransport, policy: failoverPolicy(2, 15*time.Second, time.Minute), notice: (&retryTransportRecorder{}).add}

	res, err := tr.RoundTrip(transportRequest(t, context.Background(), stub.serverURL, `{"model":"m"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer res.Body.Close()

	surfaced := fmt.Errorf("POST %q: %s", stub.serverURL+"/chat/completions", res.Status)
	if !IsQualifyingFallbackError(surfaced) {
		t.Errorf("a failed-over rate limit must qualify for the next level, got %v", surfaced)
	}
	if IsQualifyingFallbackError(fmt.Errorf("POST %q: 401 Unauthorized", stub.serverURL)) {
		t.Error("an auth failure must not qualify, or a bad key would silently spend the fallback chain too")
	}
}
