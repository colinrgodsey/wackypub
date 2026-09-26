package cmd

import (
	"strings"
	"testing"
)

// The bundled skills are wired from main.go, which tests do not run, so set them
// the way main.go does. Kept minimal: this asserts the CLI surface (names and
// resolution), not the skill contents.
func initBundledSkillsForTest() {
	BundledA2ASkill = "---\nname: wackypub-a2a\ndescription: a2a skill\n---\nbody\n"
	BundledWSSkill = "---\nname: wackypub-ws\ndescription: ws skill\n---\nbody\n"
	BundledScratchpadSkill = "---\nname: scratchpad-efficiency\ndescription: scratchpad skill\n---\nbody\n"
}

func TestGetSkillContent_ResolvesEveryBundledSkill(t *testing.T) {
	initBundledSkillsForTest()
	for _, name := range []string{"a2a", "wackypub-a2a", "ws", "workspace", "scratchpad", "scratchpad-efficiency"} {
		got, err := GetSkillContent(name)
		if err != nil {
			t.Fatalf("GetSkillContent(%q) returned error: %v", name, err)
		}
		if !strings.Contains(got, "body") {
			t.Errorf("GetSkillContent(%q) did not return skill content: %q", name, got)
		}
	}
}

func TestGetSkillContent_UnknownNameListsScratchpad(t *testing.T) {
	_, err := GetSkillContent("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown skill name")
	}
	if !strings.Contains(err.Error(), "scratchpad") {
		t.Errorf("unknown-skill error should list the scratchpad skill, got: %v", err)
	}
}

func TestBundledSkills_IncludesScratchpad(t *testing.T) {
	initBundledSkillsForTest()
	skills, err := bundledSkills()
	if err != nil {
		t.Fatalf("bundledSkills() returned error: %v", err)
	}
	var names []string
	for _, sk := range skills {
		names = append(names, sk.ShortName)
	}
	for _, want := range []string{"a2a", "ws", "scratchpad"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("bundled skills %v missing %q", names, want)
		}
	}
}
