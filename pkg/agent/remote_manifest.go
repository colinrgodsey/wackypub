package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anmitsu/go-shlex"
)

// RemoteManifestFile is the filename looked up at workspace root for remote route definitions (D116).
const RemoteManifestFile = "REMOTE_MANIFEST"

// RemoteRoute defines a non-native command execution target for an agent.
type RemoteRoute struct {
	AgentID string
	Command string // resolved binary name/path, unexpanded
	Args    []string
}

// RemoteManifest holds parsed routing definitions from REMOTE_MANIFEST.
type RemoteManifest struct {
	Routes map[string]RemoteRoute // keyed by AgentID
	Path   string                 // where it was loaded from, for error messages
	Exists bool                   // false if the file was absent (Routes is empty either way)
}

// LoadRemoteManifest loads and parses REMOTE_MANIFEST from the workspace root directory wsDir.
// If the file does not exist, returns (&RemoteManifest{Exists: false}, nil) without error.
func LoadRemoteManifest(wsDir string) (*RemoteManifest, error) {
	manifestPath := filepath.Join(wsDir, RemoteManifestFile)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &RemoteManifest{
				Routes: make(map[string]RemoteRoute),
				Path:   manifestPath,
				Exists: false,
			}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", RemoteManifestFile, err)
	}

	routes := make(map[string]RemoteRoute)
	seenLines := make(map[string]int)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		idx := strings.Index(line, ":")
		if idx == -1 {
			return nil, fmt.Errorf("%s:%d: missing ':' separator in route %q", RemoteManifestFile, lineNum, line)
		}

		agentID := strings.TrimSpace(line[:idx])
		if agentID == "" {
			return nil, fmt.Errorf("%s:%d: empty agent_id in route %q", RemoteManifestFile, lineNum, line)
		}

		if firstLine, exists := seenLines[agentID]; exists {
			return nil, fmt.Errorf("%s:%d: duplicate route for agent %q (first defined at line %d)", RemoteManifestFile, lineNum, agentID, firstLine)
		}

		cmdPart := strings.TrimSpace(line[idx+1:])
		tokens, err := shlex.Split(cmdPart, true)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: parsing command for agent %q: %w", RemoteManifestFile, lineNum, agentID, err)
		}
		if len(tokens) == 0 {
			return nil, fmt.Errorf("%s:%d: empty command for agent %q", RemoteManifestFile, lineNum, agentID)
		}

		routes[agentID] = RemoteRoute{
			AgentID: agentID,
			Command: tokens[0],
			Args:    tokens[1:],
		}
		seenLines[agentID] = lineNum
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", RemoteManifestFile, err)
	}

	return &RemoteManifest{
		Routes: routes,
		Path:   manifestPath,
		Exists: true,
	}, nil
}

// Lookup finds the configured RemoteRoute for agentID. Returns (route, true) if configured,
// or (empty, false) if native dispatch should be used.
func (m *RemoteManifest) Lookup(agentID string) (RemoteRoute, bool) {
	if m == nil || m.Routes == nil {
		return RemoteRoute{}, false
	}
	r, ok := m.Routes[agentID]
	return r, ok
}
