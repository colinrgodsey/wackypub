package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
)

type fixedConnListener struct {
	conn net.Conn
	used bool
}

func (l *fixedConnListener) Accept() (net.Conn, error) {
	if l.used {
		select {} // block forever
	}
	l.used = true
	return l.conn, nil
}

func (l *fixedConnListener) Close() error   { return nil }
func (l *fixedConnListener) Addr() net.Addr { return stdioAddr{} }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

type serverStdioConn struct {
	stdin    *os.File
	stdout   *os.File
	behavior string
}

func (c *serverStdioConn) Read(b []byte) (int, error) {
	n, err := c.stdin.Read(b)
	if err != nil && !strings.HasPrefix(c.behavior, "slow-exit=") {
		go func() {
			time.Sleep(20 * time.Millisecond)
			os.Exit(0)
		}()
	}
	return n, err
}

func (c *serverStdioConn) Write(b []byte) (int, error) { return c.stdout.Write(b) }

func (c *serverStdioConn) Close() error {
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	if !strings.HasPrefix(c.behavior, "slow-exit=") {
		go func() {
			time.Sleep(20 * time.Millisecond)
			os.Exit(0)
		}()
	}
	return nil
}

func (c *serverStdioConn) LocalAddr() net.Addr                { return stdioAddr{} }
func (c *serverStdioConn) RemoteAddr() net.Addr               { return stdioAddr{} }
func (c *serverStdioConn) SetDeadline(t time.Time) error      { return nil }
func (c *serverStdioConn) SetReadDeadline(t time.Time) error  { return c.stdin.SetReadDeadline(t) }
func (c *serverStdioConn) SetWriteDeadline(t time.Time) error { return c.stdout.SetWriteDeadline(t) }

type shimImpl struct {
	agentv1.UnimplementedAgentServiceServer
	behavior string
}

func (s *shimImpl) ReadSession(ctx context.Context, req *agentv1.ReadSessionRequest) (*agentv1.ReadSessionResponse, error) {
	return &agentv1.ReadSessionResponse{
		Turns: []*agentv1.SessionTurn{
			{
				Role: "assistant",
				Parts: []*agentv1.SessionPart{
					{
						Text: "echo from shim: " + req.GetAgentId(),
					},
				},
			},
		},
	}, nil
}

func (s *shimImpl) GenerateTurn(ctx context.Context, req *agentv1.GenerateTurnRequest) (*agentv1.GenerateTurnResponse, error) {
	return &agentv1.GenerateTurnResponse{
		Text: "canned generate turn for " + req.GetAgentId(),
		Usage: &agentv1.TurnUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			Backend:          "wackyshimbin",
		},
	}, nil
}

func (s *shimImpl) GenerateTurnStream(req *agentv1.GenerateTurnStreamRequest, stream agentv1.AgentService_GenerateTurnStreamServer) error {
	if s.behavior == "crash-mid-stream" {
		_ = stream.Send(&agentv1.GenerateTurnStreamResponse{Text: "chunk 1"})
		_ = stream.Send(&agentv1.GenerateTurnStreamResponse{Text: "chunk 2"})
		time.Sleep(30 * time.Millisecond)
		os.Exit(1)
	}

	if strings.HasPrefix(s.behavior, "stream-n=") {
		nStr := strings.TrimPrefix(s.behavior, "stream-n=")
		n, _ := strconv.Atoi(nStr)
		for i := 1; i <= n; i++ {
			if err := stream.Send(&agentv1.GenerateTurnStreamResponse{
				Text: fmt.Sprintf("chunk %d", i),
			}); err != nil {
				return err
			}
		}
		return nil
	}

	return stream.Send(&agentv1.GenerateTurnStreamResponse{Text: "echo: " + req.GetAgentId()})
}

func (s *shimImpl) AddAndGenerateTurnStream(req *agentv1.AddAndGenerateTurnStreamRequest, stream agentv1.AgentService_AddAndGenerateTurnStreamServer) error {
	return stream.Send(&agentv1.AddAndGenerateTurnStreamResponse{Text: "echo: " + req.GetUserMessage()})
}

func (s *shimImpl) AsideQuestion(ctx context.Context, req *agentv1.AsideQuestionRequest) (*agentv1.AsideQuestionResponse, error) {
	if s.behavior == "aside-slow" {
		// Hold the bridge a beat so a concurrent dispatch would overlap this process.
		time.Sleep(150 * time.Millisecond)
	}
	return &agentv1.AsideQuestionResponse{
		Text:        "aside from shim: " + req.GetQuestion(),
		ToolDenials: 3,
		Usage: &agentv1.TurnUsage{
			PromptTokens:     7,
			CompletionTokens: 8,
			TotalTokens:      15,
			Backend:          "wackyshimbin",
		},
	}, nil
}

func (s *shimImpl) Trace(ctx context.Context, req *agentv1.TraceRequest) (*agentv1.TraceResponse, error) {
	return &agentv1.TraceResponse{
		TargetAgentId: req.GetAgentId(),
	}, nil
}

func main() {
	behavior := flag.String("behavior", "echo", "Behavior configuration for test assertions")
	guardLock := flag.String("guard-lock", "", "If set, flock this path for the process lifetime; exit 1 if already held (simulates bridge collapse on concurrent spawn)")
	flag.Parse()

	if *behavior == "never-starts" {
		os.Exit(1)
	}

	// Concurrent-spawn guard: the failure mode in bugs/wackypub/bridge-concurrent-prompt-lock
	// is two bridge processes racing one agent session. When guard-lock is set, a second
	// process must fail fast - it simulates the stdio conn collapse the real bridge hits.
	if *guardLock != "" {
		guardFile, err := os.OpenFile(*guardLock, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "guard lock open: %v\n", err)
			os.Exit(1)
		}
		if err := syscall.Flock(int(guardFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			fmt.Fprintf(os.Stderr, "bridge already in flight: %v\n", err)
			os.Exit(1)
		}
		defer syscall.Flock(int(guardFile.Fd()), syscall.LOCK_UN)
	}

	if strings.HasPrefix(*behavior, "slow-exit=") {
		secStr := strings.TrimPrefix(*behavior, "slow-exit=")
		sec, _ := strconv.Atoi(secStr)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		go func() {
			<-sigCh
			time.Sleep(time.Duration(sec) * time.Second)
			os.Exit(0)
		}()
	}

	conn := &serverStdioConn{stdin: os.Stdin, stdout: os.Stdout, behavior: *behavior}
	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, &shimImpl{behavior: *behavior})

	if err := srv.Serve(&fixedConnListener{conn: conn}); err != nil {
		fmt.Fprintf(os.Stderr, "shim server error: %v\n", err)
		os.Exit(1)
	}
}
