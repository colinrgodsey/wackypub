// Package stdio adapts process stdin/stdout (both directions) to net.Conn so
// gRPC can frame messages over stdio. It is the shared substrate for wackypub's
// stdio service mode and the bridge client inside pkg/agent. wackyacp's bridge
// previously carried a third local copy of this machinery; the consolidation
// onto this package lands in wackyacp PR #20 (one Conn/listener implementation
// instead of three once that merges).
//
// Two roles share the package:
//
//   - Server side (a process serving gRPC over its own stdin/stdout):
//     NewConn(os.Stdin, os.Stdout) then ServeContext(ctx, grpcServer, conn).
//     stdin EOF (client closed) triggers a graceful shutdown, so a per-call
//     process exits promptly instead of lingering.
//   - Client side (a process talking to a spawned child): DialCommand starts
//     the child with stdio pipes, returns the Conn; Close reaps the child
//     (stdin EOF then SIGTERM/SIGKILL escalation on the process group) so the
//     child cannot outlive a killed or restarted client.
//
// stdout is the protocol channel: diagnostics belong on stderr. A stray line
// on stdout corrupts gRPC framing, and the enforced convention is documented
// here because it is load-bearing for every consumer of this package.
package stdio

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Addr is the net.Addr for stdio connections. There is exactly one of them.
type Addr struct{}

func (Addr) Network() string { return "stdio" }
func (Addr) String() string  { return "stdio" }

// Conn implements net.Conn over a pair of I/O streams in one direction.
// Server side: read from stdin, write to stdout. Client side: read from the
// child's stdout, write to its stdin.
type Conn struct {
	r     io.ReadCloser
	w     io.WriteCloser
	rFile interface{ SetReadDeadline(time.Time) error }
	wFile interface{ SetWriteDeadline(time.Time) error }

	onEOF   func() // fired once on read error (peer closed stdin)
	eofOnce sync.Once

	cmd       *exec.Cmd // client side only: reaped on Close
	waitDelay time.Duration

	closeOnce sync.Once
	closeErr  error
}

// NewConn creates a server-side (or plain) stdio Conn over r (read) and w
// (write). onEOF, if non-nil, is invoked once when the read side errors
// (typically stdin EOF = the peer closed) so a per-call server can trigger a
// graceful shutdown and exit.
func NewConn(r io.ReadCloser, w io.WriteCloser, onEOF ...func()) *Conn {
	c := &Conn{r: r, w: w}
	c.rFile, _ = r.(interface{ SetReadDeadline(time.Time) error })
	if len(onEOF) > 0 {
		c.onEOF = onEOF[0]
	}
	return c
}

// NewClientConn creates a client-side Conn over a spawned child's pipes
// (stdin is our WRITE side, stdout our READ side - named for the child).
// Close closes both pipes, then waits for the child with SIGTERM/SIGKILL
// escalation on its process group so a killed client never orphans the
// bridge.
func NewClientConn(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, waitDelay time.Duration) *Conn {
	if waitDelay <= 0 {
		waitDelay = 5 * time.Second
	}
	c := &Conn{r: stdout, w: stdin, cmd: cmd, waitDelay: waitDelay}
	c.rFile, _ = stdout.(interface{ SetReadDeadline(time.Time) error })
	c.wFile, _ = stdin.(interface{ SetWriteDeadline(time.Time) error })
	return c
}

func (c *Conn) Read(b []byte) (int, error) {
	if c.r == nil {
		c.fireEOF()
		return 0, io.EOF
	}
	n, err := c.r.Read(b)
	if err != nil {
		c.fireEOF()
	}
	return n, err
}

// fireEOF invokes the onEOF hook exactly once.
func (c *Conn) fireEOF() {
	if c.onEOF != nil {
		c.eofOnce.Do(c.onEOF)
	}
}

func (c *Conn) Write(b []byte) (int, error) {
	if c.w == nil {
		return 0, io.ErrClosedPipe
	}
	return c.w.Write(b)
}

// Close closes the pipes; on the client side it also reaps the child with
// SIGTERM then SIGKILL escalation (both on the child's process group), so the
// child cannot outlive the caller.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		var stdinErr, stdoutErr, waitErr error
		if c.w != nil {
			stdinErr = c.w.Close()
		}
		if c.r != nil {
			stdoutErr = c.r.Close()
		}
		if c.cmd != nil {
			waitErr = reap(c.cmd, c.waitDelay)
		}
		c.closeErr = errors.Join(stdinErr, stdoutErr, waitErr)
	})
	return c.closeErr
}

// reap waits for the child (which should exit on stdin EOF), escalating to
// SIGTERM then SIGKILL on its process group after waitDelay each.
func reap(cmd *exec.Cmd, waitDelay time.Duration) error {
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	signal := func(sig syscall.Signal) {
		if cmd.Process == nil {
			return
		}
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err == nil {
			_ = syscall.Kill(-pgid, sig)
		} else {
			_ = cmd.Process.Signal(sig)
		}
	}

	select {
	case err := <-waitDone:
		return err
	case <-time.After(waitDelay):
		signal(syscall.SIGTERM)
	}
	select {
	case err := <-waitDone:
		return err
	case <-time.After(waitDelay):
		signal(syscall.SIGKILL)
	}
	// Bound the post-SIGKILL wait too: a child wedged in uninterruptible sleep
	// (e.g. stuck on a hung NFS or D-state FUSE fd) will not die even on SIGKILL,
	// and Close must not hang the caller (the bot holds a per-agent is_generating
	// reset that assumes Close returns).
	select {
	case err := <-waitDone:
		return err
	case <-time.After(waitDelay):
		return fmt.Errorf("stdio child pid %d did not exit after SIGKILL within %s", cmd.Process.Pid, waitDelay)
	}
}

func (c *Conn) LocalAddr() net.Addr  { return Addr{} }
func (c *Conn) RemoteAddr() net.Addr { return Addr{} }

func (c *Conn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

// SetReadDeadline delegates to the underlying read handle when it is an
// *os.File (stdout pipe of a child, or os.Stdin of the serving process) so
// grpc-go's HTTP/2 transport can interrupt reads and support deadline
// clearing. Non-file readers are no-ops, matching the behavior of the
// pre-consolidation implementations.
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.rFile != nil {
		return c.rFile.SetReadDeadline(t)
	}
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.wFile != nil {
		return c.wFile.SetWriteDeadline(t)
	}
	return nil
}
