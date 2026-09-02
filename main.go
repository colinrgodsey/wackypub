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

// bundledDefaultCompactMD is examples/compaction/COMPACT-append.md, the default
// compaction directive shipped in the binary (D45). Assigned directly into pkg/agent's
// own DefaultCompactMD var rather than staying at the cmd layer like the two
// skills above, since LoadCompactConfig (deep inside pkg/agent) needs it
// directly, not just CLI-level commands.
//
//go:embed examples/compaction/COMPACT-append.md
var bundledDefaultCompactMD string

// bundledDefaultRuntimeJSON is examples/runtimes/openrouter-auto.json, the default
// runtime configuration shipped in the binary (D74). Assigned directly into pkg/agent's
// DefaultRuntimeJSON var so LoadRuntimeConfig falls back to it when runtime.json is absent.
//
//go:embed examples/runtimes/openrouter-auto.json
var bundledDefaultRuntimeJSON string

func main() {
	cmd.BundledA2ASkill = bundledA2ASkill
	cmd.BundledWSSkill = bundledWSSkill
	adkAgent.DefaultCompactMD = bundledDefaultCompactMD
	adkAgent.DefaultRuntimeJSON = bundledDefaultRuntimeJSON
	cmd.Execute()
}
