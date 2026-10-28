package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultModelMaxRetries mirrors the count the OpenAI SDK applies on its own
	// (openai-go requestconfig MaxRetries=2), so leaving maxRetries unset in
	// runtime.json keeps the attempt budget an agent has today.
	DefaultModelMaxRetries = 2

	// DefaultModelRetryDelayCeiling bounds how long a single rate-limit wait may
	// run. The dependency honors Retry-After with no ceiling at all: measured
	// against a stub returning "Retry-After: 60", one turn slept 1m0s twice, so a
	// provider that asks for five minutes parks a turn for ten. The ceiling is the
	// operator's lever for that, and it is what keeps a rate limit inside a turn
	// budget instead of silently outrunning turnDeadlineSeconds.
	DefaultModelRetryDelayCeiling = 15 * time.Second

	defaultRetryBaseDelay = 500 * time.Millisecond

	// shouldRetryHeader is what the dependency reads to override its own retry
	// decision. The transport stamps it false on whatever it hands back so the SDK
	// does not add attempts on top of ours: total requests stay at maxRetries+1.
	shouldRetryHeader = "X-Should-Retry"
)

// retryPolicy is the resolved rate-limit recovery policy for one model client.
type retryPolicy struct {
	maxRetries    int
	baseDelay     time.Duration
	maxRetryDelay time.Duration
}

// defaultRetryPolicy is what an agent gets with no runtime configuration: the
// dependency's own attempt count, the same 500ms doubling base, and a bounded wait.
func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		maxRetries:    DefaultModelMaxRetries,
		baseDelay:     defaultRetryBaseDelay,
		maxRetryDelay: DefaultModelRetryDelayCeiling,
	}
}

// retryPolicyFromConfig resolves the two runtime knobs. A nil field keeps today's
// value; a negative one is clamped to zero with a warning rather than being treated
// as "retry forever", because the dangerous reading of a typo must be the safe one.
func retryPolicyFromConfig(maxRetries, maxRetryDelaySeconds *int) retryPolicy {
	p := defaultRetryPolicy()
	if maxRetries != nil {
		if *maxRetries < 0 {
			fmt.Fprintf(os.Stderr, "Warning: maxRetries %d is negative, using 0 (no retries).\\n", *maxRetries)
			p.maxRetries = 0
		} else {
			p.maxRetries = *maxRetries
		}
	}
	if maxRetryDelaySeconds != nil {
		if *maxRetryDelaySeconds < 0 {
			fmt.Fprintf(os.Stderr, "Warning: maxRetryDelaySeconds %d is negative, using 0 (never sleep between retries).\\n", *maxRetryDelaySeconds)
			p.maxRetryDelay = 0
		} else {
			p.maxRetryDelay = time.Duration(*maxRetryDelaySeconds) * time.Second
		}
	}
	return p
}

// retryTransport is the rate-limit recovery layer for OpenAI-compatible endpoints.
// It exists because the model adapter exposes no retry knob at all: the only lever
// wackypub has over how its provider throttles it is the *http.Client it hands in.
// Taking it makes the attempt count and the wait ceiling configurable without
// touching the dependency, and it puts both under tests.
type retryTransport struct {
	base   http.RoundTripper
	policy retryPolicy
	notice func(string)
}

func newRetryTransport(base http.RoundTripper, policy retryPolicy) *retryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, policy: policy, notice: warnOnce}
}

// warnOnce writes a recovery notice in the repo's stderr Warning: format. It sits
// behind a field so a test can capture what a transport decided instead of
// intercepting the process's stderr.
func warnOnce(msg string) {
	fmt.Fprintln(os.Stderr, "Warning: "+msg)
}

// isRetryableStatus follows the dependency's own rule: timeouts, the conflict and
// too-many-requests family, and anything from the server. A 400 is not retryable, so
// a malformed request still fails fast instead of being hammered.
func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= http.StatusInternalServerError
}

// RoundTrip replays a throttled request until the policy is spent, then hands the
// last response to the caller with the SDK's own retry switch turned off.
//
// Two invariants matter here. The body is buffered once and re-served per attempt,
// because a chat completion POST has an unreadable stream after its first attempt and
// a silent empty body on retry would look like a fresh empty prompt. And no wait is
// ever allowed to outlive the caller's context: if the next sleep cannot fit, the
// response is returned now, so a rate limit degrades into a clean error instead of a
// wedged turn.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	var payload []byte
	if req.Body != nil {
		buf, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to buffer request body for retry: %w", err)
		}
		payload = buf
	}

	var resp *http.Response
	var rtErr error
	var waited time.Duration

	for attempt := 0; ; attempt++ {
		attemptReq := req.Clone(ctx)
		if payload != nil {
			attemptReq.Body = io.NopCloser(bytes.NewReader(payload))
			attemptReq.ContentLength = int64(len(payload))
		}
		// Per-attempt stamps must not accumulate on the shared template request.
		attemptReq.Header = req.Header.Clone()
		attemptReq.Header.Del(shouldRetryHeader)

		resp, rtErr = t.base.RoundTrip(attemptReq)
		if rtErr != nil {
			// Connection-level failures stay the dependency's problem: it already
			// retries them, and re-sending a request the transport may have already
			// delivered is not this layer's call.
			return nil, rtErr
		}
		if !isRetryableStatus(resp.StatusCode) {
			break
		}
		if attempt >= t.policy.maxRetries {
			waitedLabel := "no wait"
			if waited > 0 {
				waitedLabel = "after waiting " + waited.String()
			}
			t.notice(fmt.Sprintf("%s %s returned %s and the retry budget (%d) is spent %s - surfacing the error instead of waiting further.",
				req.Method, req.URL.Path, resp.Status, t.policy.maxRetries, waitedLabel))
			break
		}

		delay := t.delayFor(resp, attempt)
		remaining, hasDeadline := ctx.Deadline()
		if hasDeadline {
			left := time.Until(remaining)
			if delay >= left {
				t.notice(fmt.Sprintf("%s %s returned %s but the %s wait will not fit the remaining %s of this turn - surfacing the error instead of wedging it.",
					req.Method, req.URL.Path, resp.Status, delay, left))
				break
			}
		}

		drainBody(resp)
		if !sleepContext(ctx, delay) {
			// Cancellation wins over a retry: the drained response is unusable and the
			// caller is already going away.
			return nil, ctx.Err()
		}
		waited += delay
	}

	if resp != nil {
		// The policy is spent or the answer is good: nothing above us should retry.
		resp.Header.Set(shouldRetryHeader, "false")
	}
	return resp, nil
}

// delayFor picks the wait: the server's Retry-After when it gave one and it is in the
// future, otherwise exponential doubling from the base. Both are clamped by the
// policy ceiling, which is the whole point of the ceiling - a provider asking for ten
// minutes must not be able to park a turn for ten minutes.
func (t *retryTransport) delayFor(resp *http.Response, attempt int) time.Duration {
	// The doubling saturates long before any sane ceiling, so the exponent is capped
	// rather than shifted raw: a 64-bit shift at 64 or more yields zero, which would turn
	// a generous maxRetries into a tight hammering loop against a throttling provider.
	shift := attempt
	if shift > 20 {
		shift = 20
	}
	delay := time.Duration(float64(t.policy.baseDelay) * float64(int64(1)<<uint(shift)))
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && secs >= 0 {
			delay = time.Duration(secs) * time.Second
		} else if when, err := http.ParseTime(raw); err == nil {
			if until := time.Until(when); until > delay {
				delay = until
			}
		}
	}
	if delay > t.policy.maxRetryDelay {
		delay = t.policy.maxRetryDelay
	}
	if delay < 0 {
		delay = 0
	}
	// Full jitter on the computed backoff only. A server-supplied Retry-After is a
	// request, not a suggestion to spread, so it passes through unjittered.
	if resp.Header.Get("Retry-After") == "" && delay > 0 {
		delay = time.Duration(rand.Int63n(int64(delay)) + 1)
	}
	return delay
}

// sleepContext sleeps for d, reporting false if ctx was cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// drainBody closes a response we are discarding so its connection can be reused;
// an unread 429 body otherwise pins a connection per retry.
func drainBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}
