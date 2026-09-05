# AGENTS.md

Agent guidelines for working in the `WackyPub` repository.

> Companion docs in `.agents/`: [LOCAL_TESTING.md](./LOCAL_TESTING.md) (how to actually
> run and verify changes - there's no mocked LLM backend, so this is the
> workflow that's been used instead), and
> [SECURITY_TESTING.md](./SECURITY_TESTING.md) (checklist of tools that
> enforce a security boundary and whether that boundary has actually been
> pen/escape-tested via [docs/SWARM_TESTING.md](../docs/SWARM_TESTING.md)'s
> swarm process - **reset a tool there to `?` and delete its report the
> moment its enforcement logic changes**). Design decisions and deferred work
> live in the project wiki: [Decisions index](https://github.com/colinrgodsey/wackypub/wiki/Decisions) - read
> before changing session storage, the OpenAI adapter, or reasoning handling:
> several behaviors there are deliberate and load-bearing.
> Also read [README.md](../README.md)
> at the repository root for the high-level philosophy and conceptual foundation.
> For the full architecture deep-dive (directory specs, lifecycle diagrams,
> compaction mechanics, file schemas), see [docs/agents.md](../docs/agents.md) -
> this file is orientation, that one is reference.

## Personality and Harness Overrides

- Be conversative- ask questions, give explanations, be valuable during workshopping.
- Be thoughtful- this is a harness for agents much like you! Reflect on your own
  harness and usage patterns. Provide insights and feedback when beneficial.
- Take pride and ownership- drop the sycophancy, and take some responsibility for the choices that are made.
- Have fun! Hell, be funny even.
- Do not co-author git commits. No mention of your model or harness.
- Don't jump straight to the implementation. If we decided on something new,
  record the decision - use GitKB (git-kb) if it's available in your environment,
  otherwise explain the decision fully (problem, options considered, rationale)
  in the PR description so it is preserved for reviewers and future contributors.

## Project Overview

A Go CLI and SDK for managing folder-based AI agents (roleplay characters,
assistants, etc.), built on Google's Agent Development Kit. Each agent's
state - runtime config, system prompt, long-term memory, turn history - lives
in plain files under a workspace directory, so it's human-readable,
source-controllable, and inspectable without tooling.

**The CLI is a thin wrapper over the SDK, not the other way around.** Every
operation lives on `AgentSDK` (`pkg/agent/sdk.go`) first; the `cobra.Command`
for it just parses args and calls that method. This isn't incidental
structure - the primary consumers of the CLI are expected to be *other agent
platforms*, and specifically a tool that runs a single `wackypub` subcommand
per call (not a shell, not a hand-authored tool schema wrapping `AgentSDK`
methods - see [D13](https://github.com/colinrgodsey/wackypub/wiki/d13-an-agents-tool-for-using) for why). That means command help text needs
to be complete and unambiguous enough for an LLM to use correctly from
`--help` output alone (full description, every argument explained, no
assumed context) - it's the only documentation that caller ever sees.
Separately, `AgentSDK` itself stays a fully supported integration path for
Go-native agent platforms or other implementers that want direct in-process
calls without a subprocess at all. Keeping the CLI layer thin and the SDK
method as the single source of behavior is what lets both callers share the
same implementation and documentation without duplicating either.

**Module**: `github.com/colinrgodsey/wackypub` (see `go.mod`)
**Go Version**: 1.25.7
**ADK Version**: `google.golang.org/adk/v2` v2.0.0

### Key Dependencies

| Package | Purpose |
|---|---|
| `google.golang.org/adk/v2` | Google ADK core types (`model.LLM`, `agent.Agent`, `session.Event`) |
| `google.golang.org/genai` | `genai.Content`/`genai.Part` - the wire format for `session.jsonl` |
| `github.com/achetronic/adk-utils-go` | OpenAI-compatible `model.LLM` adapter (official `openai-go/v3` SDK underneath) - updated to upstream commit `1f0a646bcdfd07ad5f09363d6cbca3b5c58bd764` with Dialects support |
| `github.com/spf13/cobra` | CLI command tree |
| `gopkg.in/yaml.v3` | `wackypub.yaml` config (default model, API key) |

## Commands

### Build & Test

```bash
make build   # Builds wackypub, files-rw, wackyproc, wackydiscord into ./bin
make test    # Runs unit tests for wackypub and all submodule tools
make vet     # Runs go vet across wackypub and all submodule tools
make fmt     # Formats all Go code
make check   # Runs fmt, vet, and test in sequence
```

`tools/files-rw` ([colinrgodsey/files-rw](https://github.com/colinrgodsey/files-rw)), `tools/wackyproc` ([colinrgodsey/wackyproc](https://github.com/colinrgodsey/wackyproc)), and `tools/wackydiscord` are git submodules - run `git submodule update --init` after cloning if they are empty.

### Module Management

```bash
make tidy    # Runs go mod tidy across wackypub and all submodule tools
```

### Manual/Live Testing

There's no mocked LLM backend in this repo - correctness of the OpenAI
adapter wiring (reasoning egress modes, `extraBody`, etc.) has so far been
verified by pointing the built binary at a real workspace and either a real
backend or a local `httptest` server, not by automated tests (see the testing gaps tracked in the [wiki roadmap](https://github.com/colinrgodsey/wackypub/wiki/Roadmap)).
See [LOCAL_TESTING.md](./LOCAL_TESTING.md) for the full workflow: how
`testws/` (gitignored scratch) and `test_agents/` (committed, safe example)
are structured and when to use each, the `runtime.json` symlink pattern for
swapping backends, and the `httptest` technique for checking exact outgoing
wire payloads without spending real API credits.

```bash
go build -o bin/wackypub .
./bin/wackypub --ws testws agent bob prompt "..."
```

## Code Organization

```
WackyPub/
├── cmd/                    # Cobra CLI command tree
│   ├── root.go             # RootCmd, global flags (--ws, --config, -m, --api-key)
│   ├── agent.go             # `agent` subcommands: add, generate, prompt, strip-signatures,
│   │                         # read-session, read-memory, render-prompt, compact
│   ├── workspace.go         # `workspace` - read-only workspace/agent diagnostic (see below)
│   └── version.go           # `version`
├── pkg/
│   ├── agent/                # Folder-agent core: the bulk of this repo's logic
│   │   ├── runtime.go         # RuntimeConfig (runtime.json schema) + loader
│   │   ├── session_store.go   # session.jsonl read/write, ContentText, EstimateTokens, CleanSessionTurns
│   │   ├── agent_folder.go    # FolderAgent: loads an agent dir, GenerateTurn (the main generation path)
│   │   ├── openai_model.go    # NewOpenAIModel adapter wrapper, StripSignatures
│   │   ├── adk_agent.go       # BuildADKAgent (llmagent.New) + CreateGeminiModel - alternate ADK Runner path
│   │   ├── compaction.go      # CheckAndCompactSession - MEMORY.md summarization + session pruning
│   │   ├── macro.go           # @<FILE_PATH> expansion for AGENTS.md
│   │   ├── lock.go            # SessionLock (flock on session.lock)
│   │   ├── workspace.go       # ListAgentIDs, InspectAgentDir - read-only workspace/agent introspection
│   │   └── sdk.go             # AgentSDK - the public programmatic API
│   └── config/                # wackypub.yaml schema (default_model, api_key) - unrelated to runtime.json
├── docs/agents.md           # Deep architecture reference (see pointer above)
├── testws/                  # Gitignored scratch workspace for manual/live testing
└── test_agents/             # Committed example agent workspace (bob: AGENTS.md, IDENTITY.md,
                              # runtime.json, session.jsonl - safe, no real credentials)
```

### Package Purposes

| Package | Description |
|---|---|
| `pkg/agent` | Everything about a single folder agent: config, session storage, generation, compaction, locking, reasoning handling |
| `pkg/config` | `wackypub.yaml` schema (`default_model`, `api_key`) - a small global default-override file, unrelated to per-agent `runtime.json` |

There is currently exactly one subsystem package (`pkg/agent`) and one matching CLI
command group (`agent`) - see the naming convention below. A `roleplay`/`cluster`
subsystem (multi-agent turn-based orchestration driven by a YAML persona/cluster
config, in `pkg/roleplay`, `pkg/cluster`, `cmd/roleplay.go`, `cmd/cluster.go`) existed
earlier and was removed: folder-based agents already cover defining and driving an
agent, and maintaining a second, YAML-config-driven way to define agent "personas"
alongside it wasn't earning its cost. If multi-agent orchestration comes back, prefer
building it on top of `AgentSDK` rather than reintroducing a parallel config format.

### Naming convention: `wackypub <name>` <-> `pkg/<name>` <-> `<Name>SDK`

`wackypub agent ...` is the CLI surface for `AgentSDK`, which lives in `pkg/agent`.
The three names line up on purpose: CLI command group, package directory, and SDK
type name all share the same root. Keep to this when adding a new subsystem -
`wackypub <name>` should be backed by a `pkg/<name>` package exposing a `<Name>SDK`
(or similarly named) type, not scattered across an existing package or bolted onto
`pkg/agent` because it's convenient. This is what keeps "which package implements
this command" a one-second lookup instead of a grep.

## Patterns & Conventions

### Build binaries into `./bin`

When building binaries locally for manual testing, verification, or symlinking (e.g. `wackypub`, `files-rw`), always output them to `./bin` (e.g. `go build -o bin/wackypub .` or, for the `tools/files-rw` submodule, `cd tools/files-rw && go build -o ../../bin/files-rw .`). `./bin` is gitignored so built binaries won't clutter `git status` or accidentally get committed, and provides a single predictable location for symlinks and executable targets.

### Concurrency should always be heavily considered

Any shared state, file access, session mutation, tool invocation, or ID generation must be evaluated for concurrent execution. Google ADK dispatches multiple tool calls in a single model response concurrently in separate goroutines (`handleFunctionCalls`), and external processes or multi-agent calls can access workspace files concurrently.

**Why**: Unprotected file read-modify-write loops, shared map mutations without mutex locks, non-atomic file persistence, or assumptions about sequential tool execution introduce subtle race conditions, corrupted state, or lost updates under concurrent access. Always design state mutations and file access to be concurrency-safe.

### No magic strings or magic constants - literals and values with domain meaning become named constants

If a string literal or numeric value with domain meaning appears more than once
or carries semantic significance - a filename (`"runtime.json"`, `"AGENTS.md"`),
a role (`"user"`, `"model"`), an env var name (`"WACKYPUB_CALL_CHAIN"`), a
metadata key, a flag name, a buffer/header size (`262` bytes for media
detection, `24` bytes for binary prefix checks), or a threshold (`4000` byte
scratchpad redirection cap, `300` max scratchpad entries) - it becomes a
package-level `const` (or an exported one, if other packages need it too),
not raw literals typed out inline.

**Why**: a typo in a repeated string literal compiles fine and fails at
runtime, sometimes silently (a mistyped role string just doesn't match
anything, a mistyped filename just doesn't get found); a typo in an
identifier fails to build. Magic numbers obscure the meaning of arbitrary-looking
slices or buffer allocations and make tuning/updating behavior across the
codebase error-prone. A named constant makes the value self-documenting and
renameable/tunable from one place.

This applies going forward; existing code has known instances of this not
yet being followed (e.g. `"user"`/`"model"` role strings, `"AGENTS.md"`/
`"MEMORY.md"`/`"session.jsonl"`/`"runtime.json"` filenames appearing as
literals in multiple files) - bring a file into line when you're already
touching it for another reason, rather than treating this as a mandate to
sweep the whole codebase in one pass.

### Don't swallow errors

An error return value gets returned/wrapped to the caller, or there's a
one-line comment explaining why ignoring it is provably safe. It never just
disappears - not via `_ = fn()`/`x, _ := fn()` discarding it outright, and
not via `if err == nil { ... }` with no `else`, which silently does nothing
(or worse, silently succeeds with a zero value) on the failure path instead
of surfacing it.

**Why**: a swallowed error turns a real failure into either wrong behavior
that looks like success, or a security-relevant check that fails open
instead of closed. Both are much harder to debug later than a propagated
error would have been, because there's no signal anything went wrong at all.

Concrete examples in this repo worth fixing opportunistically (not a mandate
to sweep): `ValidateAgentTarget` (`pkg/agent/workspace.go`) skips its entire
`WACKYPUB_ALLOWED_AGENTS` authorization check if `os.Getwd()` errors
(`if err == nil { ... }`, no `else`) - failing open on a security-relevant
check rather than failing closed. `InspectAgentDir`'s tool discovery
(`discovered, shadowed, _ := DiscoverAgentTools(agentDir)`) discards a real
error return entirely, so a broken `tools/` directory (permission error,
whatever) reports "0 tools found," indistinguishable from "no `tools/`
directory at all."

### `session.jsonl` is `genai.Content`, not a custom struct

Every line is a serialized `genai.Content` (`{"role": "user"|"model",
"parts": [...]}`- see [D1](https://github.com/colinrgodsey/wackypub/wiki/d01-session-jsonl-stores-genai-content). This is what gives multi-part
messages (text + thinking + eventually images) native support with no lossy
round-trip. Never add a parallel/custom turn struct for this; extend the
`genai.Content`/`genai.Part` usage instead.

### Two ways to call the model: pick the one already used by the call site

- **Primary path** (`FolderAgent.GenerateTurn`, used by `generate`/`prompt`):
  builds a `model.LLMRequest` by hand and calls `model.LLM.GenerateContent`
  directly. No ADK `LLMAgent`/`Runner` involved.
- **Alternate path** (`FolderAgent.RunWithRunner`, built via
  `BuildADKAgent`/`llmagent.New`): routes through ADK's actual
  `LLMAgent`/`Runner`. Not used by any CLI command today (tracked as cleanup work in the [wiki roadmap](https://github.com/colinrgodsey/wackypub/wiki/Roadmap)).

Don't assume `AGENTS.md`'s `Instruction` field reaches the model on the
primary path - it doesn't; `GenerateTurn` builds its own first turn
(system prompt + `<PERSISTENT_MEMORY>`) independently. See [D2](https://github.com/colinrgodsey/wackypub/wiki/d02-compaction-window-determination).

### `runtime.json` knobs are additive and provider-specific

`RuntimeConfig` (`pkg/agent/runtime.go`) has grown several fields that only
matter for specific backends (`reasoningEgress`, `reasoningField`,
`supportsReasoningDetails`, `extraBody`, `preserveThinking`). When adding a
new one, document what it defaults to when absent and which backends
actually need it - most of these exist because one specific provider's API
shape demanded it, not because of a general design goal.

### CLI command pattern: cobra subcommand + positional dispatcher

Every `agent` subcommand (`add`, `generate`, `prompt`, `strip-signatures`,
`read-session`, `read-memory`, `render-prompt`, `compact`) is registered
twice: once as a normal cobra subcommand (`wackypub agent add ...`), and
once as a branch in
`executeAgentDispatcher` so `wackypub agent <agent_id> add ...` (agent ID
first) also works. When adding a new subcommand, wire both.

### Every command is CLI + SDK - keep the docs shared, not duplicated

The intended shape for any new operation is: one `AgentSDK` method
(`pkg/agent/sdk.go`) that does the work, one `cobra.Command` that's a thin
argument-parsing wrapper around it. A given command's CLI form and SDK form
should describe the same operation with the same argument semantics - not
two independently-drifting descriptions of "roughly the same thing."

This matters beyond code reuse: an agent platform's tool is constrained to
running one `wackypub` subcommand per call (see [D13](https://github.com/colinrgodsey/wackypub/wiki/d13-an-agents-tool-for-using)) - it never
sees `AgentSDK` directly, and `--help` is the only documentation it ever
gets. So the CLI has to be self-documenting enough to drive correctly from
`wackypub agent <cmd> --help` alone, with no separate tool schema to fall
back on. Concretely:

- Write a cobra command's `Short`/`Long` and flag descriptions as if an LLM
  agent - not a human skimming `--help` - is the primary reader: state what
  the operation does, every argument it takes, and any precondition or
  side effect (e.g. "acquires the session lock", "rewrites session.jsonl in
  place") plainly enough to act on without reading the source.
- Keep that description anchored to the `AgentSDK` method's doc comment as
  the source of truth where the two overlap, so a Go-native caller reading
  the SDK doc comment and an agent reading `--help` see the same thing,
  instead of two descriptions that quietly diverge over time.
- If a task pattern needs more than `--help` can economically provide on
  its own (a multi-step workflow spanning several commands, say), that's
  what a skill is for - it should describe *when*/*why* to use commands
  together and point at `--help` for argument-level detail, not duplicate
  argument documentation that already exists in the CLI.

`wackypub workspace [agent_id]` (`AgentSDK.ListAgents`/`InspectAgent`,
`pkg/agent/workspace.go`) exists specifically to make workspace setup
self-discoverable this way: instead of a skill/doc trying to describe "how
to structure a workspace" in prose that can drift from reality, an agent
platform can run `workspace <agent_id>` against its own directory and get
back what's actually there, what's missing, and what to do next. It's
deliberately read-only (never creates or modifies a file, including not
creating the agent directory just from being asked about it) - if
scaffolding gets added later, it should be a separate, explicitly-named
operation, not a side effect of inspection.

## Testing

- Unit tests live alongside their package (`*_test.go`), no separate test
  directory. Run with `go test ./...`.
- `pkg/agent/openai_model_test.go` covers the OpenAI adapter's
  reasoning-handling wiring (`reasoningEgress` modes, `ReasoningField`,
  `SupportsReasoningDetails`, `ExtraBody`) using `httptest`-mocked
  wire-payload assertions, plus `StripSignatures`/
  `StripSessionSignatures`. `CleanSessionTurns` is covered in
  `pkg/agent/session_store_test.go`. Extend these before reaching for a
  scratch program - see LOCAL_TESTING.md.
- What automated tests *can't* cover: whether a real provider actually
  accepts a given request shape (a mocked server only proves what we sent,
  not what a real backend does with it). That still requires a live run -
  see LOCAL_TESTING.md.
- `testws/` and `test_agents/` are both usable for manual verification;
  `testws/` is gitignored (safe to leave real API keys in its
  `runtime.json` files), `test_agents/` is committed (keep it free of
  secrets) and is a complete, working example workspace (`AGENTS.md`,
  `IDENTITY.md`, `runtime.json`, `session.jsonl`), not just a fixture.
- Tools that enforce a security boundary (filesystem access, command
  execution, cross-agent authorization, anything an agent - or a
  prompt-injected one - could try to escape) need adversarial pen/escape
  testing against a live build, not just unit tests for the happy path -
  see [SECURITY_TESTING.md](./SECURITY_TESTING.md) for the tracked
  checklist (`?`/`y`/`n`) and [docs/SWARM_TESTING.md](../docs/SWARM_TESTING.md)
  for the actual swarm-based testing process. **If you change such a tool's
  enforcement logic, reset it to `?` and delete its `docs/` report(s) in the
  same commit** - a `y`/`n` state is a claim about the code as it stood when
  last tested, not a permanent property of the tool.

## Important Gotchas

### Hand-editing `session.jsonl` can silently corrupt it

`ReadSessionTurns` skips any line it can't `json.Unmarshal` - silently, by
design (so one bad line doesn't take down the whole session). This means a
missing trailing newline (common when an editor saves without one) plus a
subsequent `AppendSessionContent` call will merge two JSON objects onto one
line, and that merged line then gets silently dropped from history the next
time it's read - producing a coherence gap with no error. If you hand-edit
`session.jsonl`, verify every line still parses (`python3 -c "import json;
[json.loads(l) for l in open('session.jsonl') if l.strip()]"` is enough) and
that trailing newline is intact.

### OpenRouter `"model": "auto"` + `supportsReasoningDetails: true` will break on the second turn

Encrypted/signed `reasoning_details` blocks are tied to the specific backend
endpoint that produced them. `"auto"` routing can pick a different endpoint
on the next request, and OpenRouter rejects the replayed block with a 404.
Only enable `supportsReasoningDetails` with a pinned `model`. See
[D6](https://github.com/colinrgodsey/wackypub/wiki/d06-openai-adapter-reasoning-egress) and `ADK_UTILS_GO_REASONING_EGRESS_BUG.md` at the repo root.

### The `adk-utils-go` dependency tracks upstream `achetronic/adk-utils-go`

`go.mod` directly tracks `github.com/achetronic/adk-utils-go` (commit `1f0a646bcdfd07ad5f09363d6cbca3b5c58bd764`, which incorporated the `Dialect` interface for reasoning & OpenRouter support).

### Don't reuse a persistent flag's shorthand on a subcommand

`RootCmd` binds `-m` to `--model` as a persistent flag (`cmd/root.go`). A
subcommand that also defines a local `-m` shorthand doesn't just get
shadowed - cobra panics as soon as the two flag sets get merged, which
happens on `--help`, shell completion, or any invocation of that command.
This exact bug existed on `agent add`/`agent prompt` (`-m` collided with a
local `--message` flag) until it was fixed by dropping the local shorthand.
Before adding `StringVarP`/`BoolVarP`/etc. with a shorthand on any command,
check `cmd/root.go`'s persistent flags first.

## Adding New Components

### New `runtime.json` field

1. Add the field to `RuntimeConfig` in `pkg/agent/runtime.go` with a doc
   comment explaining which backend(s) need it and what happens if it's
   left unset.
2. Wire it through in `NewOpenAIModel` (`pkg/agent/openai_model.go`) if it
   maps to an `adkopenai.Config` field.
3. Update `docs/agents.md`'s `runtime.json` schema/field table.

### New `agent` CLI subcommand

1. If the underlying operation doesn't already exist on `AgentSDK`
   (`pkg/agent/sdk.go`), add it there first - acquire the session lock,
   delegate to a package-level function that does the actual work without
   locking (so it's independently testable/reusable and callable without
   going through the CLI). Give it a doc comment that fully states what it
   does, its arguments, and any side effects: this is what a Go-native
   caller importing `pkg/agent` reads instead of `--help`, so write it as
   the canonical description, not CLI-specific throwaway text.
2. Add the `cobra.Command` in `cmd/agent.go`, following the existing
   `add`/`generate`/`prompt`/`strip-signatures` shape (load the SDK, resolve
   `agent_id` from args, call the `AgentSDK` method, print a result). Write
   `Short`/`Long` and flag help text for an agent-platform caller driving
   this from `--help` output, not just a human - see the CLI/SDK
   documentation pattern above.
3. Add a branch in `executeAgentDispatcher` for the positional syntax.
4. Register the command in `init()`.
5. Document it in `docs/agents.md` §8 (CLI Command Pipeline).
