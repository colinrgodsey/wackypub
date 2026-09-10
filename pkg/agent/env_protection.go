package agent

import (
	"fmt"
	"sort"
	"strings"
)

// harnessLockedEnvNames are environment variables whose values belong to the harness, not to the
// model. A tool invocation may still set any other variable it wants, but it must not be able to
// replace these. exec resolves duplicate entries last-wins, and args.Env used to be appended after
// every other layer, so a model-supplied env map could rewrite the D59 A2A trust channel: passing
// env: {"AGENT2AGENT": "{}"} erased the call chain, which defeated deadlock cycle detection and made
// the commit audit trail forgeable. See oracle audit 2026-09-09, section A2.
var harnessLockedEnvNames = []string{
	Agent2AgentEnvVar, // D33/D59 caller id, call chain, and trace id
	CallChainEnvVar,   // legacy CSV call chain
	"PATH",
	"HOME",
	"TMPDIR",
	"LANG",
}

// isHarnessLockedEnvName reports whether name is owned by the harness.
func isHarnessLockedEnvName(name string) bool {
	for _, locked := range harnessLockedEnvNames {
		if locked == name {
			return true
		}
	}
	return false
}

// envEntryName splits an "NAME=value" entry into its name.
func envEntryName(entry string) string {
	if i := strings.IndexByte(entry, '='); i >= 0 {
		return entry[:i]
	}
	return entry
}

// harnessEnvFromBase returns the effective value of every harness-owned variable that the calling
// process itself carries, using last-wins so a duplicated base entry resolves the way the child
// would resolve it. This is what preserves normal inheritance: an agent that is itself an A2A callee
// passes its own AGENT2AGENT down to tools unless a2aMeta supplies a fresher value.
func harnessEnvFromBase(base []string) map[string]string {
	harness := make(map[string]string, len(harnessLockedEnvNames))
	for _, entry := range base {
		name := envEntryName(entry)
		if isHarnessLockedEnvName(name) {
			harness[name] = strings.TrimPrefix(entry, name+"=")
		}
	}
	return harness
}

// childEnv assembles a child process environment from a base environment and successive overlays,
// with harness-owned names stripped from all of them and the authoritative harness values appended
// last. Stripping rather than relying on append order is deliberate: it keeps the guarantee true no
// matter which layer a future change appends first. Overlay order is otherwise preserved, so a
// model-supplied value still overrides .env for any variable the harness does not own. Locked names
// the harness does not have are dropped entirely, so a model cannot introduce one either.
func childEnv(base []string, harness map[string]string, overlays ...map[string]string) []string {
	env := make([]string, 0, len(base)+len(harness)+8)
	for _, entry := range base {
		if !isHarnessLockedEnvName(envEntryName(entry)) {
			env = append(env, entry)
		}
	}

	overlayNames := make([]string, 0, 16)
	for _, overlay := range overlays {
		overlayNames = overlayNames[:0]
		for name := range overlay {
			if !isHarnessLockedEnvName(name) {
				overlayNames = append(overlayNames, name)
			}
		}
		sort.Strings(overlayNames)
		for _, name := range overlayNames {
			env = append(env, fmt.Sprintf("%s=%s", name, overlay[name]))
		}
	}

	harnessNames := make([]string, 0, len(harness))
	for name := range harness {
		if isHarnessLockedEnvName(name) {
			harnessNames = append(harnessNames, name)
		}
	}
	sort.Strings(harnessNames)
	for _, name := range harnessNames {
		env = append(env, fmt.Sprintf("%s=%s", name, harness[name]))
	}
	return env
}
