package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRuntimeFallback_MidStreamFailureNeverMasks is the frankenstein-output guard: once a
// turn has yielded real text, a later qualifying failure must be surfaced straight to the
// caller - the fallback chain must NOT re-run on a different backend mid-turn (that would
// replay history across provider encodings and splice two backends' output into one reply).
//
// The realistic mid-stream failure in wackypub's non-streaming adapter is a multi-call turn:
// model call 1 yields narration text plus a tool call (the text chunk reaches the stream
// before the runner continues), the tool executes, and model call 2 fails qualifying (503).
// The fallback must never be contacted, and the caller must see both the partial text and
// the error.
func TestRuntimeFallback_MidStreamFailureNeverMasks(t *testing.T) {
	callCount := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// Model outputs narration text AND calls the built-in scratchpad tool: the text
			// is yielded to the stream before the runner executes the tool.
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Let me check that for you.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_scratchpad","arguments":"{\"text\":\"note\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// Tool round-trip completes, then the second model call fails with 503.
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"boom after narration"}}`)
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
	var gotErr error
	for chunk, err := range sdk.addAndGenerateTurnStreamImpl(ctx, "fbagent", "question") {
		if err != nil {
			gotErr = err
			break
		}
		partial = append(partial, chunk)
	}

	// (1) The error must surface to the caller, and it must be QUALIFYING - otherwise the
	// no-fallback assertion below would pass for the wrong reason (an error that never
	// qualifies never descends, with or without the yieldedText guard).
	if gotErr == nil {
		t.Fatal("expected mid-turn error to surface, got nil")
	}
	if !IsQualifyingFallbackError(gotErr) {
		t.Fatalf("mid-turn error must be qualifying for this test to be meaningful, got: %v", gotErr)
	}

	// (2) No fallback attempt - once text was yielded, failover is forbidden.
	mu.Lock()
	calls := fallbackCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("fallback backend called %d times after text yielded; frankenstein output prevented", calls)
	}

	// (3) The narration text from the first model call MUST have reached the caller before
	// the error - that is what proves the guard was exercised (text seen => no descend).
	joined := strings.Join(partial, "")
	if !strings.Contains(joined, "Let me check that for you.") {
		t.Fatalf("expected narration text to yield before the error, got %q", joined)
	}
	if strings.Contains(joined, "FALLBACK SHOULD NEVER BE REACHED") {
		t.Fatalf("fallback text leaked into reply: %q", joined)
	}
}

// TestRuntimeFallback_MidTurnErrorOnSecondCall_FailsForward is the same guard exercised
// through the continue-only generate path: narration text is yielded, the tool runs, and
// the second model call fails with a qualifying transport error. No fallback, error
// surfaces, narration preserved.
func TestRuntimeFallback_MidTurnErrorOnSecondCall_FailsForward(t *testing.T) {
	callCount := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Narration first.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_scratchpad","arguments":"{\"text\":\"n2\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// Second model call: connection reset (transport-class qualifying failure).
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer primary.Close()

	var mu sync.Mutex
	fallbackCalls := 0
	fallback := okBackendServer(t, "FALLBACK SHOULD NEVER BE REACHED")
	fallback.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fallbackCalls++
		mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"FALLBACK SHOULD NEVER BE REACHED"},"finish_reason":"stop"}]}`)
	})
	defer fallback.Close()

	sdk, _ := setupFallbackAgent(t, mustFallbackRuntime(t, primary.URL, fallback.URL))
	ctx := context.Background()

	var partial []string
	var gotErr error
	for chunk, err := range sdk.generateTurnStreamImpl(ctx, "fbagent") {
		if err != nil {
			gotErr = err
			break
		}
		partial = append(partial, chunk)
	}

	if gotErr == nil {
		t.Fatal("expected error after narration, got nil")
	}
	if !IsQualifyingFallbackError(gotErr) {
		t.Fatalf("transport error must be qualifying for this test to be meaningful, got: %v", gotErr)
	}
	mu.Lock()
	calls := fallbackCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("fallback backend called %d times after text yielded, want 0", calls)
	}
	joined := strings.Join(partial, "")
	if !strings.Contains(joined, "Narration first.") {
		t.Fatalf("expected narration text to yield before the error, got %q", joined)
	}
	if strings.Contains(joined, "FALLBACK SHOULD NEVER BE REACHED") {
		t.Fatalf("fallback text leaked into reply: %q", joined)
	}
}
