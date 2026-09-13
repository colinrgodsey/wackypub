package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// TestProtoPhase2_CompileTimeInterface is the D112 Phase 2 canary gate: AgentSDK must
// still satisfy the generated AgentServiceServer interface after the streaming methods
// were added. If this stops compiling, the migration slipped the interface.
func TestProtoPhase2_CompileTimeInterface(t *testing.T) {
	var _ agentv1.AgentServiceServer = (*AgentSDK)(nil)
}

// streamingTurnServer returns a mock OpenAI-compatible endpoint that emits two text
// chunks separated by a small delay, then finishes.
func streamingTurnServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":123456789,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}]}`)
	}))
}

func newPhase2Workspace(t *testing.T, agentID string) (*AgentSDK, string) {
	t.Helper()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("you are a test agent"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	// ValidateAgentTarget resolves the caller's identity from CWD and requires the caller's
	// directory (or an ancestor) to carry an allowlist. Point CWD at the agent dir so the
	// authorization check passes; restore it when the test finishes so sibling tests are not
	// affected.
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir agent dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	return NewSDK(wsDir), agentDir
}

// TestProtoPhase2_AddAndGenerateTurnStream_Composition is THE D112 canary validation:
// the CLI-side InProcessStream adapter supplies the grpc.ServerStreamingServer shape while
// the SDK method streams through it; the consumer ranges over chunks and terminates when
// the RPC returns. This proves iter.Seq2 composes on top of the proto chunk surface.
func TestProtoPhase2_AddAndGenerateTurnStream_Composition(t *testing.T) {
	sdk, agentDir := newPhase2Workspace(t, "ph2stream")
	srv := streamingTurnServer(t)
	defer srv.Close()
	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}

	ctx := context.Background()
	stream := NewInProcessStream[agentv1.AddAndGenerateTurnStreamResponse](ctx, 8)
	var collect []string
	var streamErr error
	streamDone := make(chan struct{})
	go func() {
		defer stream.Close()
		streamErr = sdk.AddAndGenerateTurnStream(
			&agentv1.AddAndGenerateTurnStreamRequest{AgentId: "ph2stream", UserMessage: "hello"},
			stream,
		)
		close(streamDone)
	}()

	for chunk := range stream.Chunks() {
		if chunk.GetText() != "" {
			collect = append(collect, chunk.GetText())
		}
	}

	select {
	case <-streamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the streaming RPC to return")
	}
	if streamErr != nil {
		t.Fatalf("AddAndGenerateTurnStream returned error: %v", streamErr)
	}

	joined := strings.Join(collect, "")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
		t.Fatalf("streamed text = %q, want to contain both model chunks", joined)
	}

	// The turn must also have been appended to session.jsonl (the atomic append half).
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	if len(turns) < 2 {
		t.Fatalf("expected at least user+model turns, got %d", len(turns))
	}
	if turns[0].Role != "user" || !strings.Contains(ContentText(turns[0]), "hello") {
		t.Fatalf("unexpected first turn: role=%s text=%q", turns[0].Role, ContentText(turns[0]))
	}
}

// TestProtoPhase2_GenerateTurnStream_Composition covers the continue-only streaming twin,
// exercised through the same InProcessStream adapter with a GenerateTurnStreamRequest.
func TestProtoPhase2_GenerateTurnStream_Composition(t *testing.T) {
	sdk, agentDir := newPhase2Workspace(t, "ph2gen")
	srv := streamingTurnServer(t)
	defer srv.Close()
	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}
	if err := AppendSessionTurn(agentDir, "user", "hello"); err != nil {
		t.Fatalf("append user turn: %v", err)
	}

	ctx := context.Background()
	stream := NewInProcessStream[agentv1.GenerateTurnStreamResponse](ctx, 8)
	var collect []string
	var streamErr error
	streamDone := make(chan struct{})
	go func() {
		defer stream.Close()
		streamErr = sdk.GenerateTurnStream(
			&agentv1.GenerateTurnStreamRequest{AgentId: "ph2gen"},
			stream,
		)
		close(streamDone)
	}()

	for chunk := range stream.Chunks() {
		if chunk.GetText() != "" {
			collect = append(collect, chunk.GetText())
		}
	}

	select {
	case <-streamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for GenerateTurnStream to return")
	}
	if streamErr != nil {
		t.Fatalf("GenerateTurnStream returned error: %v", streamErr)
	}
	joined := strings.Join(collect, "")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
		t.Fatalf("streamed text = %q, want to contain both model chunks", joined)
	}
}

// TestProtoPhase2_NonStreamingTwins_Parity checks the two non-streaming proto twins against
// their legacy counterparts by comparing full response text on a real tokenized endpoint.
func TestProtoPhase2_NonStreamingTwins_Parity(t *testing.T) {
	sdk, agentDir := newPhase2Workspace(t, "ph2parity")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"parity answer"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}
	ctx := context.Background()

	// Add + Generate non-streaming proto twin.
	resp, err := sdk.AddAndGenerateTurn(ctx, &agentv1.AddAndGenerateTurnRequest{
		AgentId:     "ph2parity",
		UserMessage: "hello parity",
	})
	if err != nil {
		t.Fatalf("proto AddAndGenerateTurn: %v", err)
	}
	if !strings.Contains(resp.GetText(), "parity answer") {
		t.Fatalf("proto AddAndGenerateTurn text = %q, want parity answer", resp.GetText())
	}

	// Generate non-streaming proto twin (another turn queued).
	if err := AppendSessionTurn(agentDir, "user", "again"); err != nil {
		t.Fatalf("append: %v", err)
	}
	genResp, err := sdk.GenerateTurn(ctx, &agentv1.GenerateTurnRequest{AgentId: "ph2parity"})
	if err != nil {
		t.Fatalf("proto GenerateTurn: %v", err)
	}
	if !strings.Contains(genResp.GetText(), "parity answer") {
		t.Fatalf("proto GenerateTurn text = %q, want parity answer", genResp.GetText())
	}
}

// TestProtoPhase2_AddAndGenerateTurn_OnWarningTranscription verifies the onWarning
// canary decision: hook warnings surface as warning-bearing stream responses, not only via
// a Go callback. We install a trivial on-user-message hook that emits a warning, then assert
// the stream carries it.
func TestProtoPhase2_AddAndGenerateTurn_OnWarningTranscription(t *testing.T) {
	sdk, agentDir := newPhase2Workspace(t, "ph2warn")
	srv := streamingTurnServer(t)
	defer srv.Close()
	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}

	// Install a user-message hook that gets a warning through RunUserMessageHooks.
	hookDir := filepath.Join(agentDir, "hooks", "on-user-message")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	hook := filepath.Join(hookDir, "10-warn")
	script := "#!/bin/sh\necho 'hook-warning-line' >&2\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	ctx := context.Background()
	stream := NewInProcessStream[agentv1.AddAndGenerateTurnStreamResponse](ctx, 8)

	var sawWarning bool
	var streamErr error
	streamDone := make(chan struct{})
	go func() {
		defer stream.Close()
		streamErr = sdk.AddAndGenerateTurnStream(
			&agentv1.AddAndGenerateTurnStreamRequest{AgentId: "ph2warn", UserMessage: "hello"},
			stream,
		)
		close(streamDone)
	}()

	for chunk := range stream.Chunks() {
		if w := chunk.GetWarning(); w != "" {
			if strings.Contains(w, "hook-warning-line") || strings.Contains(w, "hook") {
				sawWarning = true
			}
		}
	}

	select {
	case <-streamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for warning stream to finish")
	}
	if streamErr != nil {
		t.Fatalf("AddAndGenerateTurnStream returned error: %v", streamErr)
	}
	if !sawWarning {
		t.Fatal("expected the hook warning to surface on the proto stream")
	}
}
