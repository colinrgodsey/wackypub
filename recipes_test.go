package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecipesValidateFrontmatter is the docs-build check for the recipes/ tree
// (tasks/wackypub/recipes-setup-guides): it verifies every recipe's YAML frontmatter
// carries the required machine-readable fields (name, title, confinement, skills,
// tools), that every referenced skill resolves to a bundled skill or a known
// external skillset, every referenced tool is in the standard catalog, and that
// every FILES_RW_ACCESS grant uses the real files-rw syntax.
func TestRecipesValidateFrontmatter(t *testing.T) {
	recipeDir := filepath.Join("recipes")
	entries, err := os.ReadDir(recipeDir)
	if err != nil {
		t.Fatalf("read recipes dir: %v", err)
	}

	bundledSkills := map[string]bool{}
	bundledEntries, _ := os.ReadDir("skills")
	for _, e := range bundledEntries {
		if e.IsDir() {
			bundledSkills[e.Name()] = true
		}
	}
	// External skillsets shipped from other repos (mirrored into skillsets/).
	externalSkills := map[string]bool{"files-rw": true, "wackyproc": true}
	// The standard tool catalog: binaries recipes may symlink.
	knownTools := map[string]bool{"bash": true, "files-rw": true, "wackyproc": true, "wackypub": true}
	allowedConfinement := map[string]bool{"container-required": true, "container-recommended": true, "safe-unconfined": true}

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		checked++
		path := filepath.Join(recipeDir, e.Name())
		fm := parseRecipeFrontmatter(t, path)
		body, _ := os.ReadFile(path)
		bodyStr := string(body)

		if fm["name"] == "" || fm["title"] == "" {
			t.Errorf("%s: frontmatter must declare name and title", e.Name())
		}
		if !allowedConfinement[fm["confinement"]] {
			t.Errorf("%s: confinement %q not in {container-required, container-recommended, safe-unconfined}", e.Name(), fm["confinement"])
		}
		// Skills: every listed name must resolve.
		for _, s := range splitList(fm["skills"]) {
			if !bundledSkills[s] && !externalSkills[s] {
				t.Errorf("%s: unknown skill %q (not bundled in skills/ nor external files-rw/wackyproc)", e.Name(), s)
			}
		}
		// Tools: every listed name must be in the standard catalog.
		for _, tool := range splitList(fm["tools"]) {
			if !knownTools[tool] {
				t.Errorf("%s: unknown tool %q (catalog: bash, files-rw, wackyproc, wackypub)", e.Name(), tool)
			}
		}
		// FILES_RW_ACCESS heredocs must use real files-rw syntax (filesrw/access.go):
		// lines are r: <path> or w: <path> with literal relative paths - no globs, no **,
		// no depth markers. The parser canonicalizes the path text, so :* or glob chars
		// become literal path text that resolves to a nonexistent path and grants nothing
		// (a silently-inert ACL - the exact failure this guard exists to catch).
		for _, line := range strings.Split(bodyStr, "\n") {
			tl := strings.TrimSpace(line)
			if tl == "" || strings.HasPrefix(tl, "#") || (!strings.HasPrefix(tl, "r:") && !strings.HasPrefix(tl, "w:")) {
				continue // blank, comment, or prose/step text
			}
			if strings.ContainsAny(tl, "*?[]~") {
				t.Errorf("%s: FILES_RW_ACCESS rule %q has invalid glob/metacharacters (real syntax is r: <path> or w: <path>, literal paths only)", e.Name(), tl)
			}
			pathPart := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(tl, "r:"), "w:"))
			if pathPart == "" {
				t.Errorf("%s: FILES_RW_ACCESS rule %q has no path", e.Name(), tl)
			}
			if strings.HasPrefix(pathPart, "/") {
				t.Errorf("%s: FILES_RW_ACCESS rule %q uses an absolute path - recipes should use workspace-relative paths (the agent CWD is its own directory)", e.Name(), tl)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no recipe .md files found to validate")
	}
}

// parseRecipeFrontmatter reads the leading --- frontmatter block into a map.
func parseRecipeFrontmatter(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	fm := map[string]string{}
	sc := bufio.NewScanner(f)
	inFront := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			if !inFront {
				inFront = true
				continue
			}
			break
		}
		if !inFront {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			fm[key] = val
		}
	}
	if !inFront {
		t.Fatalf("%s: missing frontmatter (must start with ---)", path)
	}
	return fm
}

// splitList extracts identifiers from a YAML inline list like "[a, b, c]".
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
