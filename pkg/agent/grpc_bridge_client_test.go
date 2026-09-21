package agent

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTranslateBridgeError_LockContentionClassifiedAsSuperseded pins
// bugs/wackyacp/lock-wait-visibility: when a bridge exits because it was cancelled
// while waiting on the producer-side session lock (stderr carries the contention
// sentinel), the error must surface as ErrDispatchSuperseded, NOT
// ErrBridgeProcessDied - the process did not crash, it was superseded by a
// concurrent turn.
func TestTranslateBridgeError_LockContentionClassifiedAsSuperseded(t *testing.T) {
	tb := &tailBuffer{limit: 4096}
	tb.Write([]byte("acquiring lock on /x/acp-session.lock (acp-session.lock contention, waited but ctx cancelled): context canceled\n"))

	err := translateBridgeError(status.Error(codes.Unavailable, "EOF"), "agent-a", "wackyacp", nil, tb)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, ErrDispatchSuperseded) {
		t.Errorf("expected ErrDispatchSuperseded, got: %v", err)
	}
	if errors.Is(err, ErrBridgeProcessDied) {
		t.Errorf("lock-wait cancellation must not classify as ErrBridgeProcessDied, got: %v", err)
	}
}

// TestTranslateBridgeError_NoSentinelStillBridgeDied pins that the normal crash path is
// unchanged: without the sentinel an unavailable bridge still yields
// ErrBridgeProcessDied with stderr context.
func TestTranslateBridgeError_NoSentinelStillBridgeDied(t *testing.T) {
	tb := &tailBuffer{limit: 4096}
	tb.Write([]byte("panic: something crashed\n"))

	err := translateBridgeError(status.Error(codes.Unknown, "EOF"), "agent-a", "wackyacp", nil, tb)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, ErrBridgeProcessDied) {
		t.Errorf("expected ErrBridgeProcessDied for non-contention exit, got: %v", err)
	}
	if !strings.Contains(err.Error(), "panic: something crashed") {
		t.Errorf("expected stderr context in error, got: %v", err)
	}
}
