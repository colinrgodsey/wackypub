package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The spawn family (workshop-agent-spawned-persistent-subagents-layout-auth, spec
// impl-agent-spawn-command-family-c-with-changes-v1). A child is an ordinary
// workspace-level agent created from a runtime preset with recorded lineage - not a
// nested workspace. Nothing here enforces tiers or quotas; v1 records them.

const (
	// SpawnMetaFile is the lineage source of truth, written inside the child
	// directory. MANIFEST.md is generated from these files and is never authoritative.
	SpawnMetaFile = "SPAWN_META"

	// RuntimesDirName is the workspace-level folder of named runtime presets.
	RuntimesDirName = "runtimes"

	// MaxSpawnDepth is enforced by force: a child cannot spawn a child.
	MaxSpawnDepth = 1

	SpawnStatusActive  = "active"
	SpawnStatusPaused  = "paused"
	SpawnStatusRetired = "retired"
)

// SpawnMeta records why a child exists. Written by SpawnAgent, read by the manifest
// generator and the workspace listing.
type SpawnMeta struct {
	Child     string `yaml:"child"`
	Parent    string `yaml:"parent,omitempty"`
	Depth     int    `yaml:"depth"`
	Runtime   string `yaml:"runtime"`
	Tier      string `yaml:"tier,omitempty"`
	Status    string `yaml:"status"`
	Orphaned  bool   `yaml:"orphaned,omitempty"`
	CreatedAt string `yaml:"created_at"`
}

func spawnMetaPath(agentDir string) string { return filepath.Join(agentDir, SpawnMetaFile) }

// LoadSpawnMeta returns nil without error when the directory carries no lineage,
// which is the normal case for a peer agent.
func LoadSpawnMeta(agentDir string) (*SpawnMeta, error) {
	raw, err := os.ReadFile(spawnMetaPath(agentDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", SpawnMetaFile, err)
	}
	var meta SpawnMeta
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", SpawnMetaFile, err)
	}
	return &meta, nil
}

func WriteSpawnMeta(agentDir string, meta *SpawnMeta) error {
	if meta == nil {
		return fmt.Errorf("spawn meta cannot be nil")
	}
	raw, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to serialize %s: %w", SpawnMetaFile, err)
	}
	return os.WriteFile(spawnMetaPath(agentDir), raw, 0o644)
}

// ListRuntimePresets returns the preset names available to a workspace, sorted.
// A workspace without a runtimes/ folder has no presets, and therefore no
// spawnable children - that is the spend gate, not a missing feature.
func ListRuntimePresets(wsDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(wsDir, RuntimesDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// LoadRuntimePreset resolves a preset by name only. The name is confined to the
// workspace runtimes folder, so an operator cannot steer a child at an endpoint that
// was never approved - the hole is the endpoint, not the model name.
func LoadRuntimePreset(wsDir, name string) (string, *RuntimeConfig, error) {
	if name == "" {
		return "", nil, fmt.Errorf("a runtime preset name is required")
	}
	if strings.ContainsAny(name, "/\\\\") || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return "", nil, fmt.Errorf("runtime preset name %q must be a plain name, not a path", name)
	}

	presets, err := ListRuntimePresets(wsDir)
	if err != nil {
		return "", nil, err
	}
	presetPath := filepath.Join(wsDir, RuntimesDirName, name+".json")
	if _, err := os.Stat(presetPath); err != nil {
		if os.IsNotExist(err) {
			if len(presets) == 0 {
				return "", nil, fmt.Errorf("runtime preset %q not found: workspace %s has no %s/ folder", name, wsDir, RuntimesDirName)
			}
			return "", nil, fmt.Errorf("runtime preset %q not found; available presets: %s", name, strings.Join(presets, ", "))
		}
		return "", nil, err
	}

	cfg, err := LoadRuntimeConfigFile(presetPath)
	if err != nil {
		return "", nil, fmt.Errorf("runtime preset %q is not a usable runtime config: %w", name, err)
	}
	if strings.TrimSpace(cfg.Provider) == "" || strings.TrimSpace(cfg.Model) == "" || strings.TrimSpace(cfg.Endpoint) == "" {
		return "", nil, fmt.Errorf("runtime preset %q must set provider, model and endpoint", name)
	}
	return presetPath, cfg, nil
}

// SpawnOptions carries a spawn request. ParentID is required: children exist because
// a parent asked for help, and the lineage has to name who asked.
type SpawnOptions struct {
	WorkspaceDir string
	ChildID      string
	ParentID     string
	Preset       string
	Tier         string
}

// SpawnResult reports what was created so the caller can show it rather than re-walk
// the tree.
type SpawnResult struct {
	ChildDir       string
	PresetPath     string
	Depth          int
	FilesWritten   []string
	InheritedTools bool
}

func validateNewAgentID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("child agent id cannot be empty")
	}
	if id != filepath.Base(id) || id == "." || id == ".." || strings.ContainsAny(id, "/\\\\") {
		return fmt.Errorf("child agent id %q must be a single path segment", id)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("child agent id %q cannot start with a dot (reserved for workspace machinery)", id)
	}
	return nil
}

// childAgentsMD is deliberately self-contained: every macro it references is a file
// SpawnAgent writes in the same call. digestor carried a broken @BRAIN.md reference
// for 159 turns because nothing checked after the hand-copy.
func childAgentsMD(childID, parentID, preset string) string {
	return "# " + childID + "\n\n" +
		"Persistent helper agent, spawned for `" + parentID + "` from runtime preset `" + preset + "`.\n\n" +
		"@IDENTITY.md\n\n" +
		"## Working rules\n\n" +
		"- Stay inside the task your parent handed you; you have no roster of your own beyond your parent.\n" +
		"- Keep `WORKBOOK.md` current: it is the only thing that carries your state between turns.\n" +
		"- Do not spawn agents. Children of a helper are refused by the workspace.\n\n" +
		"## Memory\n\n@MEMORY.md\n\n@WORKBOOK.md\n"
}

func childIdentityMD(childID, parentID string) string {
	return "# Identity\n\n" +
		"- Name: " + childID + "\n" +
		"- Role: persistent helper for " + parentID + "\n" +
		"- Parent: " + parentID + "\n" +
		"- Spawned by a coordinator running `wackypub agent spawn`.\n"
}

// SpawnAgent creates a thin child agent from a preset, validates it, and refuses
// anything that would recreate the two known failure modes: a hand-copied scaffold
// with a dangling macro, and a child that is really a nested workspace (the E8 shape).
//
// It writes only inside <wsDir>/<childID>. Granting the parent a roster entry for the
// child is deliberately a separate step (GrantSpawnedChild), so a caller that may
// create children does not also get to rewrite another agent's allowlist implicitly.
func SpawnAgent(opts SpawnOptions) (*SpawnResult, error) {
	childDir := filepath.Join(opts.WorkspaceDir, opts.ChildID)

	// The E8 refusal, before anything is written: a child must live in the workspace
	// this process operates on. Aiming spawn at a scratch workspace that merely happens
	// to be nested inside the real one recreates the bypass shape closed in #151.
	if active := ActiveWorkspaceDir(); active != "" {
		activeInfo, activeErr := os.Stat(active)
		wsInfo, wsErr := os.Stat(opts.WorkspaceDir)
		if activeErr != nil || wsErr != nil || !os.SameFile(activeInfo, wsInfo) {
			return nil, fmt.Errorf("refused: spawn targets workspace %q but this process operates on %q; a spawned agent is a directory in the workspace being operated on, never a nested workspace", opts.WorkspaceDir, active)
		}
	}
	if err := validateNewAgentID(opts.ChildID); err != nil {
		return nil, err
	}
	if err := validateNewAgentID(opts.ParentID); err != nil {
		return nil, fmt.Errorf("invalid parent: %w", err)
	}
	if opts.WorkspaceDir == "" {
		return nil, fmt.Errorf("a workspace directory is required")
	}
	if opts.ChildID == opts.ParentID {
		return nil, fmt.Errorf("an agent cannot spawn itself")
	}

	parentDir := filepath.Join(opts.WorkspaceDir, opts.ParentID)
	if !looksLikeAgentDir(parentDir) {
		return nil, fmt.Errorf("parent agent %q does not exist in workspace %s", opts.ParentID, opts.WorkspaceDir)
	}

	parentMeta, err := LoadSpawnMeta(parentDir)
	if err != nil {
		return nil, err
	}
	depth := 1
	if parentMeta != nil {
		depth = parentMeta.Depth + 1
	}
	if depth > MaxSpawnDepth {
		return nil, fmt.Errorf("refused: %s is itself a spawned child at depth %d and the workspace caps spawning at depth %d", opts.ParentID, parentMeta.Depth, MaxSpawnDepth)
	}

	presetPath, _, err := LoadRuntimePreset(opts.WorkspaceDir, opts.Preset)
	if err != nil {
		return nil, err
	}

	if pathExists(filepath.Join(childDir, RootMarkerFile)) {
		return nil, fmt.Errorf("refused: %s already contains a %s marker; a spawned agent never carries its own workspace root", childDir, RootMarkerFile)
	}
	if _, err := os.Stat(childDir); err == nil {
		return nil, fmt.Errorf("agent %q already exists at %s", opts.ChildID, childDir)
	}
	if err := os.Mkdir(childDir, 0o755); err != nil {
		return nil, err
	}

	files := map[string]string{
		"AGENTS.md":   childAgentsMD(opts.ChildID, opts.ParentID, opts.Preset),
		"IDENTITY.md": childIdentityMD(opts.ChildID, opts.ParentID),
		"MEMORY.md":   "# MEMORY.md\n\nNothing yet. This file is written by compaction or by " + opts.ChildID + " itself.\n",
		"WORKBOOK.md": "# WORKBOOK.md\n\n## Active\n\n- Spawned for " + opts.ParentID + ". Awaiting first task.\n",
	}

	written := make([]string, 0, len(files)+2)
	rollback := func() {
		for _, name := range written {
			_ = os.Remove(filepath.Join(childDir, name))
		}
		_ = os.Remove(childDir)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(childDir, name), []byte(content), 0o644); err != nil {
			rollback()
			return nil, err
		}
		written = append(written, name)
	}

	presetBytes, err := os.ReadFile(presetPath)
	if err != nil {
		rollback()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(childDir, "runtime.json"), presetBytes, 0o644); err != nil {
		rollback()
		return nil, err
	}
	written = append(written, "runtime.json")

	// Children start closed: the only entry is the parent that asked for them.
	if err := os.WriteFile(filepath.Join(childDir, AllowedAgentsFile), []byte(opts.ParentID+"\n"), 0o644); err != nil {
		rollback()
		return nil, err
	}
	written = append(written, AllowedAgentsFile)

	// Tools and skills are inherited by reference, never copied: point the child at the
	// workspace-wide toolset when one exists.
	inherited := false
	if toolsets := filepath.Join(opts.WorkspaceDir, "toolsets", opts.ChildID); pathExists(toolsets) {
		if err := os.Symlink(toolsets, filepath.Join(childDir, ToolsDirName)); err == nil {
			written = append(written, ToolsDirName)
			inherited = true
		}
	}

	meta := &SpawnMeta{
		Child:     opts.ChildID,
		Parent:    opts.ParentID,
		Depth:     depth,
		Runtime:   opts.Preset,
		Tier:      opts.Tier,
		Status:    SpawnStatusActive,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteSpawnMeta(childDir, meta); err != nil {
		rollback()
		return nil, err
	}
	written = append(written, SpawnMetaFile)

	if problems := validateChildScaffold(opts.WorkspaceDir, opts.ChildID); len(problems) > 0 {
		rollback()
		return nil, fmt.Errorf("refused: the spawned scaffold did not validate: %s", strings.Join(problems, "; "))
	}

	return &SpawnResult{
		ChildDir:       childDir,
		PresetPath:     presetPath,
		Depth:          depth,
		FilesWritten:   written,
		InheritedTools: inherited,
	}, nil
}

// missingMacroTargets returns the @FILE references in content that resolve to nothing, in
// either the agent directory or the workspace root - the two places ExpandMacros reads
// from. ExpandMacros leaves an unresolvable reference as literal text rather than failing,
// which is exactly why digestor ran for 159 turns with a broken prompt.
func missingMacroTargets(content, agentDir string) []string {
	wsDir := FindWorkspaceRootDir(agentDir)
	seen := map[string]bool{}
	var missing []string
	for _, m := range macroRegex.FindAllStringSubmatch(content, -1) {
		target := m[3]
		if target == "" || strings.HasPrefix(target, "@") || seen[target] {
			continue
		}
		seen[target] = true
		if pathExists(filepath.Join(agentDir, target)) {
			continue
		}
		if wsDir != "" && pathExists(filepath.Join(wsDir, target)) {
			continue
		}
		missing = append(missing, target)
	}
	return missing
}

// validateChildScaffold is the digestor defect class turned into a check: a scaffold
// whose macros dangle is a broken prompt that nothing notices until it has run for
// weeks. It also re-checks the nested-root refusal after writing, since a marker is
// the one file whose presence changes authorization rather than output.
func validateChildScaffold(wsDir, childID string) []string {
	childDir := filepath.Join(wsDir, childID)
	var problems []string

	if pathExists(filepath.Join(childDir, RootMarkerFile)) {
		problems = append(problems, fmt.Sprintf("child directory contains a %s marker", RootMarkerFile))
	}

	raw, err := os.ReadFile(filepath.Join(childDir, "AGENTS.md"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("AGENTS.md unreadable: %v", err))
	} else if _, err := ExpandMacros(string(raw), childDir); err != nil {
		problems = append(problems, fmt.Sprintf("AGENTS.md macros do not resolve: %v", err))
	} else if missing := missingMacroTargets(string(raw), childDir); len(missing) > 0 {
		problems = append(problems, "AGENTS.md references files that do not exist: "+strings.Join(missing, ", "))
	}

	if _, err := LoadRuntimeConfigFile(filepath.Join(childDir, "runtime.json")); err != nil {
		problems = append(problems, fmt.Sprintf("runtime.json invalid: %v", err))
	}

	inspection, err := InspectAgentDir(wsDir, childID)
	if err != nil {
		problems = append(problems, fmt.Sprintf("agent directory cannot be inspected: %v", err))
	} else if !inspection.AgentDirExists {
		problems = append(problems, "agent directory does not exist")
	}

	return problems
}

// GrantSpawnedChild appends the child to the parent's allowlist, idempotently, so the
// pair can address each other. Kept separate from SpawnAgent because rewriting another
// agent's roster is a distinct authority from creating a directory.
func GrantSpawnedChild(wsDir, parentID, childID string) (bool, error) {
	if err := validateNewAgentID(parentID); err != nil {
		return false, err
	}
	if err := validateNewAgentID(childID); err != nil {
		return false, err
	}
	rosterPath := filepath.Join(wsDir, parentID, AllowedAgentsFile)

	var entries []string
	if raw, err := os.ReadFile(rosterPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				entries = append(entries, line)
			}
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	for _, e := range entries {
		if e == childID {
			return false, nil
		}
	}
	entries = append(entries, childID)
	return true, os.WriteFile(rosterPath, []byte(strings.Join(entries, "\n")+"\n"), 0o644)
}
