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
// nativeIDs are inspected in fixture order so the golden is deterministic.
var nativeIDs = []string{"native-broken", "native-full", "native-minimal", "native-absent"}

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
func renderWorkspace(t *testing.T, ws string, agentIDs []string) string {
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
	for _, id := range agentIDs {
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
	compareWorkspaceGolden(t, "workspace_native_only.golden", renderWorkspace(t, ws, nativeIDs))
}

// bridgedWorkspaceFixture mirrors how REMOTE_MANIFEST is actually written: one bridged
// agent that also has a folder and bridge state (the agy shape), one that exists only as a
// route, and a native agent that must keep rendering as a native.
func bridgedWorkspaceFixture(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeFixtureFile(t, filepath.Join(ws, adkAgent.RootMarkerFile), "")
	writeFixtureFile(t, filepath.Join(ws, "native-peer", "AGENTS.md"), "You are native-peer.\n")
	writeFixtureFile(t, filepath.Join(ws, "native-peer", "runtime.json"), `{"model":"test-model","apiKey":"sk-test","contextWindow":4096}`+"\n")

	agyFolder := filepath.Join(ws, "agy")
	writeFixtureFile(t, filepath.Join(agyFolder, "AGENTS.md"), "You are agy.\n")
	writeFixtureFile(t, filepath.Join(agyFolder, "acp-session.json"), `{"sessionId":"a276f8fc-384a-4f63-938b-5454742d4b92","agent_folder":"`+agyFolder+`","createdAt":"2026-09-18T01:04:21Z"}`+"\n")

	ghostFolder := filepath.Join(ws, "ghost")
	writeFixtureFile(t, filepath.Join(ws, adkAgent.RemoteManifestFile),
		"agy: /usr/local/bin/wackyacp --harness-cmd=/usr/local/bin/wackyagy --agent-folder="+agyFolder+"\n"+
			"ghost: /opt/bin/acpx-bridge --harness-cmd=/usr/local/bin/claude-agent-acp --agent-folder="+ghostFolder+"\n")
	return ws
}

func overviewOf(t *testing.T, ws string) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return printWorkspaceOverview(adkAgent.NewSDK(ws), ws)
	})
	if err != nil {
		t.Fatalf("printWorkspaceOverview: %v", err)
	}
	return out
}

func inspectionOf(t *testing.T, ws, agentID string) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return printAgentInspection(adkAgent.NewSDK(ws), agentID)
	})
	if err != nil {
		t.Fatalf("printAgentInspection(%s): %v", agentID, err)
	}
	return out
}

func tableRow(output, agentID string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), agentID) {
			return line
		}
	}
	return ""
}

func TestOverviewShowsTheRemoteBinaryOfBridgedAgents(t *testing.T) {
	out := overviewOf(t, bridgedWorkspaceFixture(t))

	if !strings.Contains(out, "Agents found: 3") {
		t.Fatalf("the manifest-only agent is not counted, output:\n%s", out)
	}

	// The column is the binary at the head of the route, never the harness handed to it:
	// remote binaries other than wackyacp are expected, and the harness is one of its arguments.
	if row := tableRow(out, "agy"); !strings.Contains(row, "wackyacp") || strings.Contains(row, "wackyagy") {
		t.Fatalf("agy row does not name the remote binary, row = %q", row)
	}
	if row := tableRow(out, "ghost"); !strings.Contains(row, "acpx-bridge") || strings.Contains(row, "claude-agent-acp") {
		t.Fatalf("ghost row does not name the remote binary, row = %q", row)
	}
	for _, row := range []string{tableRow(out, "agy"), tableRow(out, "ghost")} {
		if strings.Contains(row, "missing") {
			t.Fatalf("a bridged agent is still flagged as missing a runtime, row = %q", row)
		}
	}
	if row := tableRow(out, "native-peer"); !strings.Contains(row, "ok") || strings.Contains(row, "wackyacp") {
		t.Fatalf("the native peer changed columns, row = %q", row)
	}
}

// TestMixedWorkspaceRendersUnchanged pins the whole rendering of a workspace that mixes
// natives with bridged agents, including the tabwriter padding such a mix produces.
func TestMixedWorkspaceRendersUnchanged(t *testing.T) {
	ws := bridgedWorkspaceFixture(t)
	compareWorkspaceGolden(t, "workspace_mixed.golden", renderWorkspace(t, ws, []string{"agy", "ghost", "native-peer"}))
}

func TestInspectionOfBridgedAgentDescribesRouteNotMissingRuntime(t *testing.T) {
	ws := bridgedWorkspaceFixture(t)
	out := inspectionOf(t, ws, "agy")

	for _, want := range []string{
		"Remote route (REMOTE_MANIFEST):",
		"command                   /usr/local/bin/wackyacp",
		"harness                   wackyagy",
		"agent folder              " + filepath.Join(ws, "agy") + " (present)",
		"acp-session.json holds session a276f8fc-384a-4f63-938b-5454742d4b92, created 2026-09-18T01:04:21Z",
		"runtime.json              unused (this agent is bridged)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "runtime.json is missing - generation will fall back") {
		t.Fatalf("a bridged agent is told its runtime.json is required:\n%s", out)
	}
}

func TestInspectionOfManifestOnlyAgentExplainsTheBridge(t *testing.T) {
	ws := bridgedWorkspaceFixture(t)
	out := inspectionOf(t, ws, "ghost")

	for _, want := range []string{
		`Agent "ghost" has no local agent directory, and does not need one.`,
		"routes it to the claude-agent-acp harness",
		"agent folder              " + filepath.Join(ws, "ghost") + " (missing)",
		"no acp-session.json yet",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "does not exist yet") || strings.Contains(out, "To create it, add at minimum") {
		t.Fatalf("a bridged-only agent is told to create native files:\n%s", out)
	}
}

func TestInspectionReportsUnreadableBridgeSession(t *testing.T) {
	ws := bridgedWorkspaceFixture(t)
	writeFixtureFile(t, filepath.Join(ws, "agy", "acp-session.json"), "{not json\n")

	out := inspectionOf(t, ws, "agy")
	if !strings.Contains(out, "acp-session.json at ") || !strings.Contains(out, "is not valid JSON") {
		t.Fatalf("a corrupt acp-session.json is not reported:\n%s", out)
	}
}

func TestUnparsableManifestIsAnnouncedInsteadOfSilentlyNative(t *testing.T) {
	ws := bridgedWorkspaceFixture(t)
	routes, err := os.ReadFile(filepath.Join(ws, adkAgent.RemoteManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(ws, adkAgent.RemoteManifestFile), string(routes)+"ghost: /usr/local/bin/wackyacp\n")

	out := overviewOf(t, ws)
	if !strings.Contains(out, "REMOTE_MANIFEST did not parse") {
		t.Fatalf("a manifest that does not parse is not mentioned:\n%s", out)
	}
	if !strings.Contains(out, "native-peer") {
		t.Fatalf("a bad manifest stopped the native listing:\n%s", out)
	}
}

func TestHarnessNameComesFromTheRouteArgument(t *testing.T) {
	routes := []adkAgent.RemoteRoute{
		{Command: "/usr/bin/wackyacp", Args: []string{"--harness-cmd=/opt/bin/wackyagy"}},
		{Command: "/usr/bin/wackyacp", Args: []string{"--harness-cmd", "/opt/bin/claude-agent-acp"}},
		{Command: "/usr/bin/wackyacp", Args: []string{"-harness-cmd=/opt/bin/wackyagy", "--agent-folder=/tmp"}},
		{Command: "/opt/bin/acpx-bridge", Args: []string{"--agent-folder=/tmp"}},
		{Command: "/opt/bin/acpx-bridge", Args: []string{"--harness-cmd="}},
	}
	want := []string{"wackyagy", "claude-agent-acp", "wackyagy", "acpx-bridge", "acpx-bridge"}

	for i, route := range routes {
		if got := harnessName(route); got != want[i] {
			t.Fatalf("harnessName(%v %v) = %q, want %q", route.Command, route.Args, got, want[i])
		}
	}
}

func TestBridgeAgentFolderDefaultsToTheAgentDirectory(t *testing.T) {
	withFlag := adkAgent.RemoteRoute{Command: "wackyacp", Args: []string{"--agent-folder=/srv/claude"}}
	if got := bridgeAgentFolder("/ws", "claude", withFlag); got != "/srv/claude" {
		t.Fatalf("bridgeAgentFolder with --agent-folder = %q, want /srv/claude", got)
	}
	bare := adkAgent.RemoteRoute{Command: "wackyacp", Args: []string{"--permission-mode=approve"}}
	if got := bridgeAgentFolder("/ws", "claude", bare); got != filepath.Join("/ws", "claude") {
		t.Fatalf("bridgeAgentFolder without --agent-folder = %q, want the agent directory", got)
	}
}

func TestRouteArgsContainingSpacesStayQuoted(t *testing.T) {
	route := adkAgent.RemoteRoute{
		Command: "/usr/bin/wackyacp",
		Args:    []string{"--harness-cmd=/bin/wackyagy", "--harness-args=-a 1 -b 2", "-permission-mode=approve"},
	}
	want := `--harness-cmd=/bin/wackyagy "--harness-args=-a 1 -b 2" -permission-mode=approve`

	if got := renderArgs(route.RedactedArgs()); got != want {
		t.Fatalf("renderArgs = %s, want %s - a single argument that carries spaces must not print as three", got, want)
	}
}
