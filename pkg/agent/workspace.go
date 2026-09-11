package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	RootMarkerFile    = "WACKYPUB_ROOT"
	AllowedAgentsFile = "WACKYPUB_ALLOWED_AGENTS"
	CallChainEnvVar   = "WACKYPUB_CALL_CHAIN"
	ToolsDirName      = "tools"
)

// agentDirSignals are the files whose presence (directly inside a directory)
// marks that directory as an agent directory rather than something else
// living under the workspace (e.g. a shared runtimes/ folder holding
// runtime.json variants to symlink from - see .agents/LOCAL_TESTING.md).
var agentDirSignals = []string{"AGENTS.md", "runtime.json", "session.jsonl"}

// ListAgentIDs returns the names of subdirectories of wsDir that look like
// agent directories - see agentDirSignals. Returned in sorted order. Returns
// an empty (nil) slice without error if wsDir does not exist.
func ListAgentIDs(wsDir string) ([]string, error) {
	entries, err := os.ReadDir(wsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if looksLikeAgentDir(filepath.Join(wsDir, e.Name())) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func looksLikeAgentDir(agentDir string) bool {
	for _, name := range agentDirSignals {
		if pathExists(filepath.Join(agentDir, name)) {
			return true
		}
	}
	return false
}

// CurrentAgentIDFromCWD returns the agent ID whose directory the current working
// directory IS - not contains, not a subdirectory of - and whether one was detected
// at all, according to D41. Same looksLikeAgentDir + filepath.Base pattern
// ValidateAgentTarget already uses for its own sendingAgentID computation, applied
// directly rather than via an upward walk: run_command always sets a spawned tool's
// cmd.Dir to the calling agent's directory exactly, never a subdirectory of it, so
// there's no case in the actual call path a direct check misses.
func CurrentAgentIDFromCWD() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	if looksLikeAgentDir(cwd) {
		return filepath.Base(cwd), true
	}
	return "", false
}

// pathExists reports whether path exists, without following a symlink (a
// broken symlink still counts as present for signaling purposes).
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// AgentInspection reports the on-disk state of a single agent directory:
// which expected files are present, whether runtime.json parses, and basic
// session/memory stats. Intended for diagnosing what a workspace still
// needs without requiring prior knowledge of the file layout - see the
// `workspace` CLI command and AgentSDK.InspectAgent.
type AgentInspection struct {
	AgentID  string
	AgentDir string

	// AgentDirExists is false when AgentDir doesn't exist yet - every other
	// field is zero-valued in that case.
	AgentDirExists bool

	AgentsMDExists bool
	MemoryMDExists bool
	DotEnvExists   bool

	RuntimeJSONExists    bool
	RuntimeJSONIsSymlink bool
	// RuntimeJSONResolved is the symlink target's real path, only set when
	// RuntimeJSONIsSymlink is true and it resolves.
	RuntimeJSONResolved string
	RuntimeJSONValid    bool
	// RuntimeJSONError holds LoadRuntimeConfig's error message when
	// RuntimeJSONExists is true but RuntimeJSONValid is false.
	RuntimeJSONError string
	// RuntimeConfig is non-nil only when RuntimeJSONValid is true.
	RuntimeConfig *RuntimeConfig

	SessionJSONLExists bool
	// SessionTurnCount is the number of turns ReadSessionTurns successfully
	// parsed.
	SessionTurnCount int
	// SessionCorruptLines is the number of non-empty lines in session.jsonl
	// that ReadSessionTurns silently skipped because they didn't parse as a
	// genai.Content - see .agents/AGENTS.md's session.jsonl corruption
	// gotcha. Zero in the common case.
	SessionCorruptLines int
	AllowedAgentsExists bool
	AllowedAgents       []string

	ToolsDirExists  bool
	DiscoveredTools []string
	ShadowedTools   []string

	SkillsDirExists  bool
	DiscoveredSkills []string
	ShadowedSkills   []string
}

// ResolveWorkspaceDir resolves the workspace directory according to D15:
// - If isExplicit is true (--ws was explicitly specified), wsFlag must contain RootMarkerFile directly.
// - If isExplicit is false (default), walk up from CWD looking for RootMarkerFile. Error if not found.
func ResolveWorkspaceDir(wsFlag string, isExplicit bool) (string, error) {
	if isExplicit {
		clean := filepath.Clean(wsFlag)
		if !pathExists(filepath.Join(clean, RootMarkerFile)) {
			return "", fmt.Errorf("workspace directory %q does not contain %s marker file", wsFlag, RootMarkerFile)
		}
		return clean, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}

	dir := cwd
	for {
		if pathExists(filepath.Join(dir, RootMarkerFile)) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s marker file found in current directory (%s) or any parent directory", RootMarkerFile, cwd)
		}
		dir = parent
	}
}

// AuthorizeAgentTarget checks cross-agent authorization (AllowedAgentsFile against CWD)
// according to D16, D60. It verifies whether the current working directory's agent
// allowlist permits accessing targetAgentID. Read-only content methods use this check
// without enforcing deadlock cycle checks (reads cannot deadlock).
func AuthorizeAgentTarget(targetAgentID string) error {
	if targetAgentID == "" {
		return nil
	}

	// Authorization check: AllowedAgentsFile against CWD (before resolving workspace root)
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	dir := cwd
	for {
		if pathExists(filepath.Join(dir, RootMarkerFile)) {
			break
		}

		allowedPath := filepath.Join(dir, AllowedAgentsFile)
		if pathExists(allowedPath) {
			allowed, err := readAllowedAgents(allowedPath)
			if err != nil {
				return fmt.Errorf("failed to read %s: %w", allowedPath, err)
			}
			authorized := false
			for _, id := range allowed {
				if id == targetAgentID {
					authorized = true
					break
				}
			}
			if !authorized {
				return fmt.Errorf("agent %q is not in %s allowlist for current agent directory %q", targetAgentID, AllowedAgentsFile, dir)
			}
			break
		}

		if looksLikeAgentDir(dir) {
			return fmt.Errorf("access to agent %q denied: current agent directory %q has no %s allowlist", targetAgentID, dir, AllowedAgentsFile)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return nil
}

// ValidateAgentTarget performs cross-agent authorization (AllowedAgentsFile against CWD)
// and deadlock prevention (A2AMetadata.CallChain) according to D16, D33, D59, D60.
// Returns the updated *A2AMetadata to propagate to spawned child tools, with zero process-global os.Setenv mutation.
func ValidateAgentTarget(targetAgentID string) (*A2AMetadata, error) {
	if targetAgentID == "" {
		return nil, nil
	}

	// 1. Authorization check
	if err := AuthorizeAgentTarget(targetAgentID); err != nil {
		return nil, err
	}

	// 2. Deadlock cycle check & A2A Metadata parsing (D16, D33, D59)
	meta, err := ParseA2AMetadata()
	if err != nil {
		return nil, fmt.Errorf("failed to parse A2A metadata: %w", err)
	}

	for _, id := range meta.CallChain {
		if id == targetAgentID {
			return nil, fmt.Errorf("agent %q is already in call chain (%s); operation rejected to prevent deadlock cycle", targetAgentID, strings.Join(meta.CallChain, ","))
		}
	}

	// 3. Compute updated A2AMetadata
	callerID := ""
	if len(meta.CallChain) > 0 {
		callerID = meta.CallChain[len(meta.CallChain)-1]
	}

	newChain := append(append([]string{}, meta.CallChain...), targetAgentID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = GenerateTraceID()
	}

	newMetaMap := make(map[string]string)
	for k, v := range meta.Metadata {
		newMetaMap[k] = v
	}

	cwd, _ := os.Getwd()
	wsDir, _ := ResolveWorkspaceDir(cwd, false)
	sendingAgentID := callerID
	if sendingAgentID == "" && looksLikeAgentDir(cwd) {
		sendingAgentID = filepath.Base(cwd)
	}

	sendingRepoDir := ResolveGitRepoDir(wsDir, sendingAgentID)
	if sendingRepoDir != "" {
		if headSHA, _ := GetWorkspaceHeadCommit(sendingRepoDir); headSHA != "" {
			newMetaMap["workspace_revision"] = headSHA
		}
	}

	newMeta := &A2AMetadata{
		CallerID:  callerID,
		CallChain: newChain,
		TraceID:   traceID,
		Metadata:  newMetaMap,
	}

	return newMeta, nil
}

func readAllowedAgents(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var result []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		result = append(result, line)
	}
	return result, scanner.Err()
}

// DiscoverAgentToolsMap walks <agentDir>/tools/ recursively for executable files according to D14.
// Resolves directory and file symlinks and follows them, preventing infinite symlink cycles.
// Returns a map of tool name -> file path, discovered unique tool names, shadowing warning messages, and error.
func DiscoverAgentToolsMap(agentDir string) (map[string]string, []string, []string, error) {
	toolsDir := filepath.Join(agentDir, ToolsDirName)
	if !pathExists(toolsDir) {
		return nil, nil, nil, nil
	}

	toolMap := make(map[string]string) // tool name -> file path
	var discovered []string
	var shadowed []string
	visitedDirs := make(map[string]bool)

	var walk func(dir string) error
	walk = func(dir string) error {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil // Skip unresolvable directory symlinks
		}
		if visitedDirs[realDir] {
			return nil // Prevent cycle
		}
		visitedDirs[realDir] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			info, err := os.Stat(path) // os.Stat follows symlinks!
			if err != nil {
				continue // Skip broken symlinks or unreadable files
			}

			if info.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			} else if info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				name := entry.Name()
				if existingPath, exists := toolMap[name]; exists {
					shadowed = append(shadowed, fmt.Sprintf("tool %q at %s is shadowed by %s", name, existingPath, path))
				} else {
					discovered = append(discovered, name)
				}
				toolMap[name] = path
			}
		}
		return nil
	}

	if err := walk(toolsDir); err != nil {
		return nil, nil, nil, err
	}

	sort.Strings(discovered)
	return toolMap, discovered, shadowed, nil
}

// DiscoverAgentTools walks <agentDir>/tools/ recursively for executable files according to D14.
// Returns discovered unique tool names and shadowing warning messages.
func DiscoverAgentTools(agentDir string) ([]string, []string, error) {
	_, discovered, shadowed, err := DiscoverAgentToolsMap(agentDir)
	return discovered, shadowed, err
}

// InspectAgentDir builds an AgentInspection for <wsDir>/<agentID> without
// acquiring the session lock - callers that need a consistent snapshot
// alongside concurrent writers should hold the lock themselves (see
// AgentSDK.InspectAgent, which does). Safe to call even if the agent
// directory or any of its expected files don't exist.
func InspectAgentDir(wsDir, agentID string) (*AgentInspection, error) {
	agentDir := filepath.Join(wsDir, agentID)
	insp := &AgentInspection{AgentID: agentID, AgentDir: agentDir}

	if _, err := os.Stat(agentDir); err != nil {
		if os.IsNotExist(err) {
			return insp, nil
		}
		return nil, err
	}
	insp.AgentDirExists = true

	insp.AgentsMDExists = pathExists(filepath.Join(agentDir, "AGENTS.md"))
	insp.MemoryMDExists = pathExists(filepath.Join(agentDir, "MEMORY.md"))
	insp.DotEnvExists = pathExists(filepath.Join(agentDir, ".env"))

	allowedPath := filepath.Join(agentDir, AllowedAgentsFile)
	if pathExists(allowedPath) {
		insp.AllowedAgentsExists = true
		allowed, err := readAllowedAgents(allowedPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", allowedPath, err)
		}
		insp.AllowedAgents = allowed
	}

	toolsDir := filepath.Join(agentDir, ToolsDirName)
	if pathExists(toolsDir) {
		insp.ToolsDirExists = true
		discovered, shadowed, err := DiscoverAgentTools(agentDir)
		if err != nil {
			return nil, fmt.Errorf("failed to discover tools in %s: %w", toolsDir, err)
		}
		insp.DiscoveredTools = discovered
		insp.ShadowedTools = shadowed
	}

	skillsDir := filepath.Join(agentDir, SkillsDirName)
	if pathExists(skillsDir) {
		insp.SkillsDirExists = true
		_, onDemand, alwaysLoaded, shadowed, err := DiscoverAgentSkillsMap(agentDir)
		if err != nil {
			return nil, fmt.Errorf("failed to discover skills in %s: %w", skillsDir, err)
		}
		var skillNames []string
		for _, sk := range onDemand {
			skillNames = append(skillNames, sk.Name)
		}
		for _, sk := range alwaysLoaded {
			skillNames = append(skillNames, sk.Name+" (always_load)")
		}
		insp.DiscoveredSkills = skillNames
		insp.ShadowedSkills = shadowed
	}

	runtimePath := filepath.Join(agentDir, "runtime.json")
	if fi, err := os.Lstat(runtimePath); err == nil {
		insp.RuntimeJSONExists = true
		insp.RuntimeJSONIsSymlink = fi.Mode()&os.ModeSymlink != 0
		if insp.RuntimeJSONIsSymlink {
			if resolved, err := filepath.EvalSymlinks(runtimePath); err == nil {
				insp.RuntimeJSONResolved = resolved
			}
		}
		if cfg, err := LoadRuntimeConfig(agentDir); err != nil {
			insp.RuntimeJSONError = err.Error()
		} else {
			insp.RuntimeJSONValid = true
			insp.RuntimeConfig = cfg
		}
	}

	sessionPath := filepath.Join(agentDir, "session.jsonl")
	if pathExists(sessionPath) {
		insp.SessionJSONLExists = true

		turns, err := ReadSessionTurns(agentDir)
		if err != nil {
			return nil, err
		}
		insp.SessionTurnCount = len(turns)

		totalLines, err := countNonEmptyLines(sessionPath)
		if err != nil {
			return nil, err
		}
		insp.SessionCorruptLines = totalLines - len(turns)
	}

	return insp, nil
}

func countNonEmptyLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	count := 0
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			count++
		}
	}
	return count, scanner.Err()
}

// AgentLockObservation is a read-only snapshot of one agent directory's session lock
// and session activity, as returned by InspectAgentLocks.
type AgentLockObservation struct {
	AgentID  string
	AgentDir string

	// LockExists is whether <agent_dir>/session.lock is present. The file outlives
	// every release, so presence alone never means held: SessionLock.Release drops
	// the flock and closes the fd but does not delete the file.
	LockExists bool
	// HolderPID is the PID recorded inside the lock file by the last acquisition, or
	// zero when the file is missing or its contents cannot be parsed.
	HolderPID int
	// HolderPIDValid is whether HolderPID was parsed from the lock file. It is what
	// distinguishes "nobody has ever locked this agent" from "the lock file exists
	// but holds no readable PID", which a bare zero cannot tell apart.
	HolderPIDValid bool
	// HolderAlive is whether that PID currently exists. A held lock with a live PID
	// is not the same as progress; pair it with LastWrite.
	HolderAlive bool
	// HolderCommand is a shortened command line for the holder with credential flag
	// values redacted, or empty when there is no readable PID.
	HolderCommand string
	// LockHeldSince is the lock file modification time, which is when the current
	// holder's PID was written into it.
	LockHeldSince time.Time

	// SessionExists is whether <agent_dir>/session.jsonl is present.
	SessionExists bool
	// LastWrite is session.jsonl's modification time: the most recent evidence that
	// the agent is producing anything.
	LastWrite time.Time
}

// InspectAgentLocks reports session lock and session activity for every agent
// directory in wsDir. It exists because AcquireSessionLock blocks indefinitely on
// flock with no timeout, so a stuck holder silently queues everyone behind it and
// the only clue is the PID inside the lock file, which every acquisition overwrites.
//
// Read-only by construction: the lock file is opened for reading only, never flocked,
// truncated, created, or deleted, so observing a holder cannot disturb it or queue
// behind it. Per-file failures are not errors - a missing lock file is a normal
// state and is reported by the zero values of the relevant fields.
//
// Deliberately not gated by ValidateAgentTarget's WACKYPUB_ALLOWED_AGENTS check, for
// the same reason as InspectAgentDir (D16): this has no side effects and cannot cause
// another agent to do anything, so gating it would report an authorization failure
// where the truth is "that agent is idle".
func InspectAgentLocks(wsDir string) ([]AgentLockObservation, error) {
	ids, err := ListAgentIDs(wsDir)
	if err != nil {
		return nil, err
	}

	observations := make([]AgentLockObservation, 0, len(ids))
	for _, id := range ids {
		agentDir := filepath.Join(wsDir, id)
		obs := AgentLockObservation{AgentID: id, AgentDir: agentDir}

		lockPath := filepath.Join(agentDir, "session.lock")
		if info, err := os.Stat(lockPath); err == nil {
			obs.LockExists = true
			obs.LockHeldSince = info.ModTime()
			data, err := os.ReadFile(lockPath)
			if err != nil {
				return nil, fmt.Errorf("failed to read session lock for agent %q: %w", id, err)
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				obs.HolderPID = pid
				obs.HolderPIDValid = true
				obs.HolderAlive = processAlive(pid)
				obs.HolderCommand = processCommand(pid)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to stat session lock for agent %q: %w", id, err)
		}

		if info, err := os.Stat(filepath.Join(agentDir, "session.jsonl")); err == nil {
			obs.SessionExists = true
			obs.LastWrite = info.ModTime()
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to stat session file for agent %q: %w", id, err)
		}

		observations = append(observations, obs)
	}
	return observations, nil
}

// processAlive reports whether a process exists. Signal 0 performs the existence and
// permission checks without delivering anything, so EPERM also means alive - owned by
// another user, which an operator still needs to see as a live holder.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// secretFlagMarkers name the flags whose values must never be echoed. A long-lived
// gateway observed in a real workspace passes its Discord token as argv, so printing a
// raw command line leaks credential prefixes into terminals, scrollback, and CI logs.
var secretFlagMarkers = []string{"token", "secret", "password", "passwd", "apikey", "api-key", "credential", "key"}

// shortenCommandArgs reduces argv to a recognizable prefix, redacting the value of any
// credential-bearing flag in both the "--flag value" and "--flag=value" forms. limit
// counts the total tokens kept, including the program name; because credential flags
// are common early arguments, the limit applies to the token before any redaction, so
// a redaction can never silently free up room for a later argument.
func shortenCommandArgs(argv []string, limit int) []string {
	if len(argv) == 0 {
		return nil
	}
	if limit < 1 {
		limit = 1
	}
	parts := []string{filepath.Base(argv[0])}
	redactNext := false
	for _, arg := range argv[1:] {
		if len(parts) >= limit {
			break
		}
		if redactNext {
			parts = append(parts, "[redacted]")
			redactNext = false
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch {
		case hasValue && holdsSecretFlag(name):
			parts = append(parts, name+"=[redacted]")
		case holdsSecretFlag(name):
			parts = append(parts, arg)
			redactNext = true
		default:
			parts = append(parts, arg)
		}
	}
	return parts
}

func holdsSecretFlag(flag string) bool {
	flag = strings.ToLower(flag)
	for _, marker := range secretFlagMarkers {
		if strings.Contains(flag, marker) {
			return true
		}
	}
	return false
}

// processCommand returns a shortened, credential-redacted command line for a PID, or
// "" when it cannot be read. Linux-specific: it reads the NUL-separated
// /proc/<pid>/cmdline, and the rest of a wackypub invocation's argv can be an entire
// prompt, so only the program name and two arguments are kept. Callers treat an
// unreadable command as cosmetic: liveness and lock state come from elsewhere.
func processCommand(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	argv := strings.FieldsFunc(string(data), func(r rune) bool { return r == 0 })
	if len(argv) == 0 {
		return ""
	}
	return strings.Join(shortenCommandArgs(argv, 3), " ")
}
