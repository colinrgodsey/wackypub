package cmd

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// updateWorkspaceGoldens rewrites the pinned rendering of `wackypub workspace` for the
// native-only fixture. The goldens were captured before the command knew anything about
// REMOTE_MANIFEST, so anything that shifts native rendering fails instead of quietly
// re-baselining. Re-run with the flag only when a change to native output is intended.
var updateWorkspaceGoldens = flag.Bool("update-workspace-goldens", false, "rewrite the pinned workspace rendering goldens")

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nativeOnlyWorkspaceFixture builds a workspace with no REMOTE_MANIFEST anywhere: every
// agent is a folder agent, one fully set up, one bare, one with a runtime.json that does
// not parse and a session line that does not either.
func nativeOnlyWorkspaceFixture(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeFixtureFile(t, filepath.Join(ws, adkAgent.RootMarkerFile), "")

	full := filepath.Join(ws, "native-full")
	writeFixtureFile(t, filepath.Join(full, "AGENTS.md"), "You are native-full.\n")
	writeFixtureFile(t, filepath.Join(full, "MEMORY.md"), "# Memory\n\n- a note\n")
	writeFixtureFile(t, filepath.Join(full, "runtime.json"), "{\"model\":\"test-model\",\"apiKey\":\"sk-test\",\"contextWindow\":4096}\n")
	writeFixtureFile(t, filepath.Join(full, "session.jsonl"), "{\"role\":\"user\",\"content\":\"hi\"}\n{\"role\":\"assistant\",\"content\":\"yo\"}\n")
	writeFixtureFile(t, filepath.Join(full, adkAgent.AllowedAgentsFile), "native-minimal\n")
	tool := filepath.Join(full, "tools", "hello")
	writeFixtureFile(t, tool, "#!/bin/sh\necho hello\n")
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(full, "skills", "greeting", "SKILL.md"), "# Greeting\n\nSay hello.\n")

	writeFixtureFile(t, filepath.Join(ws, "native-minimal", "AGENTS.md"), "You are native-minimal.\n")

	broken := filepath.Join(ws, "native-broken")
	writeFixtureFile(t, filepath.Join(broken, "AGENTS.md"), "You are native-broken.\n")
	writeFixtureFile(t, filepath.Join(broken, "runtime.json"), "{not json\n")
	writeFixtureFile(t, filepath.Join(broken, "session.jsonl"), "{\"role\":\"user\",\"content\":\"truncated\n")

	return ws
}

// renderWorkspace runs both modes of the command over ws and returns the combined output
// with the fixture directory normalised away, since t.TempDir differs every run.
func renderWorkspace(t *testing.T, ws string) string {
	t.Helper()
	sdk := adkAgent.NewSDK(ws)

	overview, err := captureStdout(t, func() error {
		return printWorkspaceOverview(sdk, ws)
	})
	if err != nil {
		t.Fatalf("printWorkspaceOverview: %v", err)
	}

	var b strings.Builder
	b.WriteString(overview)
	for _, id := range []string{"native-broken", "native-full", "native-minimal", "native-absent"} {
		inspection, err := captureStdout(t, func() error {
			return printAgentInspection(sdk, id)
		})
		if err != nil {
			t.Fatalf("printAgentInspection(%s): %v", id, err)
		}
		b.WriteString("===== inspect " + id + " =====\n")
		b.WriteString(inspection)
	}
	return strings.ReplaceAll(b.String(), ws, "<WS>")
}

func compareWorkspaceGolden(t *testing.T, golden string, got string) {
	t.Helper()
	path := filepath.Join("testdata", golden)
	if *updateWorkspaceGoldens {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("wrote %s", path)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("rendering changed for %s.\n--- want (golden) ---\n%s--- got ---\n%s", golden, want, got)
	}
}

func TestNativeOnlyWorkspaceRendersUnchanged(t *testing.T) {
	ws := nativeOnlyWorkspaceFixture(t)
	compareWorkspaceGolden(t, "workspace_native_only.golden", renderWorkspace(t, ws))
}
