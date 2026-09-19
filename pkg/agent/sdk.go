// Package agent provides the Go implementation for folder-based agent orchestration.
//
// NOTE(D112): Authoritative behavior and contract documentation for AgentSDK methods
// lives in the protobuf service definition at proto/wackypub/v1/agent.proto.
// That file is canonical; this file provides the concrete in-process Go implementation.
// Method godocs for service interface methods are pointers, not standalone definitions.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	lock, err := AcquireSessionLock(agentDir)
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

// addUserTurnLegacy appends a user message to <ws_dir>/<agent_id>/session.jsonl using the
// legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) addUserTurnLegacy(agentID string, message string) (*UserTurnResult, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	if message == "" {
		return nil, fmt.Errorf("message cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := s.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agent directory %s: %w", agentDir, err)
	}

	lock, err := AcquireSessionLock(agentDir)
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

	_ = CommitWorkspaceEvent(s.WorkspaceDir, agentID, "user")
	return &UserTurnResult{
		Content:  content,
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

	lock, err := AcquireSessionLock(agentDir)
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

// addMediaLegacy appends a normalized, resized JPEG image turn read from reader to
// <ws_dir>/<agent_id>/session.jsonl using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) addMediaLegacy(agentID string, reader io.Reader) (*genai.Content, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	if reader == nil {
		return nil, fmt.Errorf("image reader cannot be nil")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := s.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agent directory %s: %w", agentDir, err)
	}

	lock, err := AcquireSessionLock(agentDir)
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

	jpegBytes, mimeType, err := NormalizeAndResizeImage(reader, runtimeCfg.MaxImageDimension)
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

	_ = CommitWorkspaceEvent(s.WorkspaceDir, agentID, "user (media)")
	return content, nil
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

// cancelTurnLegacy cancels an in-flight turn for the given agent using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) cancelTurnLegacy(agentID string) error {
	inFlightTurnsMu.Lock()
	entry, ok := inFlightTurns[agentID]
	inFlightTurnsMu.Unlock()

	if !ok || entry == nil {
		return fmt.Errorf("no in-flight turn for agent %q", agentID)
	}

	entry.cancel()
	return nil
}

// generateTurnStreamLegacy loads the folder agent and generates the assistant turn yielding text chunks as they arrive.
// Holds the session lock for the entire duration of the stream.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
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

func (s *AgentSDK) generateTurnStreamLegacy(ctx context.Context, agentID string) iter.Seq2[string, error] {
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
		lock, err := AcquireSessionLock(agentDir)
		if err != nil {
			yield("", fmt.Errorf("failed to acquire session lock: %w", err))
			return
		}
		defer lock.Release()

		turnCtx, cancel := context.WithCancel(ctx)
		defer registerInFlightTurn(agentID, cancel)()

		hookEnv := s.popLastHookEnv(agentID)
		primary, err := LoadFolderAgentWithHookEnv(s.WorkspaceDir, agentID, a2aMeta, hookEnv, s.MaxToolTurns, s.CommandTimeoutSeconds)
		if err != nil {
			yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
			return
		}
		chain := primary.RuntimeConfig.FallbackChain()

		load := func(cfg *RuntimeConfig) (*FolderAgent, error) {
			return loadFolderAgentFromRuntime(primary.AgentDir, s.WorkspaceDir, agentID, a2aMeta, hookEnv, cfg, primary.DotEnv, s.MaxToolTurns, s.CommandTimeoutSeconds)
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
	for chunk, err := range s.generateTurnStreamLegacy(stream.Context(), agentID) {
		if err != nil {
			return err
		}
		if chunk == "" {
			continue
		}
		if err := stream.Send(&agentv1.GenerateTurnStreamResponse{Text: chunk}); err != nil {
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
	text, err := s.generateTurnLegacy(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &agentv1.GenerateTurnResponse{Text: text}, nil
}

// generateTurnLegacy loads the folder agent, checks for compaction, generates the next assistant turn,
// and returns the full assistant text joined across chunks with \n\n.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) generateTurnLegacy(ctx context.Context, agentID string) (string, error) {
	var chunks []string
	for chunk, err := range s.generateTurnStreamLegacy(ctx, agentID) {
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

// addAndGenerateTurnStreamLegacy atomically appends a user message and yields assistant
// response chunks as they arrive under a single lock. Hook warnings are surfaced via the
// optional onWarning callback(s) rather than emitted into the text stream.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) addAndGenerateTurnStreamLegacy(ctx context.Context, agentID string, userMessage string, onWarning ...func(string)) iter.Seq2[string, error] {
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

		lock, err := AcquireSessionLock(agentDir)
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
		primary, err := LoadFolderAgentWithHookEnv(s.WorkspaceDir, agentID, a2aMeta, hookEnv, s.MaxToolTurns, s.CommandTimeoutSeconds)
		if err != nil {
			yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
			return
		}
		chain := primary.RuntimeConfig.FallbackChain()

		load := func(cfg *RuntimeConfig) (*FolderAgent, error) {
			return loadFolderAgentFromRuntime(primary.AgentDir, s.WorkspaceDir, agentID, a2aMeta, hookEnv, cfg, primary.DotEnv, s.MaxToolTurns, s.CommandTimeoutSeconds)
		}
		s.runTurnWithRuntimeFallback(turnCtx, agentID, primary, chain, load,
			func(fa *FolderAgent) iter.Seq2[string, error] { return fa.GenerateTurnStream(turnCtx) },
			onWarning, yield)
	}
}

// GenerateTurnResult contains the assistant response text and any hook warnings (D87).
type GenerateTurnResult struct {
	Text     string   `json:"text"`
	Warnings []string `json:"warnings,omitempty"`
}

// addAndGenerateTurnLegacy atomically appends a user message and generates the assistant
// response under a single lock. Hook warnings are collected and returned on GenerateTurnResult.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) addAndGenerateTurnLegacy(ctx context.Context, agentID string, userMessage string) (*GenerateTurnResult, error) {
	var warnings []string
	var chunks []string
	for chunk, err := range s.addAndGenerateTurnStreamLegacy(ctx, agentID, userMessage, func(w string) {
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
	for chunk, err := range s.addAndGenerateTurnStreamLegacy(stream.Context(), agentID, userMsg, func(w string) {
		if w == "" {
			return
		}
		_ = stream.Send(&agentv1.AddAndGenerateTurnStreamResponse{Warning: w})
	}) {
		if err != nil {
			return err
		}
		if chunk == "" {
			continue
		}
		if err := stream.Send(&agentv1.AddAndGenerateTurnStreamResponse{Text: chunk}); err != nil {
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
	result, err := s.addAndGenerateTurnLegacy(ctx, agentID, userMsg)
	if err != nil {
		return nil, err
	}
	return &agentv1.AddAndGenerateTurnResponse{
		Text:     result.Text,
		Warnings: result.Warnings,
	}, nil
}

// getAgentLegacy loads and returns the FolderAgent object for low-level ADK runner interactions
// using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) getAgentLegacy(agentID string) (*FolderAgent, error) {
	a2aMeta, err := ValidateAgentTarget(agentID)
	if err != nil {
		return nil, err
	}

	return LoadFolderAgentWithA2A(s.WorkspaceDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
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

// listAgentsLegacy returns the IDs of agent directories found directly under the
// workspace directory using the legacy unparameterized positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) listAgentsLegacy() ([]string, error) {
	return ListAgentIDs(s.WorkspaceDir)
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

// inspectAgentLegacy reports the on-disk state of <ws_dir>/<agent_id>: which
// expected files are present, whether runtime.json parses, and
// session/memory stats. Safe to call on an agent that doesn't exist yet or
// is only partially set up - see AgentInspection.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) inspectAgentLegacy(agentID string) (*AgentInspection, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	agentDir := s.AgentDir(agentID)
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		return &AgentInspection{AgentID: agentID, AgentDir: agentDir}, nil
	}

	return InspectAgentDir(s.WorkspaceDir, agentID)
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

// readSessionLegacy returns all conversation turns logged in <ws_dir>/<agent_id>/session.jsonl.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) readSessionLegacy(agentID string) ([]*genai.Content, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	// No session lock: ReadSessionTurns already tolerates a torn read
	// gracefully (skipped lines surface via SessionCorruptLines elsewhere),
	// and the blocking acquire here is exactly what can deadlock against an
	// agent's own already-held lock during live generation.
	agentDir := s.AgentDir(agentID)
	return ReadSessionTurns(agentDir)
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

// readMemoryLegacy returns the current contents of <ws_dir>/<agent_id>/MEMORY.md.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) readMemoryLegacy(agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return "", err
	}

	// No session lock needed: MEMORY.md isn't session.jsonl, and this read
	// doesn't need protecting against the same writers that file's lock
	// serializes.
	agentDir := s.AgentDir(agentID)
	return ReadMemoryFile(agentDir)
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

// renderSystemPromptLegacy returns the fully rendered system prompt for an agent -
// AGENTS.md (or the generic fallback if it doesn't exist) after
// @<FILE_PATH> macro expansion. Does not construct a model and does not
// require runtime.json to exist or be valid - useful for validating
// AGENTS.md/macro output independently of backend configuration.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) renderSystemPromptLegacy(agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return "", err
	}

	// No session lock needed: RenderAgentSystemPrompt reads AGENTS.md and
	// skills/, never session.jsonl - acquiring the lock here only added an
	// unnecessary blocking dependency on whatever else might be holding it.
	return RenderAgentSystemPrompt(s.WorkspaceDir, agentID)
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
	lock, err := AcquireSessionLock(agentDir)
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

// stripSignaturesLegacy permanently removes provider-specific opaque reasoning/thought signatures
// using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) stripSignaturesLegacy(agentID string) (int, error) {
	if agentID == "" {
		return 0, fmt.Errorf("agentID cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return 0, err
	}

	agentDir := s.AgentDir(agentID)
	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		return 0, fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	return StripSessionSignatures(agentDir)
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
	lock, err := AcquireSessionLock(agentDir)
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

// compactSessionLegacy manually triggers session compaction evaluation for an agent using the
// legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) compactSessionLegacy(ctx context.Context, agentID string, force bool) (bool, error) {
	return s.compactSessionWithOptionsLegacy(ctx, agentID, force, CompactSessionOptions{})
}

// compactSessionWithConfigLegacy manually triggers session compaction evaluation with config override
// using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) compactSessionWithConfigLegacy(ctx context.Context, agentID string, force bool, cfgOverride *CompactConfig) (bool, error) {
	return s.compactSessionWithOptionsLegacy(ctx, agentID, force, CompactSessionOptions{
		ConfigOverride: cfgOverride,
	})
}

// compactSessionWithOptionsLegacy manually triggers session compaction evaluation with options
// using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) compactSessionWithOptionsLegacy(ctx context.Context, agentID string, force bool, opts CompactSessionOptions) (bool, error) {
	if agentID == "" {
		return false, fmt.Errorf("agentID cannot be empty")
	}

	a2aMeta, err := ValidateAgentTarget(agentID)
	if err != nil {
		return false, err
	}

	agentDir := s.AgentDir(agentID)
	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		return false, fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	if opts.RuntimePath == "" {
		fa, err := LoadFolderAgentWithA2A(s.WorkspaceDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
		if err != nil {
			return false, err
		}
		return CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.CompactionAgent, force, opts.ConfigOverride, fa.CompactionToolDenials)
	}

	// Runtime override path (D84):
	if !pathExists(agentDir) {
		return false, fmt.Errorf("agent directory %s does not exist", agentDir)
	}

	// 0. Load .env for agent (so env vars referenced in override runtime are available)
	_, _ = LoadAgentDotEnv(agentDir)

	// 1. Load override runtime config
	overrideRuntimeCfg, err := LoadRuntimeConfigFile(opts.RuntimePath)
	if err != nil {
		return false, err
	}

	// 2. Render normal AGENTS.md system prompt
	expandedPrompt, err := RenderAgentSystemPrompt(s.WorkspaceDir, agentID)
	if err != nil {
		return false, fmt.Errorf("failed to render system prompt for agent %s: %w", agentID, err)
	}

	// 3. Initialize model adapter from override runtime
	overrideLLMModel, err := NewModelForRuntime(ctx, overrideRuntimeCfg, agentID)
	if err != nil {
		return false, err
	}

	// 4. Build disposable ADK agent with NO tools (compaction never invokes tools, D45)
	maxToolTurns := s.MaxToolTurns
	if maxToolTurns <= 0 {
		maxToolTurns = DefaultMaxToolTurns
	}
	adkAgent, err := BuildADKAgentWithConfig(agentID, expandedPrompt, maxToolTurns, overrideRuntimeCfg, overrideLLMModel)
	if err != nil {
		return false, fmt.Errorf("failed to build disposable ADK agent for compaction of %s: %w", agentID, err)
	}

	return CheckAndCompactSession(ctx, agentDir, overrideRuntimeCfg, adkAgent, force, opts.ConfigOverride, nil)
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

// createScratchpadLegacy creates a new persistent scratchpad entry using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) createScratchpadLegacy(agentID string, text string, createdBy string) (*ScratchpadEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	if text == "" {
		return nil, fmt.Errorf("scratchpad content cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := s.AgentDir(agentID)
	if createdBy == "" {
		createdBy = "cli"
	}
	return CreateScratchpad(agentDir, text, createdBy)
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

// getScratchpadLegacy retrieves stored text by entry ID using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) getScratchpadLegacy(agentID string, entryID string, skipLines *int, numLines *int) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("agentID cannot be empty")
	}
	if entryID == "" {
		return "", fmt.Errorf("entryID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return "", err
	}

	agentDir := s.AgentDir(agentID)
	return GetScratchpad(agentDir, entryID, skipLines, numLines)
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

// listScratchpadsLegacy returns metadata items for all live scratchpads using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) listScratchpadsLegacy(agentID string) ([]ScratchpadItem, int, int, error) {
	if agentID == "" {
		return nil, 0, MaxScratchpadEntries, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, 0, MaxScratchpadEntries, err
	}

	agentDir := s.AgentDir(agentID)
	return ListScratchpads(agentDir)
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

// searchScratchpadLegacy searches a specific scratchpad entry using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) searchScratchpadLegacy(agentID string, entryID string, query string, caseSensitive *bool, useRegex bool, maxResults int) (*SearchScratchpadResult, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}
	if entryID == "" {
		return nil, fmt.Errorf("entryID cannot be empty")
	}
	if query == "" {
		return nil, fmt.Errorf("query cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := s.AgentDir(agentID)
	return SearchScratchpad(agentDir, entryID, query, caseSensitive, useRegex, maxResults)
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

// diffScratchpadEntriesLegacy returns a unified diff between two entries using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) diffScratchpadEntriesLegacy(agentID string, beforeID string, afterID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("agentID cannot be empty")
	}
	if beforeID == "" || afterID == "" {
		return "", fmt.Errorf("beforeID and afterID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return "", err
	}

	return DiffScratchpadEntries(s.WorkspaceDir, agentID, beforeID, afterID)
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

// deleteScratchpadLegacy removes a scratchpad entry using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) deleteScratchpadLegacy(agentID string, entryID string) error {
	if agentID == "" {
		return fmt.Errorf("agentID cannot be empty")
	}
	if entryID == "" {
		return fmt.Errorf("entryID cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return err
	}

	agentDir := s.AgentDir(agentID)
	return DeleteScratchpad(agentDir, entryID)
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

// traceLegacy performs backward causal tracing starting from an agent commit specifier
// or global trace ID using the legacy positional signature.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) traceLegacy(agentID string, commitSpec string, traceID string, opts TraceOptions) (*TraceResult, error) {
	if traceID != "" {
		return TraceByTraceID(s.WorkspaceDir, traceID, opts)
	}
	if agentID != "" && commitSpec != "" {
		return TraceAgentCommit(s.WorkspaceDir, agentID, commitSpec, opts)
	}
	return nil, fmt.Errorf("must specify either agentID and commitSpec, or traceID")
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

// inspectSessionContextLegacy calculates the current token usage, limits, and compaction headroom for an agent (D93).
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) inspectSessionContextLegacy(agentID string) (*SessionContextReport, error) {
	agentDir := s.AgentDir(agentID)
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
	if prompt, err := RenderAgentSystemPrompt(s.WorkspaceDir, agentID); err == nil && prompt != "" {
		promptTokens = len(prompt) / 4
	}

	memTokens := 0
	if memPath := filepath.Join(agentDir, "MEMORY.md"); pathExists(memPath) {
		if data, err := os.ReadFile(memPath); err == nil {
			memTokens = len(data) / 4
		}
	}

	estimatedTotal := sessionTokens + promptTokens

	report := &SessionContextReport{
		AgentID:               agentID,
		Model:                 runtimeCfg.Model,
		ContextWindow:         contextWindow,
		CompactionThreshold:   threshold,
		CompactionOverheadPct: overheadPct,
		EstimatedTotalTokens:  estimatedTotal,
		SessionTurnsTokens:    sessionTokens,
		PromptTokensEstimate:  promptTokens,
		MemoryTokensEstimate:  memTokens,
		TurnCount:             turnCount,
	}

	if threshold > 0 {
		report.PercentToThreshold = (float64(estimatedTotal) / float64(threshold)) * 100.0
	}
	if contextWindow > 0 {
		report.PercentToWindow = (float64(estimatedTotal) / float64(contextWindow)) * 100.0
	}

	if lastUsage, err := ReadLastUsage(agentDir); err == nil && lastUsage != nil {
		report.Compacted = lastUsage.Compacted
		if !lastUsage.Compacted {
			report.LastPromptTokens = lastUsage.PromptTokens
			report.LastCandidatesTokens = lastUsage.CandidatesTokens
			report.LastTotalTokens = lastUsage.TotalTokens
		}
	}

	return report, nil
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

// inspectAgentLocksLegacy returns lock observations for agents in the workspace.
//
// TODO(D112): delete at D112 Phase 5 cutover so the D104-class orphan does not persist.
func (s *AgentSDK) inspectAgentLocksLegacy() ([]AgentLockObservation, error) {
	return InspectAgentLocks(s.WorkspaceDir)
}
