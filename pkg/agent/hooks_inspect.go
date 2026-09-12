package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HookContentLineLimit caps how many lines of a hook script's source
// InspectAgentHooks returns. Hook scripts are small by convention (see
// examples/hooks/), so this is a safety cap against an oversized or
// misused file rather than a tuning knob.
const HookContentLineLimit = 200

// HookObservation is a read-only snapshot of one installed hook script, as
// returned by InspectAgentHooks.
type HookObservation struct {
	// Event is the hook event directory name (e.g. EventOnUserMessage).
	Event string
	// Name is the script's filename, e.g. "00-date".
	Name string
	// Path is the script's full path on disk.
	Path string
	// Content holds up to HookContentLineLimit lines of the script's source.
	Content string
	// TotalLines is the script's true line count, which can exceed the
	// number of lines captured in Content.
	TotalLines int
	// Truncated is true when Content was cut off at HookContentLineLimit
	// lines short of TotalLines.
	Truncated bool
}

// InspectAgentHooks lists every hook installed under <wsDir>/<agentID>/hooks/,
// grouped by event directory in the same ascending numeric order DiscoverHooks
// runs them in, together with (a capped view of) each script's source. It
// exists so an agent - or a coordinator inspecting another agent's workspace -
// can answer "what hooks are actually wired up and what do they do" without
// raw filesystem access (see hooks-inspection-command task card).
//
// Only reuses DiscoverHooks' own filtering (regular, executable files) so the
// listing matches what RunHookChain would actually run - a non-executable or
// otherwise-ignored file in a hooks/<event>/ directory is deliberately left
// out, since reporting it here would misrepresent what fires.
//
// Returns (nil, nil) if the agent or its hooks directory doesn't exist -
// having no hooks installed is a normal, unremarkable state, not an error.
func InspectAgentHooks(wsDir, agentID string) ([]HookObservation, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	agentDir := filepath.Join(wsDir, agentID)
	hooksDir := filepath.Join(agentDir, "hooks")

	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read hooks dir %s: %w", hooksDir, err)
	}

	var events []string
	for _, e := range entries {
		if e.IsDir() {
			events = append(events, e.Name())
		}
	}
	sort.Strings(events)

	var observations []HookObservation
	for _, event := range events {
		scripts, err := DiscoverHooks(agentDir, event)
		if err != nil {
			return nil, err
		}
		for _, scriptPath := range scripts {
			content, totalLines, truncated, err := readHookSource(scriptPath, HookContentLineLimit)
			if err != nil {
				return nil, fmt.Errorf("failed to read hook %s: %w", scriptPath, err)
			}
			observations = append(observations, HookObservation{
				Event:      event,
				Name:       filepath.Base(scriptPath),
				Path:       scriptPath,
				Content:    content,
				TotalLines: totalLines,
				Truncated:  truncated,
			})
		}
	}

	return observations, nil
}

// readHookSource reads path line by line, returning at most limit lines
// joined back together, the file's true line count, and whether it was cut
// off. Line-oriented (rather than a byte-count cap) so a truncated hook
// script still reads as a coherent partial script rather than stopping
// mid-line.
func readHookSource(path string, limit int) (content string, totalLines int, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", 0, false, err
	}

	totalLines = len(lines)
	if totalLines <= limit {
		return strings.Join(lines, "\n"), totalLines, false, nil
	}
	return strings.Join(lines[:limit], "\n"), totalLines, true, nil
}
