package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MacroRegex matches @<FILE_PATH> patterns where FILE_PATH is a relative path.
var macroRegex = regexp.MustCompile(`(^|[\s(\[{<"']|` + "`" + `)(\\?|@?)@([a-zA-Z0-9_\-./]*[a-zA-Z0-9_\-/])`)

// isContained checks whether targetPath resides within baseDir.
func isContained(baseDir, targetPath string) bool {
	rel, err := filepath.Rel(baseDir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// ExpandMacros processes text content and replaces any @<FILE_PATH> directives
// with the content of the referenced file relative to agentDir.
func ExpandMacros(content string, agentDir string) (string, error) {
	if agentDir == "" {
		agentDir = "."
	}
	if abs, err := filepath.Abs(agentDir); err == nil {
		agentDir = abs
	}
	wsDir := FindWorkspaceRootDir(agentDir)
	realWsDir := wsDir
	if evaluated, err := filepath.EvalSymlinks(wsDir); err == nil {
		realWsDir = evaluated
	}
	visited := make(map[string]bool)
	return expandMacrosRecursive(content, agentDir, wsDir, realWsDir, visited, 0)
}

// RenderAgentSystemPrompt reads <wsDir>/<agentID>/AGENTS.md (falling back to
// a generic "You are agent <id>." prompt if it doesn't exist, matching
// LoadFolderAgent) and expands @<FILE_PATH> macros. Unlike LoadFolderAgent,
// it does not touch runtime.json and does not construct a model - useful for
// validating AGENTS.md/macro output independently of backend configuration.
// If hookEnv is provided and contains mutated environment variables, they are
// rendered into a model-visible [hook env] section.
func RenderAgentSystemPrompt(wsDir, agentID string, hookEnv ...map[string]string) (string, error) {
	agentDir := filepath.Join(wsDir, agentID)
	agentsPath := filepath.Join(agentDir, "AGENTS.md")

	agentsData, err := os.ReadFile(agentsPath)
	if err != nil {
		if os.IsNotExist(err) {
			agentsData = []byte(fmt.Sprintf("You are agent %s.", agentID))
		} else {
			return "", fmt.Errorf("failed to read AGENTS.md at %s: %w", agentsPath, err)
		}
	}

	expanded, err := ExpandMacros(string(agentsData), agentDir)
	if err != nil {
		return "", fmt.Errorf("macro expansion failed for %s: %w", agentsPath, err)
	}

	autoloadBlock, err := RenderAutoloadedSkills(agentDir)
	if err != nil {
		return "", fmt.Errorf("failed to render autoloaded skills for %s: %w", agentID, err)
	}
	if autoloadBlock != "" {
		expanded = expanded + "\n\n" + autoloadBlock
	}

	if len(hookEnv) > 0 && len(hookEnv[0]) > 0 {
		envMap := hookEnv[0]
		keys := make([]string, 0, len(envMap))
		for k := range envMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var sb strings.Builder
		sb.WriteString("[hook env]")
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("\n%s=%s", k, envMap[k]))
		}
		expanded = expanded + "\n\n" + sb.String()
	}

	return expanded, nil
}

func expandMacrosRecursive(content string, agentDir string, wsDir string, realWsDir string, visited map[string]bool, depth int) (string, error) {
	if depth > 10 {
		return "", fmt.Errorf("macro expansion depth exceeded limit of 10")
	}

	var firstErr error

	result := macroRegex.ReplaceAllStringFunc(content, func(match string) string {
		if firstErr != nil {
			return match
		}

		submatches := macroRegex.FindStringSubmatch(match)
		if len(submatches) < 4 {
			return match
		}

		boundary := submatches[1]
		escape := submatches[2]
		target := submatches[3]

		// Escapes handling: \@path or @@path emits literal boundary + "@" + target
		if escape == "\\" || escape == "@" {
			return boundary + "@" + target
		}

		// Workspace Root Containment (Security Traversal Defense)
		cleanPath := filepath.Clean(filepath.Join(agentDir, target))
		if !isContained(wsDir, cleanPath) {
			return boundary + "@" + target
		}

		// Existence Check & Symlink Verification (D90 Parity)
		fi, err := os.Stat(cleanPath)
		if err != nil || fi.IsDir() {
			return boundary + "@" + target
		}

		realPath, err := filepath.EvalSymlinks(cleanPath)
		if err != nil {
			return boundary + "@" + target
		}
		if !isContained(wsDir, realPath) && !isContained(realWsDir, realPath) {
			return boundary + "@" + target
		}

		// Stack-Scoped Cycle Guard
		if visited[realPath] {
			return boundary + fmt.Sprintf("<!-- Circular macro import omitted: %s -->", target)
		}
		visited[realPath] = true
		defer delete(visited, realPath)

		data, err := os.ReadFile(realPath)
		if err != nil {
			firstErr = fmt.Errorf("failed to read macro file %s: %w", target, err)
			return match
		}

		// Recursively expand macros in the included content
		expanded, err := expandMacrosRecursive(string(data), agentDir, wsDir, realWsDir, visited, depth+1)
		if err != nil {
			firstErr = err
			return match
		}
		return boundary + expanded
	})

	if firstErr != nil {
		return "", firstErr
	}

	return result, nil
}
