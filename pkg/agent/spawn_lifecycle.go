package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Lifecycle for spawned children. The ownership rule behind all of it: a child's memory
// and session belong to the workspace, not to the parent that asked for it, so nothing
// here destroys history. Pause only takes away reachability, retire moves the subtree
// aside, and delete refuses to remove anything that was never archived.

// SpawnArchiveDirName is the workspace-level folder retired children are moved into.
// It carries no agent directory signals, so it is not itself listed as an agent.
const SpawnArchiveDirName = "archive"

func childMetaOrError(wsDir, childID string) (*SpawnMeta, string, error) {
	if err := validateNewAgentID(childID); err != nil {
		return nil, "", err
	}
	childDir := filepath.Join(wsDir, childID)
	meta, err := LoadSpawnMeta(childDir)
	if err != nil {
		return nil, childDir, err
	}
	if meta == nil {
		return nil, childDir, fmt.Errorf("%s has no %s: it is a peer agent, not a spawned child, and lifecycle commands do not apply", childID, SpawnMetaFile)
	}
	return meta, childDir, nil
}

// removeRosterEntry drops one line from an allowlist, reporting whether anything changed.
func removeRosterEntry(wsDir, agentID, target string) (bool, error) {
	rosterPath := filepath.Join(wsDir, agentID, AllowedAgentsFile)
	raw, err := os.ReadFile(rosterPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	kept := make([]string, 0, 8)
	changed := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == target {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return false, nil
	}
	if len(kept) == 0 {
		return true, os.WriteFile(rosterPath, []byte(""), 0o644)
	}
	return true, os.WriteFile(rosterPath, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

// PauseSpawnedChild makes the child unreachable from its parent and marks it paused.
// Files, memory and session stay exactly where they are: pausing is reversible and must
// not be a way to lose work.
func PauseSpawnedChild(wsDir, childID string) (*SpawnMeta, error) {
	meta, childDir, err := childMetaOrError(wsDir, childID)
	if err != nil {
		return nil, err
	}
	if meta.Status == SpawnStatusRetired {
		return nil, fmt.Errorf("%s is retired and archived; there is nothing to pause", childID)
	}

	if meta.Parent != "" {
		if _, err := removeRosterEntry(wsDir, meta.Parent, childID); err != nil {
			return nil, err
		}
	}
	meta.Status = SpawnStatusPaused
	if err := WriteSpawnMeta(childDir, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// ResumeSpawnedChild reverses a pause by restoring the parent's allowlist entry.
func ResumeSpawnedChild(wsDir, childID string) (*SpawnMeta, error) {
	meta, childDir, err := childMetaOrError(wsDir, childID)
	if err != nil {
		return nil, err
	}
	if meta.Status == SpawnStatusRetired {
		return nil, fmt.Errorf("%s is retired and archived; spawn a new child instead of resuming it", childID)
	}

	meta.Status = SpawnStatusActive
	if err := WriteSpawnMeta(childDir, meta); err != nil {
		return nil, err
	}
	if meta.Parent != "" {
		if _, err := GrantSpawnedChild(wsDir, meta.Parent, childID); err != nil {
			return nil, err
		}
	}
	return meta, nil
}

// RetireSpawnedChild moves the child subtree into the workspace archive and refreshes the
// manifest. The child stops being an agent the moment it leaves the workspace root, which
// is what retirement means; the history comes with it.
func RetireSpawnedChild(wsDir, childID string) (string, *SpawnMeta, error) {
	meta, childDir, err := childMetaOrError(wsDir, childID)
	if err != nil {
		return "", nil, err
	}
	if meta.Status == SpawnStatusRetired {
		return "", nil, fmt.Errorf("%s is already retired", childID)
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	archiveRoot := filepath.Join(wsDir, SpawnArchiveDirName, "spawned")
	archivePath := filepath.Join(archiveRoot, childID+"-"+stamp)
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return "", nil, err
	}
	if err := os.Rename(childDir, archivePath); err != nil {
		return "", nil, fmt.Errorf("failed to archive %s into %s: %w", childDir, archivePath, err)
	}

	if meta.Parent != "" {
		if _, err := removeRosterEntry(wsDir, meta.Parent, childID); err != nil {
			return archivePath, meta, fmt.Errorf("%s was archived at %s but the parent allowlist could not be updated: %w", childID, archivePath, err)
		}
	}
	meta.Status = SpawnStatusRetired
	if err := WriteSpawnMeta(archivePath, meta); err != nil {
		return archivePath, meta, fmt.Errorf("%s was archived at %s but its lineage could not be updated: %w", childID, archivePath, err)
	}

	if _, err := CreateWorkspaceSnapshot(wsDir); err != nil {
		return archivePath, meta, fmt.Errorf("%s was archived at %s but the workspace snapshot failed: %w", childID, archivePath, err)
	}
	return archivePath, meta, nil
}

// DeleteSpawnedChild removes an already-retired child's archived tree. It refuses while
// the child is live, so the only path to deletion runs through an archive that exists on
// disk - there is no way to destroy a child that was never preserved.
func DeleteSpawnedChild(wsDir, childID string, confirm bool) (string, error) {
	if err := validateNewAgentID(childID); err != nil {
		return "", err
	}
	if pathExists(filepath.Join(wsDir, childID)) {
		return "", fmt.Errorf("refused: %s is still an active agent directory; retire it first so its history is archived before anything is deleted", childID)
	}
	if !confirm {
		return "", fmt.Errorf("refused: deleting archived children needs an explicit confirmation; re-run with --confirm-delete")
	}

	archives, err := listSpawnArchives(wsDir, childID)
	if err != nil {
		return "", err
	}
	if len(archives) == 0 {
		return "", fmt.Errorf("no archived copy of %s found under %s: refusing to delete something that was never preserved", childID, filepath.Join(wsDir, SpawnArchiveDirName))
	}

	// Newest last, so a repeated delete walks the history backwards rather than picking
	// the oldest record first.
	target := archives[len(archives)-1]
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("failed to remove %s: %w", target, err)
	}
	return target, nil
}

func listSpawnArchives(wsDir, childID string) ([]string, error) {
	root := filepath.Join(wsDir, SpawnArchiveDirName, "spawned")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && (e.Name() == childID || strings.HasPrefix(e.Name(), childID+"-")) {
			found = append(found, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(found)
	return found, nil
}

// MarkOrphanedSpawnedChildren finds children whose parent no longer exists and, with apply
// set, records them as orphaned. Orphans are not deleted and not quietly reassigned: the
// coordinator owns them, and the marker is how a human finds them later.
func MarkOrphanedSpawnedChildren(wsDir string, apply bool) ([]string, error) {
	ids, err := ListAgentIDs(wsDir)
	if err != nil {
		return nil, err
	}

	var orphans []string
	for _, id := range ids {
		childDir := filepath.Join(wsDir, id)
		meta, err := LoadSpawnMeta(childDir)
		if err != nil {
			return nil, err
		}
		if meta == nil || meta.Parent == "" || meta.Orphaned {
			continue
		}
		if looksLikeAgentDir(filepath.Join(wsDir, meta.Parent)) {
			continue
		}
		orphans = append(orphans, id)
		if !apply {
			continue
		}
		meta.Orphaned = true
		if err := WriteSpawnMeta(childDir, meta); err != nil {
			return nil, err
		}
	}
	return orphans, nil
}
