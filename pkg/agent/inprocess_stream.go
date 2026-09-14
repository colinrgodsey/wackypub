package agent

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// InProcessStream implements grpc.ServerStreamingServer[T] for in-process dispatch
// (D112 Phase 2). It is the CLI-side half of the streaming RPCs: callers construct one,
// pass it to the proto-typed AgentSDK method (e.g. AgentSDK.GenerateTurnStream), then
// range over Chunks(). Send blocks until the receiver drains, providing natural
// backpressure between a fast model and a slow consumer without any network pipe.
// It is safe for concurrent use: Send may be called from one goroutine while the
// consumer ranges from another, and Close closes the underlying channel exactly once.
type InProcessStream[T any] struct {
	ctx       context.Context
	ch        chan *T
	closeOnce sync.Once
}

// NewInProcessStream creates a stream adapter bound to ctx with buffer size n.
func NewInProcessStream[T any](ctx context.Context, n int) *InProcessStream[T] {
	if n < 0 {
		n = 0
	}
	return &InProcessStream[T]{
		ctx: ctx,
		ch:  make(chan *T, n),
	}
}

// Send pushes a response onto the stream. It selects on the context so a cancelled
// caller unblocks the producer instead of deadlocking on a full buffer.
func (s *InProcessStream[T]) Send(msg *T) error {
	select {
	case s.ch <- msg:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// Context returns the stream's bound context.
func (s *InProcessStream[T]) Context() context.Context {
	return s.ctx
}

// Chunks returns the receive side of the stream. The channel is closed by Close when
// the producer RPC returns, so a plain range terminates when generation completes.
func (s *InProcessStream[T]) Chunks() <-chan *T {
	return s.ch
}

// Close closes the underlying channel exactly once. The server-side RPC handler
// should defer Close after the streaming method returns so the consumer's range exits.
func (s *InProcessStream[T]) Close() {
	s.closeOnce.Do(func() { close(s.ch) })
}

// The following round out grpc.ServerStream for interface conformance; they are no-ops
// in-process since header/trailer/metadata flows have no wire to land on. SendMsg and
// RecvMsg are unused by generated server-streaming handlers (which call Send directly).

func (s *InProcessStream[T]) SetHeader(metadata.MD) error  { return nil }
func (s *InProcessStream[T]) SendHeader(metadata.MD) error { return nil }
func (s *InProcessStream[T]) SetTrailer(metadata.MD)       {}
func (s *InProcessStream[T]) SendMsg(m any) error          { return nil }
func (s *InProcessStream[T]) RecvMsg(m any) error          { return nil }

var _ grpc.ServerStreamingServer[struct{}] = (*InProcessStream[struct{}])(nil)
