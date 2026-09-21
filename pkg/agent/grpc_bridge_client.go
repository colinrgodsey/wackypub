package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var (
	// ErrBridgeCommandNotFound indicates the bridge executable configured in REMOTE_MANIFEST does not exist.
	ErrBridgeCommandNotFound = errors.New("bridge command not found")
	// ErrBridgeProcessDied indicates the bridge subprocess exited unexpectedly or crashed mid-stream.
	ErrBridgeProcessDied = errors.New("bridge process exited unexpectedly")
	// ErrDispatchSuperseded indicates the bridge subprocess exited because a concurrent
	// dispatch to the same agent was in flight and this one was cancelled while waiting on
	// the producer-side session lock (wackyacp D117 / bugs/wackyacp/lock-wait-visibility).
	// It is NOT a crash: classifying it as ErrBridgeProcessDied hides that the process
	// died from lock-wait cancellation.
	ErrDispatchSuperseded = errors.New("bridge dispatch superseded by concurrent turn")
)

// lockContentionSendinel is matched against bridge stderr to detect lock-wait cancellation.
const lockContentionSentinel = "acp-session.lock contention"

// BridgeProcessDiedError provides structured diagnostics when a bridge process terminates unexpectedly.
type BridgeProcessDiedError struct {
	AgentID  string
	Command  string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *BridgeProcessDiedError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: agent %q (command %q)", ErrBridgeProcessDied.Error(), e.AgentID, e.Command)
	if e.ExitCode != 0 {
		fmt.Fprintf(&sb, " exit code %d", e.ExitCode)
	}
	if e.Stderr != "" {
		fmt.Fprintf(&sb, " stderr: %s", strings.TrimSpace(e.Stderr))
	}
	if e.Err != nil {
		fmt.Fprintf(&sb, ": %v", e.Err)
	}
	return sb.String()
}

func (e *BridgeProcessDiedError) Unwrap() error {
	return ErrBridgeProcessDied
}

func (e *BridgeProcessDiedError) Is(target error) bool {
	return target == ErrBridgeProcessDied
}

// tailBuffer maintains a bounded trailing slice of output for diagnostic error context.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// dialBridge spawns the bridge process configured for route, establishes a gRPC client
// over stdio pipes, and returns the client and a cleanup function (D116 §4).
func dialBridge(ctx context.Context, wsDir, agentID string, route RemoteRoute) (AgentClient, func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	binPath, err := exec.LookPath(route.Command)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: REMOTE_MANIFEST route for %q: command %q: %v", ErrBridgeCommandNotFound, agentID, route.Command, err)
	}

	cmd := exec.CommandContext(ctx, binPath, route.Args...)
	cmd.Dir = wsDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Process group isolation (D116 §6)

	// Fix #4: Use Go 1.20+ exec.Cmd.Cancel and WaitDelay for SIGTERM -> 5s grace -> SIGKILL escalation
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err == nil {
			return syscall.Kill(-pgid, syscall.SIGTERM)
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second

	tailBuf := &tailBuffer{limit: 4096}
	if os.Stderr != nil {
		cmd.Stderr = io.MultiWriter(os.Stderr, tailBuf)
	} else {
		cmd.Stderr = tailBuf
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("bridge stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, nil, fmt.Errorf("bridge stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, nil, fmt.Errorf("starting bridge %q: %w", route.Command, err)
	}

	conn := newStdioConn(cmd, stdin, stdout)

	// Fix #5: Atomic flag preventing grpc-go redial loops on dead pipes
	var dialed atomic.Bool
	dialer := func(context.Context, string) (net.Conn, error) {
		if dialed.Swap(true) {
			return nil, errors.New("bridge connection closed: process cannot be reconnected")
		}
		return conn, nil
	}

	translateErr := func(err error) error {
		return translateBridgeError(err, agentID, route.Command, cmd, tailBuf)
	}

	unaryInterceptor := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			return translateErr(err)
		}
		return nil
	}

	streamInterceptor := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			return nil, translateErr(err)
		}
		return &bridgeClientStream{ClientStream: cs, translate: translateErr}, nil
	}

	gc, err := grpc.NewClient(
		"passthrough:///"+agentID,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithUnaryInterceptor(unaryInterceptor),
		grpc.WithStreamInterceptor(streamInterceptor),
	)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("constructing bridge grpc client: %w", err)
	}

	client := agentv1.NewAgentServiceClient(gc)

	var closeOnce sync.Once
	var closeErr error
	cleanup := func() error {
		closeOnce.Do(func() {
			closeErr = gc.Close()
		})
		return closeErr
	}

	return client, cleanup, nil
}

type bridgeClientStream struct {
	grpc.ClientStream
	translate func(error) error
}

func (s *bridgeClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil && err != io.EOF {
		return s.translate(err)
	}
	return err
}

func translateBridgeError(err error, agentID, command string, cmd *exec.Cmd, tailBuf *tailBuffer) error {
	if err == nil || err == io.EOF {
		return err
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return err
	}

	code := status.Code(err)
	if code == codes.Unavailable || code == codes.Internal || code == codes.Unknown {
		exitCode := 0
		if cmd != nil && cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		var stderr string
		if tailBuf != nil {
			stderr = tailBuf.String()
		}
		// Honest classification (bugs/wackyacp/lock-wait-visibility): if the bridge died
		// while waiting on the producer-side session lock (stderr carries the contention
		// sentinel), this is a superseded dispatch, not a crash.
		if strings.Contains(stderr, lockContentionSentinel) {
			return fmt.Errorf("%w: %s (exit code %d)", ErrDispatchSuperseded, strings.TrimSpace(stderr), exitCode)
		}
		return &BridgeProcessDiedError{
			AgentID:  agentID,
			Command:  command,
			ExitCode: exitCode,
			Stderr:   stderr,
			Err:      err,
		}
	}
	return err
}
