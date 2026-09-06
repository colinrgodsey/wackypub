package agent

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

func TestParseSkillFile(t *testing.T) {
	tmpDir := t.TempDir()
	skillPath := filepath.Join(tmpDir, "SKILL.md")

	content := `---
name: testing-guide
description: How to write Go unit tests
always_load: true
---
# Testing Guide
Always use t.Fatalf for fatal assertions.
`
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	sk, err := ParseSkillFile(skillPath)
	if err != nil {
		t.Fatalf("ParseSkillFile failed: %v", err)
	}

	if sk.Name != "testing-guide" {
		t.Errorf("expected Name 'testing-guide', got %q", sk.Name)
	}
	if sk.Description != "How to write Go unit tests" {
		t.Errorf("expected Description 'How to write Go unit tests', got %q", sk.Description)
	}
	if !sk.AlwaysLoad {
		t.Errorf("expected AlwaysLoad true, got false")
	}
	if !strings.Contains(sk.Body, "# Testing Guide") {
		t.Errorf("expected Body to contain '# Testing Guide', got: %q", sk.Body)
	}
}

func TestDiscoverAgentSkills_AndAutoload(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	skillsDir := filepath.Join(agentDir, "skills")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatalf("failed to create skills dir: %v", err)
	}

	// 1. On-demand skill (always_load: false)
	sk1Dir := filepath.Join(skillsDir, "git-workflow")
	if err := os.MkdirAll(sk1Dir, 0755); err != nil {
		t.Fatalf("failed to create git-workflow dir: %v", err)
	}
	sk1Content := `---
name: git-workflow
description: Best practices for git commits
---
Use clear commit messages.
`
	if err := os.WriteFile(filepath.Join(sk1Dir, "SKILL.md"), []byte(sk1Content), 0644); err != nil {
		t.Fatalf("failed to write git-workflow SKILL.md: %v", err)
	}

	// 2. Always-loaded skill (always_load: true)
	sk2Dir := filepath.Join(skillsDir, "safety-rules")
	if err := os.MkdirAll(sk2Dir, 0755); err != nil {
		t.Fatalf("failed to create safety-rules dir: %v", err)
	}
	sk2Content := `---
name: safety-rules
description: Critical safety directives
always_load: true
---
Never run rm -rf /
`
	if err := os.WriteFile(filepath.Join(sk2Dir, "SKILL.md"), []byte(sk2Content), 0644); err != nil {
		t.Fatalf("failed to write safety-rules SKILL.md: %v", err)
	}

	skillsMap, onDemand, alwaysLoaded, err := DiscoverAgentSkills(agentDir)
	if err != nil {
		t.Fatalf("DiscoverAgentSkills failed: %v", err)
	}

	if len(skillsMap) != 2 {
		t.Fatalf("expected 2 total skills, got %d", len(skillsMap))
	}
	if len(onDemand) != 1 || onDemand[0].Name != "git-workflow" {
		t.Errorf("expected 1 on-demand skill 'git-workflow', got %v", onDemand)
	}
	if len(alwaysLoaded) != 1 || alwaysLoaded[0].Name != "safety-rules" {
		t.Errorf("expected 1 always-loaded skill 'safety-rules', got %v", alwaysLoaded)
	}

	// Test RenderAutoloadedSkills
	autoloadBlock, err := RenderAutoloadedSkills(agentDir)
	if err != nil {
		t.Fatalf("RenderAutoloadedSkills failed: %v", err)
	}

	if !strings.Contains(autoloadBlock, "<AUTOLOADED_SKILLS>") {
		t.Errorf("expected autoload block to contain <AUTOLOADED_SKILLS>, got: %s", autoloadBlock)
	}
	if !strings.Contains(autoloadBlock, `<SKILL name="safety-rules" description="Critical safety directives">`) {
		t.Errorf("expected autoload block to contain <SKILL name=\"safety-rules\">, got: %s", autoloadBlock)
	}
	if !strings.Contains(autoloadBlock, "Never run rm -rf /") {
		t.Errorf("expected autoload block to contain body, got: %s", autoloadBlock)
	}

	// Test RenderAgentSystemPrompt integrates autoload block
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	prompt, err := RenderAgentSystemPrompt(wsDir, "bob")
	if err != nil {
		t.Fatalf("RenderAgentSystemPrompt failed: %v", err)
	}
	if !strings.Contains(prompt, "System prompt Bob") || !strings.Contains(prompt, "<AUTOLOADED_SKILLS>") {
		t.Errorf("RenderAgentSystemPrompt missing expected content: %s", prompt)
	}
}

type loadSkillTestModel struct {
	skillName string
}

func (m *loadSkillTestModel) Name() string { return "load-skill-test-model" }

func (m *loadSkillTestModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		res := &model.LLMResponse{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "load_skill",
							Args: map[string]any{
								"name": m.skillName,
							},
						},
					},
				},
			},
		}
		yield(res, nil)
	}
}

func TestLoadSkillToolExecution(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	skillsDir := filepath.Join(agentDir, "skills")
	skDir := filepath.Join(skillsDir, "debugging")
	if err := os.MkdirAll(skDir, 0755); err != nil {
		t.Fatalf("failed to create debugging skill dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt Bob"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"model":"dummy-model","endpoint":"http://localhost:1234/v1"}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	skContent := `---
name: debugging
description: Debugging steps
---
Step 1: Check logs.
`
	if err := os.WriteFile(filepath.Join(skDir, "SKILL.md"), []byte(skContent), 0644); err != nil {
		t.Fatalf("failed to write debugging SKILL.md: %v", err)
	}

	toolMap, decls, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}

	// 10 tools: create_scratchpad, get_scratchpad, list_scratchpads, search_scratchpad, delete_scratchpad, run_command, load_skill, load_skill_extra, list_skill_extra, run_skill_script
	if len(toolMap) != 10 {
		t.Errorf("expected 10 tools, got %d", len(toolMap))
	}
	if len(decls) != 10 {
		t.Errorf("expected 10 decls, got %d", len(decls))
	}

	loadSkillTool, ok := toolMap["load_skill"]
	if !ok {
		t.Fatalf("missing load_skill tool in toolMap")
	}

	decler := loadSkillTool.(interface {
		Declaration() *genai.FunctionDeclaration
	})
	desc := decler.Declaration().Description
	if !strings.Contains(desc, "- debugging: Debugging steps") {
		t.Errorf("expected description to contain '- debugging: Debugging steps', got: %s", desc)
	}

	// Test loading valid skill via GenerateTurn
	fa, err := LoadFolderAgent(wsDir, "bob", 1)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	fa.Model = &loadSkillTestModel{skillName: "debugging"}

	toolsList := []tool.Tool{toolMap["create_scratchpad"], toolMap["get_scratchpad"], toolMap["list_scratchpads"], toolMap["run_command"], loadSkillTool}
	fa.ADKAgent, err = BuildADKAgent("bob", fa.SystemPrompt, 1, fa.Model, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgent failed: %v", err)
	}

	uMsg := genai.NewContentFromText("load debugging skill", "user")
	if err := AppendSessionContent(agentDir, uMsg); err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	_, _ = fa.GenerateTurn(context.Background())

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}

	foundOutput := false
	for _, turn := range turns {
		if turn.Role == "user" {
			for _, part := range turn.Parts {
				if part.FunctionResponse != nil {
					respJSON, _ := json.Marshal(part.FunctionResponse.Response)
					if strings.Contains(string(respJSON), "Step 1: Check logs.") {
						foundOutput = true
					}
				}
			}
		}
	}
	if !foundOutput {
		t.Errorf("expected to find FunctionResponse containing 'Step 1: Check logs.' in session.jsonl")
	}
}

func TestParseSkillFile_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	skillPath := filepath.Join(tmpDir, "SKILL.md")

	content := `---
name: [invalid yaml syntax
description: {{bad syntax
---
# Invalid Frontmatter
`
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write invalid SKILL.md: %v", err)
	}

	_, err := ParseSkillFile(skillPath)
	if err == nil {
		t.Fatalf("expected error parsing invalid YAML frontmatter, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse YAML frontmatter") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestDiscoverAgentSkills_Shadowing(t *testing.T) {
	agentDir := t.TempDir()
	skillsDir := filepath.Join(agentDir, "skills")

	dir1 := filepath.Join(skillsDir, "a", "my-skill")
	dir2 := filepath.Join(skillsDir, "b", "my-skill")
	if err := os.MkdirAll(dir1, 0755); err != nil {
		t.Fatalf("failed to create dir1: %v", err)
	}
	if err := os.MkdirAll(dir2, 0755); err != nil {
		t.Fatalf("failed to create dir2: %v", err)
	}

	sk1 := `---
name: my-skill
description: Skill A
---
Body A
`
	sk2 := `---
name: my-skill
description: Skill B
---
Body B
`
	if err := os.WriteFile(filepath.Join(dir1, "SKILL.md"), []byte(sk1), 0644); err != nil {
		t.Fatalf("failed to write sk1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "SKILL.md"), []byte(sk2), 0644); err != nil {
		t.Fatalf("failed to write sk2: %v", err)
	}

	_, _, _, shadowed, err := DiscoverAgentSkillsMap(agentDir)
	if err != nil {
		t.Fatalf("DiscoverAgentSkillsMap failed: %v", err)
	}

	if len(shadowed) != 1 {
		t.Fatalf("expected 1 shadowed warning, got %d: %v", len(shadowed), shadowed)
	}
	if !strings.Contains(shadowed[0], "skill \"my-skill\"") || !strings.Contains(shadowed[0], "is shadowed by") {
		t.Errorf("unexpected shadowed warning message: %s", shadowed[0])
	}
}

type funcCallTestModel struct {
	toolName string
	toolArgs map[string]any
	calls    int
}

func (m *funcCallTestModel) Name() string { return "func-call-test-model" }

func (m *funcCallTestModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.calls++
		var res *model.LLMResponse
		if m.calls == 1 {
			res = &model.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								Name: m.toolName,
								Args: m.toolArgs,
							},
						},
					},
				},
			}
		} else {
			res = &model.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{Text: "Task completed successfully."},
					},
				},
			}
		}
		yield(res, nil)
	}
}

func TestSkillExtraAndScriptTools(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "doc_bot")
	skillsDir := filepath.Join(agentDir, "skills", "doc-gen")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	skillContent := `---
name: doc-gen
description: Documentation generator skill
always_load: true
---
Use load_skill_extra and run_skill_script for extras.
`
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatalf("failed writing SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("Prompt DocBot"), 0644); err != nil {
		t.Fatalf("failed writing AGENTS.md: %v", err)
	}

	refDir := filepath.Join(skillsDir, "reference")
	if err := os.MkdirAll(refDir, 0755); err != nil {
		t.Fatalf("failed creating ref dir: %v", err)
	}
	refText := `{"schema_version": "1.0.0"}`
	if err := os.WriteFile(filepath.Join(refDir, "schema.json"), []byte(refText), 0644); err != nil {
		t.Fatalf("failed writing schema.json: %v", err)
	}

	imgDir := filepath.Join(skillsDir, "images")
	if err := os.MkdirAll(imgDir, 0755); err != nil {
		t.Fatalf("failed creating img dir: %v", err)
	}
	pngHeader := createTestImage(400, 300, false)
	if err := os.WriteFile(filepath.Join(imgDir, "diag.png"), pngHeader, 0644); err != nil {
		t.Fatalf("failed writing diag.png: %v", err)
	}

	scriptsDir := filepath.Join(skillsDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		t.Fatalf("failed creating scripts dir: %v", err)
	}
	scriptContent := "#!/bin/sh\necho \"ARG1: $1\"\nif [ -n \"$2\" ]; then echo \"ARG2: $2\"; fi\n"
	scriptPath := filepath.Join(scriptsDir, "test.sh")
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed writing test.sh: %v", err)
	}

	nonExecPath := filepath.Join(scriptsDir, "non_exec.sh")
	if err := os.WriteFile(nonExecPath, []byte(scriptContent), 0644); err != nil {
		t.Fatalf("failed writing non_exec.sh: %v", err)
	}

	// runtime.json with maxImageDimension = 400
	runtimeJSON := `{"model":"test-model","endpoint":"http://localhost:1234/v1","maxImageDimension":400}`
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("failed writing runtime.json: %v", err)
	}

	// 1. Test ListSkillExtraFiles helper
	extraFiles, err := ListSkillExtraFiles(skillsDir)
	if err != nil {
		t.Fatalf("ListSkillExtraFiles failed: %v", err)
	}
	if len(extraFiles) != 4 {
		t.Errorf("expected 4 extra files, got %d: %v", len(extraFiles), extraFiles)
	}

	// 2. Test ResolveSkillRelativePath bounds checking
	resolved, err := ResolveSkillRelativePath(skillsDir, "reference/schema.json")
	if err != nil {
		t.Fatalf("ResolveSkillRelativePath failed: %v", err)
	}
	if !strings.HasSuffix(resolved, "reference/schema.json") {
		t.Errorf("unexpected resolved path: %s", resolved)
	}

	_, err = ResolveSkillRelativePath(skillsDir, "../../../etc/passwd")
	if err == nil {
		t.Errorf("expected path traversal to fail, got nil")
	}

	// 3. Test load_skill_extra (text) via GenerateTurn
	faText, err := LoadFolderAgent(wsDir, "doc_bot", 10)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	faText.Model = &funcCallTestModel{
		toolName: "load_skill_extra",
		toolArgs: map[string]any{
			"skill_name":    "doc-gen",
			"relative_path": "reference/schema.json",
		},
	}
	toolsMap, _, err := BuildFolderAgentTools(agentDir)
	if err != nil {
		t.Fatalf("BuildFolderAgentTools failed: %v", err)
	}
	var toolsList []tool.Tool
	for _, tl := range toolsMap {
		toolsList = append(toolsList, tl)
	}
	faText.ADKAgent, err = BuildADKAgentWithConfigAndTracker("doc_bot", faText.SystemPrompt, 10, faText.RuntimeConfig, faText.Model, agentDir, faText.UsageTracker, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfigAndTracker failed: %v", err)
	}

	_ = AppendSessionContent(agentDir, genai.NewContentFromText("load schema", "user"))
	_, _ = faText.GenerateTurn(context.Background())

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	foundSchema := false
	for _, t := range turns {
		if t.Role == "user" {
			for _, p := range t.Parts {
				if p.FunctionResponse != nil && p.FunctionResponse.Name == "load_skill_extra" {
					respJSON, _ := json.Marshal(p.FunctionResponse.Response)
					if strings.Contains(string(respJSON), "schema_version") {
						foundSchema = true
					}
				}
			}
		}
	}
	if !foundSchema {
		t.Errorf("expected FunctionResponse to contain schema_version in session")
	}

	// 4. Test run_skill_script via GenerateTurn
	faScript, err := LoadFolderAgent(wsDir, "doc_bot", 10)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	faScript.Model = &funcCallTestModel{
		toolName: "run_skill_script",
		toolArgs: map[string]any{
			"skill_name":    "doc-gen",
			"relative_path": "scripts/test.sh",
			"args":          []string{"foo", "bar"},
		},
	}
	faScript.ADKAgent, err = BuildADKAgentWithConfigAndTracker("doc_bot", faScript.SystemPrompt, 10, faScript.RuntimeConfig, faScript.Model, agentDir, faScript.UsageTracker, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfigAndTracker failed: %v", err)
	}

	_ = AppendSessionContent(agentDir, genai.NewContentFromText("run script", "user"))
	_, _ = faScript.GenerateTurn(context.Background())

	turns, _ = ReadSessionTurns(agentDir)
	foundScriptOut := false
	for _, t := range turns {
		if t.Role == "user" {
			for _, p := range t.Parts {
				if p.FunctionResponse != nil && p.FunctionResponse.Name == "run_skill_script" {
					respJSON, _ := json.Marshal(p.FunctionResponse.Response)
					if strings.Contains(string(respJSON), "ARG1: foo") && strings.Contains(string(respJSON), "ARG2: bar") {
						foundScriptOut = true
					}
				}
			}
		}
	}
	if !foundScriptOut {
		t.Errorf("expected FunctionResponse to contain ARG1: foo and ARG2: bar in session")
	}

	// 5. Test load_skill_extra with binary image queues deferred image turn
	faImg, err := LoadFolderAgent(wsDir, "doc_bot", 10)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}
	faImg.Model = &funcCallTestModel{
		toolName: "load_skill_extra",
		toolArgs: map[string]any{
			"skill_name":    "doc-gen",
			"relative_path": "images/diag.png",
		},
	}
	faImg.ADKAgent, err = BuildADKAgentWithConfigAndTracker("doc_bot", faImg.SystemPrompt, 10, faImg.RuntimeConfig, faImg.Model, agentDir, faImg.UsageTracker, toolsList...)
	if err != nil {
		t.Fatalf("BuildADKAgentWithConfigAndTracker failed: %v", err)
	}

	_ = AppendSessionContent(agentDir, genai.NewContentFromText("load image", "user"))
	respText, err := faImg.GenerateTurn(context.Background())
	if err != nil {
		t.Fatalf("GenerateTurn failed: %v", err)
	}
	if !strings.Contains(respText, "queued") {
		t.Errorf("expected response text to mention queued image, got: %s", respText)
	}

	turns, _ = ReadSessionTurns(agentDir)
	var foundImageTurn bool
	for _, t := range turns {
		if t.Role == "user" && len(t.Parts) == 2 && t.Parts[1].InlineData != nil {
			foundImageTurn = true
			break
		}
	}
	if !foundImageTurn {
		t.Errorf("expected deferred image user turn in session turns: %+v", turns)
	}
}

func TestRenderAutoloadedSkills_DescriptionSanitized(t *testing.T) {
	wsDir := t.TempDir()
	skillDir := filepath.Join(wsDir, "testbot", "skills", "danger-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: danger-skill\ndescription: \"Uses \\\"quotes\\\" and <brackets>;\n  spans multiple lines\"\nalways_load: true\n---\n\nBody line.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	block, err := RenderAutoloadedSkills(filepath.Join(wsDir, "testbot"))
	if err != nil {
		t.Fatalf("RenderAutoloadedSkills failed: %v", err)
	}
	if strings.Contains(block, "\"\"") && strings.Contains(block, "description=\"\"") {
		t.Errorf("expected quotes stripped from description attribute")
	}
	if strings.Contains(block, "<brackets>") {
		t.Errorf("expected angle brackets stripped from description")
	}
	if !strings.Contains(block, `description="Uses quotes and brackets; spans multiple lines"`) {
		t.Errorf("expected sanitized single-line description, got: %s", block)
	}
}

func TestRenderAutoloadedSkills_DefaultDescriptionEmitted(t *testing.T) {
	wsDir := t.TempDir()
	skillDir := filepath.Join(wsDir, "testbot", "skills", "plain-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: plain-skill\nalways_load: true\n---\n\nBody only.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	block, err := RenderAutoloadedSkills(filepath.Join(wsDir, "testbot"))
	if err != nil {
		t.Fatalf("RenderAutoloadedSkills failed: %v", err)
	}
	// ParseSkillFile fills a default description ("Skill <name>") when frontmatter omits it,
	// so the attribute is still emitted - just with the default value.
	if !strings.Contains(block, `<SKILL name="plain-skill" description="Skill plain-skill">`) {
		t.Errorf("expected default description attribute, got: %s", block)
	}
}

func TestFormatLoadedSkill_RealNewlines(t *testing.T) {
	result := FormatLoadedSkill("test-skill", "body content")
	if strings.Contains(result, "\\n") {
		t.Errorf("expected real newlines, not literal backslash-n, got: %q", result)
	}
	if !strings.Contains(result, "body content") {
		t.Errorf("expected body content, got: %q", result)
	}
	if !strings.Contains(result, `name="test-skill"`) {
		t.Errorf("expected name attribute, got: %q", result)
	}
}
