package stdio

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// testAgentServer is a minimal AgentServiceServer implementation that echoes a
// read-session response back with the agent id, enough to exercise a real
// proto-defined RPC (with codec) over the stdio framing.
type testAgentServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (s *testAgentServer) ReadSession(ctx context.Context, req *agentv1.ReadSessionRequest) (*agentv1.ReadSessionResponse, error) {
	return &agentv1.ReadSessionResponse{
		Turns: []*agentv1.SessionTurn{
			{
				Role: "assistant",
				Parts: []*agentv1.SessionPart{{
					Text: "echo: " + req.GetAgentId(),
				}},
			},
		},
	}, nil
}

// RegisterAgentTestServer is used by the tests.
func RegisterAgentTestServer(s grpc.ServiceRegistrar, srv agentv1.AgentServiceServer) {
	s.RegisterService(&testAgent_ServiceDesc, srv)
}

var testAgent_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "wackypub.agent.v1.AgentService",
	HandlerType: (*agentv1.AgentServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "ReadSession",
			Handler:    _AgentService_ReadSession_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "stdio_test.go",
}

func _AgentService_ReadSession_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(agentv1.ReadSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(agentv1.AgentServiceServer).ReadSession(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/agent.v1.AgentService/ReadSession"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(agentv1.AgentServiceServer).ReadSession(ctx, req.(*agentv1.ReadSessionRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// TestNewConnRoundTrip verifies a Conn wraps a read stream and write stream
// through to the wire: bytes written to the write side come back on the read
// side of the pairing.
func TestNewConnRoundTrip(t *testing.T) {
	// Full-duplex stdio pair: pipe A carries client->server, pipe B carries
	// server->client. c1 is the server role (reads A, writes B); c2 is the
	// client role (reads B, writes A).
	aR, aW := net.Pipe()
	bR, bW := net.Pipe()
	c1 := NewConn(aR, bW)
	c2 := NewConn(bR, aW)

	go func() {
		_, _ = c2.Write([]byte("hello stdio"))
	}()

	buf := make([]byte, 32)
	n, err := c1.Read(buf)
	if err != nil {
		t.Fatalf("c1.Read: %v", err)
	}
	if string(buf[:n]) != "hello stdio" {
		t.Fatalf("got %q, want hello stdio", buf[:n])
	}

	_ = c1.Close()
	_ = c2.Close()
}

// TestDialCommandAndCloseReapsChild spawns a child that exits on stdin EOF
// and verifies Close reaps it (no orphan).
func TestDialCommandAndCloseReapsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "child.sh")
	err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nexit 0\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script)
	cmd.Dir = tmp
	conn, err := DialCommand(context.Background(), cmd, 2*time.Second)
	if err != nil {
		t.Fatalf("DialCommand: %v", err)
	}

	_, _ = conn.Write([]byte("x"))

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("child not reaped successfully: %v", cmd.ProcessState)
	}
}

// TestDialCommandReapsEscalationKills ensures a child that ignores stdin EOF
// is killed via SIGKILL escalation (no orphan).
func TestDialCommandReapsEscalationKills(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess")
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "stubborn.sh")
	err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 1; done\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script)
	cmd.Dir = tmp
	conn, err := DialCommand(context.Background(), cmd, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("DialCommand: %v", err)
	}

	start := time.Now()
	err = conn.Close()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected Close to return a reaping error for a killed child, got nil after %v", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Close took %v; SIGKILL escalation should have fired promptly", elapsed)
	}
}

// TestListenerYieldsOnce verifies Accept returns the one conn then blocks
// until Close.
func TestListenerYieldsOnce(t *testing.T) {
	pr, pw := net.Pipe()
	c := NewConn(pr, pw)
	l := NewListener(c)

	nc, err := l.Accept()
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	if nc != c {
		t.Fatalf("expected the same conn back")
	}

	closed := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		closed <- err
	}()

	time.Sleep(20 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("second Accept returned before Close")
	default:
	}

	_ = l.Close()
	select {
	case err := <-closed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("expected net.ErrClosed after Close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not unblock on Close")
	}
}

// TestGRPCOverStdioRoundTrip wires the hand-rolled echo server onto one Conn
// and a grpc client onto the paired Conn, exercising a real unary call over
// the stdio framing and a clean graceful shutdown.
func TestGRPCOverStdioRoundTrip(t *testing.T) {
	pr1, pw1 := net.Pipe()
	pr2, pw2 := net.Pipe()

	serverConn := NewConn(pr1, pw2)
	clientConn := NewConn(pr2, pw1)

	grpcServer := grpc.NewServer()
	srv := &testAgentServer{}
	RegisterAgentTestServer(grpcServer, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = ServeContext(ctx, grpcServer, serverConn)
	}()

	gc, err := grpc.NewClient(
		"passthrough:///stdio-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return clientConn, nil
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer gc.Close()

	client := agentv1.NewAgentServiceClient(gc)
	resp, err := client.ReadSession(ctx, &agentv1.ReadSessionRequest{AgentId: "agent-1"})
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(resp.GetTurns()) != 1 || resp.GetTurns()[0].GetParts()[0].GetText() != "echo: agent-1" {
		t.Fatalf("unexpected response: %v", resp)
	}

	cancel()
	_ = gc.Close()
}

// TestConn_DeadlinePropagation mirrors the pre-consolidation deadline test:
// SetReadDeadline on a file-backed read side must interrupt a blocked Read.
func TestConn_DeadlinePropagation(t *testing.T) {
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer rOut.Close()
	defer wOut.Close()

	rIn, wIn, _ := os.Pipe()
	defer wIn.Close()
	defer rIn.Close()

	conn := NewClientConn(nil, wIn, rOut, 0)
	defer conn.Close()

	past := time.Now().Add(-100 * time.Millisecond)
	if err := conn.SetReadDeadline(past); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatalf("expected deadline exceeded error on Read, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		if !os.IsTimeout(err) {
			t.Errorf("expected timeout error, got: %v", err)
		}
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clearing deadline: %v", err)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = wOut.Write([]byte("ok"))
	}()

	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("expected successful read after clearing deadline, got: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Errorf("expected 'ok', got %q", string(buf[:n]))
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
}
