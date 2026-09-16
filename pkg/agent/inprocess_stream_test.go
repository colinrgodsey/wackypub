package agent

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestInProcessStreamClient_RepeatedRecvAfterEOF(t *testing.T) {
	ctx := context.Background()
	stream := NewInProcessStream[string](ctx, 4)
	errCh := make(chan error, 1)

	client := NewInProcessStreamClient(stream, errCh)

	// Send one item and close
	if err := stream.Send(ptr("chunk 1")); err != nil {
		t.Fatalf("send: %v", err)
	}
	errCh <- nil
	stream.Close()

	// 1. Read first chunk
	chunk, err := client.Recv()
	if err != nil || chunk == nil || *chunk != "chunk 1" {
		t.Fatalf("expected 'chunk 1', got chunk=%v err=%v", chunk, err)
	}

	// 2. Read EOF
	chunk, err = client.Recv()
	if err != io.EOF {
		t.Fatalf("expected io.EOF on end of stream, got: %v", err)
	}

	// 3. Repeated Recv calls must return cached io.EOF without blocking/deadlocking (Fix #1)
	for i := 0; i < 5; i++ {
		chunk, err = client.Recv()
		if err != io.EOF {
			t.Errorf("call %d: expected io.EOF, got err=%v chunk=%v", i, err, chunk)
		}
	}
}

func TestInProcessStreamClient_RepeatedRecvAfterError(t *testing.T) {
	ctx := context.Background()
	stream := NewInProcessStream[string](ctx, 4)
	errCh := make(chan error, 1)

	client := NewInProcessStreamClient(stream, errCh)

	boom := errors.New("boom error")
	errCh <- boom
	stream.Close()

	// 1. Read terminal error
	_, err := client.Recv()
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom error, got: %v", err)
	}

	// 2. Repeated Recv calls must return cached error without blocking/deadlocking (Fix #1)
	for i := 0; i < 5; i++ {
		_, err = client.Recv()
		if !errors.Is(err, boom) {
			t.Errorf("call %d: expected boom error, got: %v", i, err)
		}
	}
}

func TestInProcessStreamClient_NoOpMethodsNoPanic(t *testing.T) {
	ctx := context.Background()
	stream := NewInProcessStream[string](ctx, 4)
	errCh := make(chan error, 1)

	client := NewInProcessStreamClient(stream, errCh)

	// Verify all grpc.ClientStream interface methods execute without nil-interface panics (Fix #2)
	hdr, err := client.Header()
	if err != nil || hdr != nil {
		t.Errorf("Header: got hdr=%v err=%v", hdr, err)
	}

	trl := client.Trailer()
	if trl != nil {
		t.Errorf("Trailer: got %v", trl)
	}

	if err := client.CloseSend(); err != nil {
		t.Errorf("CloseSend: got %v", err)
	}

	if client.Context() != ctx {
		t.Errorf("Context: got %v, want %v", client.Context(), ctx)
	}

	if err := client.SendMsg("msg"); err != nil {
		t.Errorf("SendMsg: got %v", err)
	}

	if err := client.RecvMsg("msg"); err != nil {
		t.Errorf("RecvMsg: got %v", err)
	}
}

func ptr[T any](v T) *T {
	return &v
}
