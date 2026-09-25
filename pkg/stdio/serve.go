package stdio

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
)

// Listener is a net.Listener that yields exactly one pre-established
// connection (the stdio conn) and then blocks. Close releases the Accept
// waiters with net.ErrClosed.
type Listener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	closedCh  chan struct{}
}

// NewListener creates a single-connection listener over conn.
func NewListener(conn net.Conn) *Listener {
	return &Listener{
		conn:     conn,
		closedCh: make(chan struct{}),
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() {
		c = l.conn
	})
	if c != nil {
		return c, nil
	}
	<-l.closedCh
	return nil, net.ErrClosed
}

func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closedCh)
	})
	return l.conn.Close()
}

func (l *Listener) Addr() net.Addr { return l.conn.LocalAddr() }

// ServeContext serves grpcServer over a single stdio connection until the
// client closes (stdin EOF), ctx is cancelled, or the server stops. Returns nil
// on a clean EOF/close and on graceful stop; returns the serve error otherwise.
//
// For the CLI lifecycle contract the EOF path matters most: when the client
// disappears (stdin EOF from a killed or exited peer), the conn's onEOF hook
// fires GracefulStop so the process exits promptly instead of lingering on a
// one-shot connection.
func ServeContext(ctx context.Context, grpcServer *grpc.Server, conn net.Conn) error {
	lis := NewListener(conn)
	errCh := make(chan error, 1)
	go func() {
		errCh <- grpcServer.Serve(lis)
	}()

	// The CLI lifecycle requires the process to exit when the client goes away:
	// stdin EOF fires the conn's onEOF hook, which closes the listener so gRPC's
	// Serve returns (a plain GracefulStop does NOT unblock a Serve blocked in
	// Accept - verified empirically). GracefulStop first to flush in-flight
	// responses, then the listener close releases the Accept loop.
	//
	// If the caller supplied its own onEOF hook, COMPOSE with it: the EOF
	// shutdown is a property of the server side of this package (package doc),
	// so a consumer hook must not silently disable the auto-exit.
	var stopOnce sync.Once
	stopFn := func() {
		stopOnce.Do(func() {
			// Close the listener FIRST: that is what unblocks grpcServer.Serve
			// (blocked in Accept). GracefulStop after, to drain anything already
			// in flight; it runs in a goroutine so a server with no active RPCs
			// cannot wedge this path.
			_ = lis.Close()
			go grpcServer.GracefulStop()
		})
	}
	if c, ok := conn.(*Conn); ok {
		if c.onEOF == nil {
			c.onEOF = stopFn
		} else {
			// fireEOF runs the hook exactly once; compose in the existing hook so
			// both it and the shutdown run on the same EOF signal.
			existing := c.onEOF
			c.onEOF = func() {
				existing()
				stopFn()
			}
		}
	}

	select {
	case <-ctx.Done():
		grpcServer.GracefulStop()
		_ = lis.Close()
		return ctx.Err()
	case err := <-errCh:
		if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	}
}

// DialCommand starts cmd with stdin/stdout as gRPC-over-stdio pipes (stderr
// passes through untouched), returning a client-side Conn. The child is
// started with Setsid so Close can signal/kill the whole process group, and
// with a wait delay (default 5s) controlling SIGTERM/SIGKILL escalation.
// cmd.Dir must be set by the caller to control the child's CWD.
func DialCommand(ctx context.Context, cmd *exec.Cmd, waitDelay time.Duration) (*Conn, error) {
	if waitDelay <= 0 {
		waitDelay = 5 * time.Second
	}
	if cmd == nil {
		return nil, errors.New("stdio.DialCommand: nil cmd")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Do not set cmd.Cancel here: exec.CommandContext sets it from the caller's
	// context and exec.Command forbids a non-nil Cancel on a non-CommandContext
	// cmd. Close()'s reap() escalation handles the kill-on-client-close path
	// regardless of the context cancel behavior.
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = waitDelay
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}

	return NewClientConn(cmd, stdin, stdout, waitDelay), nil
}
