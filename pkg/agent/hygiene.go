package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// alwaysOnBudgetFraction is the fraction of the model's context window the always-on
// prefix (AGENTS.md after macro expansion, plus always_load:true skill bodies, plus
// MEMORY.md) is allowed to occupy before ValidateAgentHygiene warns. 15% matches the
// rubric harness2's always_on_max check uses: the always-on prefix is paid on every
// single turn regardless of what the turn is about, so it is the one context cost that
// compounds silently as AGENTS.md/MEMORY.md/autoloaded skills grow.
const alwaysOnBudgetFraction = 0.15

// bytesPerTokenEstimate is a rough, deliberately conservative English-text heuristic
// (~4 bytes/token) used only to turn a byte count into a token-budget comparison. It is
// an estimate for a warning threshold, not a token counter - getting it exactly right
// is not the point, catching an always-on prefix that has grown an order of magnitude
// too large is.
const bytesPerTokenEstimate = 4

// ValidateAgentHygiene runs deterministic, read-only checks over an agent's on-disk
// setup beyond what InspectAgent's file-presence inventory already covers, returning
// human-readable issue lines in the same style as printAgentInspection's existing
// issues list. It never creates, modifies, or deletes anything. Deliberately general -
// it does not name or special-case any specific external tool's own config file
// convention (see tasks/wackypub/workspace-warn-files-rw-missing-acl, superseded:
// hardcoding a specific companion tool's ACL file into wackypub core was explicitly
// rejected as unwanted coupling; the checks here apply to every agent uniformly using
// only wackypub's own, already-canonical mechanisms - skill frontmatter, the @file
// macro, and runtime.json's contextWindow).
//
// Checks:
//  1. Skill name/directory parity: a skill whose frontmatter `name:` is set but differs
//     from its own directory name is confusing to resolve (load_skill, the autoload
//     block, and shadow-detection all key off whichever name wins) and is almost always
//     a copy/rename mistake rather than an intentional alias.
//  2. Dead @file macro references in AGENTS.md (D91): ExpandMacros deliberately leaves
//     a missing target as literal `@text` rather than erroring (so one typo'd reference
//     doesn't fail an entire turn) - which also means nothing today ever tells an
//     operator the reference is dead. This re-walks the same regex/resolution rules to
//     surface exactly the ones that will never expand, as a hygiene finding rather than
//     a silent no-op.
//  3. Always-on context budget: AGENTS.md (after macro expansion) + the autoloaded-skills
//     block + MEMORY.md, as a byte-based token estimate, against alwaysOnBudgetFraction
//     of the agent's effective runtime.json contextWindow.
func ValidateAgentHygiene(wsDir, agentID string) []string {
	agentDir := filepath.Join(wsDir, agentID)
	var issues []string

	issues = append(issues, validateSkillNameParity(agentDir)...)
	issues = append(issues, validateMacroReferences(agentDir)...)

	if rendered, err := RenderAgentSystemPrompt(wsDir, agentID); err == nil {
		issues = append(issues, validateAlwaysOnBudget(agentDir, rendered)...)
	}
	// A render error here (an existing macro target that failed to read, a symlink
	// loop, depth overflow) is the same failure handleContinuationOrCompaction's
	// caller would hit mid-turn - but it's rare enough, and specific enough once it
	// happens, that duplicating its exact wording here would drift from the real
	// error message over time. Skipping the budget estimate in that case (rather than
	// estimating against partial/truncated content) is the honest choice; the
	// dead-reference check above already covers the common case this would otherwise
	// exist to catch.

	return issues
}

// validateMacroReferences re-derives ExpandMacros' own @file resolution rules (same
// regex, same workspace-containment check, same existence/symlink checks) against
// AGENTS.md's raw content, and reports every reference that will never expand -
// without duplicating ExpandMacros itself, since that function intentionally
// degrades a dead reference to literal text rather than surfacing it.
func validateMacroReferences(agentDir string) []string {
	data, err := os.ReadFile(filepath.Join(agentDir, "AGENTS.md"))
	if err != nil {
		return nil
	}

	wsDir := FindWorkspaceRootDir(agentDir)
	realWsDir := wsDir
	if evaluated, evalErr := filepath.EvalSymlinks(wsDir); evalErr == nil {
		realWsDir = evaluated
	}

	var issues []string
	seen := make(map[string]bool)
	for _, m := range macroRegex.FindAllStringSubmatch(string(data), -1) {
		if len(m) < 4 {
			continue
		}
		escape, target := m[2], m[3]
		if escape == "\\" || escape == "@" || seen[target] {
			continue
		}

		cleanPath := filepath.Clean(filepath.Join(agentDir, target))
		if !isContained(wsDir, cleanPath) {
			continue // out-of-bounds references are ExpandMacros' own security boundary, not a hygiene finding
		}

		fi, statErr := os.Stat(cleanPath)
		switch {
		case statErr != nil:
			seen[target] = true
			issues = append(issues, fmt.Sprintf("AGENTS.md references @%s, which does not exist - ExpandMacros leaves it as literal text rather than failing, so this is silent today", target))
		case fi.IsDir():
			seen[target] = true
			issues = append(issues, fmt.Sprintf("AGENTS.md references @%s, which is a directory, not a file - ExpandMacros leaves it as literal text rather than failing, so this is silent today", target))
		default:
			realPath, evalErr := filepath.EvalSymlinks(cleanPath)
			switch {
			case evalErr != nil:
				seen[target] = true
				issues = append(issues, fmt.Sprintf("AGENTS.md references @%s, a broken symlink - ExpandMacros leaves it as literal text rather than failing, so this is silent today", target))
			case !isContained(wsDir, realPath) && !isContained(realWsDir, realPath):
				// resolves outside the workspace - ExpandMacros' own security boundary, not a hygiene finding
			}
		}
	}
	return issues
}

// validateSkillNameParity flags a skill whose frontmatter name differs from its own
// directory name. ParseSkillFile falls back to the directory name only when frontmatter
// omits `name:`, so any mismatch here was set explicitly - never an artifact of the
// fallback itself.
func validateSkillNameParity(agentDir string) []string {
	skillsMap, _, _, _, err := DiscoverAgentSkillsMap(agentDir)
	if err != nil || len(skillsMap) == 0 {
		return nil
	}

	var issues []string
	for name, sk := range skillsMap {
		if sk == nil || sk.Path == "" {
			continue
		}
		dirName := filepath.Base(filepath.Dir(sk.Path))
		if name != dirName {
			issues = append(issues, fmt.Sprintf(
				"skill directory %q has frontmatter name %q - load_skill, autoload, and shadow detection all key off %q, not the directory, which is almost always a copy/rename mistake",
				dirName, name, name,
			))
		}
	}
	return issues
}

// validateAlwaysOnBudget estimates the always-on prefix's token cost (rendered AGENTS.md,
// already including the autoloaded-skills block per RenderAgentSystemPrompt, plus
// MEMORY.md) against alwaysOnBudgetFraction of the agent's effective contextWindow.
// Best-effort: a MEMORY.md read failure or an unloadable runtime.json just narrows what
// gets measured rather than failing the whole hygiene check, since this is advisory, not
// something that should ever block diagnosing the rest of an agent's setup.
func validateAlwaysOnBudget(agentDir string, renderedAgentsAndSkills string) []string {
	totalBytes := len(renderedAgentsAndSkills)
	// MEMORY.md not existing yet is normal (a brand-new agent), not a failure worth
	// narrowing the estimate's honesty over - any other read error (permissions, a
	// directory where a file should be) just leaves it out of the total rather than
	// failing the whole hygiene check.
	if memData, err := os.ReadFile(filepath.Join(agentDir, "MEMORY.md")); err == nil {
		totalBytes += len(memData)
	}

	cfg, err := LoadRuntimeConfig(agentDir)
	if err != nil || cfg == nil || cfg.ContextWindow <= 0 {
		return nil
	}

	estimatedTokens := totalBytes / bytesPerTokenEstimate
	thresholdTokens := int(float64(cfg.ContextWindow) * alwaysOnBudgetFraction)
	if estimatedTokens <= thresholdTokens {
		return nil
	}

	return []string{fmt.Sprintf(
		"always-on prefix (AGENTS.md + autoloaded skills + MEMORY.md) is an estimated ~%d tokens, over %.0f%% of the %d-token contextWindow (~%d tokens) - this is paid on every single turn regardless of what the turn is about",
		estimatedTokens, alwaysOnBudgetFraction*100, cfg.ContextWindow, thresholdTokens,
	)}
}
