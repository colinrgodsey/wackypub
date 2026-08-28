package agent

import (
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

// AgentSDK provides a clean, programmatic Go API for orchestrating folder-based agents.
type AgentSDK struct {
	WorkspaceDir          string
	MaxToolTurns          int
	CommandTimeoutSeconds int
}

// NewSDK creates an SDK instance bound to a workspace directory.
func NewSDK(workspaceDir string) *AgentSDK {
	if workspaceDir == "" {
		workspaceDir = "."
	}
	return &AgentSDK{
		WorkspaceDir:          workspaceDir,
		MaxToolTurns:          DefaultMaxToolTurns,
		CommandTimeoutSeconds: DefaultCommandTimeoutSeconds,
	}
}

// AgentDir returns the absolute or relative path for an agent folder (<ws_dir>/<agent_id>).
func (s *AgentSDK) AgentDir(agentID string) string {
	return filepath.Join(s.WorkspaceDir, agentID)
}

// AddUserTurn appends a user message to <ws_dir>/<agent_id>/session.jsonl.
// Creates the agent directory automatically if it does not exist yet.
func (s *AgentSDK) AddUserTurn(agentID string, message string) error {
	if agentID == "" {
		return fmt.Errorf("agentID cannot be empty")
	}
	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}

	if _, err := ValidateAgentTarget(agentID); err != nil {
		return err
	}

	agentDir := s.AgentDir(agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("failed to create agent directory %s: %w", agentDir, err)
	}

	lock, err := AcquireSessionLock(agentDir)
	if err != nil {
		return fmt.Errorf("failed to acquire session lock: %w", err)
	}
	defer lock.Release()

	if err := AppendSessionTurn(agentDir, "user", message); err != nil {
		return err
	}

	_ = CommitWorkspaceEvent(s.WorkspaceDir, agentID, "user")
	return nil
}

// AddMedia appends a normalized, resized JPEG image turn read from reader to
// <ws_dir>/<agent_id>/session.jsonl according to D47.
// Gated by runtime.json's maxImageDimension field — returns an error if
// maxImageDimension is absent or <= 0.
func (s *AgentSDK) AddMedia(agentID string, reader io.Reader) (*genai.Content, error) {
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

// GenerateTurnStream loads the folder agent and generates the assistant turn yielding text chunks as they arrive.
// Holds the session lock for the entire duration of the stream.
func (s *AgentSDK) GenerateTurnStream(ctx context.Context, agentID string) iter.Seq2[string, error] {
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

		fa, err := LoadFolderAgentWithA2A(s.WorkspaceDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
		if err != nil {
			yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
			return
		}

		for chunk, err := range fa.GenerateTurnStream(ctx) {
			if !yield(chunk, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// GenerateTurn loads the folder agent, checks for compaction, generates the next assistant turn,
// and returns the full assistant text joined across chunks with \n\n.
func (s *AgentSDK) GenerateTurn(ctx context.Context, agentID string) (string, error) {
	var chunks []string
	for chunk, err := range s.GenerateTurnStream(ctx, agentID) {
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

// AddAndGenerateTurnStream atomically appends a user message and yields assistant response chunks as they arrive under a single lock.
func (s *AgentSDK) AddAndGenerateTurnStream(ctx context.Context, agentID string, userMessage string) iter.Seq2[string, error] {
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

		// 1. Append User Turn
		if err := AppendSessionTurn(agentDir, "user", userMessage); err != nil {
			yield("", fmt.Errorf("failed to append user turn: %w", err))
			return
		}

		// 2. Load Folder Agent & Stream Assistant Turn
		fa, err := LoadFolderAgentWithA2A(s.WorkspaceDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
		if err != nil {
			yield("", fmt.Errorf("failed to load agent %q: %w", agentID, err))
			return
		}

		for chunk, err := range fa.GenerateTurnStream(ctx) {
			if !yield(chunk, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// AddAndGenerateTurn atomically appends a user message and generates the assistant response under a single lock.
func (s *AgentSDK) AddAndGenerateTurn(ctx context.Context, agentID string, userMessage string) (string, error) {
	var chunks []string
	for chunk, err := range s.AddAndGenerateTurnStream(ctx, agentID, userMessage) {
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

// GetAgent loads and returns the FolderAgent object for low-level ADK runner interactions.
func (s *AgentSDK) GetAgent(agentID string) (*FolderAgent, error) {
	a2aMeta, err := ValidateAgentTarget(agentID)
	if err != nil {
		return nil, err
	}

	return LoadFolderAgentWithA2A(s.WorkspaceDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
}

// ListAgents returns the IDs of agent directories found directly under the
// workspace directory (see ListAgentIDs for how a directory is recognized as
// an agent). Does not acquire any lock - it only reads directory names.
func (s *AgentSDK) ListAgents() ([]string, error) {
	return ListAgentIDs(s.WorkspaceDir)
}

// InspectAgent reports the on-disk state of <ws_dir>/<agent_id>: which
// expected files are present, whether runtime.json parses, and
// session/memory stats. Safe to call on an agent that doesn't exist yet or
// is only partially set up - see AgentInspection.
//
// Deliberately does not go through ValidateAgentTarget's WACKYPUB_ALLOWED_AGENTS
// check (D16): that authorization boundary exists to gate cross-agent tool
// invocation/generation, not read-only diagnostic visibility - InspectAgent
// has no side effects and can't cause another agent to do anything. Gating
// it the same way surfaces an "unauthorized" failure as a generic parse/
// config error in wackypub workspace's summary table, which is actively
// misleading (see D16).
//
// Does not create the agent directory as a side effect (unlike most other
// AgentSDK methods) - if it doesn't exist, returns an AgentInspection with
// AgentDirExists false and every other field zero-valued.
//
// Deliberately does not acquire the session lock. AcquireSessionLock blocks
// until the lock is free, and InspectAgent is exactly the kind of call an
// agent's own tool loop can make against itself mid-generation (directly, or
// via wackypub workspace's no-arg summary, which inspects every agent
// including the caller) - since GenerateTurn already holds that same lock
// for the whole call, that blocking acquire deadlocks forever. Reading
// without the lock is safe: ReadSessionTurns already tolerates a torn read
// gracefully (see AgentInspection.SessionCorruptLines), and the lock's real
// job is serializing concurrent writers, not protecting readers.
func (s *AgentSDK) InspectAgent(agentID string) (*AgentInspection, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	agentDir := s.AgentDir(agentID)
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		return &AgentInspection{AgentID: agentID, AgentDir: agentDir}, nil
	}

	return InspectAgentDir(s.WorkspaceDir, agentID)
}

// ReadSession returns all conversation turns logged in <ws_dir>/<agent_id>/session.jsonl.
func (s *AgentSDK) ReadSession(agentID string) ([]*genai.Content, error) {
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

// ReadMemory returns the current contents of <ws_dir>/<agent_id>/MEMORY.md.
func (s *AgentSDK) ReadMemory(agentID string) (string, error) {
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

// RenderSystemPrompt returns the fully rendered system prompt for an agent -
// AGENTS.md (or the generic fallback if it doesn't exist) after
// @<FILE_PATH> macro expansion. Does not construct a model and does not
// require runtime.json to exist or be valid - useful for validating
// AGENTS.md/macro output independently of backend configuration.
func (s *AgentSDK) RenderSystemPrompt(agentID string) (string, error) {
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

// StripSignatures permanently removes provider-specific opaque
// reasoning/thought signatures (OpenRouter reasoning_details block metadata,
// e.g. encrypted/signed reasoning tied to a specific backend endpoint, and
// Gemini's ThoughtSignature field) from every turn in
// <ws_dir>/<agent_id>/session.jsonl, rewriting the file in place. Readable
// plain-text reasoning is left untouched. Useful when switching an agent from
// one model/provider to another, since a replayed signature from the old
// provider is rejected outright by the new one. Returns the number of turns
// that were modified.
func (s *AgentSDK) StripSignatures(agentID string) (int, error) {
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

// CompactSession manually triggers session compaction evaluation for an agent.
// force bypasses the contextWindow/token-estimate gate checks (D44) - only this
// manual path can force; the automatic pre-generation check never does.
func (s *AgentSDK) CompactSession(ctx context.Context, agentID string, force bool) (bool, error) {
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

	fa, err := LoadFolderAgentWithA2A(s.WorkspaceDir, agentID, a2aMeta, s.MaxToolTurns, s.CommandTimeoutSeconds)
	if err != nil {
		return false, err
	}
	return CheckAndCompactSession(ctx, fa.AgentDir, fa.RuntimeConfig, fa.ADKAgent, force)
}

// CreateScratchpad creates a new persistent scratchpad entry for an agent (<ws_dir>/<agent_id>/scratchpad/).
// Atomic and collision-safe across processes without requiring the session lock.
func (s *AgentSDK) CreateScratchpad(agentID string, text string, createdBy string) (*ScratchpadEntry, error) {
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

// GetScratchpad retrieves stored text from <ws_dir>/<agent_id>/scratchpad.json by entry ID.
// Does not acquire the session lock (read-only against atomic temp-file replace).
func (s *AgentSDK) GetScratchpad(agentID string, entryID string, skipLines *int, numLines *int) (string, error) {
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

// ListScratchpads returns metadata items for all live scratchpad entries in <ws_dir>/<agent_id>/scratchpad.json.
// Does not acquire the session lock (read-only against atomic temp-file replace).
func (s *AgentSDK) ListScratchpads(agentID string) ([]ScratchpadItem, int, int, error) {
	if agentID == "" {
		return nil, 0, MaxScratchpadEntries, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, 0, MaxScratchpadEntries, err
	}

	agentDir := s.AgentDir(agentID)
	return ListScratchpads(agentDir)
}

// SearchScratchpad searches a specific scratchpad entry in <ws_dir>/<agent_id>/scratchpad.json for matching lines.
// Does not acquire the session lock (read-only against atomic temp-file replace).
func (s *AgentSDK) SearchScratchpad(agentID string, entryID string, query string, caseSensitive *bool, useRegex bool, maxResults int) (*SearchScratchpadResult, error) {
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

// DeleteScratchpad removes a scratchpad entry from <ws_dir>/<agent_id>/scratchpad/ by entry ID.
func (s *AgentSDK) DeleteScratchpad(agentID string, entryID string) error {
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

// Trace performs backward causal tracing starting from an agent commit specifier or global trace ID according to D36.
func (s *AgentSDK) Trace(agentID string, commitSpec string, traceID string, opts TraceOptions) (*TraceResult, error) {
	if traceID != "" {
		return TraceByTraceID(s.WorkspaceDir, traceID, opts)
	}
	if agentID != "" && commitSpec != "" {
		return TraceAgentCommit(s.WorkspaceDir, agentID, commitSpec, opts)
	}
	return nil, fmt.Errorf("must specify either agentID and commitSpec, or traceID")
}
