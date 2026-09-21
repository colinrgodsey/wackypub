package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type LoadSkillArgs struct {
	Name string `json:"name" jsonschema_description:"Name of the skill to load into conversation context"`
}

type LoadSkillResult struct {
	Output string `json:"output"`
}

type LoadSkillExtraArgs struct {
	SkillName    string `json:"skill_name" jsonschema_description:"Name of the skill whose extra file to read"`
	RelativePath string `json:"relative_path" jsonschema_description:"Relative path to the file inside the skill folder (e.g. reference/schema.md, images/sample.png)"`
}

type LoadSkillExtraResult struct {
	Output       string `json:"output,omitempty"`
	Deferred     bool   `json:"deferred,omitempty"`
	ScratchpadID string `json:"scratchpad_id,omitempty"`
}

type ListSkillExtraArgs struct {
	SkillName string `json:"skill_name" jsonschema_description:"Name of the skill whose extra files to list"`
}

type ListSkillExtraResult struct {
	Files []string `json:"files"`
	Count int      `json:"count"`
}

type RunSkillScriptArgs struct {
	SkillName    string            `json:"skill_name" jsonschema_description:"Name of the skill containing the script"`
	RelativePath string            `json:"relative_path" jsonschema_description:"Relative path to the executable script inside the skill folder (e.g. scripts/build.sh)"`
	Args         []string          `json:"args,omitempty" jsonschema_description:"List of CLI command line arguments passed positionally to the script (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
	Env          map[string]string `json:"env,omitempty" jsonschema_description:"Key-value object map of environment variables to set for the script invocation (not macro-expanded)"`
	Stdin        string            `json:"stdin,omitempty" jsonschema_description:"Optional stdin template string to pipe into the script (supports inline <SCRATCHPAD_DATA id=\"X\" /> macros)"`
}

type RunSkillScriptResult struct {
	Output   string   `json:"output"`
	Warning  string   `json:"warning,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// registerSkillTools builds the skill tools: load_skill, load_skill_extra,
// list_skill_extra, and run_skill_script.
func registerSkillTools(agentDir string, a2aMeta *A2AMetadata, timeoutSeconds int, addTool func(tool.Tool)) error {
	// 8. load_skill tool for on-demand skills
	skillsMap, onDemandSkills, _, err := DiscoverAgentSkills(agentDir)
	if err != nil {
		return fmt.Errorf("failed to discover agent skills: %w", err)
	}

	var skillLines []string
	for _, sk := range onDemandSkills {
		skillLines = append(skillLines, fmt.Sprintf("- %s: %s", sk.Name, sk.Description))
	}

	var skillListStr string
	if len(skillLines) > 0 {
		skillListStr = strings.Join(skillLines, "\n")
	} else {
		skillListStr = "none"
	}

	loadSkillDesc := fmt.Sprintf(
		"Loads an authoritative skill. Output rules and execution workflows are strictly binding.\n\n"+
			"Available skills:\n%s",
		skillListStr,
	)

	loadSkillTool, err := functiontool.New(functiontool.Config{
		Name:        "load_skill",
		Description: loadSkillDesc,
	}, func(ctx agent.Context, args LoadSkillArgs) (LoadSkillResult, error) {
		sk, ok := skillsMap[args.Name]
		if !ok || sk.AlwaysLoad {
			return LoadSkillResult{}, fmt.Errorf("unknown skill %q. See the tool description for the list of available skills", args.Name)
		}
		return LoadSkillResult{Output: FormatLoadedSkill(args.Name, sk.Body)}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create load_skill tool: %w", err)
	}
	addTool(loadSkillTool)

	// 9. load_skill_extra tool for reading reference files / images inside a skill
	loadSkillExtraDesc := "Read a reference document, example file, or image from within a skill's folder by relative path. Text content is returned directly; binary files are stored in a scratchpad entry."
	loadSkillExtraTool, err := functiontool.New(functiontool.Config{
		Name:        "load_skill_extra",
		Description: loadSkillExtraDesc,
	}, func(ctx agent.Context, args LoadSkillExtraArgs) (LoadSkillExtraResult, error) {
		sk, ok := skillsMap[args.SkillName]
		if !ok {
			return LoadSkillExtraResult{}, fmt.Errorf("unknown skill %q", args.SkillName)
		}
		skillDir := filepath.Dir(sk.Path)
		targetPath, err := ResolveSkillRelativePath(skillDir, args.RelativePath)
		if err != nil {
			return LoadSkillExtraResult{}, err
		}
		info, err := os.Stat(targetPath)
		if err != nil {
			return LoadSkillExtraResult{}, fmt.Errorf("failed to stat file %q: %w", args.RelativePath, err)
		}
		if info.IsDir() {
			return LoadSkillExtraResult{}, fmt.Errorf("%q is a directory, not a file. Use list_skill_extra to see available files", args.RelativePath)
		}
		data, err := os.ReadFile(targetPath)
		if err != nil {
			return LoadSkillExtraResult{}, fmt.Errorf("failed to read file %q: %w", args.RelativePath, err)
		}
		isBin, mimeType := DetectMediaType(data)
		if isBin {
			sanitizedPath := strings.ReplaceAll(filepath.Clean(filepath.ToSlash(args.RelativePath)), "/", "_")
			label := fmt.Sprintf("skill_%s_%s", args.SkillName, sanitizedPath)
			entry, err := CreateBinaryScratchpad(agentDir, data, label, mimeType)
			if err != nil {
				return LoadSkillExtraResult{}, fmt.Errorf("failed to store binary skill file in scratchpad: %w", err)
			}
			runtimeCfg := loadRuntimeCfgForGating(agentDir)
			if runtimeCfg != nil && runtimeCfg.MaxImageDimension > 0 && strings.HasPrefix(mimeType, "image/") {
				return LoadSkillExtraResult{
					Output:       fmt.Sprintf("Image (%s) from skill %q has been queued to scratchpad %s and will be available in your next turn.", mimeType, args.SkillName, entry.ID),
					Deferred:     true,
					ScratchpadID: entry.ID,
				}, nil
			}
			return LoadSkillExtraResult{
				Output:       fmt.Sprintf("Binary file (%s, %d bytes) stored in scratchpad entry %s.", mimeType, len(data), entry.ID),
				ScratchpadID: entry.ID,
			}, nil
		}
		return LoadSkillExtraResult{Output: string(data)}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create load_skill_extra tool: %w", err)
	}
	addTool(loadSkillExtraTool)

	// 10. list_skill_extra tool for recursively listing files in a skill folder
	listSkillExtraDesc := "Recursively list all extra reference files and bundled scripts inside a skill's folder, excluding SKILL.md itself."
	listSkillExtraTool, err := functiontool.New(functiontool.Config{
		Name:        "list_skill_extra",
		Description: listSkillExtraDesc,
	}, func(ctx agent.Context, args ListSkillExtraArgs) (ListSkillExtraResult, error) {
		sk, ok := skillsMap[args.SkillName]
		if !ok {
			return ListSkillExtraResult{}, fmt.Errorf("unknown skill %q", args.SkillName)
		}
		skillDir := filepath.Dir(sk.Path)
		files, err := ListSkillExtraFiles(skillDir)
		if err != nil {
			return ListSkillExtraResult{}, err
		}
		return ListSkillExtraResult{
			Files: files,
			Count: len(files),
		}, nil
	})
	if err != nil {
		return fmt.Errorf("failed to create list_skill_extra tool: %w", err)
	}
	addTool(listSkillExtraTool)

	// 11. run_skill_script tool for executing bundled executable scripts in a skill folder
	runSkillScriptDesc := "Execute a bundled executable script from inside a skill's folder by relative path. Reuses run_command execution semantics, macro expansion, and scratchpad redirection."
	runSkillScriptTool, err := functiontool.New(functiontool.Config{
		Name:        "run_skill_script",
		Description: runSkillScriptDesc,
	}, func(ctx agent.Context, args RunSkillScriptArgs) (RunSkillScriptResult, error) {
		return runSkillScriptToolHandler(ctx, agentDir, skillsMap, a2aMeta, timeoutSeconds, args)
	})
	if err != nil {
		return fmt.Errorf("failed to create run_skill_script tool: %w", err)
	}
	addTool(runSkillScriptTool)
	return nil
}

func runSkillScriptToolHandler(ctx context.Context, agentDir string, skillsMap map[string]*Skill, a2aMeta *A2AMetadata, timeoutSeconds int, args RunSkillScriptArgs) (RunSkillScriptResult, error) {
	sk, ok := skillsMap[args.SkillName]
	if !ok {
		return RunSkillScriptResult{}, fmt.Errorf("unknown skill %q", args.SkillName)
	}
	skillDir := filepath.Dir(sk.Path)
	targetPath, err := ResolveSkillRelativePath(skillDir, args.RelativePath)
	if err != nil {
		return RunSkillScriptResult{}, err
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		return RunSkillScriptResult{}, fmt.Errorf("failed to stat script %q: %w", args.RelativePath, err)
	}
	if info.IsDir() {
		return RunSkillScriptResult{}, fmt.Errorf("%q is a directory, not an executable script", args.RelativePath)
	}
	if info.Mode()&0111 == 0 {
		return RunSkillScriptResult{}, fmt.Errorf("script %q is not marked executable (mode %s)", args.RelativePath, info.Mode())
	}
	execArgs := ExecToolArgs{
		Args:  args.Args,
		Env:   args.Env,
		Stdin: args.Stdin,
	}
	out, warnings, err := executeTool(ctx, agentDir, filepath.Base(targetPath), targetPath, execArgs, a2aMeta, timeoutSeconds)
	var warnStr string
	if len(warnings) > 0 {
		warnStr = strings.Join(warnings, "\n")
	}
	return RunSkillScriptResult{Output: out, Warning: warnStr, Warnings: warnings}, err
}
