package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newHygieneTestAgent(t *testing.T) (wsDir, agentID, agentDir string) {
	t.Helper()
	wsDir = t.TempDir()
	agentID = "testagent"
	agentDir = filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	return wsDir, agentID, agentDir
}

func writeSkill(t *testing.T, agentDir, dirName, frontmatterName, body string) {
	t.Helper()
	skillDir := filepath.Join(agentDir, "skills", dirName)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir %s: %v", dirName, err)
	}
	var content string
	if frontmatterName != "" {
		content = "---\nname: " + frontmatterName + "\ndescription: a test skill\n---\n" + body
	} else {
		content = body
	}
	if err := os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md for %s: %v", dirName, err)
	}
}

func TestValidateSkillNameParity_MismatchFlagged(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	writeSkill(t, agentDir, "swarm", "gitkb-swarm", "body")

	issues := validateSkillNameParity(agentDir)
	if len(issues) != 1 {
		t.Fatalf("expected exactly 1 issue, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0], `"swarm"`) || !strings.Contains(issues[0], `"gitkb-swarm"`) {
		t.Errorf("expected issue to name both the directory and the frontmatter name, got: %s", issues[0])
	}
}

func TestValidateSkillNameParity_NoFrontmatterName_NoIssue(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	writeSkill(t, agentDir, "coding", "", "body with no frontmatter name")

	issues := validateSkillNameParity(agentDir)
	if len(issues) != 0 {
		t.Errorf("expected no issues when frontmatter omits name (falls back to directory name), got: %v", issues)
	}
}

func TestValidateSkillNameParity_MatchingExplicitName_NoIssue(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	writeSkill(t, agentDir, "coding", "coding", "body")

	issues := validateSkillNameParity(agentDir)
	if len(issues) != 0 {
		t.Errorf("expected no issue when the explicit frontmatter name matches the directory, got: %v", issues)
	}
}

func TestValidateSkillNameParity_NoSkillsDir_NoIssue(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	issues := validateSkillNameParity(agentDir)
	if len(issues) != 0 {
		t.Errorf("expected no issues for an agent with no skills/ at all, got: %v", issues)
	}
}

func TestValidateAgentHygiene_DeadMacroReference(t *testing.T) {
	wsDir, agentID, agentDir := newHygieneTestAgent(t)
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are an agent.\n@missing-file.md\n"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	issues := ValidateAgentHygiene(wsDir, agentID)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "@missing-file.md") && strings.Contains(issue, "does not exist") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a dead-macro-reference issue naming @missing-file.md, got: %v", issues)
	}
}

func TestValidateAgentHygiene_ValidMacroReference_NoDeadRefIssue(t *testing.T) {
	wsDir, agentID, agentDir := newHygieneTestAgent(t)
	if err := os.WriteFile(filepath.Join(agentDir, "rules.md"), []byte("Rule 1."), 0644); err != nil {
		t.Fatalf("failed to write rules.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("You are an agent.\n@rules.md\n"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	issues := ValidateAgentHygiene(wsDir, agentID)
	for _, issue := range issues {
		if strings.Contains(issue, "does not exist") {
			t.Errorf("expected no dead-macro-reference issue for a macro that resolves, got: %v", issues)
		}
	}
}

func TestValidateMacroReferences_DuplicateReference_OneIssueOnly(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("@gone.md and again @gone.md"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	issues := validateMacroReferences(agentDir)
	if len(issues) != 1 {
		t.Errorf("expected exactly 1 issue for a target referenced twice, got %d: %v", len(issues), issues)
	}
}

func TestValidateMacroReferences_DirectoryTarget_Flagged(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	if err := os.MkdirAll(filepath.Join(agentDir, "adir"), 0755); err != nil {
		t.Fatalf("failed to create adir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("@adir"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	issues := validateMacroReferences(agentDir)
	if len(issues) != 1 {
		t.Fatalf("expected exactly 1 issue for a directory target, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0], "directory") {
		t.Errorf("expected the issue to call out that the target is a directory, got: %s", issues[0])
	}
}

func TestValidateMacroReferences_NoAgentsMd_NoIssue(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	issues := validateMacroReferences(agentDir)
	if len(issues) != 0 {
		t.Errorf("expected no issues when AGENTS.md doesn't exist at all, got: %v", issues)
	}
}

func TestValidateAlwaysOnBudget_OverThreshold_Flagged(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	// contextWindow 1000 tokens * 15% = 150 tokens = 600 bytes threshold (4 bytes/token).
	// 700 bytes of rendered content estimates to 175 tokens, over the 150-token threshold.
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"contextWindow": 1000}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	rendered := strings.Repeat("x", 700)
	issues := validateAlwaysOnBudget(agentDir, rendered)
	if len(issues) != 1 {
		t.Fatalf("expected exactly 1 budget issue, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0], "175") || !strings.Contains(issues[0], "1000-token") {
		t.Errorf("expected the issue to name the estimated and context-window token counts, got: %s", issues[0])
	}
}

func TestValidateAlwaysOnBudget_UnderThreshold_NoIssue(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"contextWindow": 1000}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	rendered := strings.Repeat("x", 100)
	issues := validateAlwaysOnBudget(agentDir, rendered)
	if len(issues) != 0 {
		t.Errorf("expected no budget issue for a small always-on prefix, got: %v", issues)
	}
}

func TestValidateAlwaysOnBudget_IncludesMemoryMd(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(`{"contextWindow": 1000}`), 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}
	// 100 bytes rendered + 600 bytes MEMORY.md = 700 bytes -> 175 tokens, over the
	// 150-token threshold - proving MEMORY.md is actually counted, not just AGENTS.md.
	if err := os.WriteFile(filepath.Join(agentDir, "MEMORY.md"), []byte(strings.Repeat("y", 600)), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	rendered := strings.Repeat("x", 100)
	issues := validateAlwaysOnBudget(agentDir, rendered)
	if len(issues) != 1 {
		t.Fatalf("expected MEMORY.md's size to push this over threshold, got %d issues: %v", len(issues), issues)
	}
}

func TestValidateAlwaysOnBudget_NoContextWindow_NoIssue(t *testing.T) {
	_, _, agentDir := newHygieneTestAgent(t)
	// No runtime.json at all -> LoadRuntimeConfig falls back to the bundled default,
	// which does carry a contextWindow, so this exercises that a huge always-on prefix
	// against the *bundled default's* window still produces a sane result either way:
	// just confirm it never panics and only fires when genuinely over threshold.
	rendered := strings.Repeat("x", 10)
	issues := validateAlwaysOnBudget(agentDir, rendered)
	if len(issues) != 0 {
		t.Errorf("expected no issue for a tiny always-on prefix against the bundled default window, got: %v", issues)
	}
}

func TestValidateAgentHygiene_BridgedOrEmptyAgent_NoCrash(t *testing.T) {
	wsDir, agentID, _ := newHygieneTestAgent(t)
	// No AGENTS.md, no skills/, no runtime.json at all - must degrade gracefully, the
	// same way the rest of InspectAgent's diagnostics do for a not-yet-set-up agent.
	issues := ValidateAgentHygiene(wsDir, agentID)
	if issues == nil {
		return
	}
	for _, issue := range issues {
		if issue == "" {
			t.Errorf("expected no empty issue strings, got: %v", issues)
		}
	}
}
