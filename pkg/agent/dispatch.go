package agent

import (
	"context"
	"fmt"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
)

// AgentClient is what cmd/agent.go depends on instead of *AgentSDK directly.
// It mirrors agentv1.AgentServiceClient's shape (the grpc-generated CLIENT
// interface, not the server interface) because that's the natural shape for
// "call a method, get a response or a receive-stream back" regardless of
// whether the callee is in-process or a subprocess over a conn (D116 §2).
type AgentClient = agentv1.AgentServiceClient

// ResolveAgentClient decides native vs. bridged for a single agent_id and
// returns a ready-to-use client plus a cleanup func. Cleanup is a no-op for
// native dispatch and closes the conn + reaps the subprocess for bridged
// dispatch. Callers MUST defer cleanup() regardless of path taken.
//
// Bridge dispatch assumes same-user, same-machine execution for all routes.
// No additional authentication or authorization occurs at the bridge boundary
// beyond the standard AuthorizeAgentTarget check already performed on agent_id
// before this function is called. Remote (networked) bridges are explicitly out
// of scope for this function and must not be added without a corresponding auth
// design — see task card tasks/wackypub/acpx-bridge-hook, iteration 7's push-order item 4.
func ResolveAgentClient(ctx context.Context, sdk *AgentSDK, agentID string) (client AgentClient, cleanup func() error, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	manifest, err := LoadRemoteManifest(sdk.WorkspaceDir)
	if err != nil {
		return nil, nil, fmt.Errorf("loading REMOTE_MANIFEST: %w", err)
	}
	route, bridged := manifest.Lookup(agentID)
	if !bridged {
		return &localAgentClient{sdk: sdk}, func() error { return nil }, nil
	}
	return dialBridge(ctx, sdk.WorkspaceDir, agentID, route)
}

// localAgentClient adapts *AgentSDK to satisfy agentv1.AgentServiceClient (D116 §4).
type localAgentClient struct {
	sdk *AgentSDK
}

var _ agentv1.AgentServiceClient = (*localAgentClient)(nil)

func (l *localAgentClient) ListAgents(ctx context.Context, in *agentv1.ListAgentsRequest, _ ...grpc.CallOption) (*agentv1.ListAgentsResponse, error) {
	return l.sdk.ListAgents(ctx, in)
}

func (l *localAgentClient) InspectAgent(ctx context.Context, in *agentv1.InspectAgentRequest, _ ...grpc.CallOption) (*agentv1.InspectAgentResponse, error) {
	return l.sdk.InspectAgent(ctx, in)
}

func (l *localAgentClient) ReadSession(ctx context.Context, in *agentv1.ReadSessionRequest, _ ...grpc.CallOption) (*agentv1.ReadSessionResponse, error) {
	return l.sdk.ReadSession(ctx, in)
}

func (l *localAgentClient) ReadMemory(ctx context.Context, in *agentv1.ReadMemoryRequest, _ ...grpc.CallOption) (*agentv1.ReadMemoryResponse, error) {
	return l.sdk.ReadMemory(ctx, in)
}

func (l *localAgentClient) RenderSystemPrompt(ctx context.Context, in *agentv1.RenderSystemPromptRequest, _ ...grpc.CallOption) (*agentv1.RenderSystemPromptResponse, error) {
	return l.sdk.RenderSystemPrompt(ctx, in)
}

func (l *localAgentClient) InspectSessionContext(ctx context.Context, in *agentv1.InspectSessionContextRequest, _ ...grpc.CallOption) (*agentv1.InspectSessionContextResponse, error) {
	return l.sdk.InspectSessionContext(ctx, in)
}

func (l *localAgentClient) InspectAgentLocks(ctx context.Context, in *agentv1.InspectAgentLocksRequest, _ ...grpc.CallOption) (*agentv1.InspectAgentLocksResponse, error) {
	return l.sdk.InspectAgentLocks(ctx, in)
}

func (l *localAgentClient) AddUserTurn(ctx context.Context, in *agentv1.AddUserTurnRequest, _ ...grpc.CallOption) (*agentv1.AddUserTurnResponse, error) {
	return l.sdk.AddUserTurn(ctx, in)
}

func (l *localAgentClient) AddMedia(ctx context.Context, in *agentv1.AddMediaRequest, _ ...grpc.CallOption) (*agentv1.AddMediaResponse, error) {
	return l.sdk.AddMedia(ctx, in)
}

func (l *localAgentClient) CancelTurn(ctx context.Context, in *agentv1.CancelTurnRequest, _ ...grpc.CallOption) (*agentv1.CancelTurnResponse, error) {
	return l.sdk.CancelTurn(ctx, in)
}

func (l *localAgentClient) StripSignatures(ctx context.Context, in *agentv1.StripSignaturesRequest, _ ...grpc.CallOption) (*agentv1.StripSignaturesResponse, error) {
	return l.sdk.StripSignatures(ctx, in)
}

func (l *localAgentClient) CompactSession(ctx context.Context, in *agentv1.CompactSessionRequest, _ ...grpc.CallOption) (*agentv1.CompactSessionResponse, error) {
	return l.sdk.CompactSession(ctx, in)
}

func (l *localAgentClient) CreateScratchpad(ctx context.Context, in *agentv1.CreateScratchpadRequest, _ ...grpc.CallOption) (*agentv1.CreateScratchpadResponse, error) {
	return l.sdk.CreateScratchpad(ctx, in)
}

func (l *localAgentClient) GetScratchpad(ctx context.Context, in *agentv1.GetScratchpadRequest, _ ...grpc.CallOption) (*agentv1.GetScratchpadResponse, error) {
	return l.sdk.GetScratchpad(ctx, in)
}

func (l *localAgentClient) ListScratchpads(ctx context.Context, in *agentv1.ListScratchpadsRequest, _ ...grpc.CallOption) (*agentv1.ListScratchpadsResponse, error) {
	return l.sdk.ListScratchpads(ctx, in)
}

func (l *localAgentClient) SearchScratchpad(ctx context.Context, in *agentv1.SearchScratchpadRequest, _ ...grpc.CallOption) (*agentv1.SearchScratchpadResponse, error) {
	return l.sdk.SearchScratchpad(ctx, in)
}

func (l *localAgentClient) DiffScratchpadEntries(ctx context.Context, in *agentv1.DiffScratchpadEntriesRequest, _ ...grpc.CallOption) (*agentv1.DiffScratchpadEntriesResponse, error) {
	return l.sdk.DiffScratchpadEntries(ctx, in)
}

func (l *localAgentClient) DeleteScratchpad(ctx context.Context, in *agentv1.DeleteScratchpadRequest, _ ...grpc.CallOption) (*agentv1.DeleteScratchpadResponse, error) {
	return l.sdk.DeleteScratchpad(ctx, in)
}

func (l *localAgentClient) AsideQuestion(ctx context.Context, in *agentv1.AsideQuestionRequest, _ ...grpc.CallOption) (*agentv1.AsideQuestionResponse, error) {
	return l.sdk.AsideQuestion(ctx, in)
}

func (l *localAgentClient) GenerateTurn(ctx context.Context, in *agentv1.GenerateTurnRequest, _ ...grpc.CallOption) (*agentv1.GenerateTurnResponse, error) {
	return l.sdk.GenerateTurn(ctx, in)
}

func (l *localAgentClient) AddAndGenerateTurn(ctx context.Context, in *agentv1.AddAndGenerateTurnRequest, _ ...grpc.CallOption) (*agentv1.AddAndGenerateTurnResponse, error) {
	return l.sdk.AddAndGenerateTurn(ctx, in)
}

func (l *localAgentClient) Trace(ctx context.Context, in *agentv1.TraceRequest, _ ...grpc.CallOption) (*agentv1.TraceResponse, error) {
	return l.sdk.Trace(ctx, in)
}

func (l *localAgentClient) GenerateTurnStream(ctx context.Context, in *agentv1.GenerateTurnStreamRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.GenerateTurnStreamResponse], error) {
	stream := NewInProcessStream[agentv1.GenerateTurnStreamResponse](ctx, 16)
	errCh := make(chan error, 1)
	go func() {
		defer stream.Close()
		errCh <- l.sdk.GenerateTurnStream(in, stream)
	}()
	return NewInProcessStreamClient(stream, errCh), nil
}

func (l *localAgentClient) AddAndGenerateTurnStream(ctx context.Context, in *agentv1.AddAndGenerateTurnStreamRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.AddAndGenerateTurnStreamResponse], error) {
	stream := NewInProcessStream[agentv1.AddAndGenerateTurnStreamResponse](ctx, 16)
	errCh := make(chan error, 1)
	go func() {
		defer stream.Close()
		errCh <- l.sdk.AddAndGenerateTurnStream(in, stream)
	}()
	return NewInProcessStreamClient(stream, errCh), nil
}

func (l *localAgentClient) ReadSessionEvents(ctx context.Context, in *agentv1.ReadSessionEventsRequest, _ ...grpc.CallOption) (*agentv1.ReadSessionEventsResponse, error) {
	return l.sdk.ReadSessionEvents(ctx, in)
}

func (l *localAgentClient) SubscribeSession(ctx context.Context, in *agentv1.SubscribeSessionRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.SubscribeSessionResponse], error) {
	stream := NewInProcessStream[agentv1.SubscribeSessionResponse](ctx, 64)
	errCh := make(chan error, 1)
	go func() {
		defer stream.Close()
		errCh <- l.sdk.SubscribeSession(in, stream)
	}()
	return NewInProcessStreamClient(stream, errCh), nil
}
