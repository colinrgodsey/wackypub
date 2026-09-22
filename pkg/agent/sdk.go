// Package agent provides the Go implementation for folder-based agent orchestration.
//
// NOTE(D112): Authoritative behavior and contract documentation for AgentSDK methods
// lives in the protobuf service definition at proto/wackypub/v1/agent.proto.
// That file is canonical; this file provides the concrete in-process Go implementation.
// Method godocs for service interface methods are pointers, not standalone definitions.
//
// NOTE(D112): the former *Legacy methods are gone - the dead ones deleted (zero
// production callers) and the live generation/streaming implementations renamed
// (generateTurnStreamImpl, generateTurnImpl, addAndGenerateTurnStreamImpl,
// addAndGenerateTurnImpl). History and inventory:
// git-kb note/wackypub/d112-phase5-consumer-inventory.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// AgentSDK provides a clean, programmatic Go API for orchestrating folder-based agents.
// In D112, AgentSDK directly satisfies the generated AgentServiceServer interface.
type AgentSDK struct {
	agentv1.UnimplementedAgentServiceServer

	WorkspaceDir          string
	MaxToolTurns          int
	CommandTimeoutSeconds int
	lastHookEnvMu         sync.Mutex
	lastHookEnv           map[string]map[string]string

	// failedUntil maps a backend identity (endpoint + "/" + model) to the time at which a
	// usage-limit failure is expected to reset. Managed by the fallback machinery in
	// runTurnWithRuntimeFallback; in-memory per process for v1 (persisted nothing).
	failedUntilMu sync.Mutex
	failedUntil   map[string]time.Time
}

var _ agentv1.AgentServiceServer = (*AgentSDK)(nil)

// NewSDK creates an SDK instance bound to a workspace directory.
func NewSDK(workspaceDir string) *AgentSDK {
	if workspaceDir == "" {
		workspaceDir = "."
	}
	return &AgentSDK{
		WorkspaceDir:          workspaceDir,
		MaxToolTurns:          DefaultMaxToolTurns,
		CommandTimeoutSeconds: DefaultCommandTimeoutSeconds,
		lastHookEnv:           make(map[string]map[string]string),
		failedUntil:           make(map[string]time.Time),
	}
}

// UserTurnResult contains the stored session content, final text, and any hook warnings from AddUserTurn.
type UserTurnResult struct {
	Content  *genai.Content `json:"content,omitempty"`
	Text     string         `json:"text"`
	Warnings []string       `json:"warnings,omitempty"`
}

// Note: setLastHookEnv stores only the most recent turn's hook env per agentID (D87 single-last-env design; queued turns overwrite).
func (s *AgentSDK) setLastHookEnv(agentID string, hookEnv map[string]string) {
	s.lastHookEnvMu.Lock()
	defer s.lastHookEnvMu.Unlock()
	if s.lastHookEnv == nil {
		s.lastHookEnv = make(map[string]map[string]string)
	}
	if len(hookEnv) == 0 {
		delete(s.lastHookEnv, agentID)
	} else {
		s.lastHookEnv[agentID] = hookEnv
	}
}

func (s *AgentSDK) popLastHookEnv(agentID string) map[string]string {
	s.lastHookEnvMu.Lock()
	defer s.lastHookEnvMu.Unlock()
	if s.lastHookEnv == nil {
		return nil
	}
	env := s.lastHookEnv[agentID]
	delete(s.lastHookEnv, agentID)
	return env
}

// AgentDir returns the absolute or relative path for an agent folder (<ws_dir>/<agent_id>).
func (s *AgentSDK) AgentDir(agentID string) string {
	return filepath.Join(s.WorkspaceDir, agentID)
}

// AddUserTurn implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) AddUserTurn(ctx context.Context, req *agentv1.AddUserTurnRequest) (*agentv1.AddUserTurnResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	message := req.GetMessage()
	if message == "" {
		return nil, fmt.Errorf("message cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agent directory %s: %w", agentDir, err)
	}

	lock, err := AcquireSessionLockContext(ctx, agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	finalMsg, hookEnv, warnings, _ := RunUserMessageHooks(agentDir, message)

	content := genai.NewContentFromText(finalMsg, "user")
	if err := AppendSessionContent(agentDir, content); err != nil {
		return nil, err
	}

	s.setLastHookEnv(agentID, hookEnv)

	_ = CommitWorkspaceEvent(wsDir, agentID, "user")

	turn := &agentv1.SessionTurn{
		Role: "user",
		Parts: []*agentv1.SessionPart{
			{
				Text: finalMsg,
			},
		},
	}
	return &agentv1.AddUserTurnResponse{
		Turn:     turn,
		Text:     finalMsg,
		Warnings: warnings,
	}, nil
}

// AddMedia implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) AddMedia(ctx context.Context, req *agentv1.AddMediaRequest) (*agentv1.AddMediaResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	data := req.GetMediaData()
	if len(data) == 0 {
		return nil, fmt.Errorf("image reader cannot be nil")
	}
	if len(data) > MaxMediaPayloadBytes {
		return nil, fmt.Errorf("media payload exceeds 10MB limit (%d bytes > %d bytes)", len(data), MaxMediaPayloadBytes)
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agent directory %s: %w", agentDir, err)
	}

	lock, err := AcquireSessionLockContext(ctx, agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	runtimeCfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load runtime config for agent %q: %w", agentID, err)
	}
	if runtimeCfg.MaxImageDimension <= 0 {
		return nil, fmt.Errorf("image attachments disabled by runtime config for agent %q (maxImageDimension is not set or <= 0)", agentID)
	}

	jpegBytes, mimeType, err := NormalizeAndResizeImage(bytes.NewReader(data), runtimeCfg.MaxImageDimension)
	if err != nil {
		return nil, fmt.Errorf("failed to process image attachment for agent %q: %w", agentID, err)
	}

	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{
				InlineData: &genai.Blob{
					MIMEType: mimeType,
					Data:     jpegBytes,
				},
			},
		},
	}

	if err := AppendSessionContent(agentDir, content); err != nil {
		return nil, fmt.Errorf("failed to append image turn: %w", err)
	}

	_ = CommitWorkspaceEvent(wsDir, agentID, "user (media)")

	turn := &agentv1.SessionTurn{
		Role: "user",
		Parts: []*agentv1.SessionPart{
			{
				InlineData: jpegBytes,
				MimeType:   mimeType,
			},
		},
	}
	return &agentv1.AddMediaResponse{
		Turn:     turn,
		MimeType: mimeType,
		RawSize:  int64(len(jpegBytes)),
	}, nil
}

// inFlightTurnEntry tracks an active turn cancellation handle (D85).
type inFlightTurnEntry struct {
	cancel context.CancelFunc
}

var (
	inFlightTurnsMu sync.Mutex
	inFlightTurns   = make(map[string]*inFlightTurnEntry)
)

// registerInFlightTurn registers a cancel function for an agent's active turn.
// Returns a cleanup function that deregisters the turn and invokes cancel.
func registerInFlightTurn(agentID string, cancel context.CancelFunc) func() {
	entry := &inFlightTurnEntry{cancel: cancel}
	inFlightTurnsMu.Lock()
	// If a new turn starts for the same agent while one is registered
	// (shouldn't happen under the session lock, but be safe):
	// the new registration replaces the old; the old cancel is still
	// called by its own defer when that stream completes.
	inFlightTurns[agentID] = entry
	inFlightTurnsMu.Unlock()

	return func() {
		cancel()
		inFlightTurnsMu.Lock()
		if inFlightTurns[agentID] == entry {
			delete(inFlightTurns, agentID)
		}
		inFlightTurnsMu.Unlock()
	}
}

// CancelTurn implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) CancelTurn(ctx context.Context, req *agentv1.CancelTurnRequest) (*agentv1.CancelTurnResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	inFlightTurnsMu.Lock()
	entry, ok := inFlightTurns[agentID]
	inFlightTurnsMu.Unlock()

	if !ok || entry == nil {
		return nil, fmt.Errorf("no in-flight turn for agent %q", agentID)
	}

	entry.cancel()
	return &agentv1.CancelTurnResponse{}, nil
}

// backendIdentity returns the map key used for the failed-until-reset tracker. Endpoint and
// model together identify a backend: a fallback may differ from the primary in either, so
// both are part of the identity.
func backendIdentity(cfg *RuntimeConfig) string {
	return cfg.Endpoint + "/" + cfg.Model
}

// markFailedUntil records a backend as failed until the given reset time. In-memory per
// process (v1 scope - nothing persisted); a later turn in this process will start at the
// first fallback instead of retrying a backend that is provably down until reset.
func (s *AgentSDK) markFailedUntil(cfg *RuntimeConfig, resetAt time.Time) {
	s.failedUntilMu.Lock()
	defer s.failedUntilMu.Unlock()
	s.failedUntil[backendIdentity(cfg)] = resetAt
}

// backendFailedUntilReset reports whether cfg is currently inside its failed-until window.
func (s *AgentSDK) backendFailedUntilReset(cfg *RuntimeConfig) bool {
	s.failedUntilMu.Lock()
	resetAt, ok := s.failedUntil[backendIdentity(cfg)]
	s.failedUntilMu.Unlock()
	if !ok {
		return false
	}
	if time.Now().After(resetAt) {
		// Window closed; drop the stale entry so we don't leak map entries per outage.
		s.failedUntilMu.Lock()
		delete(s.failedUntil, backendIdentity(cfg))
		s.failedUntilMu.Unlock()
		return false
	}
	return true
}

// runTurnWithRuntimeFallback walks the runtime fallback chain at turn setup: it attempts
// the primary backend first, and on a qualifying error (transport, 429-after-retries, 5xx)
// that arrives BEFORE any text was yielded for this turn, descends to the next fallback
// level by rebuilding the folder agent with that level's fully-specified config (possibly a
// different provider, so the model constructor re-runs per level). Once text has been
// yielded, failures are NOT masked - a mid-turn backend switch would produce frankenstein
// output - but the failure is surfaced with an annotated warning (including the backend's
// quota-reset hint when present). Backends marked failed-until-reset by a prior turn's 429
// are skipped at turn start so a provably-down quota doesn't burn a turn. Every turn starts
// primary-first again unless that primary is inside its failed-until window (fail-forward
// per turn, never sticky beyond the reset hint).
func (s *AgentSDK) runTurnWithRuntimeFallback(
	ctx context.Context,
	agentID string,
	primary *FolderAgent,
	chain []*RuntimeConfig,
	load func(cfg *RuntimeConfig) (*FolderAgent, error),
	yieldStream func(fa *FolderAgent) iter.Seq2[string, error],
	onWarnings []func(string),
	yield func(string, error) bool,
) {
	var lastErr error
	for level, levelCfg := range chain {
		// Skip a backend that a previous turn's 429 marked failed-until-reset: retrying it
		// now would burn a whole turn on a quota that is provably still exhausted.
		if s.backendFailedUntilReset(levelCfg) {
			for _, fn := range onWarnings {
				if fn != nil {
					fn(fmt.Sprintf("skipping backend %s/%s: usage limit not yet reset", levelCfg.Endpoint, levelCfg.Model))
				}
			}
			continue
		}

		fa := primary
		if level > 0 {
			var err error
			fa, err = load(levelCfg)
			if err != nil {
				yield("", fmt.Errorf("failed to load fallback backend %q for agent %q: %w", levelCfg.Endpoint, agentID, err))
				return
			}
		}

		var yieldedText bool
		descend := false
		for chunk, err := range yieldStream(fa) {
			if err != nil {
				lastErr = err
				// Record quota-reset metadata when the provider supplies it, so the NEXT turn skips
				// this backend instead of waiting for another fresh 429 (semantics 3, v1 in-memory).
				if resetAt, ok := ParseQuotaResetHint(err); ok {
					s.markFailedUntil(levelCfg, resetAt)
				}
				if !yieldedText && IsQualifyingFallbackError(err) && level+1 < len(chain) {
					// Zero-text mid-turn failover (semantics 1): nothing was emitted, so re-running
					// the turn from scratch on the fallback is clean - no frankenstein risk.
					descend = true
					break
				}
				// Text was already emitted: never swap backends mid-stream (frankenstein guard).
				// Surface the failure with an annotated warning naming the backend and, when known,
				// its quota reset time so the operator/agent knows the next turn will skip it.
				if IsQualifyingFallbackError(err) {
					warn := fmt.Sprintf("backend %s/%s failed mid-turn: %v", levelCfg.Endpoint, levelCfg.Model, err)
					if resetAt, ok := ParseQuotaResetHint(err); ok {
						warn += fmt.Sprintf("; usage limit reached, resets %s", resetAt.Format("2006-01-02 15:04:05"))
					}
					for _, fn := range onWarnings {
						if fn != nil {
							fn(warn)
						}
					}
				}
				yield("", err)
				return
			}
			if chunk != "" {
				yieldedText = true
			}
			if !yield(chunk, nil) {
				return
			}
		}
		if ctx.Err() != nil {
			yield("", ctx.Err())
			return
		}
		if !descend {
			return
		}

		// Warn which backend failed and which fallback engaged, then retry next level.
		next := chain[level+1]
		warn := fmt.Sprintf("backend %s/%s failed: %v; falling back to %s/%s", levelCfg.Endpoint, levelCfg.Model, lastErr, next.Endpoint, next.Model)
		for _, fn := range onWarnings {
			if fn != nil {
				fn(warn)
			}
		}
	}
	// Chain exhausted: the last error was qualifying but every fallback failed (or all were
	// skipped as failed-until-reset).
	if ctx.Err() != nil {
		yield("", ctx.Err())
		return
	}
	if lastErr != nil {
		yield("", fmt.Errorf("all runtime fallback backends failed for agent %q; last error: %w", agentID, lastErr))
		return
	}
	yield("", fmt.Errorf("no runtime backend available for agent %q: all backends were skipped as failed-until-reset", agentID))
}

// generateTurnStreamImpl loads the folder agent and generates the assistant turn yielding text chunks as they arrive.
// Holds the session lock for the entire duration of the stream.
func (s *AgentSDK) generateTurnStreamImpl(ctx context.Context, agentID string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if agentID == "" {
			yield("", fmt.Errorf("agentID cannot be empty"))
			return
		}

		a2aMeta, err := ValidateAgentTarget(agentID)
		if err != nil {
			yield("", err)
			return
		}

		agentDir := s.AgentDir(agentID)
		lock, err := AcquireSessionLockContext(ctx, agentDir)
		if err != nil {
			yield("", fmt.Errorf("failed to acquire session lock: %w", err))
			return
		}
		defer lock.Release()

		turnCtx, cancel := context.WithCancel(ctx)
		defer registerInFlightTurn(agentID, cancel)()

		hookEnv := s.popLastHookEnv(agentID)
		primary, err := LoadFolderAgentWithHookEnvWithSink(s.WorkspaceDir, agentID, a2aMeta, hookEnv, s.MaxToolTurns, toolEventsFromCtx(turnCtx), s.CommandTimeoutSeconds)
		if err != nil {
			yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
			return
		}
		chain := primary.RuntimeConfig.FallbackChain()

		load := func(cfg *RuntimeConfig) (*FolderAgent, error) {
			return loadFolderAgentFromRuntime(primary.AgentDir, s.WorkspaceDir, agentID, a2aMeta, hookEnv, cfg, primary.DotEnv, s.MaxToolTurns, primary.ToolEvents, s.CommandTimeoutSeconds)
		}
		s.runTurnWithRuntimeFallback(turnCtx, agentID, primary, chain, load,
			func(fa *FolderAgent) iter.Seq2[string, error] { return fa.GenerateTurnStream(turnCtx) },
			nil, yield)
	}
}

// GenerateTurnStream satisfies agentv1.AgentServiceServer (D112 Phase 2 canary). It runs
// the continue-only generation path and pushes each text chunk onto the in-process server
// stream. Warnings do not apply here (no user-message hooks run for a continue-only turn),
// so every chunk carries text. Cancellation flows through stream.Context().
func (s *AgentSDK) GenerateTurnStream(req *agentv1.GenerateTurnStreamRequest, stream grpc.ServerStreamingServer[agentv1.GenerateTurnStreamResponse]) error {
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	sink := NewToolEventSinkWithJournal(toolJournalPath(s.AgentDir(agentID)))
	ctx := withToolEvents(stream.Context(), sink)
	for chunk, err := range s.generateTurnStreamImpl(ctx, agentID) {
		if err != nil {
			return err
		}
		if events := sink.Drain(); len(events) > 0 {
			if err := sendGenToolEvents(stream, events); err != nil {
				return err
			}
		}
		if chunk == "" {
			continue
		}
		if err := stream.Send(&agentv1.GenerateTurnStreamResponse{Text: chunk}); err != nil {
			return err
		}
	}
	if events := sink.Drain(); len(events) > 0 {
		if err := sendGenToolEvents(stream, events); err != nil {
			return err
		}
	}
	return nil
}

// toolEventIsAnnounce reports whether a ToolEvent is the announce (tool_call) half of the
// pair rather than the outcome (tool_call_update). Live announces have empty Status; denied
// announces carry Status=denied plus the Denied flag, so both are announce-shaped.
func toolEventIsAnnounce(ev ToolEvent) bool {
	return ev.Status == "" || ev.Denied
}

// sendGenToolEvents sends drained tool events as tool_call / tool_call_update responses.
func sendGenToolEvents(stream grpc.ServerStreamingServer[agentv1.GenerateTurnStreamResponse], events []ToolEvent) error {
	for _, ev := range events {
		resp := &agentv1.GenerateTurnStreamResponse{}
		if toolEventIsAnnounce(ev) {
			resp.ToolCall = &agentv1.ToolCall{CallId: ev.CallID, ToolName: ev.ToolName, ArgsSummary: ev.ArgsSummary, Denied: ev.Denied}
		} else {
			resp.ToolCallUpdate = &agentv1.ToolCallUpdate{CallId: ev.CallID, ToolName: ev.ToolName, Status: ev.Status, ResultBytes: ev.ResultBytes, ResultHead: ev.ResultHead, ResultRef: ev.ResultRef}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// GenerateTurn satisfies agentv1.AgentServiceServer (D112 Phase 2). It is the non-streaming
// proto twin of GenerateTurnStream: same generation path, complete assistant text returned.
func (s *AgentSDK) GenerateTurn(ctx context.Context, req *agentv1.GenerateTurnRequest) (*agentv1.GenerateTurnResponse, error) {
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	text, err := s.generateTurnImpl(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &agentv1.GenerateTurnResponse{Text: text}, nil
}

// generateTurnImpl loads the folder agent, checks for compaction, generates the next assistant turn,
// and returns the full assistant text joined across chunks with \n\n.
func (s *AgentSDK) generateTurnImpl(ctx context.Context, agentID string) (string, error) {
	var chunks []string
	for chunk, err := range s.generateTurnStreamImpl(ctx, agentID) {
		if err != nil {
			return "", err
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	if len(chunks) == 0 {
		return "", fmt.Errorf("received empty response from agent")
	}
	return strings.Join(chunks, "\n\n"), nil
}

// addAndGenerateTurnStreamImpl atomically appends a user message and yields assistant
// response chunks as they arrive under a single lock. Hook warnings are surfaced via the
// optional onWarning callback(s) rather than emitted into the text stream.
func (s *AgentSDK) addAndGenerateTurnStreamImpl(ctx context.Context, agentID string, userMessage string, onWarning ...func(string)) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if agentID == "" {
			yield("", fmt.Errorf("agentID cannot be empty"))
			return
		}
		if userMessage == "" {
			yield("", fmt.Errorf("userMessage cannot be empty"))
			return
		}

		a2aMeta, err := ValidateAgentTarget(agentID)
		if err != nil {
			yield("", err)
			return
		}

		agentDir := s.AgentDir(agentID)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			yield("", fmt.Errorf("failed to create agent directory %s: %w", agentDir, err))
			return
		}

		lock, err := AcquireSessionLockContext(ctx, agentDir)
		if err != nil {
			yield("", fmt.Errorf("failed to acquire session lock: %w", err))
			return
		}
		defer lock.Release()

		turnCtx, cancel := context.WithCancel(ctx)
		defer registerInFlightTurn(agentID, cancel)()

		// Run hooks on userMessage
		finalMsg, hookEnv, warnings, _ := RunUserMessageHooksWithContext(turnCtx, agentDir, userMessage)

		for _, w := range warnings {
			for _, fn := range onWarning {
				if fn != nil {
					fn(w)
				}
			}
		}

		if turnCtx.Err() != nil {
			yield("", turnCtx.Err())
			return
		}

		// 1. Append User Turn
		if err := AppendSessionTurn(agentDir, "user", finalMsg); err != nil {
			yield("", fmt.Errorf("failed to append user turn: %w", err))
			return
		}

		// 2. Load Folder Agent & Stream Assistant Turn, walking the runtime fallback chain.
		// The user turn is already appended above; fallback levels only re-run generation
		// against the same session, never re-append the user message.
		primary, err := LoadFolderAgentWithHookEnvWithSink(s.WorkspaceDir, agentID, a2aMeta, hookEnv, s.MaxToolTurns, toolEventsFromCtx(turnCtx), s.CommandTimeoutSeconds)
		if err != nil {
			yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
			return
		}
		chain := primary.RuntimeConfig.FallbackChain()

		load := func(cfg *RuntimeConfig) (*FolderAgent, error) {
			return loadFolderAgentFromRuntime(primary.AgentDir, s.WorkspaceDir, agentID, a2aMeta, hookEnv, cfg, primary.DotEnv, s.MaxToolTurns, primary.ToolEvents, s.CommandTimeoutSeconds)
		}
		s.runTurnWithRuntimeFallback(turnCtx, agentID, primary, chain, load,
			func(fa *FolderAgent) iter.Seq2[string, error] { return fa.GenerateTurnStream(turnCtx) },
			onWarning, yield)
	}
}

// AsideUsage carries the token counts billed to an aside turn. Returned as metadata only;
// aside never writes usage into session state (the caller may bill externally).
type asideUsage struct {
	PromptTokens     int32
	CandidatesTokens int32
	TotalTokens      int32
}

// AsideTurnResult is the completed aside: the streamed answer's full text, tool denials, and
// usage metadata. Nothing about the aside is persisted anywhere.
type asideTurnResult struct {
	Text        string
	Warnings    []string
	ToolDenials int64
	// ToolEvents carries the denied-only tool events emitted during the aside fork (stream
	// visibility; never persisted - the aside side-effect contract).
	ToolEvents []ToolEvent
	Usage      asideUsage
}

// AsideTurnStream answers a one-shot question against a FORKED in-memory copy of the agent's
// session (D45 disposable-session shape): system prompt + persistent-memory turn + session
// history + the question as the final user turn. The aside model sees the agent's tools
// (cache-prefix identity preserved) but tool INVOCATION is denied via the compaction deny
// machinery - a functionCall emits a completable denial, never an execution.
//
// Side-effect contract (archon): NOTHING persists. No session lock is taken (copy-on-read
// snapshot, never the exclusive turn lock, so a live turn is never blocked), no session.jsonl
// append, no MEMORY.md update, no scratchpad writes, no workspace git/trace events, no
// post-turn hooks, no compaction self-trigger. Usage is returned as metadata only.
// asideTurnStreamWithResult is AsideTurnStream with an out-param: callers that iterate the
// stream directly can inspect denials/usage after the loop without a second round trip.
func (s *AgentSDK) asideTurnStreamWithResult(ctx context.Context, agentID, question string, asideResult *asideTurnResult, onWarning ...func(string)) iter.Seq2[string, error] {
	return s.asideTurnStreamWithResultWorkspace(ctx, s.WorkspaceDir, agentID, question, asideResult, onWarning...)
}

// asideTurnStreamWithResultWorkspace is asideTurnStreamWithResult against an explicit
// workspace directory (used by the RPC surface to honor request workspace_dir overrides
// without copying the SDK struct, which carries a mutex).
func (s *AgentSDK) asideTurnStreamWithResultWorkspace(ctx context.Context, workspaceDir, agentID, question string, asideResult *asideTurnResult, onWarning ...func(string)) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		_ = asideInternal(s, ctx, workspaceDir, agentID, question, asideResult, onWarning, yield)
	}
}

// AsideTurn is the non-streaming twin of AsideTurnStream: same fork, same denial, same
// nothing-persists contract, returning the full text plus warnings/denials/usage metadata.
func (s *AgentSDK) asideTurn(ctx context.Context, agentID, question string) (*asideTurnResult, error) {
	var warnings []string
	var chunks []string
	result := &asideTurnResult{}
	for chunk, err := range s.asideTurnStreamWithResult(ctx, agentID, question, result, func(w string) { warnings = append(warnings, w) }) {
		if err != nil {
			return nil, err
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	result.Text = strings.Join(chunks, "\n\n")
	result.Warnings = warnings
	return result, nil
}

// asideInternal implements the aside fork-and-run. It is an iterator body factory shared by
// the streaming and non-streaming surfaces. asideResult (optional) is filled with tool
// denial count and usage metadata after the run - metadata only, never persisted.
//
// Order of operations (all read-only on the main agent):
//  1. Validate agent + A2A allowlist/cycle check (same gate as prompt). NO session lock.
//  2. Load the folder agent (system prompt, model, tools, runtime) - read-only.
//  3. Snapshot session.jsonl + MEMORY.md under copy-on-read.
//  4. Fork into session.InMemoryService using the D45 shape (memory turn + cleaned history).
//  5. Build an aside-scoped agent with tool invocation denied and its own usage tracker.
//  6. Run the runner over the in-memory fork (never FileSessionService), stream text.
//  7. Nothing written: no AppendSessionTurn, no ReadMemoryFile+write, no scratchpad, no git.
func asideInternal(s *AgentSDK, ctx context.Context, workspaceDir, agentID, question string, asideResult *asideTurnResult, onWarning []func(string), yield func(string, error) bool) error {
	if agentID == "" {
		yield("", fmt.Errorf("agentID cannot be empty"))
		return fmt.Errorf("agentID cannot be empty")
	}
	if question == "" {
		yield("", fmt.Errorf("question cannot be empty"))
		return fmt.Errorf("question cannot be empty")
	}

	a2aMeta, err := ValidateAgentTarget(agentID)
	if err != nil {
		yield("", err)
		return err
	}

	agentDir := filepath.Join(workspaceDir, agentID)
	if !pathExists(agentDir) {
		yield("", fmt.Errorf("agent directory %s does not exist", agentDir))
		return fmt.Errorf("agent directory %s does not exist", agentDir)
	}
	// NO AcquireSessionLock: aside must never block a live turn (archon lock discipline). The
	// snapshot below is a copy-on-read of whatever the file currently contains.

	fa, err := LoadFolderAgentWithHookEnv(workspaceDir, agentID, a2aMeta, nil, s.MaxToolTurns, s.CommandTimeoutSeconds)
	if err != nil {
		yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
		return err
	}

	// Copy-on-read snapshot: session history + persistent memory.
	sessionTurns, err := ReadSessionTurns(agentDir)
	if err != nil {
		yield("", fmt.Errorf("failed to read session for aside: %w", err))
		return err
	}
	memContent, _ := ReadMemoryFile(agentDir)
	memTurnText := FormatPersistentMemoryTurn(memContent)
	memTurn := genai.NewContentFromText(memTurnText, "user")
	seedContents := CleanSessionTurns(append([]*genai.Content{memTurn}, sessionTurns...))
	// The aside question rides as the final user turn in the fork - the same shape a real
	// addAndGenerate would produce, minus any persistence.
	seedContents = append(seedContents, genai.NewContentFromText(question, "user"))

	asideSessionID := agentID + "-aside"
	sessionSvc := session.InMemoryService()
	createResp, err := sessionSvc.Create(ctx, &session.CreateRequest{
		AppName:   "wackypub",
		UserID:    "user",
		SessionID: asideSessionID,
	})
	if err != nil {
		yield("", fmt.Errorf("failed to create aside session: %w", err))
		return err
	}
	for i, c := range seedContents {
		evt := session.NewEvent(ctx, fmt.Sprintf("aside_seed_%d", i))
		evt.Content = c
		if c.Role == "model" {
			evt.Author = agentID
		} else {
			evt.Author = "user"
		}
		if err := sessionSvc.AppendEvent(ctx, createResp.Session, evt); err != nil {
			yield("", fmt.Errorf("failed to seed aside session: %w", err))
			return err
		}
	}

	// Aside-scoped agent: same prompt/model/tools (cache prefix identical), but tool
	// invocation denied via the #50 deny machinery; fresh tracker for usage metadata only.
	var denials int64
	tracker := &TurnUsageTracker{}
	// Aside is denied-only for tool visibility: the fork never executes tools, but it still
	// emits tool_call announce + denied update events so clients see the denials uniformly.
	// The sink is deliberately journal-free - asides never persist (archon side-effect
	// contract), so the events are stream-visible only.
	asideSink := NewToolEventSink()
	asideAgent, err := BuildADKAgentWithConfigAndTrackerForCompactionWithSink(fa.AgentID, fa.SystemPrompt, fa.MaxToolTurns, fa.RuntimeConfig, fa.Model, fa.AgentDir, tracker, &denials, asideSink, fa.Tools...)
	if err != nil {
		yield("", fmt.Errorf("failed to build aside agent for %q: %w", agentID, err))
		return err
	}

	r, err := runner.New(runner.Config{
		AppName:        "wackypub",
		Agent:          asideAgent,
		SessionService: sessionSvc,
	})
	if err != nil {
		yield("", fmt.Errorf("failed to create aside runner: %w", err))
		return err
	}

	for event, err := range r.Run(ctx, "user", asideSessionID, nil, agent.RunConfig{}) {
		if err != nil {
			yield("", fmt.Errorf("aside generation failed: %w", err))
			return err
		}
		if event == nil {
			continue
		}
		if text := ExtractTextFromEvent(event); text != "" {
			if !yield(text, nil) {
				return nil
			}
		}
	}
	if asideResult != nil {
		asideResult.ToolEvents = asideSink.Drain()
		asideResult.ToolDenials = atomic.LoadInt64(&denials)
		tracker.mu.Lock()
		asideResult.Usage.PromptTokens = tracker.LastPromptTokens
		asideResult.Usage.CandidatesTokens = tracker.LastCandidatesTokens
		asideResult.Usage.TotalTokens = tracker.LastTotalTokens
		tracker.mu.Unlock()
	}
	return nil
}

// GenerateTurnResult contains the assistant response text and any hook warnings (D87).
type GenerateTurnResult struct {
	Text     string   `json:"text"`
	Warnings []string `json:"warnings,omitempty"`
}

// addAndGenerateTurnImpl atomically appends a user message and generates the assistant
// response under a single lock. Hook warnings are collected and returned on GenerateTurnResult.
func (s *AgentSDK) addAndGenerateTurnImpl(ctx context.Context, agentID string, userMessage string) (*GenerateTurnResult, error) {
	var warnings []string
	var chunks []string
	for chunk, err := range s.addAndGenerateTurnStreamImpl(ctx, agentID, userMessage, func(w string) {
		warnings = append(warnings, w)
	}) {
		if err != nil {
			return nil, err
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("received empty response from agent")
	}
	return &GenerateTurnResult{
		Text:     strings.Join(chunks, "\n\n"),
		Warnings: warnings,
	}, nil
}

// AddAndGenerateTurnStream satisfies agentv1.AgentServiceServer (D112 Phase 2 canary). It
// atomically appends the user message, runs generation, and pushes streamed units - a text
// chunk or a hook warning carried on a response with warning set - onto the in-process
// server stream. The variadic Go-only onWarning callback is transcribed onto the stream as
// warning-bearing responses so consumers without callbacks can still see hook warnings.
func (s *AgentSDK) AddAndGenerateTurnStream(req *agentv1.AddAndGenerateTurnStreamRequest, stream grpc.ServerStreamingServer[agentv1.AddAndGenerateTurnStreamResponse]) error {
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	userMsg := ""
	if req != nil {
		userMsg = req.GetUserMessage()
	}
	sink := NewToolEventSinkWithJournal(toolJournalPath(s.AgentDir(agentID)))
	ctx := withToolEvents(stream.Context(), sink)
	for chunk, err := range s.addAndGenerateTurnStreamImpl(ctx, agentID, userMsg, func(w string) {
		if w == "" {
			return
		}
		_ = stream.Send(&agentv1.AddAndGenerateTurnStreamResponse{Warning: w})
	}) {
		if err != nil {
			return err
		}
		if events := sink.Drain(); len(events) > 0 {
			if err := sendAAGToolEvents(stream, events); err != nil {
				return err
			}
		}
		if chunk == "" {
			continue
		}
		if err := stream.Send(&agentv1.AddAndGenerateTurnStreamResponse{Text: chunk}); err != nil {
			return err
		}
	}
	if events := sink.Drain(); len(events) > 0 {
		if err := sendAAGToolEvents(stream, events); err != nil {
			return err
		}
	}
	return nil
}

// sendAAGToolEvents sends drained tool events as tool_call / tool_call_update responses on
// the add-and-generate stream.
func sendAAGToolEvents(stream grpc.ServerStreamingServer[agentv1.AddAndGenerateTurnStreamResponse], events []ToolEvent) error {
	for _, ev := range events {
		resp := &agentv1.AddAndGenerateTurnStreamResponse{}
		if toolEventIsAnnounce(ev) {
			resp.ToolCall = &agentv1.ToolCall{CallId: ev.CallID, ToolName: ev.ToolName, ArgsSummary: ev.ArgsSummary, Denied: ev.Denied}
		} else {
			resp.ToolCallUpdate = &agentv1.ToolCallUpdate{CallId: ev.CallID, ToolName: ev.ToolName, Status: ev.Status, ResultBytes: ev.ResultBytes, ResultHead: ev.ResultHead, ResultRef: ev.ResultRef}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// AddAndGenerateTurn satisfies agentv1.AgentServiceServer (D112 Phase 2). It is the
// non-streaming proto twin of AddAndGenerateTurnStream: appends the user message, runs the
// full generation, and returns the complete text plus collected hook warnings.
func (s *AgentSDK) AddAndGenerateTurn(ctx context.Context, req *agentv1.AddAndGenerateTurnRequest) (*agentv1.AddAndGenerateTurnResponse, error) {
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	userMsg := ""
	if req != nil {
		userMsg = req.GetUserMessage()
	}
	result, err := s.addAndGenerateTurnImpl(ctx, agentID, userMsg)
	if err != nil {
		return nil, err
	}
	return &agentv1.AddAndGenerateTurnResponse{
		Text:     result.Text,
		Warnings: result.Warnings,
	}, nil
}

// ListAgents implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) ListAgents(ctx context.Context, req *agentv1.ListAgentsRequest) (*agentv1.ListAgentsResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	ids, err := ListAgentIDs(wsDir)
	if err != nil {
		return nil, err
	}
	return &agentv1.ListAgentsResponse{
		AgentIds: ids,
	}, nil
}

// InspectAgent implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) InspectAgent(ctx context.Context, req *agentv1.InspectAgentRequest) (*agentv1.InspectAgentResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	agentDir := filepath.Join(wsDir, agentID)
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		return &agentv1.InspectAgentResponse{
			AgentId:        agentID,
			AgentDir:       agentDir,
			AgentDirExists: false,
		}, nil
	}

	insp, err := InspectAgentDir(wsDir, agentID)
	if err != nil {
		return nil, err
	}

	resp := &agentv1.InspectAgentResponse{
		AgentId:              insp.AgentID,
		AgentDir:             insp.AgentDir,
		AgentDirExists:       insp.AgentDirExists,
		AgentsMdExists:       insp.AgentsMDExists,
		MemoryMdExists:       insp.MemoryMDExists,
		DotEnvExists:         insp.DotEnvExists,
		RuntimeJsonExists:    insp.RuntimeJSONExists,
		RuntimeJsonIsSymlink: insp.RuntimeJSONIsSymlink,
		RuntimeJsonResolved:  insp.RuntimeJSONResolved,
		RuntimeJsonValid:     insp.RuntimeJSONValid,
		RuntimeJsonError:     insp.RuntimeJSONError,
		SessionJsonlExists:   insp.SessionJSONLExists,
		SessionTurnCount:     int32(insp.SessionTurnCount),
		SessionCorruptLines:  int32(insp.SessionCorruptLines),
		AllowedAgentsExists:  insp.AllowedAgentsExists,
		AllowedAgents:        insp.AllowedAgents,
		ToolsDirExists:       insp.ToolsDirExists,
		DiscoveredTools:      insp.DiscoveredTools,
		ShadowedTools:        insp.ShadowedTools,
		SkillsDirExists:      insp.SkillsDirExists,
		DiscoveredSkills:     insp.DiscoveredSkills,
		ShadowedSkills:       insp.ShadowedSkills,
	}

	return resp, nil
}

// ReadSession implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) ReadSession(ctx context.Context, req *agentv1.ReadSessionRequest) (*agentv1.ReadSessionResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := filepath.Join(wsDir, agentID)
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		return nil, err
	}

	var protoTurns []*agentv1.SessionTurn
	for _, t := range turns {
		if t == nil {
			continue
		}
		st := &agentv1.SessionTurn{
			Role: t.Role,
		}
		for _, p := range t.Parts {
			if p == nil {
				continue
			}
			sp := &agentv1.SessionPart{
				Text: p.Text,
			}
			if p.InlineData != nil {
				sp.InlineData = p.InlineData.Data
				sp.MimeType = p.InlineData.MIMEType
			}
			st.Parts = append(st.Parts, sp)
		}
		protoTurns = append(protoTurns, st)
	}

	return &agentv1.ReadSessionResponse{
		Turns: protoTurns,
	}, nil
}

// ReadMemory implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) ReadMemory(ctx context.Context, req *agentv1.ReadMemoryRequest) (*agentv1.ReadMemoryResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := filepath.Join(wsDir, agentID)
	mem, err := ReadMemoryFile(agentDir)
	if err != nil {
		return nil, err
	}

	return &agentv1.ReadMemoryResponse{
		MemoryMd: mem,
	}, nil
}

// RenderSystemPrompt implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) RenderSystemPrompt(ctx context.Context, req *agentv1.RenderSystemPromptRequest) (*agentv1.RenderSystemPromptResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	prompt, err := RenderAgentSystemPrompt(wsDir, agentID)
	if err != nil {
		return nil, err
	}

	return &agentv1.RenderSystemPromptResponse{
		RenderedPrompt: prompt,
	}, nil
}

// StripSignatures implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) StripSignatures(ctx context.Context, req *agentv1.StripSignaturesRequest) (*agentv1.StripSignaturesResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)
	lock, err := AcquireSessionLockContext(ctx, agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	count, err := StripSessionSignatures(agentDir)
	if err != nil {
		return nil, err
	}
	return &agentv1.StripSignaturesResponse{
		ModifiedTurns: int32(count),
	}, nil
}

// CompactSessionOptions specifies optional configuration and runtime overrides for compaction (D83, D84).
type CompactSessionOptions struct {
	ConfigOverride *CompactConfig
	RuntimePath    string
}

// CompactSession implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) CompactSession(ctx context.Context, req *agentv1.CompactSessionRequest) (*agentv1.CompactSessionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	a2aMeta, err := ValidateAgentTarget(agentID)
	if err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)
	lock, err := AcquireSessionLockContext(ctx, agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	var cfgOverride *CompactConfig
	if cov := req.GetConfigOverride(); cov != nil {
		cfgOverride = &CompactConfig{
			AppendOnly:         cov.GetAppendOnly(),
			CompactPct:         cov.GetCompactPct(),
			CompactOverheadPct: cov.GetCompactOverheadPct(),
			CompactionNotice:   cov.GetCompactionNotice(),
			Prompt:             cov.GetPrompt(),
		}
	}

	force := req.GetForce()
	runtimePath := req.GetRuntimePath()
	var compacted bool
	if runtimePath == "" {
		fa, err := LoadFolderAgentWithA2A(wsDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
		if err != nil {
			return nil, err
		}
		compacted, err = CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.CompactionAgent, force, cfgOverride, fa.CompactionToolDenials)
		if err != nil {
			return nil, err
		}
	} else {
		if !pathExists(agentDir) {
			return nil, fmt.Errorf("agent directory %s does not exist", agentDir)
		}

		_, _ = LoadAgentDotEnv(agentDir)

		overrideRuntimeCfg, err := LoadRuntimeConfigFile(runtimePath)
		if err != nil {
			return nil, err
		}

		expandedPrompt, err := RenderAgentSystemPrompt(wsDir, agentID)
		if err != nil {
			return nil, fmt.Errorf("failed to render system prompt for agent %s: %w", agentID, err)
		}

		overrideLLMModel, err := NewModelForRuntime(ctx, overrideRuntimeCfg, agentID)
		if err != nil {
			return nil, err
		}

		maxToolTurns := s.MaxToolTurns
		if maxToolTurns <= 0 {
			maxToolTurns = DefaultMaxToolTurns
		}
		adkAgent, err := BuildADKAgentWithConfig(agentID, expandedPrompt, maxToolTurns, overrideRuntimeCfg, overrideLLMModel)
		if err != nil {
			return nil, fmt.Errorf("failed to build disposable ADK agent for compaction of %s: %w", agentID, err)
		}

		compacted, err = CheckAndCompactSession(ctx, agentDir, overrideRuntimeCfg, adkAgent, force, cfgOverride, nil)
		if err != nil {
			return nil, err
		}
	}

	return &agentv1.CompactSessionResponse{
		Compacted: compacted,
	}, nil
}

// AsideQuestion implements agentv1.AgentServiceServer: the D112 protocol surface for the
// aside-mode one-shot question (see AsideTurnStream for the side-effect contract). It reuses
// the exact same asideInternal machinery as the SDK method - no duplicated logic - so the
// no-persistence, no-exclusive-lock, tools-denied guarantees hold identically on this path.
func (s *AgentSDK) AsideQuestion(ctx context.Context, req *agentv1.AsideQuestionRequest) (*agentv1.AsideQuestionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	if req.GetQuestion() == "" {
		return nil, fmt.Errorf("question cannot be empty")
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}

	var warnings []string
	var chunks []string
	result := &asideTurnResult{}
	for chunk, err := range s.asideTurnStreamWithResultWorkspace(ctx, wsDir, agentID, req.GetQuestion(), result, func(w string) { warnings = append(warnings, w) }) {
		if err != nil {
			return nil, err
		}
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	result.Text = strings.Join(chunks, "\n\n")
	result.Warnings = warnings
	return &agentv1.AsideQuestionResponse{
		Text:        result.Text,
		Warnings:    result.Warnings,
		ToolDenials: result.ToolDenials,
		Usage: &agentv1.TurnUsage{
			PromptTokens:     int64(result.Usage.PromptTokens),
			CompletionTokens: int64(result.Usage.CandidatesTokens),
			TotalTokens:      int64(result.Usage.TotalTokens),
		},
	}, nil
}

// CreateScratchpad implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) CreateScratchpad(ctx context.Context, req *agentv1.CreateScratchpadRequest) (*agentv1.CreateScratchpadResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	text := req.GetText()
	if text == "" {
		return nil, fmt.Errorf("scratchpad content cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)
	createdBy := req.GetCreatedBy()
	if createdBy == "" {
		createdBy = "cli"
	}
	entry, err := CreateScratchpad(agentDir, text, createdBy)
	if err != nil {
		return nil, err
	}

	warnings := entry.Warnings
	if len(warnings) == 0 && entry.Warning != "" {
		warnings = []string{entry.Warning}
	}
	pbEntry := &agentv1.ScratchpadEntry{
		EntryId:   entry.ID,
		Size:      int64(entry.Size),
		Lines:     int32(entry.Lines),
		CreatedBy: entry.CreatedBy,
		Text:      entry.Text,
		IsBinary:  entry.IsBinary,
		MimeType:  entry.MIMEType,
		Warnings:  warnings,
	}
	return &agentv1.CreateScratchpadResponse{
		Entry: pbEntry,
	}, nil
}

// GetScratchpad implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) GetScratchpad(ctx context.Context, req *agentv1.GetScratchpadRequest) (*agentv1.GetScratchpadResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	entryID := req.GetEntryId()
	if entryID == "" {
		return nil, fmt.Errorf("entryID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)

	var skipPtr, numPtr *int
	if req.SkipLines != nil {
		skip := int(*req.SkipLines)
		skipPtr = &skip
	}
	if req.NumLines != nil {
		num := int(*req.NumLines)
		numPtr = &num
	}

	text, err := GetScratchpad(agentDir, entryID, skipPtr, numPtr)
	if err != nil {
		return nil, err
	}
	return &agentv1.GetScratchpadResponse{
		Text: text,
	}, nil
}

// ListScratchpads implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) ListScratchpads(ctx context.Context, req *agentv1.ListScratchpadsRequest) (*agentv1.ListScratchpadsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)

	items, count, capVal, err := ListScratchpads(agentDir)
	if err != nil {
		return nil, err
	}

	entries := make([]*agentv1.ScratchpadEntry, len(items))
	for i, item := range items {
		entries[i] = &agentv1.ScratchpadEntry{
			EntryId:   item.ID,
			Size:      int64(item.Size),
			Lines:     int32(item.Lines),
			CreatedBy: item.CreatedBy,
			IsBinary:  item.IsBinary,
			MimeType:  item.MIMEType,
		}
	}

	return &agentv1.ListScratchpadsResponse{
		Entries:      entries,
		TotalEntries: int32(count),
		MaxCapacity:  int32(capVal),
	}, nil
}

// SearchScratchpad implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) SearchScratchpad(ctx context.Context, req *agentv1.SearchScratchpadRequest) (*agentv1.SearchScratchpadResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	entryID := req.GetEntryId()
	if entryID == "" {
		return nil, fmt.Errorf("entryID cannot be empty")
	}
	query := req.GetQuery()
	if query == "" {
		return nil, fmt.Errorf("query cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)

	var caseSensPtr *bool
	if req.CaseSensitive != nil {
		val := *req.CaseSensitive
		caseSensPtr = &val
	}

	res, err := SearchScratchpad(agentDir, entryID, query, caseSensPtr, req.GetUseRegex(), int(req.GetMaxResults()))
	if err != nil {
		return nil, err
	}

	matches := make([]*agentv1.ScratchpadMatch, len(res.Matches))
	for i, m := range res.Matches {
		matches[i] = &agentv1.ScratchpadMatch{
			Line:      int32(m.Line),
			SkipLines: int32(m.SkipLines),
			Text:      m.Text,
		}
	}

	return &agentv1.SearchScratchpadResponse{
		EntryId:      res.ID,
		Query:        res.Query,
		TotalMatches: int32(res.TotalMatches),
		MaxResults:   int32(res.MaxResults),
		Matches:      matches,
	}, nil
}

// DiffScratchpadEntries implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) DiffScratchpadEntries(ctx context.Context, req *agentv1.DiffScratchpadEntriesRequest) (*agentv1.DiffScratchpadEntriesResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	beforeID := req.GetBeforeEntryId()
	afterID := req.GetAfterEntryId()
	if beforeID == "" || afterID == "" {
		return nil, fmt.Errorf("beforeID and afterID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}

	diff, err := DiffScratchpadEntries(wsDir, agentID, beforeID, afterID)
	if err != nil {
		return nil, err
	}
	return &agentv1.DiffScratchpadEntriesResponse{
		Diff: diff,
	}, nil
}

// DeleteScratchpad implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) DeleteScratchpad(ctx context.Context, req *agentv1.DeleteScratchpadRequest) (*agentv1.DeleteScratchpadResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	entryID := req.GetEntryId()
	if entryID == "" {
		return nil, fmt.Errorf("entryID cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentDir := filepath.Join(wsDir, agentID)
	if err := DeleteScratchpad(agentDir, entryID); err != nil {
		return nil, err
	}
	return &agentv1.DeleteScratchpadResponse{}, nil
}

// Trace implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) Trace(ctx context.Context, req *agentv1.TraceRequest) (*agentv1.TraceResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}

	wsDir := s.WorkspaceDir
	if req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}

	opts := DefaultTraceOptions()
	if req.GetMaxSteps() > 0 {
		opts.MaxSteps = int(req.GetMaxSteps())
	}
	if req.GetVerbosity() > 0 {
		opts.Verbosity = int(req.GetVerbosity())
	}

	var res *TraceResult
	var err error

	switch t := req.GetTarget().(type) {
	case *agentv1.TraceRequest_TraceId:
		if t.TraceId == "" {
			return nil, fmt.Errorf("trace_id cannot be empty")
		}
		res, err = TraceByTraceID(wsDir, t.TraceId, opts)
	case *agentv1.TraceRequest_CommitSpec:
		if t.CommitSpec == "" {
			return nil, fmt.Errorf("commit_spec cannot be empty")
		}
		agentID := req.GetAgentId()
		if agentID == "" {
			return nil, fmt.Errorf("agent_id is required when commit_spec is specified")
		}
		res, err = TraceAgentCommit(wsDir, agentID, t.CommitSpec, opts)
	default:
		if traceID := req.GetTraceId(); traceID != "" {
			res, err = TraceByTraceID(wsDir, traceID, opts)
		} else if commitSpec := req.GetCommitSpec(); commitSpec != "" {
			agentID := req.GetAgentId()
			if agentID == "" {
				return nil, fmt.Errorf("agent_id is required when commit_spec is specified")
			}
			res, err = TraceAgentCommit(wsDir, agentID, commitSpec, opts)
		} else {
			return nil, fmt.Errorf("must specify either commit_spec or trace_id")
		}
	}

	if err != nil {
		return nil, err
	}

	return TraceResultToProto(res), nil
}

type SessionContextReport struct {
	AgentID               string  `json:"agent_id"`
	Model                 string  `json:"model"`
	ContextWindow         int     `json:"context_window"`
	CompactionThreshold   int     `json:"compaction_threshold"`
	CompactionOverheadPct float64 `json:"compaction_overhead_pct"`
	EstimatedTotalTokens  int     `json:"estimated_total_tokens"`
	SessionTurnsTokens    int     `json:"session_turns_tokens"`
	PromptTokensEstimate  int     `json:"prompt_tokens_estimate"`
	MemoryTokensEstimate  int     `json:"memory_tokens_estimate"`
	PercentToThreshold    float64 `json:"percent_to_threshold"`
	PercentToWindow       float64 `json:"percent_to_window"`
	TurnCount             int     `json:"turn_count"`
	Compacted             bool    `json:"compacted,omitempty"`
	LastPromptTokens      int32   `json:"last_prompt_tokens,omitempty"`
	LastCandidatesTokens  int32   `json:"last_candidates_tokens,omitempty"`
	LastTotalTokens       int32   `json:"last_total_tokens,omitempty"`
}

// InspectSessionContext implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) InspectSessionContext(ctx context.Context, req *agentv1.InspectSessionContextRequest) (*agentv1.InspectSessionContextResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	agentDir := filepath.Join(wsDir, agentID)
	runtimeCfg, err := LoadRuntimeConfig(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load runtime config for %s: %w", agentID, err)
	}

	compactCfg, err := LoadCompactConfig(agentDir)
	overheadPct := DefaultCompactionOverheadPct
	if err == nil && compactCfg != nil {
		if compactCfg.CompactOverheadPct >= 0 && compactCfg.CompactOverheadPct < 100 {
			overheadPct = compactCfg.CompactOverheadPct
		}
	}

	contextWindow := runtimeCfg.ContextWindow
	threshold := int(float64(contextWindow) * (1.0 - (overheadPct / 100.0)))

	turns, _ := ReadSessionTurns(agentDir)
	turnCount := len(turns)
	sessionTokens := 0
	if turnCount > 0 {
		sessionTokens = EstimateTokens(turns, runtimeCfg.PreserveThinking)
	}

	promptTokens := 0
	if prompt, err := RenderAgentSystemPrompt(wsDir, agentID); err == nil && prompt != "" {
		promptTokens = len(prompt) / 4
	}

	memTokens := 0
	if memPath := filepath.Join(agentDir, "MEMORY.md"); pathExists(memPath) {
		if data, err := os.ReadFile(memPath); err == nil {
			memTokens = len(data) / 4
		}
	}

	estimatedTotal := sessionTokens + promptTokens

	resp := &agentv1.InspectSessionContextResponse{
		AgentId:               agentID,
		Model:                 runtimeCfg.Model,
		ContextWindow:         int32(contextWindow),
		CompactionThreshold:   int32(threshold),
		CompactionOverheadPct: overheadPct,
		EstimatedTotalTokens:  int32(estimatedTotal),
		SessionTurnsTokens:    int32(sessionTokens),
		PromptTokensEstimate:  int32(promptTokens),
		MemoryTokensEstimate:  int32(memTokens),
		TurnCount:             int32(turnCount),
	}

	if threshold > 0 {
		resp.PercentToThreshold = (float64(estimatedTotal) / float64(threshold)) * 100.0
	}
	if contextWindow > 0 {
		resp.PercentToWindow = (float64(estimatedTotal) / float64(contextWindow)) * 100.0
	}

	if lastUsage, err := ReadLastUsage(agentDir); err == nil && lastUsage != nil {
		resp.Compacted = lastUsage.Compacted
		if !lastUsage.Compacted {
			resp.LastPromptTokens = lastUsage.PromptTokens
			resp.LastCandidatesTokens = lastUsage.CandidatesTokens
			resp.LastTotalTokens = lastUsage.TotalTokens
		}
	}

	return resp, nil
}

// InspectAgentLocks implements the behavior defined in proto/wackypub/v1/agent.proto.
func (s *AgentSDK) InspectAgentLocks(ctx context.Context, req *agentv1.InspectAgentLocksRequest) (*agentv1.InspectAgentLocksResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	obs, err := InspectAgentLocks(wsDir)
	if err != nil {
		return nil, err
	}
	var res []*agentv1.AgentLockObservation
	for _, o := range obs {
		item := &agentv1.AgentLockObservation{
			AgentId:        o.AgentID,
			AgentDir:       o.AgentDir,
			LockExists:     o.LockExists,
			HolderPid:      int32(o.HolderPID),
			HolderPidValid: o.HolderPIDValid,
			HolderAlive:    o.HolderAlive,
			HolderCommand:  o.HolderCommand,
			SessionExists:  o.SessionExists,
		}
		if !o.LockHeldSince.IsZero() {
			item.LockHeldSince = timestamppb.New(o.LockHeldSince)
		}
		if !o.LastWrite.IsZero() {
			item.LastWrite = timestamppb.New(o.LastWrite)
		}
		res = append(res, item)
	}
	return &agentv1.InspectAgentLocksResponse{
		Observations: res,
	}, nil
}
