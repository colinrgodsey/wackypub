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

// --- Semantics 1: zero-text mid-turn failover ---

// TestRuntimeFallback_ZeroTextMidTurnFailover pins semantics 1 from the usage-limit
// follow-up: a qualifying error (429 before any body/chunk) that arrives mid-turn with ZERO
// assistant text emitted must fail over to the fallback SAME TURN - re-running the turn from
// scratch on the next backend. Nothing was emitted, so regeneration is clean; the caller must
// see the fallback's answer, not an error.
func TestRuntimeFallback_ZeroTextMidTurnFailover(t *testing.T) {
	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		// 429 with the z.ai-style quota-reset body, before any assistant body/chunks.
		io.WriteString(w, `{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2099-01-02 03:04:05"}}`)
	}))
	defer primary.Close()
	fallback := okBackendServer(t, "fallback answered")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))

	ctx := context.Background()
	var chunks []string
	var warnings []string
	var gotErr error
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "question", func(w string) { warnings = append(warnings, w) }) {
		if err != nil {
			gotErr = err
			break
		}
		chunks = append(chunks, chunk)
	}

	if gotErr != nil {
		t.Fatalf("zero-text mid-turn failover should not surface an error, got: %v", gotErr)
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "fallback answered") {
		t.Fatalf("expected fallback text same turn, got %q", joined)
	}
	// The fallback-engaged warning must be present and name both backends.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, primary.URL) && strings.Contains(w, fallback.URL) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected failover warning naming both backends, got %v", warnings)
	}
}

// --- Semantics 2: text-emitted mid-turn failure surfaces + quota-reset hint annotation ---

// TestRuntimeFallback_TextThenQuota429_AnnotatesReset pins semantics 2: when text HAS been
// emitted and a qualifying 429 arrives, the error surfaces to the caller (frankenstein
// guard: never swap mid-stream) AND a warning names the failure including the quota reset
// time from the provider body.
func TestRuntimeFallback_TextThenQuota429_AnnotatesReset(t *testing.T) {
	callCount := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Narration text + tool call: text reaches the stream before the runner continues.
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Let me check that for you.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_scratchpad","arguments":"{\"text\":\"note\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// Second model call: usage-limit 429 with reset metadata.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2026-09-19 12:14:43"}}`)
	}))
	defer primary.Close()

	var mu sync.Mutex
	fallbackCalls := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fallbackCalls++
		mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"FALLBACK SHOULD NEVER BE REACHED"},"finish_reason":"stop"}]}`)
	}))
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))

	ctx := context.Background()
	var partial []string
	var warnings []string
	var gotErr error
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "question", func(w string) { warnings = append(warnings, w) }) {
		if err != nil {
			gotErr = err
			break
		}
		partial = append(partial, chunk)
	}

	if gotErr == nil {
		t.Fatal("expected 429 after text to surface, got nil")
	}
	if !IsQualifyingFallbackError(gotErr) {
		t.Fatalf("429 must be qualifying, got: %v", gotErr)
	}
	if !strings.Contains(strings.Join(partial, ""), "Let me check that for you.") {
		t.Fatalf("narration text must have reached the caller, got %q", strings.Join(partial, ""))
	}
	// No fallback attempted (frankenstein guard).
	mu.Lock()
	calls := fallbackCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("fallback called %d times after text emitted, want 0", calls)
	}
	// Warning must include the reset-time hint.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "2026-09-19 12:14:43") && strings.Contains(w, "resets") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warning with quota reset hint, got %v", warnings)
	}
}

// --- Semantics 3: skip primary while inside failed-until-reset window ---

// TestRuntimeFallback_SkipPrimaryUntilReset pins semantics 3: after a 429 with a reset hint,
// the NEXT turn skips the failed primary and starts at the first fallback instead of burning
// a turn on the still-down quota.
func TestRuntimeFallback_SkipPrimaryUntilReset(t *testing.T) {
	var mu sync.Mutex
	primaryCalls := 0
	resetAt := time.Now().Add(time.Hour).UTC().Format(quotaResetTimeLayout)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		primaryCalls++
		mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, fmt.Sprintf(`{"error":{"code":"1308","message":"Usage limit reached. Your limit will reset at %s"}}`, resetAt))
	}))
	defer primary.Close()
	fallback := okBackendServer(t, "fallback answered")
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))
	ctx := context.Background()

	// Turn 1: primary 429s with reset metadata -> same-turn fallback + records failed-until.
	var chunks1 []string
	var warnings1 []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "q1", func(w string) { warnings1 = append(warnings1, w) }) {
		if err != nil {
			t.Fatalf("turn 1 error: %v", err)
		}
		chunks1 = append(chunks1, chunk)
	}
	if !strings.Contains(strings.Join(chunks1, ""), "fallback answered") {
		t.Fatalf("turn 1 should come from fallback, got %q", strings.Join(chunks1, ""))
	}

	// Turn 2 (same process, still inside the reset window): primary must be SKIPPED - the
	// turn starts at the fallback, so primary call count does not grow.
	before := 0
	mu.Lock()
	before = primaryCalls
	mu.Unlock()

	var chunks2 []string
	var warnings2 []string
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "q2", func(w string) { warnings2 = append(warnings2, w) }) {
		if err != nil {
			t.Fatalf("turn 2 error: %v", err)
		}
		chunks2 = append(chunks2, chunk)
	}
	if !strings.Contains(strings.Join(chunks2, ""), "fallback answered") {
		t.Fatalf("turn 2 should come from fallback (primary skipped), got %q", strings.Join(chunks2, ""))
	}

	mu.Lock()
	after := primaryCalls
	mu.Unlock()
	if after != before {
		t.Fatalf("primary was called on turn 2 despite failed-until-reset window (before=%d after=%d)", before, after)
	}

	// The skip is surfaced via a warning so the operator knows why the primary was bypassed.
	found := false
	for _, w := range warnings2 {
		if strings.Contains(w, "skipping backend") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected skip warning on turn 2, got %v", warnings2)
	}
}
