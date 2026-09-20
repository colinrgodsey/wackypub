package agent

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// stdioConn adapts a spawned process's Stdin (our write side) and Stdout
// (our read side) into a net.Conn so grpc-go's transport can frame gRPC
// messages over it.
type stdioConn struct {
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stdinFile  *os.File
	stdoutFile *os.File
	cmd        *exec.Cmd

	closeOnce sync.Once
	closeErr  error
}

// newStdioConn creates a client-side net.Conn wrapping a subprocess's stdio pipes.
func newStdioConn(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) *stdioConn {
	c := &stdioConn{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
	}
	if f, ok := stdin.(*os.File); ok {
		c.stdinFile = f
	}
	if f, ok := stdout.(*os.File); ok {
		c.stdoutFile = f
	}
	return c
}

func (c *stdioConn) Read(b []byte) (int, error) {
	if c.stdout == nil {
		return 0, io.EOF
	}
	return c.stdout.Read(b)
}

func (c *stdioConn) Write(b []byte) (int, error) {
	if c.stdin == nil {
		return 0, io.ErrClosedPipe
	}
	return c.stdin.Write(b)
}

func (c *stdioConn) Close() error {
	c.closeOnce.Do(func() {
		var stdinErr, stdoutErr, waitErr error
		if c.stdin != nil {
			stdinErr = c.stdin.Close()
		}
		if c.stdout != nil {
			stdoutErr = c.stdout.Close()
		}
		if c.cmd != nil {
			waitDone := make(chan error, 1)
			go func() {
				waitDone <- c.cmd.Wait()
			}()

			select {
			case waitErr = <-waitDone:
			case <-time.After(5 * time.Second):
				if c.cmd.Process != nil {
					pgid, err := syscall.Getpgid(c.cmd.Process.Pid)
					if err == nil {
						_ = syscall.Kill(-pgid, syscall.SIGKILL)
					} else {
						_ = c.cmd.Process.Kill()
					}
				}
				waitErr = <-waitDone
			}
		}
		c.closeErr = errors.Join(stdinErr, stdoutErr, waitErr)
	})
	return c.closeErr
}

func (c *stdioConn) LocalAddr() net.Addr {
	return stdioAddr{}
}

func (c *stdioConn) RemoteAddr() net.Addr {
	return stdioAddr{}
}

func (c *stdioConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

// SetReadDeadline delegates deadline handling directly to the underlying *os.File
// to interrupt active syscalls and support deadline clearing (Fix #3).
func (c *stdioConn) SetReadDeadline(t time.Time) error {
	if c.stdoutFile != nil {
		return c.stdoutFile.SetReadDeadline(t)
	}
	return nil
}

// SetWriteDeadline delegates deadline handling directly to the underlying *os.File
// to interrupt active syscalls and support deadline clearing (Fix #3).
func (c *stdioConn) SetWriteDeadline(t time.Time) error {
	if c.stdinFile != nil {
		return c.stdinFile.SetWriteDeadline(t)
	}
	return nil
}

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

var _ net.Conn = (*stdioConn)(nil)
