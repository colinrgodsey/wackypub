package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"

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

// compactionSeedTokenFraction bounds the disposable compaction session's
// seeded tokens to half the context window (D101 P1.5). Without it, one
// megabyte-scale archived turn gets seeded verbatim into the compaction
// runner and the provider rejects the request with a context-exceeded error,
// wedging compaction - and therefore the whole session - permanently.
const compactionSeedTokenFraction = 0.5

// hasFunctionCall reports whether content contains any FunctionCall parts.
func hasFunctionCall(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// hasFunctionResponse reports whether content contains any FunctionResponse parts.
func hasFunctionResponse(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// findToolPairings maps tool-call model turns to their corresponding tool-response
// user turns. Returns a slice where pairIndex[i] is the paired turn index, or -1 if
// turn i does not participate in a call/response pair.
func findToolPairings(turns []*genai.Content) []int {
	pairs := make([]int, len(turns))
	for i := range pairs {
		pairs[i] = -1
	}
	for i := 0; i < len(turns)-1; i++ {
		modelTurn := turns[i]
		if modelTurn == nil || modelTurn.Role != "model" || !hasFunctionCall(modelTurn) {
			continue
		}
		userTurn := turns[i+1]
		if userTurn != nil && userTurn.Role == "user" && hasFunctionResponse(userTurn) {
			pairs[i] = i + 1
			pairs[i+1] = i
		}
	}
	return pairs
}

// capSeedTokensForCompaction stubs oversized turns until the total seed estimate
// fits maxTokens (D101 P1.5).
//
// Pair-preserving invariant: A model turn containing FunctionCalls and its paired
// user turn containing FunctionResponses must be stubbed together as a unit. If one
// side of a call/response pair is stubbed (converting non-text tool calls/responses
// into text drop markers), the other side must also be stubbed into text drop markers
// so that neither orphaned FunctionCalls nor orphaned FunctionResponses ever appear
// in the compaction seed.
//
// Stubbing priority:
// 1. Pure text history turns (oldest to newest): least disruptive to tool state.
// 2. Tool call/response turn pairs (oldest to newest): stubbed together in pairs.
// 3. Turn 0 (persistent memory): stubbed only as a last resort.
//
// Token estimation maintains a running total adjusted by turn deltas (O(1)) rather
// than re-estimating the entire slice on every iteration. Never mutates input turns.
func capSeedTokensForCompaction(turns []*genai.Content, maxTokens int, includeThinking bool) []*genai.Content {
	if len(turns) == 0 || maxTokens <= 0 {
		return turns
	}
	runningTokens := EstimateTokens(turns, includeThinking)
	if runningTokens <= maxTokens {
		return turns
	}

	out := make([]*genai.Content, len(turns))
	copy(out, turns)

	pairs := findToolPairings(out)

	order := make([]int, 0, len(out))
	// 1. Pure text history turns (oldest to newest)
	for i := 1; i < len(out); i++ {
		if pairs[i] == -1 {
			order = append(order, i)
		}
	}
	// 2. Tool-pair turns (only add the lower index of each pair to process once)
	for i := 1; i < len(out); i++ {
		if pairs[i] != -1 && i < pairs[i] {
			order = append(order, i)
		}
	}
	// 3. Persistent memory turn 0 last
	order = append(order, 0)

	stubbedCount := 0
	for share := maxTokens / len(out); share >= 8; share /= 2 {
		for _, i := range order {
			j := pairs[i]
			if j != -1 {
				// Tool call/response pair: stub both together if either exceeds share
				t1Tokens := EstimateTokens([]*genai.Content{out[i]}, includeThinking)
				t2Tokens := EstimateTokens([]*genai.Content{out[j]}, includeThinking)
				if t1Tokens <= share && t2Tokens <= share {
					continue
				}

				newI := stubTurnForSeedShare(out[i], share)
				newITokens := EstimateTokens([]*genai.Content{newI}, includeThinking)
				out[i] = newI
				runningTokens += (newITokens - t1Tokens)

				newJ := stubTurnForSeedShare(out[j], share)
				newJTokens := EstimateTokens([]*genai.Content{newJ}, includeThinking)
				out[j] = newJ
				runningTokens += (newJTokens - t2Tokens)

				stubbedCount += 2
			} else {
				// Standalone turn (pure text or persistent memory)
				tTokens := EstimateTokens([]*genai.Content{out[i]}, includeThinking)
				if tTokens <= share {
					continue
				}
				newT := stubTurnForSeedShare(out[i], share)
				newTTokens := EstimateTokens([]*genai.Content{newT}, includeThinking)
				out[i] = newT
				runningTokens += (newTTokens - tTokens)
				stubbedCount++
			}

			if runningTokens <= maxTokens {
				fmt.Fprintf(os.Stderr, "Warning: compaction seed exceeded the %d token budget (D101 P1.5) - stubbed oversized turn(s) before invoking the compaction runner.\n", maxTokens)
				return out
			}
		}
	}

	if stubbedCount > 0 {
		fmt.Fprintf(os.Stderr, "Warning: compaction seed still exceeds the %d token budget after stubbing oversized turn(s) (D101 P1.5) - proceeding with minimal stubs.\n", maxTokens)
	} else {
		fmt.Fprintf(os.Stderr, "Warning: compaction seed exceeds the %d token budget but turn share is below minimal stub floor (D101 P1.5) - proceeding without stubbing.\n", maxTokens)
	}
	return out
}

// stubTurnForSeedShare returns a budget-fitting copy of a turn: TEXT parts are
// truncated to an equal share of the budget with the same head/banner/tail
// shape the D101 P0.2 persist cap writes, and non-text parts (tool calls,
// tool responses, inline data) are replaced by a single drop marker so the
// summarizing model can still see that they existed.
func stubTurnForSeedShare(t *genai.Content, maxTurnTokens int) *genai.Content {
	if t == nil {
		return t
	}
	textCount := 0
	for _, p := range t.Parts {
		if isTextPart(p) {
			textCount++
		}
	}
	origTokens := EstimateTokens([]*genai.Content{t}, true)
	nonText := len(t.Parts) - textCount
	if textCount == 0 || maxTurnTokens*4 < 32 {
		return &genai.Content{Role: t.Role, Parts: []*genai.Part{{Text: fmt.Sprintf("[...%d non-text part(s) dropped for compaction seed budget - original turn ~%d tokens...]", nonText, origTokens)}}}
	}

	per := maxTurnTokens * 4 / textCount
	stub := &genai.Content{Role: t.Role}
	dropped := 0
	for _, p := range t.Parts {
		if p == nil {
			continue
		}
		if !isTextPart(p) {
			dropped++
			continue
		}
		cloned := *p
		cloned.Text = truncateTurnTextToBudget(cloned.Text, per)
		stub.Parts = append(stub.Parts, &cloned)
	}
	if dropped > 0 {
		stub.Parts = append(stub.Parts, &genai.Part{Text: fmt.Sprintf("[...%d non-text part(s) dropped for compaction seed budget...]", dropped)})
	}
	return stub
}

// truncateTurnTextToBudget is the adaptive-head/tail twin of
// truncatePersistTextPart: identical banner format, head and tail sized to
// half of budgetChars each at valid rune boundaries.
func truncateTurnTextToBudget(text string, budgetChars int) string {
	if len(text) <= budgetChars {
		return text
	}
	half := budgetChars / 2
	if half < 16 {
		return fmt.Sprintf("[...part dropped for compaction seed budget - original part was %d chars...]", len(text))
	}
	head := text[:half]
	for len(head) > 0 && !utf8.RuneStart(text[len(head)]) {
		head = head[:len(head)-1]
	}
	tailStart := len(text) - half
	for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	banner := fmt.Sprintf("\n[...truncated - original part was %d chars...]\n", len(text))
	return head + banner + text[tailStart:]
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
//
// toolDenials, when non-nil, is the counter a compaction-scoped agent's BeforeToolCallback
// increments per denied tool call (D50/CompactionToolDenials). CheckAndCompactSession owns
// its full lifecycle for this run: reset before the compaction runner executes, read once
// more for the post-compact hook payload. Pass nil for a no-tools compaction agent, where
// denials are structurally impossible.
//
// Hook lifecycle (pre-compact/post-compact/compact-failed): this function is the single
// choke point every compaction trigger - the natural token-threshold check, an explicit
// force from a CLI/RPC call, and a mid-turn short-circuit - routes through, so hooking here
// once covers all of them. pre-compact and compact-failed run synchronously and never alter
// or abort compaction (hook failures are logged as warnings only); post-compact runs
// asynchronously so a slow hook script never taxes the next turn.
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
// used to send directly to an *model.LLM (no Tools, system prompt glued into the
// message - the cache-prefix identity fix).
//
// cfgOverride, when non-nil, replaces the agent's COMPACT.md configuration without
// reading or modifying it on disk (D83). When nil, LoadCompactConfig(agentDir) is used.
//
// toolDenials, when non-nil, is the counter a compaction-scoped agent's BeforeToolCallback
// increments per denied tool call (D50/CompactionToolDenials). CheckAndCompactSession owns
// its full lifecycle for this run: reset before the compaction runner executes, read once
// more for the post-compact hook payload. Pass nil for a no-tools compaction agent, where
// denials are structurally impossible.
//
// Hook lifecycle (pre-compact/post-compact/compact-failed): this function is the single
// choke point every compaction trigger - the natural token-threshold check, an explicit
// force from a CLI/RPC call, and a mid-turn short-circuit - routes through, so hooking here
// once covers all of them. pre-compact and compact-failed run synchronously and never alter
// or abort compaction (hook failures are logged as warnings only); post-compact runs
// asynchronously so a slow hook script never taxes the next turn.
//
// This is the LOW-LEVEL single-backend entry. The runtime fallback chain is walked by
// CheckAndCompactSessionWithFallback; this wrapper keeps the pre-fallback API for direct
// callers (tests, the runtime-path override in CompactSession) by loading one level agent
// unconditionally.
func CheckAndCompactSession(ctx context.Context, agentDir string, runtimeCfg *RuntimeConfig, adkAgent agent.Agent, force bool, cfgOverride *CompactConfig, toolDenials *int64) (bool, error) {
	loadCompactionAgent := func(cfg *RuntimeConfig) (agent.Agent, *int64, error) {
		return adkAgent, toolDenials, nil
	}
	return CheckAndCompactSessionWithFallback(ctx, agentDir, runtimeCfg, loadCompactionAgent, force, cfgOverride)
}

func backendName(cfg *RuntimeConfig) string {
	if cfg == nil {
		return "<nil>"
	}
	return cfg.Endpoint + "/" + cfg.Model
}

// CheckAndCompactSessionWithFallback is CheckAndCompactSession with the runtime fallback
// chain walk: it builds the compaction agent per level from each level's cfg/model (mirroring
// runTurnWithRuntimeFallback's per-level rebuild for normal turns), descends on
// IsQualifyingFallbackError when NO summary text has been produced yet, skips backends
// marked failed-until-reset, records quota-reset hints, and reports the serving level's
// model in the post-compact hook payload.
//
// loadCompactionAgent(cfg) must return the deny-tools compaction agent (and its denial
// counter) for the given RuntimeConfig. The primary level is normally the caller's already-
// built fa.CompactionAgent; fallback levels call loadFolderAgentFromRuntime (or the caller's
// equivalent per-level loader) so the model constructor re-runs per provider.
func CheckAndCompactSessionWithFallback(ctx context.Context, agentDir string, runtimeCfg *RuntimeConfig, loadCompactionAgent func(cfg *RuntimeConfig) (agent.Agent, *int64, error), force bool, cfgOverride *CompactConfig) (bool, error) {
	agentID := filepath.Base(agentDir)
	sessionPath := filepath.Join(agentDir, SessionFileName)
	trigger := "forced"
	if !force {
		trigger = "auto"
	}
	fail := func(stage string, err error) (bool, error) {
		runCompactionHookSync(context.Background(), agentDir, EventCompactFailed, CompactFailedPayload{
			AgentID:     agentID,
			SessionPath: sessionPath,
			Stage:       stage,
			Error:       err.Error(),
		})
		return false, err
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		return fail("read-session", err)
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
			return fail("load-config", err)
		}
	}

	if !force {
		if runtimeCfg == nil || runtimeCfg.ContextWindow <= 0 {
			return false, nil
		}
		// Shares the ceiling with every other decision but keeps the raw session
		// estimate as the measure. The calibrated measure exists so the mid-turn stop
		// can face the provider's real ceiling; running it through this gate would
		// silently compact every session earlier, which is a policy change rather
		// than part of making the warning truthful.
		//
		// Note for any future caller passing force: false. This gate's ceiling moved
		// with the shared budget and is now stricter about what it lets through - on a
		// 500k window the trigger rose from 400,000 tokens to 485,952, because the
		// reserve and margin replace the flat 20% share. Production always sends
		// force: true, so nothing depends on this today.
		budget := contextBudget(runtimeCfg.ContextWindow, runtimeCfg.MaxOutputReserve, compactCfg.CompactOverheadPct)
		if EstimateTokens(turns, runtimeCfg.PreserveThinking) < budget {
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

	// We have now committed to compacting. Fire pre-compact before the compaction runner
	// executes (observe-only - its return value is discarded, so nothing here can veto or
	// alter what is about to happen). The denial counter for whichever level actually runs
	// is reset inside the chain loop below, when that level's agent is loaded.
	runCompactionHookSync(ctx, agentDir, EventPreCompact, PreCompactPayload{
		AgentID:       agentID,
		SessionPath:   sessionPath,
		TurnCount:     len(turns),
		TokenEstimate: totalTokens,
		Trigger:       trigger,
	})

	existingMemory, err := ReadMemoryFile(agentDir)
	if err != nil {
		return fail("read-memory", err)
	}

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

	// D101 P1.5: cap the disposable seed so a megabyte-scale archived turn
	// cannot blow the compaction runner itself up with a context-exceeded
	// provider error.
	if runtimeCfg != nil && runtimeCfg.ContextWindow > 0 {
		maxSeedTokens := int(float64(runtimeCfg.ContextWindow) * compactionSeedTokenFraction)
		seedContents = capSeedTokensForCompaction(seedContents, maxSeedTokens, preserveThinking)
	}

	sessionSvc := session.InMemoryService()
	createResp, err := sessionSvc.Create(ctx, &session.CreateRequest{
		AppName:   "wackypub",
		UserID:    "user",
		SessionID: compactSessionID,
	})
	if err != nil {
		return fail("create-session", fmt.Errorf("failed to create in-memory compaction session: %w", err))
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
			return fail("seed-session", fmt.Errorf("failed to seed compaction session: %w", err))
		}
	}

	// Walk the runtime fallback chain EXACTLY like a normal turn: attempt the primary
	// level first; on a qualifying error that arrives BEFORE any summary text was
	// produced, descend to the next level by rebuilding the compaction-scoped agent from
	// that level's config (the model constructor re-runs per provider, so a degraded
	// primary cannot block compaction the way it could pre-fallback).
	//
	// Mid-stream rule (mirrors the turn path): once ANY summary text has been produced at
	// a level, a later error is FATAL - descending after text would interleave two
	// providers' summaries into the memory file (frankenstein memory). Compaction writes
	// MEMORY.md exactly once, after the winning level completes.
	//
	// Backends marked failed-until-reset by a prior turn's 429 are skipped like normal
	// turns skip them, and a 429 carrying a quota-reset hint records the window for the
	// next attempt. The post-compact hook payload names the model of the level that
	// actually served the summary.
	var lastErr error
	var servedModel string
	var addendum string
	// toolDenials is the denial counter of the level that ACTUALLY ran; the post-compact
	// payload (below) reads it from the same pointer the deny callback incremented.
	var servedDenials *int64
	chain := []*RuntimeConfig{runtimeCfg}
	if runtimeCfg != nil {
		chain = runtimeCfg.FallbackChain()
	}
	directive := genai.NewContentFromText(compactCfg.Prompt, "user")

	for level, levelCfg := range chain {
		if backendFailedUntilReset(levelCfg) {
			fmt.Fprintf(os.Stderr, "Warning: skipping backend %s for compaction: usage limit not yet reset\n", backendName(levelCfg))
			lastErr = fmt.Errorf("backend %s skipped: usage limit not yet reset", backendName(levelCfg))
			continue
		}

		compactionAgent, denials, err := loadCompactionAgent(levelCfg)
		if err != nil {
			lastErr = fmt.Errorf("failed to load compaction agent for %s: %w", backendName(levelCfg), err)
			if level+1 < len(chain) {
				fmt.Fprintf(os.Stderr, "Warning: backend %s load failed: %v; falling back\n", backendName(levelCfg), err)
				continue
			}
			return fail("load-agent", lastErr)
		}
		if denials != nil {
			atomic.StoreInt64(denials, 0)
		}

		r, err := runner.New(runner.Config{
			AppName:        "wackypub",
			Agent:          compactionAgent,
			SessionService: sessionSvc,
		})
		if err != nil {
			lastErr = fmt.Errorf("failed to create compaction runner for %s: %w", backendName(levelCfg), err)
			if level+1 < len(chain) {
				fmt.Fprintf(os.Stderr, "Warning: compaction runner for %s failed: %v; falling back\n", backendName(levelCfg), err)
				continue
			}
			return fail("create-runner", lastErr)
		}

		var levelAddendum string
		for event, err := range r.Run(ctx, "user", compactSessionID, directive, agent.RunConfig{}) {
			if err != nil {
				lastErr = err
				if resetAt, ok := ParseQuotaResetHint(err); ok {
					markBackendFailedUntil(levelCfg, resetAt)
				}
				if levelAddendum == "" && IsQualifyingFallbackError(err) && level+1 < len(chain) {
					// Nothing emitted at this level: re-run the summary from scratch on the
					// next backend is clean - no frankenstein memory risk.

					fmt.Fprintf(os.Stderr, "Warning: compaction backend %s failed: %v; falling back to %s\n", backendName(levelCfg), err, backendName(chain[level+1]))
					break
				}
				// Text already produced (or non-qualifying, or chain exhausted): fatal.
				return fail("generation", fmt.Errorf("LLM compaction generation failed on %s: %w", backendName(levelCfg), err))
			}
			if event != nil {
				if text := ExtractTextFromEvent(event); text != "" {
					levelAddendum = text
				}
			}
		}
		if ctx.Err() != nil {
			return fail("generation", ctx.Err())
		}
		if levelAddendum != "" {
			// This level succeeded; it is the one whose model we report.
			addendum = levelAddendum
			if levelCfg != nil {
				servedModel = levelCfg.Model
			}
			servedDenials = denials
			break
		}
		// levelAddendum == "" without an error: the level produced nothing (empty response).
		// Treat as a qualifying failure ONLY if a fallback exists; otherwise fall through.
		if level+1 == len(chain) {
			return fail("generation", fmt.Errorf("compaction backend %s returned an empty summary", backendName(levelCfg)))
		}
	}
	if addendum == "" && lastErr != nil {
		return fail("generation", fmt.Errorf("LLM compaction generation failed: %w", lastErr))
	}
	if addendum == "" && servedModel == "" {
		return fail("generation", fmt.Errorf("no runtime backend produced a compaction summary"))
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
			return fail("write-memory", fmt.Errorf("failed to update MEMORY.md: %w", err))
		}

		wsDir := filepath.Dir(agentDir)
		commitEventBestEffort(wsDir, agentID, "compact (memory)")
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
	existing, _ := ReadPersistedTurns(agentDir)
	numCompacted := len(existing) - len(remainingTurns)
	if numCompacted < 0 {
		numCompacted = 0
	}

	var survivingTurns []PersistedTurn
	for i, t := range remainingTurns {
		if t == nil {
			continue
		}
		var seq int64
		idx := numCompacted + i
		if idx >= 0 && idx < len(existing) && existing[idx].Seq > 0 {
			seq = existing[idx].Seq
		}
		survivingTurns = append(survivingTurns, PersistedTurn{
			Content: genai.Content{Role: t.Role, Parts: t.Parts},
			Seq:     seq,
		})
	}

	var baselineSeq int64
	if len(survivingTurns) > 0 {
		baselineSeq = survivingTurns[0].Seq
	}

	summarySeq, err := NextSeq(agentDir)
	if err != nil {
		return fail("next-seq", fmt.Errorf("allocating sequence number for compaction: %w", err))
	}
	if baselineSeq == 0 {
		baselineSeq = summarySeq
	}

	var pTurns []PersistedTurn
	if len(remainingTurns) > 0 && notice != "" {
		noticeTurn := genai.NewContentFromText(FormatCompactionNotice(notice), "user")
		pTurns = append(pTurns, PersistedTurn{
			Content: genai.Content{Role: "user", Parts: noticeTurn.Parts},
			Seq:     summarySeq,
		})
	}

	pTurns = append(pTurns, survivingTurns...)

	if err := WritePersistedTurns(agentDir, pTurns); err != nil {
		return fail("write-session", fmt.Errorf("failed to update session.jsonl after compaction: %w", err))
	}

	wsDir := filepath.Dir(agentDir)
	commitEventBestEffort(wsDir, agentID, "compact")
	if err := InvalidateLastUsage(agentDir); err != nil {
		// A stale LastUsageRecord after compaction would mislead the next turn's usage-based
		// compaction decision (it describes the pre-compaction session), so log it loudly.
		fmt.Fprintf(os.Stderr, "Warning: failed to invalidate usage record after compaction for agent %s: %v\n", agentID, err)
	}

	// Report the model that ACTUALLY served the summary: the fallback-level model once the
	// chain walked, not always the primary (was runtimeCfg.Model pre-fallback).
	compactionModel := servedModel
	var denials int64
	if servedDenials != nil {
		denials = atomic.LoadInt64(servedDenials)
	}
	runPostCompactHookAsync(agentDir, PostCompactPayload{
		AgentID:         agentID,
		SessionPath:     sessionPath,
		TurnsArchived:   len(compactTurns),
		TokensBefore:    totalTokens,
		TokensAfter:     EstimateTokens(remainingTurns, preserveThinking),
		CompactionModel: compactionModel,
		ToolDenials:     denials,
	})

	return true, nil
}
