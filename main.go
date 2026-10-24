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

// bundledDefaultCompactMD is examples/compaction/COMPACT-append.md, the default
// compaction directive shipped in the binary (D45). Assigned directly into pkg/agent's
// own DefaultCompactMD var rather than staying at the cmd layer like the two
// skills above, since LoadCompactConfig (deep inside pkg/agent) needs it
// directly, not just CLI-level commands.
//
//go:embed examples/compaction/COMPACT-append.md
var bundledDefaultCompactMD string

// bundledDefaultCompactMDHybrid is examples/compaction/COMPACT-hybrid.md, the hybrid
// minor/major compaction directive shipped in the binary alongside the plain append-only
// default above - embedded the same way, but not itself the fallback LoadCompactConfig
// reaches for when an agent has no COMPACT.md (that stays bundledDefaultCompactMD, so
// existing agents with no major-compact-* config keep running exactly as before). An agent
// opts into hybrid mode by copying this file in as its own COMPACT.md.
//
//go:embed examples/compaction/COMPACT-hybrid.md
var bundledDefaultCompactMDHybrid string

// bundledDefaultRuntimeJSON is examples/runtimes/openrouter-auto.json, the default
// runtime configuration shipped in the binary (D74). Assigned directly into pkg/agent's
// DefaultRuntimeJSON var so LoadRuntimeConfig falls back to it when runtime.json is absent.
//
//go:embed examples/runtimes/openrouter-auto.json
var bundledDefaultRuntimeJSON string

func main() {
	cmd.BundledA2ASkill = bundledA2ASkill
	cmd.BundledWSSkill = bundledWSSkill
	cmd.BundledScratchpadSkill = bundledScratchpadSkill
	adkAgent.DefaultCompactMD = bundledDefaultCompactMD
	adkAgent.DefaultCompactMDHybrid = bundledDefaultCompactMDHybrid
	adkAgent.DefaultRuntimeJSON = bundledDefaultRuntimeJSON
	cmd.Execute()
}
