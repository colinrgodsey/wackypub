package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const DefaultCompactionPct = 50.0
const DefaultCompactionOverheadPct = 20.0

// DefaultCompactMD holds examples/compaction/COMPACT-append.md's content, in the same
// append-only/compact-pct frontmatter + body shape a real <agentDir>/COMPACT.md
// has - parsed through the exact same ParseCompactConfig path, according to D44.
//
// Set from main.go (D45), which embeds examples/compaction/COMPACT-append.md and assigns
// it here before cmd.Execute() runs - mirrors cmd.BundledA2ASkill/BundledWSSkill (D34),
// required because examples/ isn't reachable by a //go:embed directive living
// in pkg/agent (embed patterns can't use ".." to leave their own package
// directory, and a symlink pointing back into pkg/agent doesn't work either -
// confirmed live, embed refuses to read a symlink at all: "cannot embed
// irregular file"). Tests populate this themselves (see TestMain) rather than
// relying on main.go ever running.
var DefaultCompactMD string

type CompactFrontmatter struct {
	AppendOnly         *bool    `yaml:"append-only"`
	CompactPct         *float64 `yaml:"compact-pct"`
	CompactOverheadPct *float64 `yaml:"compact-overhead-pct"`
	CompactionNotice   *string  `yaml:"compaction-notice"`
}

type CompactConfig struct {
	AppendOnly         bool
	CompactPct         float64
	CompactOverheadPct float64
	CompactionNotice   string
	Prompt             string
}

// defaultCompactionNotice is CompactConfig.CompactionNotice's fallback when a
// COMPACT.md doesn't set the field at all (D46) - generic rather than naming
// any specific search/memory tool, since wackypub has no idea what a given
// agent actually has available.
const defaultCompactionNotice = "Some turns from earlier in this session were just archived into persistent memory above during compaction. If what follows references something not fully detailed there, it's no longer directly visible here - consider using memory or search tools to recover it rather than assuming it never happened."

// ParseCompactConfig parses COMPACT.md's YAML frontmatter + body from an
// in-memory string - either read from an agent's own <agentDir>/COMPACT.md or
// the embedded DefaultCompactMD - mirroring ParseSkillFile/ParseSkillContent's
// split (D40). Fields left unset in the frontmatter keep cfg's zero-value
// defaults (AppendOnly=false, CompactPct=0) - callers seed cfg with real
// defaults before calling if that matters, the way LoadCompactConfig does.
func ParseCompactConfig(content string) (*CompactConfig, error) {
	cfg := &CompactConfig{
		AppendOnly:         true,
		CompactPct:         DefaultCompactionPct,
		CompactOverheadPct: DefaultCompactionOverheadPct,
		CompactionNotice:   defaultCompactionNotice,
	}

	body := strings.TrimSpace(content)

	var fm CompactFrontmatter
	if strings.HasPrefix(body, "---") {
		parts := strings.SplitN(body[3:], "---", 2)
		if len(parts) == 2 {
			yamlText := parts[0]
			body = strings.TrimSpace(parts[1])
			if err := yaml.Unmarshal([]byte(yamlText), &fm); err != nil {
				return nil, fmt.Errorf("failed to parse YAML frontmatter: %w", err)
			}
		}
	}

	if fm.AppendOnly != nil {
		cfg.AppendOnly = *fm.AppendOnly
	}

	if fm.CompactPct != nil {
		pct := *fm.CompactPct
		if pct > 0 && pct <= 100 {
			cfg.CompactPct = pct
		}
	}

	if fm.CompactOverheadPct != nil {
		overhead := *fm.CompactOverheadPct
		if overhead >= 0 && overhead < 100 {
			cfg.CompactOverheadPct = overhead
		}
	}

	if fm.CompactionNotice != nil {
		cfg.CompactionNotice = *fm.CompactionNotice
	}

	cfg.Prompt = body

	return cfg, nil
}

// LoadCompactConfig loads per-agent COMPACT.md from <agentDir>/COMPACT.md if
// present according to D38. Falls back to the embedded default (DefaultCompactMD)
// if absent, according to D44.
func LoadCompactConfig(agentDir string) (*CompactConfig, error) {
	if agentDir != "" {
		compactPath := filepath.Join(agentDir, "COMPACT.md")
		data, err := os.ReadFile(compactPath)
		if err == nil {
			return ParseCompactConfig(string(data))
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read COMPACT.md at %s: %w", compactPath, err)
		}
	}
	return ParseCompactConfig(DefaultCompactMD)
}

// ReadMemoryFile reads the contents of <agent_dir>/MEMORY.md.
// If the file does not exist, returns empty string without error.
func ReadMemoryFile(agentDir string) (string, error) {
	memPath := filepath.Join(agentDir, "MEMORY.md")
	data, err := os.ReadFile(memPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read MEMORY.md at %s: %w", memPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteMemoryFile updates the contents of <agent_dir>/MEMORY.md.
func WriteMemoryFile(agentDir string, memoryContent string) error {
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("failed to create agent directory: %w", err)
	}
	memPath := filepath.Join(agentDir, "MEMORY.md")
	return os.WriteFile(memPath, []byte(strings.TrimSpace(memoryContent)+"\n"), 0644)
}

// FormatPersistentMemoryTurn constructs User Turn 1 wrapping MEMORY.md in <PERSISTENT_MEMORY> tags.
func FormatPersistentMemoryTurn(memoryContent string) string {
	return fmt.Sprintf("<PERSISTENT_MEMORY>\n%s\n</PERSISTENT_MEMORY>", strings.TrimSpace(memoryContent))
}

// FormatCompactionNotice wraps a compaction-notice string in <COMPACTION_NOTICE>
// tags, mirroring FormatPersistentMemoryTurn (D46).
func FormatCompactionNotice(notice string) string {
	return fmt.Sprintf("<COMPACTION_NOTICE>\n%s\n</COMPACTION_NOTICE>", strings.TrimSpace(notice))
}

// CheckAndCompactSession checks if the session exceeds contextWindow and performs compaction,
// preserving the exact session prefix to optimize prompt caching according to D38/D45.
// force skips the contextWindow/token-estimate gate checks below (D44) - still
// refuses on a genuinely empty session regardless, since forcing compaction with
// nothing to compact isn't a testing use case, it's a no-op either way.
//
// adkAgent is the calling FolderAgent's real ADK agent (fa.ADKAgent) - already
// carries the agent's system instruction and tool declarations, so routing the
// compaction call through it (via a disposable in-memory session + one
// runner.Run call, D45) sends a request whose shared prefix - system
// instruction, tools, memory turn, the archived turns - is structurally
// identical to a real generation call, unlike the hand-built request this
// used to send directly to an *model.LLM (no Tools, system prompt glued into
// cfgOverride, when non-nil, replaces the agent's COMPACT.md configuration without
// reading or modifying it on disk (D83). When nil, LoadCompactConfig(agentDir) is used.
func CheckAndCompactSession(ctx context.Context, agentDir string, runtimeCfg *RuntimeConfig, adkAgent agent.Agent, force bool, cfgOverride *CompactConfig) (bool, error) {
	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		return false, err
	}

	if len(turns) == 0 {
		return false, nil
	}

	var compactCfg *CompactConfig
	if cfgOverride != nil {
		compactCfg = cfgOverride
	} else {
		var err error
		compactCfg, err = LoadCompactConfig(agentDir)
		if err != nil {
			return false, err
		}
	}

	if !force {
		if runtimeCfg == nil || runtimeCfg.ContextWindow <= 0 {
			return false, nil
		}
		overheadPct := compactCfg.CompactOverheadPct
		if overheadPct < 0 || overheadPct >= 100 {
			overheadPct = DefaultCompactionOverheadPct
		}
		threshold := int(float64(runtimeCfg.ContextWindow) * (1.0 - (overheadPct / 100.0)))
		estimatedTokens := EstimateTokens(turns, runtimeCfg.PreserveThinking)
		if estimatedTokens < threshold {
			return false, nil
		}
	}

	// Calculate turns to compact based on compactCfg.CompactPct (D38/D63).
	// D63: Reinterpret CompactPct as a percentage of estimated session tokens (not turn count).
	pct := compactCfg.CompactPct
	if pct <= 0 || pct > 100 {
		pct = DefaultCompactionPct
	}

	preserveThinking := false
	if runtimeCfg != nil {
		preserveThinking = runtimeCfg.PreserveThinking
	}

	totalTokens := EstimateTokens(turns, preserveThinking)
	targetTokens := int(float64(totalTokens) * (pct / 100.0))

	numToCompact := 1
	if totalTokens > 0 && targetTokens > 0 {
		var accumulatedTokens int
		for i, t := range turns {
			turnTokens := EstimateTokens([]*genai.Content{t}, preserveThinking)
			accumulatedTokens += turnTokens
			numToCompact = i + 1
			if accumulatedTokens >= targetTokens {
				break
			}
		}
	}
	if numToCompact < 1 {
		numToCompact = 1
	}
	if numToCompact > len(turns) {
		numToCompact = len(turns)
	}

	// Extend the boundary forward until it lands on a model turn, so the
	// remaining session always starts with a fresh user turn right after the
	// injected memory block - never a dangling assistant response whose
	// prompting user turn was just archived away.
	for numToCompact < len(turns) && turns[numToCompact-1].Role != "model" {
		numToCompact++
	}

	compactTurns := turns[:numToCompact]
	remainingTurns := turns[numToCompact:]

	existingMemory, err := ReadMemoryFile(agentDir)
	if err != nil {
		return false, err
	}

	agentID := filepath.Base(agentDir)
	compactSessionID := agentID + "-compact"

	// Seed a fresh, disposable in-memory session with the exact turn shape
	// FileSessionService.Get builds for a real turn (D45): the persistent-memory
	// turn alone (no system prompt glued in - that lives on adkAgent's own
	// Instruction field), then the slice of session history being archived.
	// AppendEvent on an in-memory session is a pure in-memory write - nothing
	// here touches disk, and the whole session is discarded when this
	// function returns.
	memTurnText := FormatPersistentMemoryTurn(existingMemory)
	memTurn := genai.NewContentFromText(memTurnText, "user")
	seedContents := CleanSessionTurns(append([]*genai.Content{memTurn}, compactTurns...))

	sessionSvc := session.InMemoryService()
	createResp, err := sessionSvc.Create(ctx, &session.CreateRequest{
		AppName:   "wackypub",
		UserID:    "user",
		SessionID: compactSessionID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to create in-memory compaction session: %w", err)
	}
	for i, c := range seedContents {
		evt := session.NewEvent(ctx, fmt.Sprintf("compact_seed_%d", i))
		evt.Content = c
		if c.Role == "model" {
			evt.Author = agentID
		} else {
			evt.Author = "user"
		}
		if err := sessionSvc.AppendEvent(ctx, createResp.Session, evt); err != nil {
			return false, fmt.Errorf("failed to seed compaction session: %w", err)
		}
	}

	r, err := runner.New(runner.Config{
		AppName:        "wackypub",
		Agent:          adkAgent,
		SessionService: sessionSvc,
	})
	if err != nil {
		return false, fmt.Errorf("failed to create compaction runner: %w", err)
	}

	directive := genai.NewContentFromText(compactCfg.Prompt, "user")

	var addendum string
	for event, err := range r.Run(ctx, "user", compactSessionID, directive, agent.RunConfig{}) {
		if err != nil {
			return false, fmt.Errorf("LLM compaction generation failed: %w", err)
		}
		if event != nil {
			if text := ExtractTextFromEvent(event); text != "" {
				addendum = text
			}
		}
	}

	addendum = strings.TrimSpace(addendum)
	if addendum != "" {
		var newMemory string
		if compactCfg.AppendOnly {
			existingTrimmed := strings.TrimSpace(existingMemory)
			if existingTrimmed != "" {
				newMemory = existingTrimmed + "\n\n" + addendum
			} else {
				newMemory = addendum
			}
		} else {
			newMemory = addendum
		}

		if err := WriteMemoryFile(agentDir, newMemory); err != nil {
			return false, fmt.Errorf("failed to update MEMORY.md: %w", err)
		}

		wsDir := filepath.Dir(agentDir)
		_ = CommitWorkspaceEvent(wsDir, agentID, "compact (memory)")
	}

	// Flag the discontinuity to whatever generates the next real turn (D46):
	// a separate synthetic user turn, not spliced into the surviving boundary
	// turn's own text, so what the user/agent actually said stays intact.
	// Lands as its own turn in session.jsonl and gets folded into the
	// following real user turn by CleanSessionTurns the next time
	// anything reads the session (FileSessionService.Get, same as the memory
	// turn) - no special-casing needed. Skipped on an empty remaining session
	// (nothing to attach it in front of) or an explicit opt-out.
	notice := strings.TrimSpace(compactCfg.CompactionNotice)
	if len(remainingTurns) > 0 && notice != "" {
		noticeTurn := genai.NewContentFromText(FormatCompactionNotice(notice), "user")
		remainingTurns = append([]*genai.Content{noticeTurn}, remainingTurns...)
	}

	if err := WriteSessionTurns(agentDir, remainingTurns); err != nil {
		return false, fmt.Errorf("failed to update session.jsonl after compaction: %w", err)
	}

	wsDir := filepath.Dir(agentDir)
	_ = CommitWorkspaceEvent(wsDir, agentID, "compact")

	return true, nil
}
