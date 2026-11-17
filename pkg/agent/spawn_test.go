package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const spawnTestPreset = "fast"

func resetActiveWorkspaceForSpawn(t *testing.T) {
	t.Helper()
	SetActiveWorkspaceDir("")
	t.Cleanup(func() { SetActiveWorkspaceDir("") })
}

func writePreset(t *testing.T, wsDir, name string, cfg RuntimeConfig) string {
	t.Helper()
	runtimes := filepath.Join(wsDir, RuntimesDirName)
	if err := os.MkdirAll(runtimes, 0o755); err != nil {
		t.Fatalf("mkdir runtimes: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal preset: %v", err)
	}
	path := filepath.Join(runtimes, name+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write preset: %v", err)
	}
	return path
}

func spawnFixture(t *testing.T) string {
	t.Helper()
	wsDir := t.TempDir()

	parent := filepath.Join(wsDir, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte("# Parent\n"), 0o644); err != nil {
		t.Fatalf("write parent AGENTS.md: %v", err)
	}

	writePreset(t, wsDir, spawnTestPreset, RuntimeConfig{
		Provider: "openai",
		Endpoint: "http://127.0.0.1:9/v1",
		Model:    "preset-model",
	})
	return wsDir
}

func TestSpawnAgentCreatesThinValidatedChild(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)
	SetActiveWorkspaceDir(wsDir)

	result, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: "helper", ParentID: "parent", Preset: spawnTestPreset, Tier: "2"})
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	if result.Depth != 1 {
		t.Errorf("depth = %d, want 1", result.Depth)
	}

	childDir := filepath.Join(wsDir, "helper")
	for _, name := range []string{"AGENTS.md", "IDENTITY.md", "MEMORY.md", "WORKBOOK.md", "runtime.json", AllowedAgentsFile, SpawnMetaFile} {
		if _, err := os.Stat(filepath.Join(childDir, name)); err != nil {
			t.Errorf("expected %s to be written: %v", name, err)
		}
	}

	roster, err := os.ReadFile(filepath.Join(childDir, AllowedAgentsFile))
	if err != nil {
		t.Fatalf("read child roster: %v", err)
	}
	if strings.TrimSpace(string(roster)) != "parent" {
		t.Errorf("child roster = %q, want only the parent", string(roster))
	}

	meta, err := LoadSpawnMeta(childDir)
	if err != nil || meta == nil {
		t.Fatalf("LoadSpawnMeta: %v (meta=%v)", err, meta)
	}
	if meta.Parent != "parent" || meta.Depth != 1 || meta.Runtime != spawnTestPreset || meta.Tier != "2" || meta.Status != SpawnStatusActive {
		t.Errorf("unexpected lineage: %+v", meta)
	}

	// The scaffold must be usable, not merely present: every macro has to resolve.
	if problems := validateChildScaffold(wsDir, "helper"); len(problems) > 0 {
		t.Errorf("scaffold did not validate: %v", problems)
	}
}

func TestSpawnAgentUnknownPresetListsWhatExists(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)

	_, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: "helper", ParentID: "parent", Preset: "nope"})
	if err == nil {
		t.Fatal("expected an unknown preset to be refused")
	}
	if !strings.Contains(err.Error(), spawnTestPreset) {
		t.Errorf("expected the error to list available presets, got: %v", err)
	}
}

// The spend gate is the endpoint, so a preset must be a name, never a path or URL.
func TestSpawnAgentRejectsFreeTextRuntime(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)

	for _, name := range []string{"http://evil.example/v1", "../../etc/passwd", "../outside", "/abs/path"} {
		if _, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: "helper", ParentID: "parent", Preset: name}); err == nil {
			t.Errorf("runtime %q was accepted", name)
		}
	}
}

func TestSpawnAgentEnforcesDepthOne(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)

	helper := filepath.Join(wsDir, "helper")
	if err := os.MkdirAll(helper, 0o755); err != nil {
		t.Fatalf("mkdir helper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(helper, "AGENTS.md"), []byte("# Helper\n"), 0o644); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	if err := WriteSpawnMeta(helper, &SpawnMeta{Child: "helper", Parent: "parent", Depth: 1, Runtime: spawnTestPreset, Status: SpawnStatusActive}); err != nil {
		t.Fatalf("write helper meta: %v", err)
	}

	_, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: "grandchild", ParentID: "helper", Preset: spawnTestPreset})
	if err == nil {
		t.Fatal("expected a spawned child to be refused as a parent")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("expected the refusal to cite the depth cap, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(wsDir, "grandchild")); statErr == nil {
		t.Error("a refused spawn must leave no directory behind")
	}
}

// The E8 shape: a scratch workspace nested inside the real one must not be able to
// pass as the workspace a spawn operates on.
func TestSpawnAgentRefusesWorkspaceThatIsNotTheActiveOne(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)

	// A second workspace, complete with its own parent and preset: the only thing wrong
	// with it is that this process operates somewhere else.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, RootMarkerFile), nil, 0o644); err != nil {
		t.Fatalf("write other marker: %v", err)
	}
	for _, dir := range []string{"parent", RuntimesDirName} {
		if err := os.MkdirAll(filepath.Join(other, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(other, "parent", "AGENTS.md"), []byte("# Parent\n"), 0o644); err != nil {
		t.Fatalf("write other parent: %v", err)
	}
	writePreset(t, other, spawnTestPreset, RuntimeConfig{Provider: "openai", Endpoint: "http://127.0.0.1:9/v1", Model: "preset-model"})

	SetActiveWorkspaceDir(wsDir)
	_, err := SpawnAgent(SpawnOptions{WorkspaceDir: other, ChildID: "helper", ParentID: "parent", Preset: spawnTestPreset})
	if err == nil {
		t.Fatal("expected a workspace that is not the active one to be refused")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("expected the refusal to name the nesting, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(other, "helper")); statErr == nil {
		t.Error("a refused spawn must leave no directory behind")
	}

	// Same directory, genuinely the workspace in use: the refusal was about the
	// mismatch, not about markers existing at all.
	SetActiveWorkspaceDir(other)
	if _, err := SpawnAgent(SpawnOptions{WorkspaceDir: other, ChildID: "helper", ParentID: "parent", Preset: spawnTestPreset}); err != nil {
		t.Errorf("spawning inside the workspace actually in use should succeed, got: %v", err)
	}
}

func TestSpawnAgentRefusesChildCarryingAWorkspaceRoot(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)

	childDir := filepath.Join(wsDir, "helper")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, RootMarkerFile), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	_, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: "helper", ParentID: "parent", Preset: spawnTestPreset})
	if err == nil || !strings.Contains(err.Error(), RootMarkerFile) {
		t.Fatalf("expected a refusal naming %s, got: %v", RootMarkerFile, err)
	}
}

func TestSpawnAgentRejectsBadChildIDs(t *testing.T) {
	wsDir := spawnFixture(t)
	resetActiveWorkspaceForSpawn(t)

	for _, id := range []string{"", "a/b", "..", ".hidden", "parent"} {
		if _, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: id, ParentID: "parent", Preset: spawnTestPreset}); err == nil {
			t.Errorf("child id %q was accepted", id)
		}
	}
}

// The digestor defect class, tested head-on: a scaffold whose macro dangles must be
// reported rather than silently rendered as literal text in a prompt.
func TestValidateChildScaffoldReportsDanglingMacro(t *testing.T) {
	wsDir := t.TempDir()
	dir := filepath.Join(wsDir, "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Broken\n\n@BRAIN.md\n"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	problems := validateChildScaffold(wsDir, "broken")
	var found bool
	for _, p := range problems {
		if strings.Contains(p, "BRAIN.md") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the dangling @BRAIN.md to be reported, got: %v", problems)
	}
}

func TestGrantSpawnedChildIsIdempotent(t *testing.T) {
	wsDir := spawnFixture(t)

	changed, err := GrantSpawnedChild(wsDir, "parent", "helper")
	if err != nil || !changed {
		t.Fatalf("first grant: changed=%v err=%v", changed, err)
	}
	changed, err = GrantSpawnedChild(wsDir, "parent", "helper")
	if err != nil || changed {
		t.Fatalf("second grant: changed=%v err=%v", changed, err)
	}

	raw, err := os.ReadFile(filepath.Join(wsDir, "parent", AllowedAgentsFile))
	if err != nil {
		t.Fatalf("read roster: %v", err)
	}
	if got := strings.Fields(string(raw)); len(got) != 1 || got[0] != "helper" {
		t.Errorf("roster = %q, want a single helper entry", string(raw))
	}
}

func TestLoadRuntimePresetRequiresUsableConfig(t *testing.T) {
	wsDir := t.TempDir()
	writePreset(t, wsDir, "thin", RuntimeConfig{Provider: "openai", Model: "m"})

	if _, _, err := LoadRuntimePreset(wsDir, "thin"); err == nil {
		t.Error("a preset without an endpoint must be refused")
	}
	if _, _, err := LoadRuntimePreset(wsDir, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected a refusal mentioning the preset, got: %v", err)
	}
}
