package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"github.com/colinrgodsey/wackypub/pkg/stdio"
	"google.golang.org/grpc"
)

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
	flag.Parse()

	if *behavior == "never-starts" {
		os.Exit(1)
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

	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, &shimImpl{behavior: *behavior})
	conn := stdio.NewConn(os.Stdin, os.Stdout)

	// ServeContext returns when the client closes stdin (EOF) or cancels; the
	// shim then exits like the product stdio service mode (per-call lifecycle).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := stdio.ServeContext(ctx, srv, conn); err != nil {
		fmt.Fprintf(os.Stderr, "shim serve error: %v\n", err)
		os.Exit(1)
	}
}
