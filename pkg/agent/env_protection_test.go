package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envProbeVars are the variables the probe tool prints. Add a name here and the script below to
// observe another variable in the child.
var envProbeVars = []string{"AGENT2AGENT", "WACKYPUB_CALL_CHAIN", "MY_CUSTOM_VAR", "OVERRIDDEN_VAR", "PATH", "HOME", "TMPDIR", "LANG"}

const envProbeUnset = "<unset>"

// newEnvProbeAgent builds an agent dir whose only tool echoes selected environment variables, so
// assertions run against what a real child process actually observes rather than against cmd.Env.
func newEnvProbeAgent(t *testing.T, dotenv string) (string, string) {
	t.Helper()
	agentDir := filepath.Join(t.TempDir(), "probebot")
	if err := os.MkdirAll(filepath.Join(agentDir, "tools"), 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(agentDir, ".env"), []byte(dotenv), 0644); err != nil {
			t.Fatalf("failed to write .env: %v", err)
		}
	}
	toolPath := filepath.Join(agentDir, "tools", "env_probe.sh")
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	for _, name := range envProbeVars {
		sb.WriteString("echo \"" + name + "=${" + name + "-" + envProbeUnset + "}\"\n")
	}
	if err := os.WriteFile(toolPath, []byte(sb.String()), 0755); err != nil {
		t.Fatalf("failed to write probe tool: %v", err)
	}
	return agentDir, toolPath
}

// runEnvProbe executes the probe tool and returns the child's view of the environment.
func runEnvProbe(t *testing.T, agentDir, toolPath string, args ExecToolArgs, a2aMeta *A2AMetadata) map[string]string {
	t.Helper()
	out, _, err := executeTool(context.Background(), agentDir, "env_probe.sh", toolPath, args, a2aMeta)
	if err != nil {
		t.Fatalf("executeTool failed: %v (output: %s)", err, out)
	}
	seen := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, value, found := strings.Cut(line, "=")
		if found {
			seen[name] = value
		}
	}
	return seen
}

// TestChildEnvStripsLockedNamesFromEveryLayer is the order-independent core of the fix: locked names
// are removed from base and overlays and only the authoritative harness copy survives, so no future
// reordering can reintroduce the override.
func TestChildEnvStripsLockedNamesFromEveryLayer(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/moltbot", "LANG=en_US.UTF-8", "BASE_ONLY=keep"}
	harness := map[string]string{"PATH": "/usr/bin", "HOME": "/home/moltbot", "LANG": "en_US.UTF-8", Agent2AgentEnvVar: "harness-value"}
	overlay := map[string]string{"AGENT2AGENT": "forged", "MY_CUSTOM_VAR": "mine"}

	env := childEnv(base, harness, overlay)

	counts := map[string]int{}
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		counts[name]++
		values[name] = value
	}

	if got := values[Agent2AgentEnvVar]; got != "harness-value" {
		t.Errorf("%s = %q, want the harness value", Agent2AgentEnvVar, got)
	}
	// TMPDIR is locked but the harness has none, so the model must not be able to introduce it.
	if _, present := values["TMPDIR"]; present {
		t.Errorf("TMPDIR present in %v, want it absent when the harness has none", env)
	}
	if values["MY_CUSTOM_VAR"] != "mine" || values["BASE_ONLY"] != "keep" {
		t.Errorf("unlocked variables changed: %v", env)
	}
	for _, name := range []string{Agent2AgentEnvVar, "PATH", "HOME", "LANG"} {
		if counts[name] != 1 {
			t.Errorf("%s appears %d times in %v, want exactly one authoritative entry", name, counts[name], env)
		}
	}
}

func TestExecuteTool_ModelEnvCannotOverrideA2A(t *testing.T) {
	agentDir, toolPath := newEnvProbeAgent(t, "")
	a2aMeta := &A2AMetadata{CallerID: "dranbo", CallChain: []string{"dranbo", "barnaby"}, TraceID: "a2a-testtrace"}
	harnessJSON, err := a2aMeta.Encode()
	if err != nil || harnessJSON == "" {
		t.Fatalf("failed to encode test metadata: %v (%q)", err, harnessJSON)
	}

	seen := runEnvProbe(t, agentDir, toolPath, ExecToolArgs{Env: map[string]string{
		Agent2AgentEnvVar: "{}",
		CallChainEnvVar:   "attacker,victim",
	}}, a2aMeta)

	if seen[Agent2AgentEnvVar] != harnessJSON {
		t.Errorf("%s = %q, want the harness payload %q", Agent2AgentEnvVar, seen[Agent2AgentEnvVar], harnessJSON)
	}
	if seen[CallChainEnvVar] != "dranbo,barnaby" {
		t.Errorf("%s = %q, want the harness call chain", CallChainEnvVar, seen[CallChainEnvVar])
	}
}

func TestExecuteTool_DotEnvCannotOverrideA2A(t *testing.T) {
	agentDir, toolPath := newEnvProbeAgent(t, "AGENT2AGENT=\"{\\\"caller_id\\\":\\\"forged\\\"}\"\nWACKYPUB_CALL_CHAIN=forged,chain\nMY_CUSTOM_VAR=from_dotenv\n")
	a2aMeta := &A2AMetadata{CallerID: "dranbo", CallChain: []string{"dranbo"}, TraceID: "a2a-dotenv"}
	harnessJSON, err := a2aMeta.Encode()
	if err != nil || harnessJSON == "" {
		t.Fatalf("failed to encode test metadata: %v (%q)", err, harnessJSON)
	}

	seen := runEnvProbe(t, agentDir, toolPath, ExecToolArgs{}, a2aMeta)

	if seen[Agent2AgentEnvVar] != harnessJSON {
		t.Errorf("%s = %q, want the harness payload %q", Agent2AgentEnvVar, seen[Agent2AgentEnvVar], harnessJSON)
	}
	if seen[CallChainEnvVar] != "dranbo" {
		t.Errorf("%s = %q, want the harness call chain", CallChainEnvVar, seen[CallChainEnvVar])
	}
	if seen["MY_CUSTOM_VAR"] != "from_dotenv" {
		t.Errorf("unlocked .env variable was lost: %q", seen["MY_CUSTOM_VAR"])
	}
}

func TestExecuteTool_ModelEnvCannotOverrideHarnessBaseVars(t *testing.T) {
	agentDir, toolPath := newEnvProbeAgent(t, "")

	seen := runEnvProbe(t, agentDir, toolPath, ExecToolArgs{Env: map[string]string{
		"PATH": "/tmp/evil",
		"HOME": "/tmp/evil-home",
	}}, nil)

	for name, harnessValue := range map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME")} {
		if harnessValue == "" {
			continue // nothing to inherit in this test environment
		}
		if seen[name] != harnessValue {
			t.Errorf("%s = %q, want the harness value %q", name, seen[name], harnessValue)
		}
	}
}

// TestExecuteTool_ModelEnvCannotInjectA2AWithoutCallContext covers the other half of the forgery:
// when the harness is not in an A2A call, the variable must stay absent rather than accept a value
// the model supplied.
func TestExecuteTool_ModelEnvCannotInjectA2AWithoutCallContext(t *testing.T) {
	agentDir, toolPath := newEnvProbeAgent(t, "")

	seen := runEnvProbe(t, agentDir, toolPath, ExecToolArgs{Env: map[string]string{
		Agent2AgentEnvVar: `{"caller_id":"ghost","call_chain":["ghost"]}`,
	}}, nil)

	if seen[Agent2AgentEnvVar] != envProbeUnset && seen[Agent2AgentEnvVar] != os.Getenv(Agent2AgentEnvVar) {
		t.Errorf("%s = %q, want absent (or inherited %q), not a model-supplied value",
			Agent2AgentEnvVar, seen[Agent2AgentEnvVar], os.Getenv(Agent2AgentEnvVar))
	}
}

// TestExecuteTool_ModelEnvStillOverridesUnlockedDotEnvAndPassesThrough guards the behavior this fix
// must not break: model-supplied environment still wins for anything the harness does not own.
func TestExecuteTool_ModelEnvStillOverridesUnlockedDotEnvAndPassesThrough(t *testing.T) {
	agentDir, toolPath := newEnvProbeAgent(t, "MY_CUSTOM_VAR=from_dotenv\nOVERRIDDEN_VAR=from_dotenv\n")

	seen := runEnvProbe(t, agentDir, toolPath, ExecToolArgs{Env: map[string]string{
		"OVERRIDDEN_VAR": "from_args",
		"MY_CUSTOM_VAR":  "from_args",
	}}, nil)

	if seen["OVERRIDDEN_VAR"] != "from_args" || seen["MY_CUSTOM_VAR"] != "from_args" {
		t.Errorf("model-supplied env stopped overriding .env: %v", seen)
	}
}
