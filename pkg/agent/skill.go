package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	SkillsDirName = "skills"
	SkillFileName = "SKILL.md"
)

type SkillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	AlwaysLoad  bool   `yaml:"always_load"`
}

type Skill struct {
	Name        string
	Description string
	AlwaysLoad  bool
	Body        string
	Path        string
}

// ParseSkillFile reads SKILL.md at filePath and parses optional YAML frontmatter.
func ParseSkillFile(filePath string) (*Skill, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read skill file %s: %w", filePath, err)
	}

	folderName := filepath.Base(filepath.Dir(filePath))
	sk, err := ParseSkillContent(string(data), folderName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse skill file %s: %w", filePath, err)
	}
	sk.Path = filePath
	return sk, nil
}

// ParseSkillContent parses a SKILL.md document's optional YAML frontmatter and body from
// an in-memory string (e.g. one embedded via go:embed) rather than a file on disk.
// fallbackName is used as Name when the frontmatter doesn't specify one.
func ParseSkillContent(content string, fallbackName string) (*Skill, error) {
	var fm SkillFrontmatter
	body := content

	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "---") {
		parts := strings.SplitN(trimmed[3:], "---", 2)
		if len(parts) == 2 {
			yamlText := parts[0]
			body = strings.TrimSpace(parts[1])
			if err := yaml.Unmarshal([]byte(yamlText), &fm); err != nil {
				return nil, fmt.Errorf("failed to parse YAML frontmatter: %w", err)
			}
		}
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = fallbackName
	}

	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		desc = fmt.Sprintf("Skill %s", name)
	}

	return &Skill{
		Name:        name,
		Description: desc,
		AlwaysLoad:  fm.AlwaysLoad,
		Body:        body,
	}, nil
}

// DiscoverAgentSkillsMap walks <agentDir>/skills/ recursively looking for SKILL.md files according to D20.
// Resolves directory and file symlinks and follows them, preventing infinite symlink cycles.
// Returns:
//   - skillsMap: map of skillName -> *Skill
//   - onDemandSkills: sorted slice of *Skill where AlwaysLoad is false
//   - alwaysLoadedSkills: sorted slice of *Skill where AlwaysLoad is true
//   - shadowed: shadowing warning messages
//   - error
func DiscoverAgentSkillsMap(agentDir string) (map[string]*Skill, []*Skill, []*Skill, []string, error) {
	skillsDir := filepath.Join(agentDir, SkillsDirName)
	if !pathExists(skillsDir) {
		return make(map[string]*Skill), nil, nil, nil, nil
	}

	skillsMap := make(map[string]*Skill)
	skillPaths := make(map[string]string)
	var shadowed []string
	visitedDirs := make(map[string]bool)

	var walk func(dir string) error
	walk = func(dir string) error {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil // Skip unresolvable symlinks
		}
		if visitedDirs[realDir] {
			return nil // Cycle prevention
		}
		visitedDirs[realDir] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}

			if info.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			} else if strings.EqualFold(entry.Name(), SkillFileName) {
				sk, err := ParseSkillFile(path)
				if err != nil {
					return err
				}
				if existingPath, exists := skillPaths[sk.Name]; exists {
					shadowed = append(shadowed, fmt.Sprintf("skill %q at %s is shadowed by %s", sk.Name, existingPath, path))
				} else {
					skillPaths[sk.Name] = path
					skillsMap[sk.Name] = sk
				}
			}
		}
		return nil
	}

	if err := walk(skillsDir); err != nil {
		return nil, nil, nil, nil, err
	}

	var onDemand []*Skill
	var alwaysLoaded []*Skill

	for _, sk := range skillsMap {
		if sk.AlwaysLoad {
			alwaysLoaded = append(alwaysLoaded, sk)
		} else {
			onDemand = append(onDemand, sk)
		}
	}

	sort.Slice(onDemand, func(i, j int) bool {
		return onDemand[i].Name < onDemand[j].Name
	})

	sort.Slice(alwaysLoaded, func(i, j int) bool {
		return alwaysLoaded[i].Name < alwaysLoaded[j].Name
	})

	sort.Strings(shadowed)
	return skillsMap, onDemand, alwaysLoaded, shadowed, nil
}

// DiscoverAgentSkills walks <agentDir>/skills/ recursively looking for SKILL.md files according to D20.
// Returns:
//   - skillsMap: map of skillName -> *Skill
//   - onDemandSkills: sorted slice of *Skill where AlwaysLoad is false
//   - alwaysLoadedSkills: sorted slice of *Skill where AlwaysLoad is true
//   - error
func DiscoverAgentSkills(agentDir string) (map[string]*Skill, []*Skill, []*Skill, error) {
	skillsMap, onDemand, alwaysLoaded, _, err := DiscoverAgentSkillsMap(agentDir)
	return skillsMap, onDemand, alwaysLoaded, err
}

// RenderAutoloadedSkills formats always-loaded skills into the <AUTOLOADED_SKILLS> block.
func RenderAutoloadedSkills(agentDir string) (string, error) {
	_, _, alwaysLoaded, err := DiscoverAgentSkills(agentDir)
	if err != nil || len(alwaysLoaded) == 0 {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("<AUTOLOADED_SKILLS>\n")
	for _, sk := range alwaysLoaded {
		sb.WriteString(fmt.Sprintf("<SKILL name=%q>\n%s\n</SKILL>\n", sk.Name, sk.Body))
	}
	sb.WriteString("</AUTOLOADED_SKILLS>")
	return sb.String(), nil
}

// ResolveSkillRelativePath resolves relativePath bounded inside skillDir, preventing path traversal.
func ResolveSkillRelativePath(skillDir string, relativePath string) (string, error) {
	if relativePath == "" {
		return "", fmt.Errorf("relative path cannot be empty")
	}
	cleanRel := filepath.Clean(filepath.FromSlash(relativePath))
	if filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, "..") || cleanRel == "." {
		return "", fmt.Errorf("path %q escapes skill directory", relativePath)
	}

	targetPath := filepath.Join(skillDir, cleanRel)

	realSkillDir, err := filepath.EvalSymlinks(skillDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve skill directory: %w", err)
	}

	realTarget, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file %q does not exist in skill", relativePath)
		}
		return "", fmt.Errorf("failed to resolve file path: %w", err)
	}

	rel, err := filepath.Rel(realSkillDir, realTarget)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", fmt.Errorf("path %q escapes skill directory", relativePath)
	}

	return realTarget, nil
}

// ListSkillExtraFiles recursively lists relative paths of all extra files in skillDir other than SKILL.md.
func ListSkillExtraFiles(skillDir string) ([]string, error) {
	realSkillDir, err := filepath.EvalSymlinks(skillDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve skill directory: %w", err)
	}

	var files []string
	visited := make(map[string]bool)

	var walk func(dir string) error
	walk = func(dir string) error {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil
		}
		if visited[realDir] {
			return nil
		}
		visited[realDir] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}

			if info.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			} else {
				if strings.EqualFold(entry.Name(), SkillFileName) {
					continue
				}
				rel, err := filepath.Rel(realSkillDir, path)
				if err == nil && !strings.HasPrefix(rel, "..") {
					files = append(files, filepath.ToSlash(rel))
				}
			}
		}
		return nil
	}

	if err := walk(skillDir); err != nil {
		return nil, err
	}

	sort.Strings(files)
	return files, nil
}
