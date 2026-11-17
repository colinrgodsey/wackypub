package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spawnGrantedChild(t *testing.T, wsDir string) {
	t.Helper()
	resetActiveWorkspaceForSpawn(t)
	SetActiveWorkspaceDir(wsDir)
	if _, err := SpawnAgent(SpawnOptions{WorkspaceDir: wsDir, ChildID: "helper", ParentID: "parent", Preset: spawnTestPreset}); err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	if _, err := GrantSpawnedChild(wsDir, "parent", "helper"); err != nil {
		t.Fatalf("GrantSpawnedChild: %v", err)
	}
}

func rosterHas(t *testing.T, wsDir, agentID, target string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(wsDir, agentID, AllowedAgentsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read roster: %v", err)
	}
	return strings.Contains(string(raw), target)
}

func TestPauseAndResumeRoundtripKeepsHistory(t *testing.T) {
	wsDir := spawnFixture(t)
	spawnGrantedChild(t, wsDir)

	session := filepath.Join(wsDir, "helper", "session.jsonl")
	if err := os.WriteFile(session, []byte("{\"role\":\"user\",\"parts\":[{\"text\":\"keep me\"}]}\n"), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	meta, err := PauseSpawnedChild(wsDir, "helper")
	if err != nil {
		t.Fatalf("PauseSpawnedChild: %v", err)
	}
	if meta.Status != SpawnStatusPaused {
		t.Errorf("status = %q, want paused", meta.Status)
	}
	if rosterHas(t, wsDir, "parent", "helper") {
		t.Error("the parent can still address a paused child")
	}
	if !rosterHas(t, wsDir, "helper", "parent") {
		t.Error("pausing should not strip the child's own allowlist")
	}
	raw, err := os.ReadFile(session)
	if err != nil || !strings.Contains(string(raw), "keep me") {
		t.Errorf("pausing must not touch session data: %v %q", err, string(raw))
	}

	meta, err = ResumeSpawnedChild(wsDir, "helper")
	if err != nil {
		t.Fatalf("ResumeSpawnedChild: %v", err)
	}
	if meta.Status != SpawnStatusActive || !rosterHas(t, wsDir, "parent", "helper") {
		t.Errorf("resume did not restore the pair: status=%q roster=%v", meta.Status, rosterHas(t, wsDir, "parent", "helper"))
	}
}

func TestRetireArchivesTheSubtreeAndStopsListingTheChild(t *testing.T) {
	wsDir := spawnFixture(t)
	spawnGrantedChild(t, wsDir)

	archivePath, meta, err := RetireSpawnedChild(wsDir, "helper")
	if err != nil {
		t.Fatalf("RetireSpawnedChild: %v", err)
	}
	if meta.Status != SpawnStatusRetired {
		t.Errorf("status = %q, want retired", meta.Status)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "helper")); !os.IsNotExist(err) {
		t.Errorf("the child directory should be gone, stat err = %v", err)
	}
	for _, name := range []string{"AGENTS.md", SpawnMetaFile, "runtime.json"} {
		if _, err := os.Stat(filepath.Join(archivePath, name)); err != nil {
			t.Errorf("archived %s missing: %v", name, err)
		}
	}

	ids, err := ListAgentIDs(wsDir)
	if err != nil {
		t.Fatalf("ListAgentIDs: %v", err)
	}
	for _, id := range ids {
		if id == "helper" {
			t.Errorf("a retired child is still listed as an agent: %v", ids)
		}
		if id == SpawnArchiveDirName {
			t.Errorf("the archive folder must not look like an agent: %v", ids)
		}
	}
	if rosterHas(t, wsDir, "parent", "helper") {
		t.Error("a retired child should be out of the parent allowlist")
	}
	manifest, err := os.ReadFile(filepath.Join(wsDir, "MANIFEST.md"))
	if err != nil {
		t.Fatalf("retire should refresh MANIFEST.md: %v", err)
	}
	if !strings.Contains(string(manifest), "parent") {
		t.Errorf("manifest lost the lineage columns: %s", string(manifest))
	}
}

func TestDeleteRequiresRetireThenConfirmation(t *testing.T) {
	wsDir := spawnFixture(t)
	spawnGrantedChild(t, wsDir)

	if _, err := DeleteSpawnedChild(wsDir, "helper", true); err == nil {
		t.Fatal("deleting a live child must be refused even with confirmation")
	}

	archivePath, _, err := RetireSpawnedChild(wsDir, "helper")
	if err != nil {
		t.Fatalf("RetireSpawnedChild: %v", err)
	}
	if _, err := DeleteSpawnedChild(wsDir, "helper", false); err == nil {
		t.Fatal("deleting an archived child must need explicit confirmation")
	}

	removed, err := DeleteSpawnedChild(wsDir, "helper", true)
	if err != nil {
		t.Fatalf("DeleteSpawnedChild: %v", err)
	}
	if removed != archivePath {
		t.Errorf("removed %q, expected the archive %q", removed, archivePath)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Errorf("the archive should be gone, stat err = %v", err)
	}
}

func TestOrphanedChildrenAreMarkedAndKept(t *testing.T) {
	wsDir := spawnFixture(t)
	spawnGrantedChild(t, wsDir)

	if err := os.RemoveAll(filepath.Join(wsDir, "parent")); err != nil {
		t.Fatalf("remove parent: %v", err)
	}

	orphans, err := MarkOrphanedSpawnedChildren(wsDir, false)
	if err != nil {
		t.Fatalf("MarkOrphanedSpawnedChildren: %v", err)
	}
	if len(orphans) != 1 || orphans[0] != "helper" {
		t.Fatalf("orphans = %v, want [helper]", orphans)
	}
	meta, err := LoadSpawnMeta(filepath.Join(wsDir, "helper"))
	if err != nil || meta == nil || meta.Orphaned {
		t.Fatalf("a dry run must not write the marker: %v %+v", err, meta)
	}

	if _, err := MarkOrphanedSpawnedChildren(wsDir, true); err != nil {
		t.Fatalf("mark apply: %v", err)
	}
	meta, err = LoadSpawnMeta(filepath.Join(wsDir, "helper"))
	if err != nil || meta == nil || !meta.Orphaned {
		t.Fatalf("expected the orphan marker to be recorded: %v %+v", err, meta)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "helper", "AGENTS.md")); err != nil {
		t.Errorf("an orphan keeps its files: %v", err)
	}

	again, err := MarkOrphanedSpawnedChildren(wsDir, true)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("an already-marked orphan should not be reported twice, got %v", again)
	}
}

func TestLifecycleCommandsRefusePeerAgents(t *testing.T) {
	wsDir := spawnFixture(t)

	if _, err := PauseSpawnedChild(wsDir, "parent"); err == nil || !strings.Contains(err.Error(), SpawnMetaFile) {
		t.Errorf("pausing a peer should explain that it has no lineage, got: %v", err)
	}
}
