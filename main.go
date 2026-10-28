package main

import (
	_ "embed"

	"github.com/colinrgodsey/wackypub/cmd"
	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

//go:embed skills/wackypub-a2a/SKILL.md
var bundledA2ASkill string

//go:embed skills/wackypub-ws/SKILL.md
var bundledWSSkill string

//go:embed skills/scratchpad-efficiency/SKILL.md
var bundledScratchpadSkill string

// bundledDefaultCompactMD is examples/compaction/COMPACT-append.md, the plain append-only
// compaction directive shipped in the binary (D45). No longer LoadCompactConfig's fallback
// (see bundledDefaultCompactMDHybrid below) - stays embedded and available for anyone who
// copies it in as their own COMPACT.md. Assigned directly into pkg/agent's own
// DefaultCompactMD var rather than staying at the cmd layer like the two skills above, since
// LoadCompactConfig (deep inside pkg/agent) needs it directly, not just CLI-level commands.
//
//go:embed examples/compaction/COMPACT-append.md
var bundledDefaultCompactMD string

// bundledDefaultCompactMDHybrid is examples/compaction/COMPACT-hybrid.md, the hybrid
// minor/major compaction directive shipped in the binary - embedded the same way as
// bundledDefaultCompactMD above, and now THE fallback LoadCompactConfig reaches for when an
// agent has no COMPACT.md at all (Colin, 2026-10-24: every agent gets the pass-mode
// conditional template by default). The plain append-only variant above, and
// examples/compaction/COMPACT-replace.md, stay available for anyone who copies one in as
// their own COMPACT.md instead.
//
//go:embed examples/compaction/COMPACT-hybrid.md
var bundledDefaultCompactMDHybrid string

// bundledDefaultRuntimeJSON is examples/runtimes/openrouter-auto.json, the default
// runtime configuration shipped in the binary (D74). Assigned directly into pkg/agent's
// DefaultRuntimeJSON var so LoadRuntimeConfig falls back to it when runtime.json is absent.
//
//go:embed examples/runtimes/openrouter-auto.json
var bundledDefaultRuntimeJSON string

// applyBundledDefaults pushes the bytes embedded at compile time from examples/ into the
// pkg/agent variables that read them as their no-config fallback. The embed lives here
// because examples/ is outside pkg/agent's embed reach, and the vars live there because
// LoadCompactConfig and LoadRuntimeConfig need them; this function is the whole wiring,
// extracted from main so main_test.go can prove it instead of leaving it to be eyeballed.
func applyBundledDefaults() {
	cmd.BundledA2ASkill = bundledA2ASkill
	cmd.BundledWSSkill = bundledWSSkill
	cmd.BundledScratchpadSkill = bundledScratchpadSkill
	adkAgent.DefaultCompactMD = bundledDefaultCompactMD
	adkAgent.DefaultCompactMDHybrid = bundledDefaultCompactMDHybrid
	adkAgent.DefaultRuntimeJSON = bundledDefaultRuntimeJSON
}

func main() {
	applyBundledDefaults()
	cmd.Execute()
}
