package agent

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Unrecoverable represents a fatal condition where internal state is corrupt
// and the server process cannot safely continue serving subsequent RPCs.
// When a panic value implements Unrecoverable (or unwraps to an Unrecoverable error),
// the stdio-serve panic recovery mechanism will NOT swallow the panic: it logs the
// unrecoverable state to stderr and re-panics, terminating the process so supervision
// (systemd / ProcessDialer watchdog) can cleanly restart the server from a fresh state.
//
// In contrast, per-turn recoverable failures (model API timeouts/500s, tool errors,
// session lock contention, runtime panics) are caught, formatted, and returned as
// protocol errors over gRPC, keeping the server process alive.
type Unrecoverable interface {
	IsUnrecoverable() bool
}

// CorruptStateError marks an internal error as unrecoverable corrupt state.
type CorruptStateError struct {
	Reason string
	Err    error
}

func (e *CorruptStateError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("unrecoverable corrupt state (%s): %v", e.Reason, e.Err)
	}
	return fmt.Sprintf("unrecoverable corrupt state: %s", e.Reason)
}

func (e *CorruptStateError) Unwrap() error {
	return e.Err
}

func (e *CorruptStateError) IsUnrecoverable() bool {
	return true
}

// MarkUnrecoverable wraps a reason (and optional error) into an unrecoverable CorruptStateError.
func MarkUnrecoverable(reason string, err ...error) error {
	var inner error
	if len(err) > 0 {
		inner = err[0]
	}
	return &CorruptStateError{Reason: reason, Err: inner}
}

// IsUnrecoverable reports whether val represents an unrecoverable state.
func IsUnrecoverable(val any) bool {
	if val == nil {
		return false
	}
	if u, ok := val.(Unrecoverable); ok && u.IsUnrecoverable() {
		return true
	}
	if err, ok := val.(error); ok {
		var u Unrecoverable
		if errors.As(err, &u) && u.IsUnrecoverable() {
			return true
		}
	}
	return false
}

// ToGRPCError converts any Go error into an appropriate gRPC status error.
// Existing status errors are returned unmodified. Context errors are mapped to
// codes.DeadlineExceeded or codes.Canceled. All other errors default to codes.Unknown.
func ToGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Errorf(codes.DeadlineExceeded, "%v", err)
	}
	if errors.Is(err, context.Canceled) {
		return status.Errorf(codes.Canceled, "%v", err)
	}
	return status.Errorf(codes.Unknown, "%v", err)
}

// Test hooks for fault injection in tests.
var (
	testHookTurnPreGenerate    func(agentID, userMessage string)
	testHookRoutingPreDispatch func(methodName string, req any)
)
