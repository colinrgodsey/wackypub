# DECISIONS.md

Design decisions for `WackyPub` and the rationale behind them. This is
living documentation: it states what's true now and why, not how it got
there. When a decision changes, this file is rewritten to reflect the
current state - it doesn't accumulate a history of reversals.

Referencing these IDs (D1, D2, ...) is fine here and in code comments. They
don't belong in commit messages or PR bodies aimed at someone who hasn't
read this file.

## D1: `session.jsonl` stores `genai.Content` directly

Each line is a serialized `google.golang.org/genai.Content`
(`{"role": "user"|"model", "parts": [...]}`), not a custom
`{role, content, timestamp}` struct.

**Why**: `genai.Content` already has JSON serialization, already supports
multi-part messages (text, thinking, and eventually images/audio), and is
the type every ADK model adapter natively produces and consumes. A custom
struct would mean writing a lossy conversion in both directions, and losing
information (like reasoning parts) on every round-trip through storage.

## D2: The system prompt is folded into the first user turn, not sent as a `system`-role message

`FolderAgent.GenerateTurn` and `CheckAndCompactSession` both build a single
first turn combining the rendered `AGENTS.md` system prompt and the
`<PERSISTENT_MEMORY>` block, sent with `role: "user"`. `LLMRequest.Config`
never sets `SystemInstruction`.

**Why**: some local-model chat templates (llama.cpp-served Gemma, in
particular) mishandle or reject a `system`-role turn. An earlier version of
this code had a `systemRole` config knob (`"system"`/`"developer"`/`"user"`)
to make this configurable per-agent. That knob was removed: folding the
system prompt into the first user turn works uniformly across every backend
tested so far, and the configurability wasn't buying anything a fixed
behavior didn't already cover.

**Consequence**: `llmagent.Config.Instruction` (set in `BuildADKAgent`) is
never read by the primary generation path - it only matters for the
alternate `RunWithRunner` path. Don't assume setting `Instruction` affects
`generate`/`prompt` output.

## D3: The OpenAI adapter is `achetronic/adk-utils-go` (upstream tracking)

`pkg/agent/openai_model.go` wraps `achetronic/adk-utils-go`'s `genai/openai`
package (which wraps the official `openai-go/v3` SDK).

**Why**: a hand-rolled HTTP client adapter previously lived directly in this
repo. It worked, but only covered plain-text `content`/`reasoning_content`
and had to be extended by hand for every new provider quirk (tool calls,
images, OpenRouter's block format, etc.). Switching to the official SDK via
`adk-utils-go` gets that coverage for free. Upstream `achetronic/adk-utils-go`
incorporated dialect-based reasoning handling (`Dialect` interface, OpenRouter, DeepSeek, TextDialect) in commit `1f0a646bcdfd07ad5f09363d6cbca3b5c58bd764`, allowing `go.mod` to track upstream directly without a fork `replace` directive.

## D4: Migrated from ADK v1 to `google.golang.org/adk/v2`

**Why**: `adk-utils-go` requires ADK v2. Before migrating, every type this
repo actually uses (`model.LLM`, `llmagent.Config`, `runner.Config`,
`session.Event`) was diffed between v1 and v2 and found unchanged aside from
the import path - the migration was a mechanical `google.golang.org/adk/...`
-> `google.golang.org/adk/v2/...` rewrite plus a `go.mod` bump, not a
behavioral change.

## D5: Reasoning is replayed as history by default (native egress), never stripped

The OpenAI adapter's default (`reasoningEgress: "native"`, or field left
unset) sends captured reasoning back to the provider as its own field on the
next request, rather than omitting it or merging it into `content`.

**Why**: several providers *require* this. DeepSeek V4 in thinking mode
returns a 400 if `reasoning_content` is missing from a prior assistant
message once thinking mode has been used in the conversation. Kimi K2
Thinking expects prior reasoning resent to continue its chain of thought
across tool calls. Dropping reasoning by default (an earlier, mistaken
assumption) breaks both outright; Qwen3 is the only tested provider that
ignores replayed reasoning by default, and ignoring extra data is harmless.

## D6: `reasoning_details` (OpenRouter's structured block format) is captured unconditionally, replayed only when explicitly enabled and only safe with a pinned model

Ingest of OpenRouter's `reasoning_details` array (typed blocks, including
opaque encrypted/signed reasoning) always happens, regardless of
`supportsReasoningDetails`. Egress (replaying the array back as history) is
gated by that config flag.

**Why capture is unconditional**: so turning the flag on later still
replays what earlier turns already recorded, instead of losing history that
predates the flag.

**Why egress is gated, and why it needs a pinned model**: encrypted/signed
blocks are cryptographically tied to the exact backend endpoint that
produced them. `"model": "auto"` on OpenRouter can route a later request to
a *different* endpoint, and that endpoint can't decrypt reasoning it didn't
produce - OpenRouter returns a 404 ("Encrypted payloads can only be replayed
to the endpoint that created them"). This was hit and confirmed live, not
just inferred from docs. `supportsReasoningDetails: true` is only safe with
`model` pinned to a specific slug.

**Corollary**: `StripSignatures`/`StripSessionSignatures` exist
specifically to remove stale block metadata (never readable `Thought` text)
when an agent switches away from a backend that emitted encrypted blocks, or
between providers entirely (also covers Gemini's `ThoughtSignature` field,
confirmed live to be rejected outright when replayed to Anthropic) - see the
`strip-signatures` CLI command.

## D7: `ContentText` always excludes `Thought`-marked parts

The helper used for the CLI's printed/returned response and for
`MEMORY.md` addenda strips `Thought` parts before joining text; full
`genai.Content` (with `Thought` parts intact) is what gets persisted to
`session.jsonl`.

**Why**: reasoning is internal monologue, not something the character said
out loud. Showing it as if it were dialogue, or letting it leak into a
compacted memory summary, breaks the fiction and pollutes memory with
scratch reasoning instead of actual events/decisions.

## D8: `EstimateTokens`'s inclusion of reasoning text is gated by `RuntimeConfig.PreserveThinking`, not a fixed choice

**Why**: whether replayed reasoning actually counts against a model's
context budget depends on the backend. Providers that require it resent
(D5) bill for and consume context on that text every turn; providers that
ignore replayed reasoning (Qwen3-style) don't. A fixed always-count or
never-count policy would be wrong for one side or the other, so the
`preserveThinking` config flag exists to let the agent's own operator state
which behavior applies to their backend.

## D9: Consecutive `user` turns are merged only at request-build time, never in storage

`MergeConsecutiveUserTurns` runs inside `GenerateTurn` and
`CheckAndCompactSession`, right before a request is sent to the model. It is
never applied to what `AppendSessionContent`/`WriteSessionTurns` persist.

**Why**: `session.jsonl` legitimately accumulates consecutive user turns
(multiple `add` calls without an intervening `generate`, and the injected
system-prompt+memory turn landing before whatever the first real turn is,
which is usually also `user`). Storage should stay a simple, permissive log
of what actually happened. Some backends' chat templates reject or
mishandle non-alternating roles, so the *request* needs normalizing, but
collapsing turns in storage would throw away the actual turn boundaries for
no benefit.

## D10: `runtime.json`'s API key field is `apiKey`, not `api_key`

**Why**: every other field in `RuntimeConfig` is camelCase
(`sessionCompactPct`, `contextWindow`, `reasoningEgress`, ...). `api_key`
was a snake_case holdover; renamed for consistency. No backward-compatible
alias was kept - this is a config file under active development, not a
public API with external consumers to avoid breaking.

## D11: The CLI is a thin wrapper over `AgentSDK`; every subsystem gets its own `pkg/<name>` + `<Name>SDK` + `wackypub <name>` triple

Every `agent` CLI subcommand's `RunE` does argument parsing and formatting
only - the actual work happens in an `AgentSDK` method
(`pkg/agent/sdk.go`), which itself delegates to a package-level function
that doesn't acquire the session lock (so it's independently reusable). No
CLI-only business logic is allowed to live in `cmd/agent.go`.

**Why**: two different kinds of caller need the same operations. The CLI's
primary consumers are agent platforms shelling out to `wackypub` as a single
command per call (see D13) - they only ever see the CLI surface. Separately,
a Go-native agent platform or other implementer can `import
"github.com/colinrgodsey/wackypub/pkg/agent"` and call `AgentSDK` directly,
skipping the CLI entirely. Keeping the SDK method as the single place
behavior and documentation live means both callers get the same behavior
for free, and neither one requires re-deriving or duplicating what already
exists in `cmd/agent.go`.

**Corollary - `pkg/roleplay` and `pkg/cluster` were removed**: they were a
second, YAML-config-driven (`wackypub.yaml` personas/clusters) way to define
and orchestrate agents, existing in parallel with folder-based agents rather
than building on `AgentSDK`. Folder-based agents already cover defining and
driving an agent; the parallel system wasn't earning its maintenance cost
and didn't fit the CLI-is-a-wrapper-over-SDK shape (its commands read/wrote
YAML config directly in `RunE`). If multi-agent orchestration is needed
again, build it as `AgentSDK`-backed methods plus a `pkg/roleplay`-style
package following this same pattern, not as a parallel config format.

## D12: `workspace` is a top-level command, and is read-only with no scaffolding

`wackypub workspace [agent_id]` sits at the root (`wackypub workspace ...`),
not under `agent` (`wackypub agent ... workspace`), and it never creates or
modifies a file - not even the agent directory itself when inspecting an
agent that doesn't exist yet, which every other `AgentSDK` method does via
`os.MkdirAll` as a side effect of acquiring the session lock.

**Why top-level**: it reports on the workspace and on agents within it, but
it isn't itself an agent-session operation the way `add`/`generate`/`prompt`
are - it doesn't touch any particular agent's conversation. Nesting it under
`agent` would suggest it belongs to the same category of operation as those.

**Why read-only, no scaffolding**: the goal is to replace a prose
description of "how to structure a workspace" (in a skill file or doc that
can drift from what the code actually checks for) with something an agent
platform can run against its own directory and get a truthful, current
answer from. Mixing that in with "and also let me create the missing files
for you" would conflate two different concerns - what "correct" starter
content for `AGENTS.md` or `runtime.json` should look like is a separate,
underspecified design question, and answering it wrong as a side effect of
a diagnostic command is worse than not answering it. If scaffolding is
wanted later, it should be its own explicitly-named operation (e.g.
`agent <id> init`), not something `workspace` does implicitly.

## D13: An agent's tool for using WackyPub is single `wackypub` command execution, not a shell and not a hand-authored tool schema

The intended integration for an LLM agent platform is a tool constrained to
running one `wackypub` subcommand per call - not general shell/bash access
(no pipes, no other binaries, no scripting), and not a bespoke
function-calling schema wrapping `AgentSDK` methods one by one.

**Why not a bespoke tool schema**: the CLI's `--help` output (`Short`/`Long`
on every command, per D11's argument-completeness requirement) already *is*
a complete, accurate description of every operation. A hand-authored schema
listing every command and argument up front would either duplicate that
text (and drift from it over time) or under-specify it to stay within a
reasonable context budget. A single root command sidesteps both problems:
an agent explores progressively (`wackypub --help` -> `agent --help` ->
`agent generate --help`), paying token cost only for the part of the
surface it actually needs on a given turn, instead of every registered tool
being listed regardless of relevance - which is the usual failure mode of
large tool-listing setups (context bloat, and models confusing
similar-sounding tools).

**Why not general shell access**: constraining the tool to "run one
`wackypub` subcommand" keeps the blast radius no larger than `wackypub`'s
own command set. An agent with this tool cannot `rm -rf`, pipe output into
another program, or otherwise act outside what `wackypub` itself permits,
unlike raw bash access.

**Where skills fit**: if a specific task pattern needs more guidance than
`--help` economically provides on its own (e.g. a multi-step workflow spread
across several commands), a skill supplements it. A skill should describe
*when* and *why* to use which commands together, and point at `--help` for
argument details - not duplicate argument documentation that already lives
in the CLI, for the same drift reason a hand-authored tool schema would.

**Corollary**: `AgentSDK` (see D11) remains a fully separate, independent
integration path for Go-native agent platforms or other implementers that
want direct in-process calls without running a subprocess at all. The two
paths aren't in tension - they're different callers with different needs,
and both are already fully supported today.

## D14: Agent tools are executables discovered under `tools/`, not a hand-authored registry

An agent's available tools are whatever executable files are found by recursively walking `<agent_dir>/tools/` (itself possibly a symlink, and free to contain symlinks to shared "toolpack" directories rather than individual tool files) - mirroring the existing symlink-sharing pattern used for `runtime.json` and `AGENTS.md`/`IDENTITY.md`.

**Symlink Resolution Mechanics**:
- `DiscoverAgentToolsMap` resolves directory and file symlinks using `os.Stat` and `filepath.EvalSymlinks`, traversing into symlinked "toolpack" directories (e.g. `tools/read-only-fs` -> `/path/to/toolpack`) and discovering the executable files inside them (`cat`, `ls`, `man`).
- Symlinks pointing directly to executable files are followed to their target, registering the symlink's basename as the discovered tool.
- Broken symlinks are ignored safely without halting traversal.
- Visited real directory paths are tracked to prevent infinite recursion on circular symlink loops.

**Why filesystem discovery over a hand-authored tool registry**: same
rationale as D13 - an executable that self-describes (name, description,
argument schema, on request) is a single source of truth for its own tool
definition, the same way `--help` is for a CLI command. A separate
hand-maintained list of "here are agent X's tools and their schemas" would
duplicate what the executables already know about themselves and drift out
of sync. The exact self-description query protocol (what flag or convention
a tool executable responds to) is not settled yet.

**Name collisions are accepted, not an error.** If multiple toolpacks
symlinked into `tools/` happen to contain a same-named executable, the one
that "wins" is whichever the traversal encounters last - and that order is
*not* guaranteed deterministic across filesystems, deliberately. Anyone
symlinking in toolpacks with generically-named tools that might collide is
expected to give the link itself a distinguishing name if they care which
one wins. `workspace <agent_id>` should surface detected shadowing in its
diagnostic output (see D12) so it's discoverable when someone needs to know,
without requiring the discovery process itself to block or warn.

**Scope of this decision**: it covers tool *discovery* and *naming* only.
The actual tool-use loop (model returns a `FunctionCall` part -> exec the
matching tool -> append a `FunctionResponse` -> generate again -> repeat
until the model stops calling tools) is a separate, larger, not-yet-designed
piece of work - `GenerateTurn` is single-shot today (see D2's description of
the current generation path).

## D15: Workspace root is marked by a `WACKYPUB_ROOT` file, discovered by walking up from CWD only when `--ws` isn't explicit

**Not implemented yet - design only, see TODOS.md.** Every valid workspace
must have an (empty, content doesn't matter) `WACKYPUB_ROOT` file directly
in its root directory - the same marker-file pattern `.git`, `package.json`,
and `Cargo.toml` use for their own tools' root discovery.

- If `--ws X` is passed explicitly, `X` itself must directly contain
  `WACKYPUB_ROOT`; error if it doesn't. No walking past an explicitly-given
  path - explicit intent is respected, not second-guessed.
- If `--ws` isn't given (the default), walk up from the current directory
  looking for the nearest ancestor containing `WACKYPUB_ROOT`, and use that
  as the workspace root. Error if none is found up to filesystem root -
  never silently fall back to treating an arbitrary CWD as a workspace.

**Why**: the concrete problem this fixes already happened - running
`wackypub` from the wrong directory with no `--ws` given silently created
`session.lock`/`session.jsonl` files wherever CWD happened to be, since the
old default was a bare `.`. A hard marker-file requirement makes that
failure mode impossible: either you're somewhere under a real workspace, or
you get a clear error, never a silently-wrong location.

The walk-up also does double duty once tool invocation exists: a tool
executable (or `wackypub` invoked as a tool - see D16) running with CWD
somewhere under an agent's own folder doesn't need `--ws` passed to it at
all; it discovers its own workspace root automatically.

Bootstrapping a new workspace's `WACKYPUB_ROOT` file has no dedicated
command yet (`touch WACKYPUB_ROOT` is sufficient by hand) - see TODOS.md for
whether that's worth a command later.

## D16: Cross-agent tool invocation is governed by two separate mechanisms - `WACKYPUB_ALLOWED_AGENTS` for authorization, `WACKYPUB_CALL_CHAIN` for deadlock safety

Agents can invoke `wackypub` itself as a discovered tool (e.g. to message
another agent - see D17). Two failure modes need independent guards: an
agent reaching an agent nobody authorized it to reach, and an agent
reaching (directly or transitively) back to itself mid-generation.

**`WACKYPUB_ALLOWED_AGENTS`** is a file in an agent's own folder listing
which other agent IDs it's permitted to target via a `wackypub`-as-tool
invocation. It's checked against CWD *before* the `WACKYPUB_ROOT` walk-up
(D15) - a cheap, local check that can reject before even resolving the
workspace root. **If the file doesn't exist, the default is deny-all**: no
cross-agent access without an explicit opt-in allowlist. This also covers
self-targeting for free - an agent's own ID simply isn't in its own
allowlist unless someone deliberately puts it there, so no separate
self-check is needed.

Whether this check applies to *any* invocation with a matching CWD
(including a human manually running `wackypub agent <id> ...` from inside
another agent's folder for debugging) or only to invocations actually
happening as a tool call from a live generation is an open question - see
TODOS.md. Until it's resolved, assume the simpler CWD-only check.

**Why authorization alone doesn't prevent deadlocks**: session locking
(`session.lock`, see `.agents/AGENTS.md`'s gotchas) does not provide cycle
protection - it produces a *deadlock*, not a clean rejection. If agent X's
live generation (process A, holding X's session lock) spawns a tool call
that invokes `wackypub agent X ...` again (process B), process B blocks
trying to acquire the same lock, and process A can't release it until B's
tool call returns - both hang forever. Worse, `WACKYPUB_ALLOWED_AGENTS`
alone doesn't close this either: if X's allowlist includes Y and Y's
allowlist includes X, that authorizes exactly the X -> Y -> X cycle that
deadlocks, just deliberately instead of accidentally. Authorization and
cycle-safety are different concerns and need different mechanisms.

**`WACKYPUB_CALL_CHAIN`** is the cycle-safety mechanism: an environment
variable carrying the list of agent IDs already active in the current call
stack (e.g. `bob,jax`), which each `wackypub`-as-tool invocation appends its
own agent ID to before spawning a further tool subprocess (env vars
propagate to child processes for free). Before targeting an agent, refuse
immediately if that agent ID is already present in the chain - regardless
of what any `WACKYPUB_ALLOWED_AGENTS` file permits. This is a structural
guarantee (catches a cycle of any length, doesn't depend on anyone
configuring allowlists acyclically), not a policy one, and it's cheap (no
filesystem access, just an inherited env var).

**Division of responsibility**: `WACKYPUB_ALLOWED_AGENTS` decides who's
authorized to talk to whom. `WACKYPUB_CALL_CHAIN` guarantees that no
authorized configuration can still produce a deadlock. Neither is a
substitute for the other.

**Concurrency Scope Note**: `WACKYPUB_CALL_CHAIN` environment variable propagation is designed for subprocess CLI calls (where each spawned tool process inherits its own environment snapshot). For multi-threaded Go SDK consumers calling `AgentSDK` directly across concurrent goroutines in the same process, `os.Setenv` is process-global; if concurrent in-process Goroutine SDK calls targeting different agents are required in the future, `context.Context` key propagation can supplement env var checks.

**Read-only inspection is deliberately exempt**: `AgentSDK.InspectAgent` (and therefore `wackypub workspace`) does *not* go through `ValidateAgentTarget`. This authorization boundary exists to gate cross-agent tool invocation/generation - something that can cause a side effect or a deadlock risk - not read-only diagnostic visibility, which has neither. Gating it the same way was a real bug in practice: an agent inspecting the workspace (including inspecting *itself*, since an agent's own ID isn't automatically in its own allowlist) would get an authorization failure that `wackypub workspace`'s summary table rendered as a generic `"error"` in the RUNTIME.JSON column - indistinguishable from an actually-broken config file, and actively misleading. This exemption is scoped narrowly to inspection; mutating/generating SDK methods still go through the full check, and the open question in TODOS.md about whether CWD-based *mutating* invocations should be scoped differently remains unresolved.

## D17: Tool execution loop and self-describing tool protocol

`<agent_dir>/tools/` executables discovered per D14 are registered as LLM function declarations on each generation request.

**Tool Schema Registration**:
- Built-in tools (`create_scratchpad`, `get_scratchpad`, `list_scratchpads` - see D18) and a single generic `run_command` tool covering every discovered executable are constructed as Google ADK `tool.Tool` instances using `google.golang.org/adk/v2/tool/functiontool.New`.
- Strongly typed Go structs automatically generate their `genai.FunctionDeclaration` schemas and handle JSON argument unmarshaling and type validation.
- Discovered executables under `tools/` are **not** individually registered as separate function declarations - they're dispatched through the one `run_command` tool's `command` argument. `run_command`'s description is built dynamically at agent-load time from the discovered command names, plus general usage guidance (see below), so an agent gets both "what commands exist" and "how command invocation works in general" from a single tool description instead of N near-duplicate `"Command <Name>"` descriptions with no shared context.
- The command list embedded in the description is always alphabetically sorted (`DiscoverAgentToolsMap` already sorts before returning) - filesystem readdir order is not guaranteed stable across runs, and an unsorted list would change the description's bytes between generations for no reason, defeating prompt caching on every single request.

**`run_command` Usage Guidance** (baked into its description, not repeated per-agent in AGENTS.md):
- The working directory is always the agent's own directory - there's no way to `cd` elsewhere, since commands don't chain (see TODOS.md for the deliberately-deferred question of whether that's ever needed).
- `args` entries are passed as literal argv elements, not shell-parsed - no quoting or escaping needed for spaces/special characters.
- The agent's scratchpad may already contain the data it needs - check before running a command to regenerate something already available.
- Running a command with no arguments or `--help` is a legitimate way to learn what it is, how to use it, and what arguments it takes.

**Tool Execution Protocol**:
- When the model returns a `FunctionCall` part for `run_command` naming a command `X`:
  - The handler validates `X` against the discovered command map first and returns a real error for an unrecognized command (ADK's own dispatch can no longer catch this, since `run_command` itself is always a valid, registered tool).
  - `wackypub` executes `<agent_dir>/tools/X` (or its discovered path) with the positional `args` list.
  - Any key-value entries in `env` are added to the subprocess environment (along with inheriting process environment including `WACKYPUB_CALL_CHAIN`).
  - ~~Full raw JSON arguments are also passed to the tool's stdin and as `WACKYPUB_TOOL_ARGS`.~~ Removed in D53 - no recorded rationale ever existed for this, and it turned out to have a real, surprising downstream effect on `wackyproc`.
  - `wackypub` captures stdout (and stderr on failure) from the subprocess.
  - A `FunctionResponse` part is constructed with the response output and appended to the session log.

**Loop Termination & Max Tool Turns**:
- `GenerateTurn` executes in a loop: after receiving tool calls and appending tool responses, it invokes the model again until the model returns a text response (no function calls) or the maximum tool turns limit is reached.
- Default max tool turns is 10 per `GenerateTurn` invocation, configurable via the persistent `--max-tool-turns` CLI flag.

**Tool Error Signaling**:
- `executeTool` returns `(string, error)`, not just a formatted string. A non-zero exit, a missing binary, or a scratchpad read/write failure surfaces as a real Go `error`.
- Every `functiontool.New` handler (`create_scratchpad`, `get_scratchpad`, `list_scratchpads`, and `run_command`) propagates that error as its own return value instead of packing failure text into a normal-looking result and always returning `nil`.
- Google ADK's own tool dispatch (`internal/llminternal/base_flow.go`) turns a non-nil handler error into a `FunctionResponse.Response` shaped as `{"error": "<message>"}`, structurally distinct from the `{"output": "..."}` shape a successful call produces - no extra callback wiring required on our side.

**Why**: A uniform `{args: []string, env: map[string]string}` schema allows arbitrary CLI tool binaries under `tools/` to be invoked natively as subprocesses without requiring every tool to author custom schema metadata parser extensions, keeping discovery fast and compatible with any shell command. Error signaling matters because a model can only react differently to a tool failure if the failure looks different on the wire - previously, `"Error executing tool X: ..."` was just prose living in the same `output` field a successful call would use, so recognizing a failure depended entirely on the model reading it as English rather than on any structural signal. Collapsing per-discovered-tool declarations into one `run_command` mirrors how most modern coding agents actually work (a single shell/command-execution tool, not one declaration per binary) - it lets general operating guidance (working directory behavior, argv conventions, "check your scratchpad first," "use `--help` to explore") live in exactly one place instead of being absent or repeated per tool, at the cost of the model no longer seeing each command as its own named function in the schema.

## D18: Scratchpad system - collision-safe IDs, bounded retention, and inline macro expansion for command I/O

Agents can store text payloads and intermediate command output in a persistent, session-level scratchpad (`<agent_dir>/scratchpad.json`) instead of paying to regenerate or re-read that data through the model on every turn it's needed.

**Why a scratchpad at all**: three distinct token-economics wins, not just "big output goes somewhere else." (1) An agent can pipe one agent's response directly into another tool call without ever entering those tokens into its own context. (2) One write can be reused across multiple downstream calls (send the same text to three peer agents) without re-entering it each time. (3) Even reading a scratchpad back is comparatively cheap - it's tokens the model consumes, not tokens it has to *generate*, and generation is the expensive side of that trade.

**Tools**:
- `create_scratchpad(text: string) -> id`: stores `text` under a freshly generated ID and returns it. Replaces the old `set_scratchpad(id, text)` - the caller never chooses an ID, which is what makes the concurrency race below structurally impossible rather than just discouraged.
- `get_scratchpad(id: string, skip_lines?: int, num_lines?: int)`: retrieves the stored text, optionally paginated by line range.
- `list_scratchpads() -> [{id, seq, size, created_by}]`: metadata-only forensic listing of every currently-live entry (plus live-count/cap), described below.

**ID shape and eviction**:
- IDs are a randomly generated 4-character string from `[0-9a-z]` (~1.68M possible values), collision-checked against currently-*live* entries only - a since-evicted ID is fair game to reuse, since nothing is stored there anymore.
- Each entry also stores a monotonically increasing `seq` integer (`max(existing seq) + 1` at creation), used purely for FIFO ordering - it's never shown as the entry's identity, only used internally to decide what to evict.
- The scratchpad holds a bounded number of live entries (default 50). When a new entry would exceed that cap, the entry with the lowest `seq` is evicted - its data is actually deleted, not just marked stale.
- Because eviction deletes rather than tombstones, a lookup miss on `get_scratchpad` is **structurally indistinguishable** between "this ID was evicted" and "this ID never existed" - there's no surviving record to tell the difference. `list_scratchpads` exists specifically to give a confused agent a way to check current reality directly, rather than trying (and failing) to explain history we no longer have.

**Concurrent Same-Turn Race - now closed by construction**: Google ADK dispatches every `FunctionCall` in a single model response concurrently (`handleFunctionCalls` spawns one goroutine per call, no ordering guarantee), which previously meant a model calling `set_scratchpad` and something consuming that slot in the *same* turn could have the read execute before the write landed. Since `create_scratchpad` always returns a freshly generated ID in its response, a model cannot reference a slot before seeing the response that creates it - it has no way to know the ID in advance. The race isn't mitigated anymore, it's impossible to trigger.

**`run_command` I/O integration** (see D17 for `run_command` itself):
- `args[]` entries and a new `stdin` template string both support inline `<SCRATCHPAD_DATA id="X" skip_lines="N" num_lines="M" />` macro expansion, resolved server-side against stored scratchpad content *before* the subprocess is built - never round-tripping the data through model-generated tokens. This replaces the old bare `stdin_scratchpad_id` integer field: `stdin` is now a template (which can be just the macro alone, or the macro embedded in a larger string with a prefix/suffix).
- Any single argument, after macro expansion, that exceeds 500,000 bytes fails with an explicit internal error before `exec` is ever called (`"expanded argument exceeds 500000 bytes (was N) - use stdin/stdout scratchpad redirection instead"`) rather than surfacing a raw OS `E2BIG`.
- `run_command` is the only tool that auto-creates scratchpad entries from its own output, since it's the only unbounded-output producer in the system (`create_scratchpad`/`get_scratchpad`/`list_scratchpads` all have naturally small, self-limiting output). Past a size threshold (`ScratchpadOutputThreshold`), stdout/stderr are each captured into a fresh scratchpad entry instead of being inlined; the response is tagged either way so the shape is uniform and self-documenting (including the payload `size` in bytes):
  - `<STDOUT><SCRATCHPAD_DATA id="k3p1" size="4500" /></STDOUT><STDERR><SCRATCHPAD_DATA id="k3p2" size="4100" /></STDERR>` (both large)
  - `<STDOUT><SCRATCHPAD_DATA id="k3p1" size="4500" /></STDOUT>` (only stdout was large; nothing on stderr)
  - `<STDOUT><SCRATCHPAD_DATA id="k3p1" size="4500" /></STDOUT><STDERR>low memory</STDERR>` (stdout large, stderr small enough to inline)
  - `<STDOUT>operation complete</STDOUT>` (both small enough to inline directly)
- `env` map values are explicitly **not** macro-expanded - env vars are expected to stay small, and adding a second expansion surface for something that doesn't need it isn't worth the complexity.

## D19: Folder agents migrate to Google ADK `runner.Runner` backed by `FileSessionService`

Agent generation turns migrate from manual `model.LLMRequest` construction to Google ADK's `runner.Runner` engine, backed by a custom `FileSessionService` (`pkg/agent/file_session_service.go`).

**FileSessionService & Storage Compatibility**:
- Implements ADK's `session.Service` interface (`Create`, `Get`, `List`, `Delete`, `AppendEvent`).
- Reads and writes serialized `genai.Content` objects directly from/to `<agent_dir>/session.jsonl` under the session lock, preserving 100% backward compatibility with existing agent sessions and file formats.

**In-Memory Event List Synchronization**:
- `FileSessionService.AppendEvent` appends new events (`evt`) directly to the in-memory `fileSession.events` list in addition to writing to `session.jsonl` on disk.
- Ensures subsequent iterations within a multi-turn tool execution loop read live assistant tool calls and user tool response events from `sess.Events()` rather than a frozen snapshot.

**Event Author**:
- `FileSessionService.Get()` sets `evt.Author` to the actual agent id for model-role turns and `"user"` for user-role turns, not the raw `genai.Content.Role` string. ADK's runner expects `Author` to be a real participant identifier and logs "Event from an unknown agent" warnings (spamming the console on every turn of loaded history) if it's just handed the literal string `"model"`.

**System Prompt & Persistent Memory Layout**:
- Rendered `AGENTS.md` is passed directly as `Instruction` on `llmagent.Config` (ADK's native system prompt).
- `FileSessionService.Get()` formats `MEMORY.md` (`<PERSISTENT_MEMORY>`) as User Turn 1 without prepending `AGENTS.md`, eliminating duplicate system prompt messages.
- Consecutive user turns are merged (`MergeConsecutiveUserTurns`) to ensure prompt cache consistency and model template compatibility.

**Runner Execution & User Turn Handling**:
- `GenerateTurn` passes `msg = nil` into `r.Run(ctx, "user", agentID, nil, ...)` because user turns are already loaded from disk via `SessionService.Get()`. This prevents duplicate user turn appending.

**Tool Loop Termination & `--max-tool-turns` Cap**:
- `LoadFolderAgent` accepts `maxToolTurns int` (defaulting to 10 if <= 0) and threads it to `BuildADKAgent` at agent construction time.
- `AgentSDK` passes `s.MaxToolTurns` into `LoadFolderAgent`, binding the CLI flag `--max-tool-turns` directly to `llmagent.Config`'s `BeforeModelCallbacks`.
- When consecutive tool loop requests exceed `maxToolTurns`, `BeforeModelCallback` skips the model call and returns `"exceeded maximum tool turns limit (%d)"`.

**Compaction**:
- Compaction remains single-shot via `CheckAndCompactSession` before generation runs.

**Why**: Using ADK's `runner.Runner` and `session.Service` standardizes agent execution on official Google ADK primitives while preserving full filesystem control, multi-turn tool loop fidelity, and `session.jsonl` compatibility.

## D20: Skills system - distilled, discoverable knowledge for agents

Agents can be given pre-written, distilled guidance ("skills") instead of having to re-derive how something works from raw `--help` output or trial and error every session - the same problem `run_command`'s baked-in usage guidance (D17) addresses for command execution in general, but for anything else worth writing down once.

**Discovery**:
- A `skills/` folder per agent, discovered recursively the same way `tools/` is (D14) - including symlinked "skill packs" shared across agents the same way toolpacks are today.
- A skill is a folder containing `SKILL.md` with YAML frontmatter: standard `name` and `description` fields, matching the format other agent harnesses already use so off-the-shelf skills can be dropped in as-is, plus one non-standard field: `always_load: true`. `gopkg.in/yaml.v3` is already a dependency (`pkg/config/config.go`), so this doesn't add a new one.

**On-demand skills** (the default, `always_load` unset or `false`):
- Only `name` + `description` are ever in context - surfaced in the `load_skill` tool's own dynamically-built description, the same pattern `run_command` uses for its command list (D17), always alphabetically sorted by skill name for prompt-cache stability.
- `load_skill(name)` returns the skill body as a normal tool response (`FunctionResponse`) - the same mechanical pattern `get_scratchpad`/`run_command` already use. There's no "inject into system prompt mid-session" mechanism to build, since ADK's `Instruction` isn't mutable per-turn anyway; a loaded skill just becomes part of the ordinary conversation history from that point on, same as any other tool result.

**Always-loaded skills** (`always_load: true`):
- Excluded entirely from `load_skill`'s registry - no on-demand entry for something already in context.
- Bodies are concatenated onto the end of the rendered `Instruction` string (`macro.go`'s system prompt rendering, alongside AGENTS.md), sorted alphabetically by skill name, wrapped as `<AUTOLOADED_SKILLS><SKILL name="...">...</SKILL>...</AUTOLOADED_SKILLS>`.

**Why**: Mirrors D17's `run_command` reasoning - a short, always-visible description plus content loaded only when actually needed keeps the always-in-context cost near zero while still making distilled knowledge discoverable, rather than forcing a choice between "nothing is ever pre-written" and "everything is always in every prompt." Reusing the standard `SKILL.md` + YAML-frontmatter shape (rather than inventing a new format) means existing skills written for other harnesses work here with no translation beyond the one added `always_load` field. Loading a skill as a normal tool response - not a special system-prompt mutation - keeps the mechanism consistent with everything else in the tool-calling system and avoids building a second, bespoke content-injection path alongside the one `FunctionResponse` already provides.

## D21: Explicit model provider selection (`"openai"`, `"gemini"`, `"anthropic"`) and Anthropic ADK model adapter

Agent runtime configurations (`runtime.json`) support explicit model provider selection via a `provider` field (`"openai"` | `"gemini"` | `"anthropic"`), eliminating implicit fallback ambiguity and enabling native Anthropic Claude models via `github.com/achetronic/adk-utils-go/genai/anthropic` (resolved to the `colinrgodsey` fork - see D3).

**Provider Resolution (`runtime.json`)**:
- `"openai"` (or `"openai-compatible"`): instantiates `NewOpenAIModel` (`pkg/agent/openai_model.go`).
- `"anthropic"`: instantiates `NewAnthropicModel` (`pkg/agent/anthropic_model.go`), backed by `github.com/achetronic/adk-utils-go/genai/anthropic`.
- `"gemini"`: instantiates `CreateGeminiModel` (`pkg/agent/adk_agent.go`), backed by native `google.golang.org/adk/v2/model/gemini`.
- **Defaulting & Backwards Compatibility**: If `provider` is empty or unset, `LoadRuntimeConfig` defaults to `"openai"` if `endpoint` is set, or `"gemini"` if `endpoint` is empty.

**Anthropic Thinking Knobs**:
`RuntimeConfig` exposes native Anthropic thinking/reasoning configuration:
- `thinkingBudgetTokens`: Sets classic token budget for Anthropic thinking (`"enabled"` mode, e.g. 1024).
- `thinkingEffort`: Sets reasoning effort (`"low"`, `"medium"`, `"high"`) for adaptive thinking mode.
- `thinkingMode`: Sets thinking mode (`"enabled"`, `"adaptive"`, or empty for auto-detection).

**Why**: Explicit provider selection removes provider ambiguity and gives every LLM backend (OpenAI-compatible gateways, native Gemini ADK models, and native Anthropic Claude models) first-class runtime config support with dedicated thinking/reasoning controls.

## D22: `files-rw` - standalone read/write/edit/patch/list tool gated by a per-directory access file

A companion binary, `files-rw` (`cmd/files-rw/main.go` + `pkg/filesrw/`, same module as `wackypub` - not a separate repo, so it can reuse conventions like the comment/blank-line rule parsing already established for `WACKYPUB_ALLOWED_AGENTS` rather than duplicating it), gives agents an explicit, per-directory-scoped read/write/edit/patch/list tool suite instead of relying solely on the invoking harness's own sandboxing. It's registered like any other agent tool: symlinked into `<agent_dir>/tools/`, invoked via the generic `run_command` tool with a real argv (never shell-parsed) and `cmd.Dir` set to the agent's own directory (see D14, `pkg/agent/agent_folder.go`) - which is why the access grant below can safely be scoped to "the tool's cwd" with no upward search.

**Access grant (`FILES_RW_ACCESS`)**:
- Exact filename, read only from the tool's cwd, never searched upward - since `cmd.Dir` is always the agent's own directory, this is always that agent's own grant, never inherited from a parent.
- Missing file -> deny everything. No default-allow, no partial trust.
- One rule per line: `w: <path>` or `r: <path>`. `w` implies `r`. Blank lines and `#`-prefixed lines are ignored (mirrors `WACKYPUB_ALLOWED_AGENTS`, D16).
- Re-read fresh on every invocation - no caching, no state carried between runs.
- The access file's own canonical path is always denied, unconditionally, even to a rule that would otherwise cover it (e.g. a broad `r: .`).

**Path handling**:
- `~` is rejected outright (fail loud) wherever it can appear - in a rule's path or a request target. Argv-based invocation means the shell never expands `~` for the tool; a literal `~` reaching it is almost certainly the model assuming shell semantics that aren't happening, not a case worth silently guessing at (`$HOME`) - matches the project's established preference for loud failure over silent disambiguation (see D-note on the Gemini thinking-config conflict fix).
- Relative paths, in both rules and request targets, resolve against the tool's cwd.
- Every path - granted root and request target alike - is canonicalized via `filepath.EvalSymlinks` before any containment check, so a symlink inside a granted root that points outside it can't be used to escape it. A granted root must already exist (fails loudly at load time otherwise); a request target may not (`write` creates new files) - in that case the longest existing ancestor is resolved through symlinks and the not-yet-existing tail is trusted as given relative to that resolved ancestor.
- Containment is a path-separator-aware boundary check (`path == root || strings.HasPrefix(path, root+sep)`), not a naive string prefix - avoids a false-positive match of a granted `/home/bob/Downloads` against a request for `/home/bob/Downloads-secret`.

**Command surface**:
- `read <path> [--start N] [--end N]`: `cat -n`-style numbered output, whole file by default. No built-in pagination - relies on the existing `run_command`-to-scratchpad redirect (D18) for output too large for one tool response, same as any other tool.
- `write <path>`: content via stdin only (plays cleanly with D18's `<SCRATCHPAD_DATA id="X" />` stdin macro for large content without spending model output tokens). Atomic (temp file + rename). Auto-creates missing parent directories, bounded inside the already-validated writable root.
- `edit <path> --old <str> --new <str> [--replace-all]`: exact string replace, implemented directly rather than shelled to `patch` - rejects zero or more-than-one match unless `--replace-all` is given, so the caller supplies more surrounding context instead of an edit silently landing on the wrong occurrence.
- `patch <path>`: unified diff via stdin, shelled to the system `patch` binary (`-o <tempfile>` then atomic rename over the target) - the actual "piggyback on `patch`" piece, for multi-hunk edits `edit` isn't suited for.
- `list <path> [-l] [-a] [-R]`: shells to `ls`, but only ever with a fixed, hardcoded set of boolean flags translated ourselves - deliberately not raw argv passthrough, since that would let a second positional path argument slip past access control entirely (`ls` lists every path it's given; only the first would ever get checked).
- `read`/`edit` refuse binary files (NUL-byte heuristic) - numbering/string-replace don't mean anything on non-text content.

**Why**: Tool-level filesystem access in this project so far has been all-or-nothing (whatever `<agent_dir>/tools/` symlinks in, the executable gets whatever access the OS user running it has). `files-rw` adds an explicit, fail-loud, per-agent-directory allowlist on top, without requiring every future file-touching tool to reimplement its own sandboxing.

## D23: `files-rw` round two - `copy`/`move`/`delete`, `read` defaults to raw (no numbering), `patch` restricted to unified diffs, hard read size cap

Found live: gave a wiped-clean test agent (clerk, no other file tools) `files-rw` and an 82KB source file, asked for a copy. `files-rw` has no `copy` command, so the only path available was `read` piped through the scratchpad into `write` - but `read`'s default output is `cat -n`-style numbered (`D22`), and clerk had no way to know that numbering wasn't part of the real file content. The "copy" came out with a `<N>\t` prefix injected into every line, and clerk reported success without re-reading to verify. Re-run with a raw `cat` available (not `files-rw read`) alongside `files-rw`, the same read-through-scratchpad-into-write pattern produced a byte-identical copy - confirming the numbering, not the pipe pattern itself, was the actual bug.

**New commands**:
- `copy <src> <dst>`: reads raw bytes internally - never through the numbered-`read` presentation layer - and writes them to `dst`. Requires `r:` on `src`, `w:` on `dst`.
- `move <src> <dst>`: `os.Rename`-based (with the same cross-device fallback copy+delete a plain rename would need). Requires `w:` on both `src` and `dst` - moving relinquishes `src`, so the relevant grant is write, not read.
- `delete <path>`: `os.Remove`. Requires `w:` on `path`.

Before this, `move` was impossible (no rename primitive and no delete to fake it with) and `delete` didn't exist at all - not inconvenient, genuinely unreachable.

**`read` behavior change**: numbering flips from default-on to default-**off**, opt-in via `-n`/`--numbers` (`cat -n` itself defaults to no numbering; `read` should match that convention, not invert it). Numbered format matches `cat -n` exactly - `%6d\t%s` (line number right-justified in a minimum 6-wide field, then a literal tab, then the raw line text), confirmed against real `cat -n` output including the padding behavior once line numbers exceed 6 digits (the field just grows, `%6d`'s normal behavior). Default (no `-n`) is the file's raw bytes - safe to pipe through the scratchpad into `write`/`copy`/anything else without silently mutating the content. `-n` exists for when an agent actually needs to reference specific lines - before constructing an `edit` or `patch` call - not as a general-purpose default; the help text should say so explicitly, since "numbering exists to help you write a correct patch hunk, not because it's part of the file" was exactly the piece of context clerk didn't have.

**`read` gets a hard size cap** (target ~200KB): if the requested read - whole file, or the given `--start`/`--end` range - would exceed it, refuse with a clear error suggesting line-based pagination (`--start`/`--end`) instead of silently truncating or dumping it anyway. `files-rw` shouldn't depend on the invoking harness happening to have its own large-output handling (D18's scratchpad auto-redirect is a `wackypub`/`run_command` feature, not something `files-rw` can assume - it's meant to be usable by any harness that can exec a tool with an argv and stdin).

**`patch` restricted to unified-diff format only**: validate the incoming diff actually looks like a unified diff (`---`/`+++` headers, `@@` hunk markers) before ever handing it to the system `patch` binary, rejecting anything else with a clear error - don't rely on `patch`'s own format auto-detection leniency (context diffs, normal diffs, ed scripts are all things GNU `patch` will happily also try to interpret). `patch`'s help text should say to read the target with `read -n` first (to get correct line numbers for the hunk header) and that only unified diffs are accepted. Unified diffs carry unchanged context lines that must match before a hunk applies, which is exactly the self-verifying property that makes them more resistant to drift/hallucination than an agent guessing at raw line-offset edits - and it's the format models are most commonly trained to produce anyway.

**Explicitly rejected**: a dedicated line-range replace command (`edit-lines <path> --start N --end M`, new content via stdin). Considered as a lower-ceremony alternative to constructing a one-hunk unified diff, but decided against - it reintroduces exactly the kind of unverified, drift-prone line-offset editing that restricting `patch` to unified-diff format is meant to avoid. `edit` (exact-string match) + `patch` (unified diff, self-verifying via context lines) stay the only two edit primitives. Matches how Claude Code's own file-editing toolset gets by on exact-string `Edit` + full-file `Write` alone, no line-range primitive.

**Why**: `read`+`write` alone can't safely stand in for `copy` when `read`'s own output isn't the raw file - that's a correctness bug, not a missing convenience, confirmed by an actual corrupted "copy." `move`/`delete` close a real capability gap (not just ergonomics: previously unreachable, not merely awkward). Restricting `patch` to unified diffs and requiring an explicit opt-in for `read` numbering both trade a small amount of flexibility for a lot less room for an agent to hallucinate its way into a corrupted or drifted file.

**Addendum: `FILES_RW_ACCESS` is now readable, still never writable.** D22's original "always denied, unconditionally" rule applied to both read and write. Found live (a swarm worker with no other file tools had no way to introspect its own grant except trial-and-error probing - real thrashing, several wasted rounds just rediscovering what was already granted): the actual risk `FILES_RW_ACCESS` denial exists to prevent is an agent *mutating* its own grant, not an agent *seeing* it - the contents aren't secret, and an agent can already infer them through probing anyway. `Access.Resolve` now special-cases the deny-path check on `needWrite`: `read` succeeds unconditionally (regardless of any `r:`/`w:` rule, same as the old always-denied special case just flipped for the read side), while `write`/`edit`/`patch`/`delete`/`move` (either side) stay denied exactly as before - confirmed live, writing/deleting/moving `FILES_RW_ACCESS` still fails with the same "always denied" error.

## D24: `files-rw`'s path-based access model has two real, unfixed gaps - hardlink read bypass (including cross-agent) and a TOCTOU race - accepted for now, not fixed in code

A second swarm run (report deleted per the invalidation rule once D26 changed the command surface again - `SECURITY_TESTING.md` has the current state; 4 of 10 requested rounds completed at the time, stopped early for cost, not because ideas ran out) against the D23 command surface found 7 confirmed bypasses, two of which are more serious than anything found in the first run and go beyond what D22's "if an agent already has `bash`, sandboxing that agent's own reach is moot" reasoning excuses:

1. **Hardlink read bypass generalizes past a single agent's own files.** `ln target/secret.txt ./scratch/hl && files-rw read ./scratch/hl` succeeds, same root cause the first run found (D23's `WriteFile` doc comment already covers the write-side version of this). New this run: the same technique reads **any** same-filesystem file, including **other agents' files across `/ws/`** (`ln /ws/worker-sonnet/target/secret.txt ./scratch/hl` from a *different* worker's own directory - confirmed live). That's not "an agent can read its own out-of-bounds files given shell access" (D22's accepted-risk framing) - it's one agent reading a *different* agent's private files, a materially different concern that has nothing to do with the reading agent's own privilege level.
2. **TOCTOU race between `Access.Resolve()` and the actual I/O, no hardlink required at all.** `Access.Resolve()` returns a path *string*; the real filesystem operation happens afterward, as a separate step, against that same string, with nothing holding a file descriptor open across the gap. A background process racing to swap the file's content between those two steps wins 99-100% of the time in testing (confirmed live against both `read` and, separately, `copy`). This needs no `ln`, no hardlink capability, nothing beyond ordinary write access to a directory the racing process already has - it's a pure timing bug in the check-then-use pattern, present by inspection in every command (`read`/`copy`/`edit`/`patch`/`move`/`delete` all resolve a path once, then act on that string later).

**Root cause for both, ultimately the same one**: path-based access control validates a path string, then either (a) never re-validates which inode that path actually names by the time the syscall runs (hardlink case), or (b) doesn't hold the checked file open at all, so the path can point somewhere else entirely by the time it's used (TOCTOU case). The real fix for both is the same shape: open the file (`O_NOFOLLOW`) as part of, or immediately after, the access check, and do all subsequent I/O through that already-open file descriptor rather than re-resolving the path string - collapses the TOCTOU window to nothing and, as a side effect, makes the hardlink case checkable too (validate the open fd's identity, not just the path that produced it).

**Decision: not fixed in this pass.** Scoped as a TODO (`.agents/TODOS.md`) instead of implemented now - an fd-based rewrite of `Access.Resolve` and every call site touches the core of `pkg/filesrw` broadly enough to warrant its own dedicated pass and a fresh swarm re-test afterward, not a rushed fix bolted onto an already-large round of findings. `SECURITY_TESTING.md`'s `files-rw` entry is `n`, not `?` - this is a confirmed, real finding staying on record, not an invalidated-by-code-change reset.

**Why accept this now instead of fixing immediately**: same reasoning as D22's original hardlink note, extended - `files-rw`'s threat model has always assumed the tool itself is the only file-touching capability an agent has (see D22's "Why"). Every worker in this swarm run also had raw `bash`, which was necessary for building attack fixtures (creating hardlinks, running race loops) but means the *specific* deployment tested here isn't `files-rw`'s intended one either. That doesn't make the finding not real - the TOCTOU race and cross-agent hardlink read are structural gaps in `files-rw` itself, not gaps that require `bash` to exploit once a hardlink can be planted by any means - but it does mean shipping a rushed fix under time pressure is worse than documenting the gap precisely and fixing it properly in a dedicated pass.

## D25: `search_scratchpad` - a fifth built-in scratchpad tool, search as an index into pagination, not a replacement for it

Implemented in `pkg/agent/scratchpad.go` (`SearchScratchpad`), `cmd/agent.go` (`scratchpadSearchCmd`), `pkg/agent/sdk.go` (`AgentSDK.SearchScratchpad`).

A large scratchpad entry (a big file dump, a long command output) is currently only navigable by paginating blind through `get_scratchpad`'s `skip_lines`/`num_lines` - fine once you know roughly where the interesting part is, tedious to locate it in the first place. `search_scratchpad` closes that gap as a fourth-ish built-in alongside `create_scratchpad`/`get_scratchpad`/`list_scratchpads`.

**Signature**: `search_scratchpad(id, query, case_sensitive=true, regex=false, max_results=50)`.
- `id` required - scoped to one entry, not a search-everything-live mode. A "search across all live entries" tool would be a different feature (more like "which entry has X") - not ruled out for later, just not conflated with this one, which matches the concrete use case (an agent already holding a big entry, looking for where in it something is).
- `query` is a literal substring by default; `regex` is an opt-in escape hatch, off by default. Mirrors the pattern already used twice in `files-rw` (`read -n` opt-in numbering, `patch` unified-diff-only default): the safe/predictable primitive is the default, power is available but never assumed.
- `case_sensitive` defaults `true`, matching real `grep`'s own default rather than a more "forgiving" case-insensitive-by-default some tools use - avoids surprising a model that already has `grep` conventions baked into its training.
- `max_results` hard-caps the returned list (default ~50), same reasoning as `files-rw read`'s 200KB cap: a pathological query (searching for `"the"` in a large log) shouldn't be able to blow up the response. Total match count is reported separately from the capped list, same shape as `list_scratchpads`' count/cap fields, so the agent knows whether it's seeing everything.

**Result shape, per match**: `{line, skip_lines, text}` - `line` is 1-indexed and human-readable, `skip_lines` is precomputed (`line - 1`) so the agent can hand it straight to `get_scratchpad` or the `<SCRATCHPAD_DATA skip_lines="N" />` macro without doing its own off-by-one arithmetic, and `text` is the single matching line, truncated (~200 chars) so one absurdly long line can't dominate the result.

**Explicitly rejected**: including N lines of surrounding context per match (`grep -C`-style). Search stays a lightweight index - *where* the matches are - and `get_scratchpad` stays the one place that actually extracts content. Two tools each doing one thing kept simpler than one tool that both finds and previews, and avoids two different, only-loosely-consistent pagination code paths.

**Why**: same motivation as D18's original scratchpad design (D18: scratchpad exists because generation is the expensive part of a token budget, not consumption) - search is another way to keep an agent from having to re-read or re-generate content it doesn't need, this time by narrowing *where* to look before it pays for a `get_scratchpad` call at all.

## D26: `files-rw` D24 fix, revised before a swarm re-test - `go-gitdiff` replaces the `patch` subprocess, O(1) `Nlink>1` replaces the O(N) directory-walk hardlink check

Reviewed the first-pass D24 fix before spending a swarm run verifying it, and found two problems in its own coverage:

1. **`patch` still has the TOCTOU gap it was meant to close.** `PatchFile` opens the target via `Access.OpenFile` (the new fd-based check), then immediately closes that fd and does the real work via the `patch` subprocess against the path string. Everything gained by holding the fd open is lost the moment it's closed before the actual syscall - the window this fix exists to close is still open, just for `patch` specifically.
2. **The hardlink check itself is a new performance/DoS surface.** `checkHardlinkSafety`'s `countInodesInRoots` does a full recursive `filepath.Walk` over every allowed root, on *every single access check* - not once at load time. A directory tree of any real size under an allowed root turns every `read`/`write`/`edit`/etc. call into an O(number of files under the roots) operation, which didn't exist before this fix and wasn't itself tested.

**Decision, both fixed in the same revision**:

- **`patch` moves off the system binary entirely, onto `github.com/bluekeyes/go-gitdiff`** (`gitdiff.Parse` + `gitdiff.Apply`), following the exact shape `EditFile` already uses: open via `Access.OpenFile`, read the original content through that fd, apply the parsed diff in memory, hand the result to `WriteFile`'s existing atomic-rename path. This closes `patch`'s TOCTOU gap the same way `read`/`copy`/`edit` are closed, and as a side benefit replaces the old heuristic `isUnifiedDiff` string-sniffing (checks for `---`/`+++`/`@@` substrings) with a real parser that rejects malformed diffs properly. Also drops the dependency on the system `patch` binary entirely - the `Dockerfile`'s `patch` apt-get addition (added for the original D24 swarm test, so `files-rw`'s patch-subcommand attack surface wouldn't go untested for lack of the binary) becomes unnecessary and should be reverted alongside this change.
- **The hardlink check drops the directory walk in favor of a flat `Nlink > 1` rejection** - O(1), a single field off an already-`stat`ed file, no traversal. Every attack the second swarm run actually demonstrated (single-agent hardlink read, hardlink+copy, cross-agent hardlink read, delete-via-hardlink) involved creating an *extra* hardlink, which always bumps `Nlink` from 1 to 2+ - so the cheap version closes every demonstrated attack. Acknowledged cost: it also refuses to touch a legitimate file that happens to have more than one hardlink for unrelated reasons, which is expected to be rare for typical agent-workspace files but is a real, accepted false-positive surface, not a hypothetical one. `countInodesInRoots`/`getNlinkAndDevIno`'s walk-based inode-matching machinery is removed, not kept as a fallback - maintaining two hardlink-detection strategies side by side wasn't judged worth the complexity over just picking the cheap one.

**Deferred, not abandoned**: a real, non-blunt hardlink defense (one that doesn't also block legitimate multi-linked files) remains an open question, alongside the option of pushing more of this mitigation to deployment/environment hardening instead of application code - e.g. actually enforcing `fs.protected_hardlinks` (confirmed disabled/unenforced in the swarm test's container, D24) or simply not co-locating mutually-distrusting agents on a shared writable filesystem in the first place, rather than trying to detect the aliasing after the fact from inside `files-rw` itself.

**Why**: same reasoning as D24's own "why accept this now" - a security fix that isn't actually verified to close what it claims to close, or that trades one real gap for a new unverified one (the walk-based DoS surface), isn't worth spending a swarm run confirming. Catching this in review, before the swarm run, is cheaper than a swarm run rediscovering the exact same category of incomplete fix.

## D27: `wackypub agent <id> scratchpad {create,read,list,search}` - CLI-level scratchpad access

Implemented in `cmd/agent.go` (`scratchpadCmd`), `pkg/agent/sdk.go`, `pkg/agent/scratchpad.go`. Closes an already-logged gap (the former "Future Scratchpad management" TODO): scratchpad slots have only ever been reachable from inside a live agent turn via the built-in tools (`create_scratchpad`/`get_scratchpad`/`list_scratchpads`/`search_scratchpad`, D18/D25) - no way for a human operator, external tooling, or another agent driving `wackypub` from the CLI to read or write one directly.

**Surface** (mirrors the four in-agent tools 1:1):
```
wackypub agent <id> scratchpad create [message]     # positional/--message flag/stdin, same 3-way pattern as `agent add`
wackypub agent <id> scratchpad read <entry-id> [--skip-lines N] [--num-lines M]
wackypub agent <id> scratchpad list
wackypub agent <id> scratchpad search <entry-id> <query> [--regex] [--case-insensitive] [--max-results N]
```
Same `ValidateAgentTarget` authorization already applied to every other `agent <id> <verb>` command - no new authorization scheme, just consistent reuse of the existing one.

**Locking**: `create` acquires the session lock (`AcquireSessionLock`) - it's a read-modify-write over the whole `scratchpad.json`, and two concurrent CLI *processes* (not goroutines - the existing in-process `getScratchpadMutex` doesn't help across process boundaries) racing to create an entry could lose one. `read`/`list`/`search` are pure reads against a file that's only ever atomically replaced (temp file + rename, D18), so per the same precedent that already dropped locking from read-only `AgentSDK` methods (self-deadlock fix, see TODOS.md), they don't acquire it.

**What this replaces, and why nothing else needed building**:
- The original idea was three separate features: (1) auto-expand `<SCRATCHPAD_DATA />` macros in an agent's *final response text* (not just tool-call args) so a caller gets the full content without the agent regenerating it; (2) auto-redirect a large *incoming* user message into scratchpad, mirroring the existing large-tool-*output* auto-capture (`ScratchpadOutputThreshold`); (3) this CLI exposure.
- (1) turns out to need no new machinery: an agent can already put a raw `<SCRATCHPAD_DATA id="X" />` reference in its plain response text today (nothing expands it, but nothing breaks either) - once `scratchpad read` exists, the caller just pulls that specific entry on demand instead of every macro in every response getting force-expanded into stdout whether wanted or not. Caller-decides is strictly better than always-expand here.
- (2) was explicitly rejected in favor of explicit-only: the caller stashes large content itself (`wackypub agent bob scratchpad create < bigfile.txt` returns an ID, then `wackypub agent bob prompt "summarize scratchpad <id>"`) rather than `add`/`prompt` silently redirecting a message above some threshold. Matches the project's standing preference for explicit tools over implicit magic (D-numerous precedent, most recently D23/D26's own "predictable primitive by default, no silent guessing") - no threshold to pick, explain, or have surprise a caller who didn't expect their message to be intercepted.

**Explicitly deferred, not part of this decision**: a `scratchpad delete <entry-id>` CLI command (and matching in-agent tool - neither currently exists; entries only leave via automatic lowest-`seq` eviction past the 50-entry cap). Worth its own decision if a real need shows up, not bundled in here just because it's adjacent.

**Why**: enables direct scratchpad-to-scratchpad piping between agents and CLI-level file-into-scratchpad piping for a human operator, both previously impossible without going through a live agent turn - and does it by exposing the same four operations that already exist in-session, not inventing a new mechanism.

## D28: `CreateScratchpad` server-side macro expansion - out-of-band scratchpad concatenation and templating

Implemented in `CreateScratchpad` (`pkg/agent/scratchpad.go`). Automatically expands inline `<SCRATCHPAD_DATA id="X" skip_lines="N" num_lines="M" />` macros server-side before storing a new scratchpad entry payload in `scratchpad.json`.

**Mechanics**:
- `CreateScratchpad` calls `ExpandScratchpadMacros(agentDir, text)` *before* acquiring the per-agent directory mutex (`getScratchpadMutex`), resolving any referenced scratchpad entries (or slices) from disk and substituting their text into the payload before saving under a new 4-character ID.
- Applies uniformly across all creation paths: the ADK `create_scratchpad` tool, the CLI `wackypub agent <id> scratchpad create` subcommand, and `AgentSDK.CreateScratchpad`.
- Thread-safe and deadlock-free: macro resolution reads referenced entries via `GetScratchpad` (which acquires and releases the mutex per read) *before* `CreateScratchpad` locks the mutex for the main write, avoiding recursive/re-entrant mutex lock deadlocks.

**Use Cases**:
- **Out-of-Band Concatenation & Templating**: An agent or CLI script can stitch together multiple scratchpad entries (e.g. `text: "Header:\n<SCRATCHPAD_DATA id=\"hdr1\" />\nBody:\n<SCRATCHPAD_DATA id=\"dat2\" />"`) into a single new scratchpad entry in one tool call, without reading or outputting their text payloads into LLM context turns.
- **Token Efficiency**: Allows agents and multi-agent swarms to combine arbitrary-sized datasets out-of-band with zero LLM generation tokens spent on payload contents.

**Why**: `<SCRATCHPAD_DATA />` macros were originally expanded only inside `run_command` (tool `args` and `stdin`). Extending server-side macro expansion to `CreateScratchpad` brings the same zero-token macro capability to scratchpad creation itself, enabling out-of-band data combination without inventing a separate "combine_scratchpads" tool or forcing data through the model's context window.

## D29: `files-rw` gets `tail` and `append`; "replace the last line" is a composition, not a new primitive

Implemented in `pkg/filesrw/ops.go` and `cmd/files-rw/main.go`. Motivating case: an agent maintaining a large append-only ledger needs to (1) see what's at the end of the file, which currently requires already knowing the total line count to compute a `read --start`/`--end` range, (2) append new entries without reading/rewriting the whole file, which the current `read`'s 200KB cap makes outright impossible for a large ledger via existing primitives, and (3) occasionally replace the last line.

**New: `tail <path> [-n N] [--numbers]`.** Native Go implementation (not a subprocess - D26's lesson: a subprocess is exactly where a TOCTOU window reopens), reads through the same `Access.OpenFile`-protected fd `read`/`edit` already use. Returns the last N lines plus a `total_lines` count reported separately from the (possibly capped) returned lines - same shape `search_scratchpad`'s `total_matches`/capped-`matches` split already established (D25). Solves both "see the end" and "know the total line count" in one call - an agent can use the returned `total_lines` to compute a `read --start`/`--end` range itself afterward if it wants a different window.

**New: `append <path>`.** Content via stdin, same convention as `write`. Genuine `O_APPEND` write to the already-open, already-access-checked fd - not read-the-whole-file-then-atomically-rewrite, which would just be `read`+`write` composed manually and still hit `read`'s 200KB cap for a large ledger, defeating the entire point. This is safe to do post-D26 specifically because D26 moved hardlink defense into the access check itself (`Nlink > 1` rejected before any I/O happens, via `Access.OpenFile`) rather than relying on `write`'s atomic-rename as an incidental side effect (the original D22/D23 mechanism) - so `append`'s in-place write doesn't lose any hardlink protection `write` has. **Honest tradeoff, stated plainly**: `append` does lose `write`'s full-swap crash safety - a crash mid-append can leave a truncated trailing write on disk, where `write` never leaves a half-written file (temp file discarded, original untouched). Worth documenting in `append`'s own `--help`, not just here.

**Explicitly rejected: a dedicated "replace the last line" command.** Resolved by composition instead, using `tail` plus the two edit primitives that already exist:
- If the last line's content is unique in the file: `tail -n 2` to see it, then `edit --old "<second-to-last>\n<last line>" --new "..."` (widening the match to a two-line window is usually enough even when the last line *alone* isn't unique - an empty line or a repeated value is unlikely to repeat as part of the exact same *pair*).
- If even that's not unique: `tail -n N` to learn `total_lines` and the current tail content, then a one-hunk unified diff via `patch` targeting `@@ -total_lines-1,2 +total_lines-1,2 @@` - `patch` targets by *position*, verified against *local* context, and never requires the target line's content to be unique globally the way `edit` does. This is the case `edit` structurally can't cover (a genuinely repeated/empty last line), and it's exactly what `patch`'s design is already good at.

A dedicated primitive would reopen the same line-offset-editing risk D23 already argued against rejecting for the general case - "the last line" is a narrower, lower-risk target than an arbitrary `N..M` range (no counting arithmetic against a file the agent hasn't fully read), but narrower isn't risk-free, and composing existing primitives fully covers both the common case (unique tail) and the edge case (non-unique tail) without adding one.

**Why**: `tail` and `append` close a real capability gap the same way `copy`/`move`/`delete` did in D23 - a ledger-style workload is currently unreachable through `files-rw`'s existing primitives, not just awkward. "Replace the last line" turned out not to need a new primitive at all once `patch`'s position-plus-context-verification model (rather than `edit`'s uniqueness model) was actually walked through - composing what already exists both avoids the D23 risk and confirms the existing primitive set is more complete than it first looked.

## D30: Scratchpad storage moves from a single `scratchpad.json` blob to one file per entry

Implemented in `pkg/agent/scratchpad.go`, `pkg/agent/sdk.go`, `pkg/agent/agent_folder.go`, and `cmd/agent.go`. `pkg/agent/scratchpad.go`'s current design (`ReadScratchpadStore`/`WriteScratchpadStore`) reads and JSON-parses *all* live entries' full `Text` on every single operation - `get_scratchpad`, `list_scratchpads`, and `search_scratchpad` each pay the cost of every other entry's content just to touch one, and `create_scratchpad` does a full read-modify-write of the same blob. At a 50-entry cap this is wasteful; it doesn't get better by raising the cap under the current design, it gets worse.

**New storage**: `<agentDir>/scratchpad/<id>-<createdBy>.txt`, one file per entry, content is the raw stored text with no envelope. `id` keeps the existing 4-character `[0-9a-z]` random format; `createdBy` is the short, closed-set tag already in use (`create_scratchpad`, `run_command`, `cli`) - safe to put directly in the filename, no encoding needed. `ScratchpadFileName`/`ScratchpadStore`/`ReadScratchpadStore`/`WriteScratchpadStore` are removed entirely.

**Fields dropped, not just hidden**: `Seq` and `CreatedAt` are removed from `ScratchpadEntry`/`ScratchpadItem` and from every tool result and CLI output that currently surfaces them (`CreateScratchpadResult.Seq`, `cmd/agent.go`'s `scratchpad create` print statement, `list_scratchpads`' entries). Ordering is now conveyed purely by position: `list_scratchpads`/`scratchpad list` sort entries by file mtime ascending (oldest first) and return them in that order, with no explicit sequence number and no timestamp in the output. Real-world wall-clock time is deliberately never exposed to the model through this path - mtime is used only as an internal sort key, never rendered. (The file's mtime still exists as normal filesystem metadata and isn't otherwise hidden from the OS - this is about not routinely handing it to the model in scratchpad output, not a hard confidentiality boundary.)

**ID uniqueness and creation**: generate a random ID, attempt `os.OpenFile(path, O_CREATE|O_EXCL|O_WRONLY, ...)`, retry on collision. This is atomic and correct across separate OS processes for free - unlike the uniqueness check it replaces (scanning the in-memory `liveEntries` map from a just-read blob).

**Get/search by ID**: entries are looked up with a `filepath.Glob(id + "-*.txt")` (one directory scan, filenames only, no content read) since the caller only has the ID, not the `createdBy` suffix.

**Eviction/cap**: raised from 50 to 300, now cheap to raise because per-entry cost stopped scaling with total blob size. On `create`, if live-entry count exceeds the cap, evict the file with the oldest mtime - a `ReadDir` + stat pass over filenames, never file content.

**Locking removed entirely**: `getScratchpadMutex`/`scratchpadMutexes`/`globalScratchpadMu` (`sync.Map` of in-process `*sync.Mutex`, D27's own review already flagged these as giving zero protection across separate CLI processes) are deleted, not replaced. Entries are write-once and never mutated after creation (no `UpdateScratchpad` exists), so reads need no lock; creates are collision-safe via `O_CREATE|O_EXCL`; the only mutating op left, eviction's `os.Remove`, is fine to race - a reader that loses the race just sees "not found," a legitimate outcome for an entry that just aged out. This is a strict correctness improvement, not just a simplification: D27 gave `scratchpad create` the session lock specifically to work around the old in-memory mutex's cross-process blindness, and that workaround is no longer needed either.

**Migration**: none. Existing `scratchpad.json` files are simply never read again post-upgrade (orphaned, harmless, not auto-deleted) - scratchpads are ephemeral working memory, not history worth preserving across this change, unlike `session.jsonl`/`MEMORY.md`.

**Why**: fixes a real, measured resource-waste problem (full-blob read/parse/rewrite on every operation, including read-only ones) and incidentally closes a real cross-process locking gap that was only ever papered over, not fixed, by D27's session-lock workaround. Found while reviewing D29 and evaluating whether `files-rw` had an equivalent problem (it doesn't, per-file already) - prompted checking whether the scratchpad system had the same shape of issue, and it did, worse.

## D31: `a2a-announce-self` skill - agents self-identify by convention, not by injected metadata

Implemented as `skills/a2a-announce-self/SKILL.md`, `always_load: true`. Resolves the previously-logged "a receiving agent has no idea whether it was called by another agent or a human" TODO, choosing the skill-based direction that TODO had already left as the current lean.

**Mechanics**: the skill instructs an agent to prefix any message it sends to another agent (`wackypub agent <id> prompt "..."`) with a one-line preamble - `[Message from agent: <id>]` - naming its own agent ID before the actual message content. Nothing enforces this server-side; it's a convention an `always_load` skill teaches every agent that has it loaded, the same way `WACKYPUB_ALLOWED_AGENTS`'s self-targeting gotcha is taught via `scratchpad-efficiency` rather than special-cased in code.

**Why self-reported over `WACKYPUB_CALL_CHAIN`-derived**: the TODO's hard-coded alternative (have `wackypub` itself read `WACKYPUB_CALL_CHAIN` and auto-inject the caller's ID) would be more reliable - a caller can't misreport an ID it never had to type - but rigid, and solves a reliability problem ("what if the caller lies") that doesn't come up in the motivating case: cooperative agents in the same workspace that just need to know who they're talking to, not agents defending against an adversarial peer. Self-reporting is enough for that, costs a single line, and leaves room for a caller to say more about itself than a bare ID if useful later. The hard-coded, `WACKYPUB_CALL_CHAIN`-backed version remains available to build if a use case ever needs sender identity that can't be spoofed - not done here.

**Why now**: surfaced by live use, not the earlier design discussion alone - agents in a live multi-agent test were seen reading each other's `MEMORY.md` to reconstruct context because they had no idea who else was in the conversation, in a workspace where the cross-agent read access was itself legitimate (gated correctly by `WACKYPUB_ALLOWED_AGENTS`) but the resulting confusion wasn't - a cheap, real fix for a problem that had already shown up, not a hypothetical one.

## D32: `.env` file support in agent workspaces for tool execution

Implemented in `pkg/agent/dotenv.go` and `pkg/agent/agent_folder.go`. Allows agent directories (`<ws_dir>/<agent_id>/.env`) to define environment variables that are automatically loaded into `exec.Cmd.Env` when executing discovered tools via `run_command`.

**Location & Format**:
- `.env` file at `<ws_dir>/<agent_id>/.env` (parsed via `ParseDotEnv` helper in `pkg/agent/dotenv.go`).
- Supports standard `.env` line formats: `KEY=VAL`, `export KEY=VAL`, double-quoted strings (`"..."` with unescaping), single-quoted strings (`'...'`), blank lines, and `#` comments. Missing file (`os.IsNotExist`) is handled silently (returns empty map, no error).

**Environment Precedence & Isolation**:
- **Precedence Order**: Host process environment (`os.Environ()`) < Agent `.env` (`<agentDir>/.env`) < Invocation `args.Env` (from LLM tool call).
- **Execution Isolation**: Variables from `.env` are loaded per-agent into `FolderAgent` (and `LoadAgentDotEnv`) and passed to `executeTool` during `run_command` invocation. The global process environment (`os.Setenv`) is **never** mutated, preserving thread-safety for concurrent `AgentSDK` callers.

**Deferred**:
- Workspace-level fallback (`<ws_dir>/.env`) and using `.env` for LLM model adapter API keys are explicitly deferred. `.env` is strictly scoped to tool execution environment.

**Why**: Tool binaries in `tools/` often require credentials, database URIs, or execution flags. Stashing these in `.env` avoids hardcoding secrets in `runtime.json` or forcing the LLM to supply sensitive values in `args.Env` on every turn.

## D33: Standardize agent-to-agent metadata via `AGENT2AGENT` JSON environment variable

Implemented in `pkg/agent/a2a.go` and `pkg/agent/workspace.go`. Standardizes inter-agent communication and call-chain propagation using the Agent2Agent (A2A) protocol metadata format.

**Payload Structure**:
- `AGENT2AGENT` environment variable carries a dense (minified) JSON payload:
  `{"caller_id":"<id>","call_chain":["id1","id2"],"trace_id":"<id>","metadata":{...}}`
- `caller_id`: The immediate caller agent ID.
- `call_chain`: Ordered array of agent IDs in the active execution path used for cycle/deadlock prevention (supersedes legacy `WACKYPUB_CALL_CHAIN`).
- `trace_id`: Optional correlation ID for multi-agent swarm flows (auto-generated if empty).
- `metadata`: Key-value map for out-of-band context flags.

**Validation & Propagation (`ValidateAgentTarget`)**:
- Ingests `AGENT2AGENT`. If `AGENT2AGENT` is missing, falls back to parsing legacy `WACKYPUB_CALL_CHAIN` CSV strings for backward compatibility.
- Rejects calls if `targetAgentID` already exists in `call_chain` (deadlock cycle prevention).
- Appends `targetAgentID`, sets `caller_id`, serializes to dense JSON, and sets `os.Setenv("AGENT2AGENT", json)` for the duration of the target command, restoring the original environment state on cleanup.

**Why**: Standardizing on A2A metadata enables seamless interoperability with external agent platforms while providing receiving agents with reliable, un-spoofable caller identity and trace context.

## D34: Bundled `wackypub-a2a` and `wackypub-ws` Skills via `go:embed` and `wackypub skill`

Implemented in `main.go` and `cmd/root.go`. Embeds `skills/wackypub-a2a/SKILL.md` and `skills/wackypub-ws/SKILL.md` directly into the `wackypub` binary using Go's `//go:embed` directive and exposes them via `wackypub skill [a2a|ws]` subcommand and `wackypub --skill [a2a|ws]` persistent flag.

**Mechanics**:
- `main.go` uses `//go:embed skills/wackypub-a2a/SKILL.md` and `//go:embed skills/wackypub-ws/SKILL.md` to embed the skill files at build time.
- **`wackypub-a2a`**: Covers CLI usage for agent-to-agent (A2A) communications, command self-discovery, flag ordering caveats, and cross-agent execution. Retrieved via `wackypub skill a2a` or `wackypub --skill a2a`.
- **`wackypub-ws`**: Covers workspace setup, environment secret stashing (`.env`), agent scaffolding, Git versioning management (D35), causal tracing notes (D36), and recommended symlink organization patterns (`runtimes/`, `skillsets/`, `toolsets/`). Retrieved via `wackypub skill ws` or `wackypub --skill ws`.

**Why**: Modularizing workspace setup from inter-agent calling gives agents and human operators focused, context-efficient skill guidance tailored to their specific task (workspace creation vs A2A messaging).

## D35: Per-Agent Git Repositories and Workspace Manifest Coordination

Implemented in `pkg/agent/git.go` and `cmd/workspace.go`. Provides pure-Go per-agent event versioning, A2A revision lineage, workspace snapshots, tagging, and remote pushing via `github.com/go-git/go-git/v5`. This entry reflects the state after review - the first pass had three real problems, all fixed before this was trusted enough to commit; see "Reviewed and fixed" below rather than treating this as having landed clean.

**Per-Agent Isolation (`<ws_dir>/<agent_id>/.git`)**:
- Each agent directory operates its own isolated git repository, committed to independently of every other agent's repo.
- **Default `.gitignore`**: Per-agent `.gitignore` excludes everything by default (`*`) except core agent files (`AGENTS.md`, `IDENTITY.md`, `MEMORY.md`, `runtime.json`, `.env`, `session.jsonl`, `scratchpad/`, `skills/`, `tools/`). Workspace root `.gitignore` ignores everything except root metadata files (`.gitignore`, `WACKYPUB_ROOT`, `MANIFEST.md`).

**A2A Spec-Compliant Revision Lineage**:
- When **Agent A** targets **Agent B**, `ValidateAgentTarget` reads Agent A's HEAD commit SHA and injects `workspace_revision` into `A2AMetadata.Metadata`.
- Agent B's event commit message embeds Agent A's SHA, forming an un-spoofable cross-agent audit trail.

**Workspace Coordinator Subcommands**:
- **`wackypub workspace snapshot`**: Scans all agent repos under `<ws_dir>`, writes `<ws_dir>/MANIFEST.md` with each agent's active commit SHA, and commits `MANIFEST.md` in the workspace root repo.
- **`wackypub workspace tag <name>`**: Tags the workspace root repo with `<name>` and tags each agent repo with `tag-<agent_id>`.
- **`wackypub workspace push <remote>`**: Reads the named `<remote>` configuration and pushes each agent repo (`<ws_dir>/<agent_id>/.git`) to the remote as a branch matching its `<agent_id>`, along with all workspace and agent tags.

**`runtime.json` gets `${VAR}`/`$VAR` expansion**: `LoadRuntimeConfig` now loads workspace-root and per-agent `.env` files and expands environment variables in `runtime.json` before parsing it - lets a shared, symlinked `runtime.json` template reference a per-agent or per-workspace secret (`"apiKey": "${OPENROUTER_API_KEY}"`) instead of hardcoding one per file. Doesn't change what ends up tracked in git (the `.env` holding the real value is tracked too, see below) - this is about config reuse, not secret hygiene.

**Reviewed and fixed before commit** (caught in review, not shipped as first written):
1. **Every tool call committed twice.** The first pass called `CommitWorkspaceEvent` before and after every single `run_command` invocation, plus again inside `CreateScratchpad` for auto-captured output - with `--max-tool-turns` defaulting to 300, a single generation could trigger hundreds of full-tree `git add -A` + commit cycles. Fixed by committing once per user turn, once per assistant turn, and once per compaction instead - matches normal turn cadence, not tool-call cadence.
2. **`workspace push`/`workspace tag` silently "succeeded" on total failure.** Every per-repo push/tag error was discarded (`_ = repo.Push(...)`). Verified live: pointed a remote at a nonexistent host, ran `push`, got a clean "Pushed" message and exit 0 with nothing actually pushed. Fixed - both now collect every per-repo error and return them.
3. **The workspace-root fallback for agents without their own repo did nothing.** `ResolveGitRepoDir` fell back to the shared workspace-root repo for a named agent lacking its own `.git` - but the root `.gitignore` excludes every agent directory, so that fallback could never actually capture anything about the agent, while reporting no error. Verified live: 5 scratchpad creates against such an agent produced exactly 1 commit containing only root metadata. Fixed - the fallback now only applies to genuinely workspace-level events (no `agentID`), not per-agent ones.

**`runtime.json`/`.env` being tracked is intentional, not a bug - deliberately, not by oversight.** Both hold real secrets (API keys, tokens) and both get committed in plaintext, confirmed live. Kept anyway: model/backend switches are meant to be part of an agent's auditable history, and workspace data as a whole - not just these two files - is meant to be treated as sensitive regardless of what's in it. The standing assumption is that a workspace only ever gets pushed to a private, trusted remote, never a public one. That assumption is a real risk if someone forgets it once, so `workspace push` backs it with an actual gate rather than just a docs warning: it hard-refuses without `--i-understand`, and that flag is deliberately left out of `--help` - the point is to force at least one encounter with the warning text before the flag can be used, not to make it hard to find permanently. `skills/wackypub-ws/SKILL.md` repeats the same warning and adds "notify your user before pushing."

**Revised again, not yet implemented: commit cadence moves from turn-boundary-only to once per `run_command` dispatch.** Turn-boundary-only (item 1 above) is cheap but breaks `wackypub trace` (D36) across an A2A hop: if Agent X calls Agent Y mid-turn, X's HEAD at that instant is still the *start-of-turn* commit ("assistant" hasn't fired yet, generation isn't done) - so `workspace_revision` handed to Y always points at the state before X's turn even started, permanently, since it's baked into Y's own commit at that instant and nothing can retroactively point it at a commit that doesn't exist yet. Tracing back from Y always lands at X's turn boundary, never at what X actually did before deciding to call out.

Considered and rejected: committing X's pending state from inside `ValidateAgentTarget` specifically. Doesn't work - `ValidateAgentTarget` runs inside the *newly spawned child process* (the `wackypub agent Y prompt` subprocess X's `run_command` call launches), not inside X's own still-running parent process. Having that child process commit into X's repo means two separate OS processes mutating the same agent's workspace at once - the one invariant every other part of this system leans on (only one process touches a workspace at a time) would break, quietly, for the first time.

Landed on instead: the calling process itself commits once, synchronously, immediately before it spawns *any* `run_command` subprocess - not just wackypub/A2A ones, every tool call uniformly, no detection logic for "is this an A2A call" needed at all. Built-in in-process tools (`create_scratchpad`, `get_scratchpad`, `list_scratchpads`, `search_scratchpad`, `load_skill`) are deliberately excluded - they never spawn a subprocess, carry no handoff risk, and get captured by whatever surrounding commit already exists.

Honest cost: this is close to the per-tool-call cadence item 1 above just moved away from - up to ~300 commits in a worst-case tool-heavy generation again, not the handful per turn item 1 landed on. Accepted anyway: git tracking is opt-in per agent (a no-op if `.git` was never initialized), each commit is small, and it's the same total bytes changed either way - six parts across three small commits instead of six parts in one, not six parts written six times. The DAG/object-count overhead is real but bounded, not a duplication-of-work problem.

**Why**: solves concurrency lock contention between agents (not "100% lock-free" full stop - the root-repo fallback path for un-migrated agents still shares one repo, which is why item 3 above matters) while providing on-demand swarm history coordination, snapshotting, and single-remote multi-branch sync.

## D36: Causal Swarm Tracing (`wackypub trace`)

Implemented in `pkg/agent/trace.go` and `cmd/trace.go`. Provides multi-turn, backward causal graph traversal across per-agent git repositories and correlation IDs. First pass rendered only the bare git commit message (no real turn content, at any verbosity level) - caught in review, fixed by diffing `session.jsonl` against the parent commit to isolate what each commit actually added, verified live. One known gap logged as a TODO rather than blocking: a compaction commit shrinks `session.jsonl` instead of growing it, so the count-based diff shows misleading content specifically at `compact` steps - narrow, doesn't affect hop logic or the normal turn-by-turn case.

**1. Invocation Syntax**:
- `wackypub trace [flags] <agent_id> <commit>`: Backward trace from a specific commit ref in `<agent_id>`.
- `wackypub trace [flags] <trace_id>`: Global trace matching `<trace_id>` across all workspace agent repositories.

**2. Commit Specifier Resolution**:
- Supports full 40-character SHAs, short SHA prefixes/suffixes, relative refs (`HEAD`, `HEAD~1`), branch names, and git tags (`v1.0.0`, `tag-<agent_id>`).

**3. Intra-Turn & Inter-Agent Traversal**:
- **Intra-Agent Walking**: Within an agent's repository, `trace` steps backward commit-by-commit through the agent's turn history (showing tool calls, tool responses, and intermediate sub-agent dispatches).
- **Inter-Agent Boundary Hop**: When `trace` encounters a turn origin (`user` commit) or an outgoing A2A tool call, it reads `metadata.workspace_revision` from the `AGENT2AGENT` payload and hops to the parent agent's git repository.
- **Termination**: Stops when it reaches $N$ steps (`-n 20`), a root user input, an unresolvable commit, or an external non-local workspace boundary.

**4. Verbosity Levels (`-v / --verbosity 0..4`)**:
- `0` (Minimal): Event type, `functionCall` names, user prompt text (no assistant text, no `functionResponse`).
- `1` (Compact — Default): Event type, tool names (plus `command` string for `run_command`), user text, assistant text.
- `2` (Clean Full): User, assistant, tool call, and tool response text, stripped of reasoning/thinking blocks and signatures.
- `3` (Full with Thinking): Complete content including thinking/reasoning blocks (stripped of provider signatures).
- `4` (Raw JSONL): Dumps raw `genai.Content` JSON lines and `AGENT2AGENT` payloads as-is.

**Why**: Gives swarm operators and LLM debuggers standard debugger-like step-by-step causal tracing across multi-agent execution graphs, linking failures, tool calls, and responses back to the originating prompt across isolated git commit histories.

*Note*: `skills/wackypub-ws/SKILL.md` holds a reference section for `wackypub trace` and will be updated with full command usage options upon completion of D36 implementation.

## D37: `json_escape` attribute for `<SCRATCHPAD_DATA />` macros

Implemented in `pkg/agent/scratchpad.go`. Verified live against the exact motivating scenario (a JSON template with a macro nested in a string value, escaped-quote attribute syntax and all) - expands to valid JSON, content round-trips byte-for-byte through encode/decode. `id`/`skip_lines`/`num_lines`'s regexes also needed loosening to tolerate escaped quotes (`\"` as well as `"`) around their own attribute values, not just `json_escape`'s - a real, necessary consequence of the macro syntax itself now needing to survive being embedded inside a JSON string, not scope creep. Surfaced by a real live snag: an agent using an MCP-wrapped Notion tool (via `mcporter`) needed to inject a large chapter's text into a `create-pages` call whose `content` field sits nested inside a JSON request body. Raw macro substitution (the only mode that exists today) works fine when the target slot is a plain string flag/stdin (`create-attachment --content "<SCRATCHPAD_DATA id=... />"` worked cleanly) but breaks when the payload lands inside a JSON string value - unescaped quotes, newlines, and control characters produce invalid JSON. The agent's only working fallback was reading the chapter into its own context and re-emitting it as an escaped JSON string by hand, which is exactly the token cost and truncation/escaping risk the macro system exists to avoid.

**Considered and rejected**: having the runner auto-detect when a macro occurrence sits inside a JSON string value and escape it automatically. Rejected as fragile and disproportionate - it would mean parsing the surrounding text as JSON while it still has a template hole in it, a much harder and less predictable problem than anything else in the macro system, which is otherwise a plain regex substitution with no context-sensitivity at all.

**Landed on instead**: an explicit `json_escape="true"` attribute on `<SCRATCHPAD_DATA />`, alongside the existing `skip_lines`/`num_lines`. When set, the referenced scratchpad content is substituted as JSON-escaped text (backslashes, quotes, newlines, control characters, per RFC 8259) instead of raw bytes. Implementation note: `encoding/json.Marshal` on a Go string already produces correct, spec-compliant escaping including outer quotes - the simplest correct approach is to marshal and then trim exactly one leading and one trailing `"` from the result, rather than hand-rolling escaping rules.

**Deliberately does not wrap the result in quotes.** The macro expands to just the escaped inner text; the caller's own template supplies the surrounding quotes (`"content": "<SCRATCHPAD_DATA id=\"chapter1\" json_escape=\"true\" />"`). Keeps this composable the same way raw injection already is - the macro doesn't know or assume anything about where it's being dropped in. This needs to be stated explicitly wherever `json_escape` is documented (tool description text, `--help`, any skill mentioning it), since it's the one behavior an agent can't infer from the attribute name alone.

**Doesn't by itself solve payload size for very large documents** - that's a separate, already-solved concern: `args` entries are capped at `MaxExpandedArgBytes` (500,000 bytes) after expansion, but `stdin` expansion has no size cap at all (confirmed in `executeTool`, `pkg/agent/agent_folder.go`). A large `json_escape`'d payload should go through `stdin` (assembling a full request body in a scratchpad entry via macro composition, then `stdin: <SCRATCHPAD_DATA id="assembled" json_escape="true" />`), not `args`. Whether that's viable for a specific external tool depends on whether that tool's own CLI accepts a full body on stdin - not something wackypub controls.

**Explicitly deferred, not part of this decision**: a `<SCRATCHPAD_FILE id="X" />` out-of-band file-handle token, per-tool "injectable/large parameter" capability metadata, and server-side chunked injection for streaming into an existing document. All were proposed alongside `json_escape` for the same live problem; none are needed unless `json_escape` + `stdin` turns out insufficient for some case not yet seen - matches this project's standing preference for composing existing primitives over adding new ones ahead of demonstrated need (D29, the rejected `files-rw search` command).

**Why**: closes a real gap found live, not hypothetical - the existing macro system had exactly one substitution mode (raw bytes into any slot), which is correct for the common case and silently wrong the moment the slot is JSON-structured. `json_escape` extends the existing mechanism with one more escaping mode rather than replacing it or adding a parallel injection system.

## D38: `sessionCompactPct` moves out of `runtime.json`; per-agent `COMPACT.md` for custom compaction

Implemented in `pkg/agent/compaction.go` and `pkg/agent/runtime.go`. One real bug caught in review: the implementation's edit to `compaction_test.go` left `TestCompactionEndsOnModelTurn` with an empty `if remaining[0].Role != "user" { }` block (assertion body deleted, empty shell left behind - syntactically valid, so nothing caught it) while also deleting a separate, still-correct turn-count assertion that didn't need to change (the default compaction percentage is unchanged at 50%, just sourced from a different place). Both restored and verified passing. Real, permanent test coverage for the new behavior (`TestLoadCompactConfig_Defaults`, `TestLoadCompactConfig_CustomFrontmatterAndBody`, `TestCompactionWithCustomCompactMD`) was initially missing and added after review - all three verified: custom prompt body actually reaches the model and the default doesn't leak through, `append-only: false` genuinely replaces `MEMORY.md`, `compact-pct` overrides are honored.

**`sessionCompactPct` leaves `RuntimeConfig` entirely.** It was never a model/provider/backend-connection setting - it's a compaction-process knob, and `runtime.json` is specifically model backend config (endpoint, model, apiKey, contextWindow). Default compaction percentage becomes a fixed internal constant (50%, same value `RuntimeConfig`'s own default already was) when nothing overrides it. Existing `runtime.json` files that still set `sessionCompactPct` aren't migrated - the field is just gone from the struct, and Go's JSON unmarshal silently drops unrecognized fields, same non-migration already used for `scratchpad.json` post-D30.

**New: `<agentDir>/COMPACT.md`, optional, per-agent only** (not a workspace-root fallback like `.env`'s - deliberately simpler, one location, no dual-lookup merge logic). Same shape as `SKILL.md`: YAML frontmatter for parameters, Markdown body for the actual payload.

```yaml
---
append-only: true   # default true
compact-pct: 50      # default 50
---
<compaction directive prompt - replaces CompactionDirectivePrompt entirely if this file exists>
```

- **`append-only`** (bool, default `true`): `true` keeps current behavior - the addendum is appended to `MEMORY.md`. `false` replaces `MEMORY.md` wholesale with the addendum instead.
- **`compact-pct`** (default 50): replaces `sessionCompactPct` - how much of the session gets included in a compaction run and stripped out after. Same validation as today (invalid/out-of-range falls back to 50).
- **Body, if the file exists, fully replaces `CompactionDirectivePrompt`** - not supplemented, not merged, a straight swap of which text becomes the "Compaction Directive" user turn in `CheckAndCompactSession`'s request. Everything else about how that request gets built (system prompt + `<PERSISTENT_MEMORY>` first turn, turn-merging, etc.) is unchanged regardless of which prompt is in use.
- **No format enforced on custom bodies.** The default prompt's output is a bullet list meant to be appended; a custom body in `append-only: false` mode is presumably producing something meant to stand alone as a full `MEMORY.md`. Wackypub doesn't validate or reshape either - whoever writes a custom `COMPACT.md` body is responsible for producing output that matches the `append-only` mode they picked.
- **When `COMPACT.md` doesn't exist**: identical behavior to today, just sourced from fixed defaults instead of `runtime.json` - `compact-pct` 50, `append-only` true, built-in `CompactionDirectivePrompt`.

**`use-memory-md` explicitly dropped, not deferred with a flag.** Discussed alongside `append-only`/`compact-pct` as a way to redirect compaction output somewhere other than `MEMORY.md` entirely, but cut once it became clear it wasn't fully thought through yet and, more importantly, wouldn't currently do anything real: `CheckAndCompactSession` calls `llmModel.GenerateContent` directly, not through the runner/tool pipeline (see the existing "compaction bypasses the runner pipeline" TODO) - there's no tool-calling available during compaction, so "the compaction instructions do something else instead" has nowhere to actually execute. Revisit if a real use case shows up, likely bundled with actually fixing that pipeline gap rather than before it.

**Why**: `sessionCompactPct` living in `runtime.json` was a category error from the start - a process-tuning knob sitting next to backend connection details. `COMPACT.md` gives per-agent control over both the mechanical parameters and, if needed, the entire compaction prompt, using a shape (`SKILL.md`-style frontmatter + body) the codebase and its documentation already teach rather than inventing a new config convention.

**Scope includes cleanup, not just new code**: `examples/runtimes/openrouter-haiku.json`, `anthropic-sonnet.json`, `openrouter-auto.json`, `gemini-flash.json`, and `llamacpp.json` all currently set `sessionCompactPct` and need it removed as part of this change, not left as dead fields in the shipped examples. Also add a new example `COMPACT.md` (`examples/` alongside `examples/runtimes/`, exact location TBD) showing the frontmatter shape and a minimal custom body, so the feature has a working reference the same way `examples/runtimes/` already does for backend config.

## D39: Line counts on auto-captured scratchpad output and `list_scratchpads`

Implemented in `pkg/agent/scratchpad.go` and `pkg/agent/agent_folder.go`. Verified live: filename format, `CountLines` (matches `TailFile`'s split-on-`\n`-drop-trailing-empty convention, table-tested), auto-capture tags, and `list_scratchpads` output all correct, `list_scratchpads` still doing zero content reads as intended.

**Auto-capture (`<STDOUT>`/`<STDERR>` tags from `run_command`, `agent_folder.go`)**: add `lines="N"` alongside the existing `size="N"` attribute - `<SCRATCHPAD_DATA id="X" size="1234" lines="42" />`. Free to add: `CreateScratchpad` already has the full text in memory at the moment it writes the entry, so counting lines costs nothing extra there.

**`list_scratchpads`**: also gets line counts, but *not* by reading every entry's content on every list call, which would partially reintroduce the exact problem D30 eliminated (`ListScratchpads` today is `ReadDir` + `os.Stat` per entry only, zero content reads). Since entries are write-once and never mutated after creation (same invariant D30's whole no-locking design already leans on), the line count is fixed the instant an entry is created - same as `createdBy` already is. So it gets encoded in the filename the same way: `<id>-<lines>-<createdBy>.txt` instead of today's `<id>-<createdBy>.txt`. `list_scratchpads` stays a pure filename-parse operation with no content reads, just one more field to parse out of a name it's already parsing.

**Landed less "clean break" than originally decided, kept anyway.** The plan was for old-format `<id>-<createdBy>.txt` files (D30) to become invisible to the new parser rather than specially handled. What actually shipped (`parseFilenameParts`, `pkg/agent/scratchpad.go`) branches on segment count and recovers `id`/`createdBy` from old-format files too - `lines: 0` for those, not full invisibility - with a dedicated test (`TestListScratchpads_OldFormatRobustness`) locking that in. Reviewed and kept as-is rather than sent back: it's not dangerous (never misparses `createdBy` as a line count, never crashes on the old shape) and arguably more graceful than the original plan, just not what "clean break" specified. Noting the deviation here rather than silently treating the original wording as still accurate.

**Line-counting convention**: match `TailFile`'s existing logic (`pkg/filesrw/ops.go`, D29) exactly rather than inventing a second one - split on `\n`, drop a trailing empty segment so a file ending in a newline doesn't get counted as having one extra (empty) line.

**Why**: line count is something an agent deciding how to paginate a large auto-captured entry (`get_scratchpad`'s `skip_lines`/`num_lines`) needs before it can plan a sensible read, and `list_scratchpads` giving byte size without line count forces a guess. Doing it via filename encoding rather than a content read keeps `list_scratchpads` exactly as cheap as D30 made it, rather than trading away that property for a UI convenience.

## D40: `wackypub skill` drops the `--skill` flag; no-argument form lists instead of defaulting

Implemented in `cmd/root.go` and `pkg/agent/skill.go` (factored `ParseSkillFile` into a shared `ParseSkillContent` core so the listing reads real frontmatter instead of a hardcoded copy). Verified live - no-arg listing, named-skill printing, the new `--help` hint, and the removed `--skill` flag correctly erroring. Amends D34.

**`--skill` persistent flag removed entirely.** D34 shipped both a `wackypub skill [a2a|ws]` subcommand and a `--skill [a2a|ws]` flag doing the same thing two ways - one canonical path is simpler for both a human and an agent discovering the CLI to reason about, and the subcommand is the one that fits this project's own `--help`-driven discovery convention (a flag on the root command doesn't show up in `wackypub --help`'s own subcommand listing the way `wackypub skill --help` does).

**`wackypub skill` with no argument now lists the available skills instead of silently defaulting to `a2a`.** D34's `GetSkillContent("")` fell back to printing `wackypub-a2a`'s content - meaning running the bare command with no argument silently picked one skill over the other with no indication a choice was even being made. Now it prints a list (name + each skill's own `description` from its frontmatter, not a separately hardcoded copy that could drift from it) and requires picking one by name to actually print it, same shape as the on-demand skill picker inside a live agent already uses (`agent_folder.go`'s `load_skill` listing).

**Agent-facing hint added to the command's own `Short` description**, since that's the line that actually shows up in `wackypub --help`'s "Available Commands" listing - something to the effect of "if you're an agent, you'll want to load one of these" - so an agent doing cold `--help`-driven discovery (the CLI's own stated design constraint, see Philosophy) has a reason to actually run `wackypub skill` rather than skip past it as a human-only utility command.

**Why**: two ways to do the same thing is exactly what D34 shouldn't have shipped given this project's own repeated stance against redundant surfaces, and silently defaulting a no-argument invocation to one specific skill hides that a choice is being made at all - listing makes the choice explicit instead of guessing on the caller's behalf.

## D41: `wackypub workspace` self-identifies from CWD; `trace`/`workspace snapshot`/`tag`/`push` blocked from agent context

Implemented in `pkg/agent/workspace.go` and `cmd/workspace.go`/`cmd/trace.go`. Verified live - identity display only fires when CWD is exactly an agent's own directory, all four commands correctly refused with a clear message from that context, unaffected from the workspace root, `init-git` confirmed still unblocked.

**The problem, confirmed by tracing the code, not assumed**: an agent has no structural way to know its own ID or who it's allowed to call. There's no env var, no macro, nothing - a system prompt only says "you are agent bob" if whoever wrote that agent's `AGENTS.md` typed it by hand, and `WACKYPUB_ALLOWED_AGENTS` is a plain file the agent has no built-in way to see (would need `files-rw`/`bash` access to that specific file even to try). This is exactly the kind of thing that drifts across copy-pasted `AGENTS.md` files.

**Fix: extend `wackypub workspace` (the bare, no-argument form) rather than add a macro or new tool.** Agents already gravitate to running it cold at the start of a session to orient themselves - so when CWD is detected as being an agent's own directory, prepend an identity section ("You are agent X. Agents you can talk to: ...") to the existing workspace overview, rather than inventing a new command or a `AGENTS.md` macro for something the CLI can already tell you if it looks.

**Detection method**: reuses the exact pattern D16's `ValidateAgentTarget` already established for the same purpose (its own `sendingAgentID` computation) - `looksLikeAgentDir(cwd)` true means CWD *is* (not contains, not is a subdirectory of) an agent's directory, and `filepath.Base(cwd)` is that agent's ID by convention. Factored into a new shared `CurrentAgentIDFromCWD()` helper (`pkg/agent/workspace.go`) rather than duplicated. Deliberately a direct check, not an upward walk from CWD toward the workspace root the way `ValidateAgentTarget`'s authorization check does - matches `run_command`'s own behavior exactly (`executeTool` always sets `cmd.Dir` to the calling agent's directory precisely, never a subdirectory of it), so there's no case in the actual call path this needs to handle that a direct check misses.

**This resolves the open question in TODOS.md** ("does `WACKYPUB_ALLOWED_AGENTS` restrict CWD-based invocations in general, or only actual tool-call context") in the sense that matters here, though it's a different application, not the same question answered: that TODO was about whether *authorization* should be CWD-scoped (unchanged by this decision - D16's existing behavior stands). This decision uses the same CWD-is-an-agent-dir signal for two new, different purposes - self-identification (informational) and blocking a specific set of commands (below) - not for authorization itself.

**`trace`, `workspace snapshot`, `workspace tag`, and `workspace push` all blanket-refuse when `CurrentAgentIDFromCWD()` detects an agent context.** These are operator/diagnostic and cross-workspace-coordination commands, not something an agent driving its own turn should be invoking on itself - `trace` in particular is a debugging tool for a human or external process to inspect causal history *across* agents, not something meaningful for a single agent to run on itself mid-turn. No override flag, no exception - if the goal is to run one of these against a specific agent's data, do it from the workspace root (or anywhere that isn't literally that agent's own directory), which remains completely unaffected. `workspace init-git` is deliberately *not* in this list - initializing git tracking on one's own directory is a reasonable thing for an agent to do for itself, unlike the other four.

**Why**: the identity gap was a real, live-reported problem, not speculative, and the fix reuses two things that already exist (`wackypub workspace`'s role as the "orient yourself" entry point, and D16's own CWD-detection pattern) rather than adding a third mechanism (a macro system) on top of an already-growing set of ways an agent learns about its environment. Blocking the four operator commands from the same detected context closes off a surface that was never intentionally exposed to begin with - nothing ever decided agents should be able to run `trace`/`snapshot`/`tag`/`push` on themselves, it was just never prevented.

## D42: `files-rw access` - report an agent's own granted permissions

Implemented in `pkg/filesrw/access.go` (`Access.Summary()`) and `cmd/files-rw/main.go`. Verified live - correct read-write/read-only split, dedup (a write root does not also list under read-only), graceful "(none)" when a list is empty. Same family of problem as D41 (self-orientation), different tool: agents were getting confused about what `files-rw` actually lets them touch, with no way to ask other than trial and error against `read`/`write` and reading the error message.

**New `files-rw access` command, no arguments.** Reads `FILES_RW_ACCESS` via the same `LoadAccess` every other `files-rw` command already calls, then reports what it parsed: read-write roots, read-only roots (the two are currently conflated in `Access.readableRoots`, a superset of `writableRoots` - the command reports them as two clearly separate lists, not the internal representation's shape), and a note that `FILES_RW_ACCESS`'s own path is always denied for writes regardless of the rules. Requires exporting a `Summary()` (or equivalent) method on `Access`, since its fields are all unexported today.

**Deliberately does not read `FILES_RW_ACCESS` as a file through the normal access-check path.** Confirmed live while designing this: `files-rw read FILES_RW_ACCESS` already fails today whenever the granted root is a subdirectory rather than the CWD itself (`Resolve` has a bypass for reading `FILES_RW_ACCESS` itself, `OpenFile` - what every real read operation actually goes through - doesn't reach that bypass unless root-membership already passed). Logged as its own TODO rather than fixed here, since it's a separate, pre-existing inconsistency. `files-rw access` sidesteps it entirely by formatting the struct `LoadAccess` already parsed into memory, rather than re-reading the file as text through the same path that has the gap.

**Why**: an agent that can't answer "what am I actually allowed to touch" without trial-and-error against real read/write attempts is exactly the kind of self-orientation gap D41 just fixed for agent identity - same shape of problem, same fix pattern (make the tool answer directly instead of making the agent guess), applied to `files-rw` instead of `wackypub` itself.

## D43: Identifying HTTP headers on every OpenAI-compatible request

Implemented in `pkg/agent/openai_model.go` and `pkg/agent/runtime.go`. Surfaced live: on OpenRouter, every request from `wackypub` showed up as "unknown" - no app name, no attribution - because nothing was ever setting the headers OpenRouter's own dashboard/rankings key off (`X-Title`, `HTTP-Referer`).

**The capability already existed one layer down and was just never wired up.** `adk-utils-go`'s `Config.HTTPOptions.Headers` (a plain `http.Header`) is already threaded through to every request via `option.WithHeaderAdd` - `pkg/agent/openai_model.go` just never populated it. No fork changes needed for the actual fix.

**Defaults**: `X-Title: WackyPub`, `HTTP-Referer: https://github.com/colinrgodsey/wackypub`, sent unconditionally on every request regardless of provider - harmless no-ops on providers that don't recognize them, and specifically what fixes "unknown" on OpenRouter. **Overridable per agent** via a new `runtime.json` field, `extraHeaders map[string]string`, mirroring the existing `extraBody` pattern exactly - a key present there replaces the default of the same name, for anyone embedding wackypub under their own product name rather than "WackyPub."

**`User-Agent` is deliberately not part of this** - tried it, confirmed live it doesn't work. `openai-go`'s own `requestconfig.getDefaultHeaders()` sets `User-Agent: OpenAI/Go <version>` on the request's header map before our options ever apply, and regardless of `adk-utils-go` using append-semantics (`WithHeaderAdd`) for every header we pass, the actual received request only ever has one `User-Agent` value - the SDK's own. Making that overridable would mean patching the `adk-utils-go` fork to use replace-semantics (`option.WithHeader`) for at least that one key, which isn't worth it for a header that isn't the one actually driving the "unknown" problem this was about.

**OpenRouter-specific headers, gated on detection, not sent unconditionally like the two above.** `X-OpenRouter-Categories` (marketplace category assignment - set to `creative-writing,personal-agent`, the two categories that apply) has no non-prefixed alias and no meaning to any other provider, so it's only sent when `isOpenRouter` (the same endpoint-substring check the existing `extraBody` reasoning-effort logic already used - hoisted to the top of `NewOpenAIModel` and shared rather than computed twice). `X-OpenRouter-Title` is also set alongside it for explicitness even though `X-Title` above already covers app naming (OpenRouter documents `X-Title` as an accepted alias for `X-OpenRouter-Title`) - redundant by design, not a mistake. Verified live for both the positive case (endpoint string containing `openrouter.ai`) and the negative case (plain endpoint, headers absent), covered by a permanent test.

**Why**: real, live-observed problem (not hypothetical), fixed by wiring up a capability that already existed rather than adding new plumbing - and the `User-Agent` dead end is recorded here specifically so nobody re-discovers the same wall from scratch later.

## D44: Default compaction prompt moves into an embedded `COMPACT.md`; skill loads no longer get condensed; `--force` for testing

Implemented in `pkg/agent/compaction.go`, `pkg/agent/default_compact.md`, `pkg/agent/sdk.go`, `pkg/agent/agent_folder.go`, and `cmd/agent.go`. Verified live end-to-end against a real mock LLM backend - default embedded prompt (including the new SKILL LOADS guideline) genuinely reaches the model, --force correctly bypasses the threshold check and a plain compact still no-ops without it.

**The hardcoded `CompactionDirectivePrompt` Go constant moves into a real `COMPACT.md`, embedded into the binary, parsed through the exact same code path a user's own `<agentDir>/COMPACT.md` already goes through.** `LoadCompactConfig`'s parsing logic gets factored into a shared `ParseCompactConfig(content string)` (mirrors D40's `ParseSkillFile`/`ParseSkillContent` split) - used for both an on-disk `COMPACT.md` and the embedded default, so there's exactly one parser, not one for real files and a second implicit one baked into Go constants.

**Canonical file location corrected in D45** (see below) to `examples/COMPACT.md`, embedded from `main.go` like the two skills, rather than the `pkg/agent/default_compact.md` + symlink arrangement this decision originally shipped.

**New guideline in the prompt: don't condense loaded skills.** Resolves the previously-open "How compaction should treat loaded skills" TODO - the memory addendum notes that a skill was loaded and that its content wasn't preserved (reload via `load_skill` if still needed) rather than trying to summarize what the skill actually said. Matches how compaction already treats everything else as re-derivable rather than something worth spending addendum space on.

**`wackypub agent compact --force`**: `CheckAndCompactSession` gains a `force bool` parameter that skips the `contextWindow`/token-estimate gate checks (still refuses on a genuinely empty session - forcing compaction with nothing to compact isn't a testing use case, it's a no-op either way). Threaded through `AgentSDK.CompactSession(ctx, agentID, force)`. The automatic pre-generation call site (`GenerateTurn` in `agent_folder.go`) always passes `force=false` - only the manual CLI/SDK path can force it. For testing COMPACT.md changes without needing to actually blow past a real context window first.

**Why**: closes a real TODO (skill-load condensation), removes a Go-constant/on-disk-format split that had no reason to exist once `COMPACT.md`'s shape already existed, and `--force` turns "does my custom compaction prompt actually work" from "wait until a session grows huge enough" into a one-command check.

## D45: Compaction routes through the real runner/`llmagent` pipeline instead of a hand-built request

Implemented in `pkg/agent/compaction.go`, `pkg/agent/agent_folder.go`, `pkg/agent/sdk.go`, and `pkg/agent/compaction_test.go`. Verified live end-to-end (real `agent add`/`generate`/`compact --force` sequence against a mock OpenAI-compatible backend, wire request captured on both sides): the outgoing HTTP JSON body for a forced compaction call and for a normal `generate` call have byte-identical `messages[0]` (system role) and `messages[1]` (memory + first archived user turn), and byte-identical `tools` arrays - the exact prefix-parity the standing TODO asked for, not just a plausible design.

**Also corrects D44's `default_compact.md` file placement**, caught in review immediately after: D44 shipped the real file at `pkg/agent/default_compact.md` with `examples/COMPACT.md` as a symlink pointing into it - backwards from where a human would expect the canonical, discoverable copy to live. Tried reversing the symlink direction first (real file in `examples/`, `pkg/agent/default_compact.md` a symlink pointing out to it) - confirmed live this doesn't work at all: `//go:embed` refuses to read a symlink as its source (`cannot embed irregular file`), regardless of what it points to. The only way to make `examples/COMPACT.md` the one real file is `main.go`'s existing pattern for `wackypub-a2a`/`wackypub-ws` (D34): embed it where a legal, non-`..` path can reach it (only `main.go`, since `examples/` isn't under `pkg/agent/`), then hand the string down - `adkAgent.DefaultCompactMD = bundledDefaultCompactMD`, set directly on `pkg/agent`'s own exported var (not staying at the `cmd` layer like the two skill vars, since `LoadCompactConfig` needs it deep inside `pkg/agent`, not just from CLI commands).

**Cost, accepted deliberately**: `pkg/agent`'s test suite loses its self-contained embed of the real default content - `main.go` never runs under `go test`, so `compaction_test.go` gained a `TestMain` that reads the real `examples/COMPACT.md` via a relative `os.ReadFile` and seeds `DefaultCompactMD` before tests run, rather than every test getting the real shipped content for free the way the original `pkg/agent`-local embed provided. Chosen anyway over leaving the backwards symlink in place - discoverability at the location a person or agent would actually go looking outweighs the small loss of test self-containment for this one file.

## D46: `<COMPACTION_NOTICE>` turn injected right after a compaction, so the continuing agent knows a discontinuity just happened

Implemented in `pkg/agent/compaction.go` and `examples/COMPACT.md`. Verified live end-to-end: forced a real compaction via the CLI, confirmed the notice turn landed as its own line in `session.jsonl` right after the surviving boundary turn, then added one more real user turn and captured the *next* `generate` call's actual wire request - `<PERSISTENT_MEMORY>` (now containing the addendum), `<COMPACTION_NOTICE>`, and the surviving turn's own text all appear merged into a single native user-role message, exactly as `MergeConsecutiveUserTurns` was expected to fold them, with the rest of history continuing normally after it.

**Motivating case**: the "ledger" role in a space-rp deployment has QMD (deep local search) available for recovering context it no longer sees directly. Worry: after compaction silently drops older turns, the model has no signal that anything is missing - it might treat a sudden topic gap as normal rather than reaching for a search/memory tool to recover detail.

**Compared to what other harnesses do**: Claude Code and similar typically inject an explicit "conversation summarized, continuing from here" marker at the resumption point, because their normal mode is raw, uncompacted history - compaction is a rare deviation worth flagging. wackypub's `<PERSISTENT_MEMORY>` block is architecturally different: it's re-injected as turn 1 on *every* request, compacted or not, so "here's your memory, now react to what follows" is already the model's normal operating mode regardless of compaction history - there's no abnormal state to signal in the same sense those harnesses have. What's still missing is something narrower: a signal *at the specific point* a discontinuity just occurred, so the model knows to treat memory as authoritative for that gap and consider searching/recovering detail rather than assuming nothing was cut.

**Design**: a new optional `compaction-notice` frontmatter field on `COMPACT.md` (mirrors `append-only`/`compact-pct` exactly - `*string` in `CompactFrontmatter` so an absent key keeps the built-in default, matching the existing pointer-based unset-vs-zero-value pattern; an explicit `compaction-notice: ""` opts out entirely, no separate boolean needed). Default value shipped in `examples/COMPACT.md`'s frontmatter, generic rather than QMD-specific (wackypub itself has no idea what tools/memory strategy a given agent has), wrapped the same way `<PERSISTENT_MEMORY>` wraps `MEMORY.md`: a new `FormatCompactionNotice` helper producing a `<COMPACTION_NOTICE>...</COMPACTION_NOTICE>` block.

**Injection point**: prepended as a new, separate synthetic `genai.Content` (role `user`) at the front of `remainingTurns`, right before `WriteSessionTurns` persists them - not spliced into the surviving boundary turn's own text, so the turn the user/agent actually produced stays byte-for-byte intact in history. Landing as its own turn works cleanly with the machinery that already exists for exactly this shape: `MergeConsecutiveUserTurns` (already called both by `FileSessionService.Get` at every real read and by `CheckAndCompactSession`'s own D45 seeding step) folds it into the immediately-following real user turn automatically, the same way the memory turn and turn 1 already merge - no new special case. Skipped when `remainingTurns` is empty (compaction consumed the whole session - nothing to attach a notice in front of yet) or when `compaction-notice` resolves to `""`.

**Why prepending a persisted turn doesn't cost anything against D45's prefix-caching goal**: the shared request prefix already breaks at the memory turn on every compaction event regardless, since `MEMORY.md`'s content itself just changed - one cache-miss on the next call is unavoidable either way. Adding one more turn to what becomes the new stable prefix afterward doesn't add an *ongoing* cost; it's identical in kind to the memory-turn content change, one-time at the moment of compaction, then cacheable again for every subsequent call until the next compaction.

**Why**: closes a real, if narrow, gap the live "ledger" use case flagged - a compacting agent has no way today to tell "fresh session with preloaded memory" apart from "just lost visibility into some of what it was working with a moment ago," and the latter is exactly when reaching for a search/memory tool matters most.

## D47: `wackypub agent <id> add-media` - stdin-only image attachments, resized and normalized to JPEG

Implemented in `pkg/agent/image.go`, `pkg/agent/sdk.go`, `pkg/agent/runtime.go`, `pkg/agent/session_store.go`, and `cmd/agent.go`. Verified live: piped a real 1000x500 RGBA PNG through `wackypub agent bob add-media`, confirmed the stored `session.jsonl` turn decodes to a 400x200 JPEG (downscaled correctly, aspect ratio preserved, alpha flattened without error) when `maxImageDimension: 400`, and confirmed `EstimateTokens` now returns a non-zero count for an image-only turn.

**Adapter already supports this end-to-end - confirmed by reading the source, not assumed.** `genai.Part.InlineData` (`*genai.Blob{Data []byte, MIMEType string}`) already round-trips through `session.jsonl` for free (`[]byte` marshals as base64 automatically, no format changes needed), and `achetronic/adk-utils-go`'s OpenAI dialect already converts it into a proper `image_url` data-URI content part (`convertInlineDataToPart` in its `genai/openai/openai.go`) - the Anthropic dialect has the equivalent. This is entirely wackypub-side plumbing, not an adapter gap.

**Turns merge "for free" - no new merge logic.** `MergeConsecutiveUserTurns` already concatenates `Parts` arrays across consecutive user-role `Content`s regardless of what's in them. A text `add` followed by an `add-media` (or several) before the next `generate` collapses into one user turn the same way multiple text `add` calls already do today - confirmed this needs no changes to that function.

**No `--image <path>` flag, no path parameter anywhere - stdin only, mirroring `agent add`'s own text path.** Caught mid-design: `agent add` never takes a file-path argument for its message either - text comes from the CLI arg, `--message`, or a piped stdin, deliberately never "here's a path, go read it yourself". A raw `os.ReadFile(path)` inside a new command would be a completely unrestricted file-read primitive - harmless for a human running `wackypub` directly from a shell, but this project explicitly recommends linking `wackypub` itself into an agent's own `tools/` (README) so agents can call it - any agent with that link would gain the ability to read *any* file the OS user can see, sidestepping `files-rw`'s per-directory allowlist gating entirely, which is the opposite of what that whole mechanism exists for. `add-media` reads its image bytes from stdin only (`wackypub agent <id> add-media < photo.jpg`) - identical access story to `agent add`'s stdin path today, nothing new to gate. A human pipes a real file in directly (ordinary shell trust); an agent wanting to attach something it generated (a chart from a `run_command` tool, say) pipes that tool's own stdout straight in.

**Command named `add-media`, not `add-image`** - scoped to images only for now (decode via stdlib `image`/`image/png`/`image/jpeg`/`image/gif`, WEBP explicitly out of scope, no stdlib encoder and not worth a dependency for uncertain benefit yet), but named generically since audio is a plausible next media type and `genai.Blob`/`InlineData` is already MIME-type-generic - keeps the CLI surface stable if/when that gets added, rather than a second differently-named command later. No `--mime` flag: since every input gets decoded (to resize/re-encode anyway), the format is detected from content, not asserted by the caller.

**No fused "add-media-and-generate" atomic command** - `agent prompt` (atomic text-add + generate) stays as-is; a media attachment composes with a separate `add`/`generate` call the same way multiple `add`s already do, so a second atomic variant isn't needed.

**Gated by a new `runtime.json` field, `maxImageDimension` (int, pixels, applies to the longer side).** Absent or `<= 0` means image attachments are rejected outright with a clear error - matches the ask ("if it's not in the runtime, no image support"). Resize is downscale-only (never upsamples a smaller image up to the cap) and only touches an image that actually exceeds the cap.

**Always re-encoded to JPEG at a fixed default quality, regardless of input format** - not "keep the original format" as first floated. Deliberate: a uniform output encoding is what makes a single token-estimate heuristic valid across every attached image (see below); mixing PNG's much looser bytes-per-pixel ratio in would break it. PNGs with transparency get their alpha flattened onto a white background before encoding, since JPEG has no alpha channel. Resize scaling uses `golang.org/x/image/draw`'s CatmullRom scaler (new dependency - Go's stdlib `image/draw` has no interpolation, only raw copies) - chosen over the faster `ApproxBiLinear` since these are occasional, user-facing attachments, not a high-volume thumbnail pipeline, so quality over speed.

**`EstimateTokens` gains a per-image contribution - it counted zero for images before this.** Heuristic (worked out from re-encoding always landing on JPEG at one fixed quality): `len(base64String) / 150` tokens per image, computed from the final resized/re-encoded JPEG bytes' base64-expanded length (`(len(rawBytes)+2)/3*4`, computed directly rather than actually base64-encoding just to measure it). Without this, `contextWindow`-based auto-compaction would silently undercount real usage the moment any session has image turns in it - a real gap the image feature would otherwise introduce, not merely a nice-to-have.

**Why**: gives agents (the "ledger" role's eventual multimodal use cases, and others) a way to attach visual content without inventing a second file-access mechanism alongside `files-rw`, without breaking the token-budget accounting compaction already depends on, and without adding CLI surface that doesn't compose with what's already there.

## D48: Binary scratchpad entries (`.dat`) - detection, clean stdin piping into `run_command`, `delete_scratchpad`

Implemented in `pkg/agent/scratchpad.go`, `pkg/agent/agent_folder.go`, `pkg/agent/sdk.go`, and `cmd/agent.go`. Verified live: piped a 10KB random binary payload through a scratchpad entry into a real subprocess's stdin via the clean-pipe path and confirmed the round-tripped output is byte-for-byte identical (`bytes.Equal`); confirmed `skip_lines`/`num_lines`/`json_escape` on a binary stdin reference now errors instead of silently piping the whole file; confirmed a forced `.txt`/`.dat` ID collision gets fully cleaned up by a single `delete_scratchpad` call rather than leaving one file orphaned.

**Two real gaps found in review, both fixed before landing.** First pass shipped `CreateScratchpad`/`CreateBinaryScratchpad` checking ID uniqueness only within their own extension's glob pattern (`id-*.txt` vs `id-*.dat` independently) - confirmed live with a forced collision that the same 4-char ID could end up on both a text and a binary entry at once, and that `delete_scratchpad` on that ID only removed one of the two, permanently orphaning the other (unreachable by ID afterward). Not astronomically rare either - with up to 300 live entries split across two independently-generated 1.6M-value ID spaces, back-of-envelope odds were around ~1% for a fairly full scratchpad directory over an agent's lifetime. Fixed by having both creation paths glob-check `id-*` (both extensions) before the extension-specific `O_CREATE|O_EXCL` open, and having `DeleteScratchpad` glob-remove every file matching `id-*` rather than resolving to a single path - the latter also self-heals any collision that already happened before this fix shipped. (Residual note, not blocking: the pre-check and the actual `O_EXCL` create aren't atomic together, so a true simultaneous-concurrent-call collision on the identical candidate ID is still theoretically possible - astronomically less likely than the original gap, which covered any eventual collision, not just a simultaneous one.)

Second, `skip_lines`/`num_lines`/`json_escape` attributes on an otherwise-clean binary stdin macro reference were being silently ignored rather than rejected - the full file piped through regardless, matching this decision's original text but not what the design actually called for. Fixed with an explicit attribute check right after the exact-match check, before the file ever gets opened.

**Why**: both gaps were exactly the kind of thing a first implementation pass misses under a deadline - the ID-namespace one especially, since neither code path had any reason to know the other existed - and both are now covered by permanent regression tests (`TestScratchpad_UnifiedIDNamespace`, `TestExecuteTool_BinaryStdinAttributesRejection` in `pkg/agent/media_test.go`) rather than just fixed and left unverified.

**Closes the real usability gap D47 left open.** Stdin-only `add-media` (D47) sidesteps `files-rw` entanglement for a human operator, but an agent has no way to actually use it: `run_command`'s `args`/`stdin` are literal, not shell-parsed, so there's no way to compose `files-rw read img.png | wackypub agent bob add-media` as a real pipeline in one call. This decision makes that composition work anyway, through machinery that already exists for exactly this "chain one tool's output into another's input" problem: `files-rw read <path>` → its stdout gets auto-captured into a scratchpad entry (already existing, D29) → `<SCRATCHPAD_DATA id="X" />` macro-expands into `add-media`'s `stdin` field (already existing, D18/D28/D30/D37) - the only real gap is that scratchpad entries are `.txt`-only today, with binary bytes either getting mis-typed as line-oriented text or (below the auto-capture size threshold) inlined directly, which is actively lossy. `files-rw` itself needs zero changes.

**Bonus, not the main motivation: fixes a live latent corruption bug.** Small stdout/stderr (under `ScratchpadOutputThreshold`, 4000 bytes) gets inlined today as literal `<STDOUT>...</STDOUT>` text via a raw `string(bytes)` conversion. That conversion is byte-safe at the Go level, but if the result contains invalid UTF-8 and later gets `json.Marshal`'d into the outgoing request, Go silently replaces the bad bytes with U+FFFD - meaning any tool that emits small binary output today is already having it corrupted before it reaches the model, independent of anything to do with images. Detecting binary and always routing it to a `.dat` scratchpad entry (regardless of size, overriding the existing threshold - confirmed live the library only needs the first 262 bytes to classify) closes this as a side effect.

**Detection: `h2non/filetype`, two-stage.** Chosen over stdlib `http.DetectContentType` for broader/less-dated category coverage (image/video/audio/archive), particularly for audio formats a future media type might need. The library's own docs admit it can misidentify text documents, so text/binary itself is decided by a separate, authoritative heuristic (maintainer-recommended, ported verbatim) over the first 24 bytes - a control byte (value ≤ 8, e.g. NUL) anywhere in that prefix means binary; `filetype`'s own `Match` result is only consulted afterward, purely to attach a friendly category (`kind.IsImage()` etc.) to content already established as binary, not to make the text/binary call itself. (Side note, doesn't change what ships: the pasted heuristic's `charCode == 65533` branch is dead in practice - `string(buf[i])` on a lone byte always round-trips to a valid rune, so `DecodeRuneInString` never actually produces `RuneError` there. The `<= 8` branch is doing all the real work and is a legitimate heuristic on its own.)

**Clean binary pipe into `run_command`'s `stdin` - a stricter path than the general `<SCRATCHPAD_DATA>` macro expansion, not the same one.** When `stdin` is *exactly* one `<SCRATCHPAD_DATA id="X" />` reference (trimmed, nothing else) to a `.dat` entry, skip string-based expansion entirely and set `cmd.Stdin` to a direct file handle (`os.Open` on the scratchpad file) - no round-trip through a Go `string` at all, which is both the only way to *guarantee* zero corruption and (confirmed, not just theorized) more efficient than reading the whole blob into memory first. A `.dat` reference mixed with other text, or alongside another macro, is a clear error - you can't meaningfully concatenate literal text around binary bytes and get anything coherent back out. `args` entries reject `.dat` references outright, unconditionally: argv strings are C strings under the hood, so embedded NUL bytes would just silently truncate the argument.

**`delete_scratchpad` tool added** (plus `wackypub agent <id> scratchpad delete` for CLI parity), with its description specifically nudging toward binary entries - text entries are cheap enough to just let age out via the existing eviction-by-cap (300) mechanism, but media can be large enough that an agent sure it's done with one should proactively free it rather than wait.

**`get_scratchpad`/`search_scratchpad` reject `.dat` entries outright, unconditionally - no model-capability detection attempted.** Confirmed live, not assumed: `achetronic/adk-utils-go`'s OpenAI dialect builds tool-role messages as `openai.ToolMessage(string(responseJSON), id)` - a plain string, no multipart slot for inline image content, regardless of whether the underlying model itself is multimodal. Chat Completions' tool-response wire format simply has no field for this, so there's no model-capability check worth attempting - it's a protocol-level absence, not a per-model one. `list_scratchpads`/`ScratchpadItem`/`wackypub agent <id> scratchpad` (list/read subcommands) surface the type/MIME distinctly, so an agent can tell entries apart before attempting a read that's going to fail.

**Deferred to a TODO, not solved here**: transparently redirecting a binary/image `get_scratchpad` read to "available on your next turn" instead of a flat rejection - queuing the content as a pending turn and forcing the *current* tool loop to terminate early so the next `generate` call picks it up fresh (where D47 confirmed multipart image content in a normal user turn does work). Real and valuable, but a meaningfully bigger piece: it needs the tool loop force-stopped the moment the read happens, not just left to stop on its own, which means reusing/extending the exact `BeforeModelCallback` short-circuit `BuildADKAgentWithConfig` already uses for the max-tool-turns case (same shape - return a canned response instead of calling the model - different trigger). Worth its own dedicated design pass once the binary-scratchpad plumbing here is solid and tested, not bolted onto an already-large format change.

**Why**: unblocks D47's `add-media` for actual agent use (not just a human piping a file in by hand), fixes a real corruption bug along the way, and reuses every piece of existing machinery (`files-rw`'s access gating, the `<SCRATCHPAD_DATA>` macro system, auto-capture) rather than inventing a second file-access mechanism to sit alongside them.

## D49: Deferred-to-next-turn image reads - `get_scratchpad` on an image entry queues it instead of rejecting

Implemented in `pkg/agent/agent_folder.go`, `pkg/agent/adk_agent.go`, and `pkg/agent/scratchpad.go`. Verified live end-to-end, including independently re-running the fix repro below rather than trusting the report. Resolves the TODO D48 deliberately deferred ("Binary/image `get_scratchpad` reads should redirect to the next turn instead of just rejecting").

**Real gap found in review, fixed before landing.** First pass deferred (and injected) an image regardless of `runtime.json`'s `maxImageDimension` - the injection code silently fell back to a hardcoded `1024`-pixel default whenever the field was unset instead of honoring the same gate `AddMedia` (D47) enforces, meaning image support ended up de facto always-on through this one path even for an agent with no `maxImageDimension` configured at all, contradicting D47's explicit requirement ("if it's not in the runtime, no image support"). Confirmed live with an agent whose `runtime.json` never mentions `maxImageDimension`: `get_scratchpad` on an image entry still deferred, and the image still landed in `session.jsonl` on the next turn. Fixed at both points that needed it, not just one - `get_scratchpad` itself now only defers when `runtimeCfg.MaxImageDimension > 0` (falling back to the ordinary binary-rejection error otherwise), and `GenerateTurn`'s injection loop is skipped entirely under the same condition, with no fallback dimension left anywhere. Re-verified with the identical repro after the fix: no image injected, gate correctly honored.

**Behavior**: `get_scratchpad` on a `.dat` entry whose detected category is an image no longer flatly rejects - it acknowledges ("this scratchpad contains media that will be available in your next turn," reusing the same scratchpad ID rather than minting a new one) and queues the entry. Once the current tool loop ends (see below), one new user-role turn gets appended per queued image directly to `session.jsonl`: a text part (`<IMAGE>The following image is stored in scratchpad 'as8f'</IMAGE>`) followed by the actual `InlineData` part read from the `.dat` file, repeated per image if more than one was queued. This lands the session on a fresh user turn - exactly what `GenerateTurn`'s own precondition already requires - so the *next* external `generate`/`prompt` call picks the image up automatically, the same way D47 confirmed multipart image content works in a normal user turn. Deliberately no auto-recursion within the same invocation to reach that next turn - the caller (human, MARP orchestrator, whatever's driving the agent loop) triggers it the same way it triggers every other turn.

**Suppressing the model's normal concluding text after a deferral, via the same lever the max-tool-turns case already uses.** `BuildADKAgentWithConfig`'s `BeforeModelCallback` (`pkg/agent/adk_agent.go`) already proves the mechanism: returning a non-nil `*model.LLMResponse` from the callback makes the runner treat that as the model call's result *without ever invoking the LLM* - currently used to stop a runaway tool loop at `maxToolTurns`. Same shape here, different trigger: the callback inspects the tail of `req.Contents` for a `FunctionResponse` carrying the deferred marker and, if found, short-circuits with a canned message ("image queued, send another message to continue") instead of letting the model comment on content it can't see yet.

**The marker is a real struct field, not text - checked before any wire serialization happens, which is what actually rules out collision, not just makes it rare.** `GetScratchpadResult` gains `Deferred bool` / `ScratchpadID string` alongside the existing `Output string`. `FunctionResponse.Response` stays a typed `map[string]any` all the way through `BeforeModelCallback` - it only becomes text at the very last step before the HTTP request goes out (confirmed against source: `adk-utils-go`'s OpenAI dialect calls `common.MarshalToolPayload(part.FunctionResponse.Response)` right before building the wire body, well after our callback runs). So the check is never "does this text contain a phrase somewhere" - it's "does the `FunctionResponse` named exactly `get_scratchpad` have a `deferred` key that type-asserts to Go `true`." Three things have to line up exactly, on data only wackypub's own tool-execution code ever produces - never model-generated text, never a subprocess's stdout, never file content - so there's no path for an unrelated tool's legitimate output, or an adversarial one, to trip it.

**No new shared mutable state needed between `BuildFolderAgentTools` (where `get_scratchpad` lives) and `BuildADKAgentWithConfig` (where the callback lives), despite both needing to react to the same event.** The tool's return value already flows into the request/session content stream ADK builds - that stream *is* the shared channel. `BeforeModelCallback` reads it from `req.Contents`; `GenerateTurn`'s own event loop (`for event, err := range r.Run(...)`, which already iterates every event live, no extra re-read of `session.jsonl` needed) reads the identical field off the same events to know which scratchpad IDs to inject after the loop ends. Two independent readers of one already-existing signal, not two things that need to be wired together.

**Confirmed against the ADK source, not assumed, that this naturally handles "more than one image deferred in a turn" - within one real boundary.** `handleFunctionCalls` → `mergeParallelFunctionResponseEvents` (`internal/llminternal/base_flow.go`) merges *parallel* tool calls from a single model response into one event with multiple `FunctionResponse` parts - so if the model defers two images in one batched response, both markers land together and get picked up in the same pass. What doesn't work: deferring images *sequentially* across separate exchanges within one turn - the short-circuit fires on the very next model call after the *first* deferral, which is exactly the call the model would have needed to make to sequentially request a second image. Accepted deliberately as the simpler, stateless design over the alternative (closure-shared state letting the loop run longer to accumulate sequential defers) - confirmed acceptable rather than assumed.

**Why**: gives a tool-loop-discovered image (e.g. a chart a `run_command` tool just generated) a real path to actually being seen by the agent, not just a permanent dead end (D48's flat rejection) - closes the exact gap that rejection left open, using only mechanisms that already exist (the callback short-circuit pattern, the event stream `GenerateTurn` already iterates, the request/response content pipeline) rather than inventing new state-passing machinery.

## D50: `wackypub agent <id> repl` replaces `scripts/repl.sh`; blocked from agent context

Implemented in `cmd/agent.go`, `scripts/init_container_env.sh`, `scripts/run_container.sh`. Verified live: multi-turn conversation against a mock backend correctly persists alternating user/model turns to `session.jsonl`, `exit`/`quit`/Ctrl+D all correctly end the session, and invoking it from inside an agent's own directory correctly refuses (both `wackypub agent bob repl` and `wackypub agent repl bob` dispatcher forms).

**A real Go subcommand replaces the standalone `scripts/repl.sh`** the Docker demo environment (`run_container.sh`/`init_container_env.sh`) was shelling out to - a thin `while read -p` loop wrapping repeated `wackypub agent prompt` calls. Same interactive shape (prompt, read a line, print the response, repeat), but now built into the binary itself rather than a separate script that has to be copied into the container workspace and kept in sync on every run (`run_container.sh` previously re-copied it on every single invocation specifically to guard against drift - that whole step is gone now, there's nothing to drift). `docker exec -it $CONTAINER_NAME wackypub agent main repl` replaces `docker exec -it $CONTAINER_NAME /ws/repl.sh main` directly - `/bin/wackypub` is already on the image's `PATH` (`Dockerfile`'s `COPY wackypub /bin`), and `docker exec` inherits the image's `WORKDIR /ws` by default, so no `--ws` flag is needed either.

**Blocked from agent context via the same `refuseIfAgentContext` helper D41 already established for `trace`/`workspace snapshot`/`tag`/`push`** - reused directly, no new detection logic. This is an interactive tool built for a human at a real terminal to drive an agent by hand; it was never meant to be something an agent invokes on itself via `run_command`. In practice a `run_command`-spawned subprocess wouldn't actually hang forever on this (Go's `exec.Cmd` defaults an unset `Stdin` to `/dev/null`, which reads as immediate EOF, not a blocked read on an inherited real terminal) - but it's still a nonsensical call for an agent to make, the same category of thing the other four blocked commands already are, so it gets the same treatment on principle, not just to prevent a literal hang.

**Why**: one less thing to keep in sync (a shell script copied into a generated directory, versus a subcommand that ships with the binary itself), and closes a real gap the shell version had no way to address at all - nothing stopped an agent from invoking `/ws/repl.sh main` via `run_command` before this, since it was just another executable in the workspace with no awareness of who was calling it.

## D51: `wackyproc` - a zero-daemon background process manager, companion tool like `files-rw`

Implemented in `tools/wackyproc`. Standalone repo (`github.com/colinrgodsey/wackyproc`), vendored the same way as `files-rw` (D50-era extraction): a git submodule at `tools/wackyproc`, source and CI live there, shared process (security testing methodology, decision history) stays in wackypub's own `.agents/`.

**The problem**: wackypub's agent runtime is entirely non-persistent - a `wackypub agent prompt`/`generate` process only lives as long as one turn, then exits. `run_command` runs a tool synchronously and returns its output; there's no way today for an agent to kick off something long-running (a dev server, a slow build, a test suite) and check back on it across turns without either blocking the whole turn on it or losing it the moment the process exits.

**Core architecture: self-supervising, no separate daemon or script.** The `wackyproc` binary forks *itself* with a hidden `__supervise <id>` subcommand (resolved via `os.Executable()`), given `Setsid: true` so it detaches from `run_command`'s process tree and survives the agent turn ending - no master daemon, no long-running background service, nothing running except when a process is actually being supervised. The actual target command runs as a child of the supervisor with `Setpgid: true`, isolating it into its own process group distinct from the supervisor's session - `stop` (and, later, a timeout) signals that group directly without touching the supervisor itself.

**State lives entirely on disk**, one directory per process: `.proc/<id>/` (4-character `a-z0-9` ID, same generation scheme as wackypub's own scratchpad IDs - not shared code, just the same small approach replicated, no reason to add a cross-repo dependency for a ~10-line ID generator) containing `meta.json`, `pid`, `pgid`, `status`, `exit_code`, and (see below) `stdin`/`stdout`/`stderr`.

**CLI surface, deliberately minimal - four commands, not the larger surface earlier drafts of this design sketched:**
- `run <tool> [args...]` - analog to `run_command`: spawns `<tool>` as a detached background process, returns its `proc_id` immediately. Resolves `<tool>` strictly against `<cwd>/tools/<tool>` - **no `$PATH` fallback**, matching `run_command`'s own existing, deliberate resolution behavior exactly (no new capability surface, no separate security story to reason about).
- `list` - tracked processes and their current states.
- `wait <seconds>` - blocks up to N seconds for **any** tracked process to reach a terminal state, returns the `proc_id` of whichever one completes first (not a specific target - an agent that kicked off several things doesn't have to guess which to poll).
- `get <proc_id>` - dumps the captured stdout/stderr back out through `wackyproc`'s own stdout/stderr.

**The key design decision: zero integration with wackypub's scratchpad file format, on either side.** An earlier draft of this design had the supervisor stream output directly into a wackypub-scratchpad-shaped file, to reuse `get_scratchpad`/`search_scratchpad` for reading it back. Reconsidered and rejected, for concrete reasons, not just unease:
1. Wackypub's scratchpad filename format has already changed three times within this same development window (D30: single blob → one-file-per-entry; D39: added a line count to the filename; D48: added the `.dat` extension for binary entries). A second repo hand-rolling that exact format is one internal-format change away from silently breaking.
2. It violates the write-once immutability D30 built the entire no-locking-needed design around - a live-growing file from an external process is a fundamentally different thing than what `EvictOldestScratchpad`/`O_CREATE|O_EXCL` were designed against.
3. A scratchpad entry's line count is baked into its filename at creation time and never updates - a continuously-appended file would report permanently stale metadata to `list_scratchpads` from the moment it's created.

Instead: `get <proc_id>` just prints the captured output through `wackyproc`'s own stdout/stderr, exactly like any other tool. Since `wackyproc` itself is only ever invoked *through* `run_command`, that output automatically rides `run_command`'s existing, fully generic auto-capture (D29/D48) - large or binary output becomes a real scratchpad entry the normal way, with zero code in `wackyproc` that knows wackypub's scratchpad format exists. This is the same relationship every other tool (`files-rw` included) already has with wackypub - `wackyproc` isn't special-cased.

**The identical principle resolves stdin, which looked harder at first.** `run`'s own stdin - however `run_command` delivered it to `wackyproc` (literal text, or a D48 clean binary pipe from a scratchpad reference) - gets drained synchronously into `.proc/<id>/stdin` *before* the supervisor detaches, never held open across the detach boundary (a file descriptor inherited from the shortly-to-exit `run` invocation isn't something to rely on staying valid inside a fully independent, longer-lived supervisor - drain to a real file first, then have the supervisor read from that stable file). If `run`'s stdin wasn't piped at all, no `stdin` file gets created, and the spawned child gets none either (matching `run_command`'s own default for unset `Stdin`). `wackyproc` never reads wackypub's scratchpad directory directly on the input side either - only its own inherited process stdin, a completely generic Unix primitive. "wackypub can only ever provide stdin from generated content or a scratchpad" - already well-bounded data by the time it reaches `wackyproc`, nothing new to bound here.

**Liveness and lifecycle**: `exit_code` file present → terminal (`COMPLETED`/`FAILED`, from the recorded code); absent → PID zero-signal check (`os.FindProcess` + `Signal(0)`, the Go equivalent of `kill -0`) → `RUNNING`, or `CRASHED` (exit code recorded as 137) if the process is simply gone without ever writing one (OOM-killed, host rebooted). Guarded against PID reuse: the process's start time gets recorded in `meta.json` at launch (`ps -p <pid> -o lstart=` equivalent) and re-verified on every liveness check, since a live PID matching by number alone isn't sufficient once a process has actually died and the number's been recycled.

**Deliberately not decided here, parked as a TODO instead**: whether `run_command` itself should gain a timeout. Real tension, not resolved by this design - a hard safety-net timeout would catch an agent accidentally running something that blocks forever on an interactive prompt (`apt install` without `-y`, an unflagged `git rebase`), but the right duration is a guess, and a timeout that fires on a legitimate-but-slow command is its own kind of friction. Doesn't block `wackyproc` - a `wackyproc`-specific skill nudging agents toward `run` for anything expected to be slow can exist regardless of what core `run_command` does or doesn't do.

**Why**: gives wackypub's non-persistent turn model a real way to survive across turns for genuinely long-running work, without inventing a master daemon (which the whole platform's "all on disk, only alive for the duration of a turn" design deliberately avoids everywhere else) and without coupling a second repo to wackypub's internal file formats on either the output or input side - `wackyproc` only ever depends on `run_command`'s public behavior (resolve from `tools/`, capture large/binary stdout automatically), the same contract every other companion tool already relies on.

**Two real bugs found in review, both fixed before landing, both independently re-verified afterward (not just trusted from the report).** First pass allocated a process ID via `os.Stat` (check) then, later, `os.MkdirAll` (create) as two separate non-atomic steps - `MkdirAll` doesn't error on an already-existing directory, so two concurrent `wackyproc run` calls landing on the same random ID would silently collide and write into the same `.proc/<id>/`. Confirmed live with a targeted test forcing the exact collision before the fix landed. Fixed with `ClaimUniqueProcessDir` (`proc/id.go`): `os.Mkdir` - not `MkdirAll` - in a retry loop, atomic via `os.ErrExist` on collision, mirroring wackypub's own `O_CREATE|O_EXCL` scratchpad ID generation exactly. Second, a narrower race: a target child that had just died but whose supervisor hadn't yet finished writing `exit_code` could get misreported as `CRASHED` by a concurrent `list`/`get`/`wait` call. Fixed by recording the supervisor's own PID (`supervisor_pid`, written before the child is ever spawned) and checking it's still alive before concluding a missing `exit_code` means a crash rather than an in-flight finalization. Both covered by regression tests (`TestClaimUniqueProcessDir_AtomicCollision`, `TestRun_Concurrent` - 20 real goroutines racing `proc.Run`), full suite passing under `-race`.

## D52: `run_command` gets process-group isolation and a configurable, disable-able timeout

Implemented. Resolves the TODO parked during D51's design ("Should `run_command` gain a timeout?").

**Process-group isolation, independently justified - not just a timeout prerequisite.** `executeTool` (`pkg/agent/agent_folder.go`) spawns every tool today with no `SysProcAttr`/`Setpgid` at all, so a spawned command's own children land in the same process group as `wackypub` itself. Confirmed by live observation, not just code inspection: this is the source of zombie processes noticed over time in real use - nothing has ever reaped a tool's orphaned grandchildren, because nothing isolates or tracks them as a group in the first place. Fixed regardless of the timeout question: `Setpgid: true` on every spawned command, mirroring the exact isolation `wackyproc`'s own supervisor (D51) already uses for the same reason.

**Timeout mechanism, following the existing `--api-key`/`GEMINI_API_KEY` precedent exactly rather than inventing a new configuration convention.** New persistent flag `--command-timeout-seconds` (default 900 - 15 minutes), `-1` disables it entirely. Precedence resolved the same way `GetWorkspaceDir()`/`GetMaxToolTurns()` already do it (`cmd/root.go`): `RootCmd.PersistentFlags().Changed("command-timeout-seconds")` detects an explicit flag and wins outright; otherwise falls back to an env var (`WACKYPUB_COMMAND_TIMEOUT_SECONDS`), then the flag's own built-in default.

**The env var needs zero new plumbing - it rides `LoadAgentDotEnv`, which already exists for exactly this purpose.** Wackypub already loads a workspace-root `.env` and applies it via `os.Setenv` before any agent's turn runs (called from `LoadFolderAgent`). Setting `WACKYPUB_COMMAND_TIMEOUT_SECONDS` in that file is already "workspace-level config via env var" the moment `os.Getenv` gets checked downstream - no new file format, no new loader, no new precedent to establish.

**Threaded through exactly like `maxToolTurns` already is** - `cmd.newSDK()` resolves the value once via the precedence chain above, sets it on `AgentSDK`, flows down through `LoadFolderAgent` → `FolderAgent` → `executeTool`'s call site, the same path `maxToolTurns` already takes end to end.

**On expiry, kill the whole process group, not just the root PID** - `exec.CommandContext`'s default cancellation only signals the root process; with `Setpgid: true` now isolating each spawned command into its own group, the timeout path sends the kill to `-pgid` instead, so a hung command's own children actually die with it rather than getting orphaned into exactly the zombie-process pattern this decision's first half fixes.

**Error message stays generic** - no hardcoded mention of `wackyproc` or any other specific companion tool by name, confirmed as a requirement when this was originally parked.

**Why**: closes a real, live-observed gap (zombie processes from unisolated child processes) that exists independent of the timeout question, and gives operators a way to recover from an agent accidentally running something that blocks forever (an unflagged interactive prompt, etc.) without forcing a specific duration on anyone - a 15-minute default is generous enough to almost never fire on legitimate work, configurable per-workspace with zero new config surface, and fully disable-able for anyone who'd rather not have it at all.

**D51 addendum: `wackyproc wait` gets a 500-second max, silently clamped.** Caught in review after the fact - `wait <seconds>` had no upper bound, so a caller requesting an unreasonably long single wait would just block for that long, defeating the point of a tool meant to avoid tying up a turn on long-running work. `run_command`'s own D52 timeout would have incidentally rescued a wackypub-driven caller from this, but `wackyproc` is meant to work outside wackypub too, so it needed its own bound rather than relying on the caller's. Clamped, not rejected with an error - preserves `wait`'s existing "nothing finished in time" contract (empty string) rather than adding a new failure mode for a too-large request. Implemented as a small pure `clampWaitSeconds` helper specifically so the boundary logic could be unit-tested directly, without a test actually needing to block for 500 real seconds to prove the cap works. Also added, concurrently: a `wackyproc skill` command (bundled `SKILL.md`, same `//go:embed` pattern as wackypub's own `wackypub skill` a2a/ws skills) giving agents the "use `wackyproc run` instead of a synchronous `run_command` for anything long-running" guidance discussed during D52 - confirmed while reviewing it that `wackyproc` has no `prune`/cleanup command despite the original design doc proposing one; `.proc/` entries accumulate on disk indefinitely with no eviction, unlike wackypub's own scratchpad system. Not fixed here, just now honestly documented in the skill itself rather than silently absent.

## D53: `run_command` no longer echoes raw call args into a tool's stdin / `WACKYPUB_TOOL_ARGS`

Implemented in `pkg/agent/agent_folder.go`. Verified live with a real model: rebuilt, re-ran the exact `wackyproc`/`build-repo.sh` scenario that surfaced this (see below), confirmed `.proc/<id>/stdin` no longer gets created at all when the agent didn't provide explicit stdin.

**Found live, testing `wackyproc` with a real model (`testws/clerk`), not by inspection.** `executeTool` had a fallback, present since the very first tool-use-loop commit (`79f9deb`, bundled with D14/D17/D18): whenever a tool was called with `Args`/`Env` but no explicit `Stdin`, it fed the tool a JSON-serialized echo of its own call (`{"args": [...], "env": {...}}`) as stdin, and set the same blob as `WACKYPUB_TOOL_ARGS` in its environment. Harmless for a tool that ignores stdin, but `wackyproc run <tool>` always has non-empty `Args` (the target tool name is required) and always drains whatever's on its own stdin into `.proc/<id>/stdin` to relay to the spawned child - so this fallback meant `wackyproc` was silently forwarding wackypub's internal invocation JSON to every backgrounded process as if the calling agent had asked for it to be piped in, even when it never had. Confirmed via a real trace: `.proc/<id>/stdin` contained `{"args":["run","build-repo.sh"]}` after a call where the agent never mentioned stdin at all.

**No use case for it survived scrutiny - args are already available to the tool the normal way.** `args` are passed as real argv (`cmd := exec.CommandContext(execCtx, absToolPath, cmdArgs...)`), so nothing about this fallback gave a tool information it couldn't already get from `os.Args`. No prior decision recorded a reason for it either - D17's own "Why" section justifies everything else in that decision except this specific line, which is listed as a bare feature with no rationale, and the introducing commit's message doesn't call it out either. Concluded it was carried over from an early design instinct (predating the scratchpad macro system as the real mechanism for passing data through a tool call) that never actually had a justification, not something anyone deliberately decided was load-bearing.

**Removed both halves, not narrowed.** No explicit `Stdin` now means `cmd.Stdin` stays unset entirely - the spawned tool gets `/dev/null` (`exec.Cmd`'s own default), the same as any tool with no `Args`/`Env` already got before this fallback existed. `WACKYPUB_TOOL_ARGS` is gone from the environment too. A tool that wants to know its own invocation still has `args`/`env` the normal way; nothing lost that wasn't already redundant.

**Why**: closes a real, live-discovered surprise - a companion tool that itself relays its own stdin (like `wackyproc`) had no way to distinguish "the agent intentionally wants this piped through" from "wackypub always sends something regardless" - for a feature that never had a documented reason to exist and was already fully redundant with normal argv.

## D54: `MergeConsecutiveUserTurns` refactored into generic `CleanSessionTurns` with dangling function response removal

Implemented in `pkg/agent/session_store.go`.

**Why**: Backends across the board (Anthropic, OpenAI, OpenRouter, Google Gemini) enforce strict role and tool invocation invariants on multi-turn history. If a `FunctionResponse` (or `tool_result` block) appears without a matching `FunctionCall` (`tool_use` / `tool_call_id`) in the immediately preceding assistant message, providers reject the request with a hard 400 Bad Request error. Such dangling responses naturally accumulate in real-world use:
1. **Compaction boundary truncation**: Compacting the oldest turns can slice a session between a `model` turn's `FunctionCall` and the subsequent `user` turn's `FunctionResponse`, leaving an orphaned response at the start of surviving history.
2. **Crashed / interrupted runs**: Partial tool turns or manual session file edits can orphan tool responses.

**Pipeline Structure**: `CleanSessionTurns(contents []*genai.Content) []*genai.Content` replaces and extends `MergeConsecutiveUserTurns`:
1. **Dangling `FunctionResponse` Stripping**: Iterates through each turn. Any `FunctionResponse` part is checked against the immediately preceding kept turn. If the previous turn is not a `model` turn or does not have a matching, unconsumed `FunctionCall` (matched by `ID` if set, otherwise by `Name`), the `FunctionResponse` is stripped. Mixed turns (e.g. text + dangling tool response) have only the dangling response removed.
2. **Empty Turn Pruning**: If stripping dangling responses leaves a turn with 0 parts, the entire turn is dropped.
3. **Consecutive User Turn Merging**: Collapses runs of consecutive `user` turns into single `user` turns, preserving part order (text, blobs/images, and valid tool responses). Pruning an empty turn between two user turns allows them to merge cleanly.
4. **Backwards Compatibility**: `MergeConsecutiveUserTurns` is retained as a thin wrapper delegating to `CleanSessionTurns`.

## D55: `wackydiscord` — Workspace-driven Discord REPL & multi-agent channel bridge

Implemented in `tools/wackydiscord`.

**Architecture**: A standalone Go module in `tools/wackydiscord` (`github.com/colinrgodsey/wackydiscord`) that wraps `AgentSDK` in-process (`pkg/agent/sdk.go`) and connects a Discord bot to a WackyPub workspace directory.

**Core Mechanics**:
1. **Channel-to-Agent Bindings (`/bind`, `/unbind`, `/status`, `/agents`)**:
   - Pure Discord Application Slash Commands registered at bot startup.
   - Binds a Discord channel (or DM) to a specific `agent_id` in the workspace.
   - Binding state persisted to `<ws_dir>/.wackydiscord.json` so associations survive bot restarts.
2. **Auto-Fill & Compaction-Resilient Syncing**:
   - Automatically backfills any unseen turns before running an interactive user turn (or on explicit `/fill`).
   - Tracks `last_synced_turn_hash` (SHA-256 of canonical turn content) alongside `last_synced_turn_index` in state. If compaction truncates history, the hash scan anchors the sync boundary without duplicate message spam.
   - User turns from background activity are formatted distinctly with blockquotes (`> 👤 **[User Turn]** ...`).
3. **Live File Watcher (`fsnotify`)**:
   - Watches bound agent directories in real-time for `session.jsonl` modifications, creations, and atomic renames.
   - Debounces events (~300ms) and pushes newly appended background turns directly into bound Discord channels without waiting for a user message.
4. **Webhook Persona Posting**:
   - Messages are posted via channel webhooks with the bound agent's username and custom avatar if present, falling back gracefully to standard channel message delivery if webhook management permissions are absent.
5. **Interactive Turn Driving**:
   - Heartbeat typing indicator (`s.ChannelTyping`) kept alive in a background goroutine while `AgentSDK.AddAndGenerateTurn` executes under the session lock.
   - Long responses automatically split along newline boundaries to stay within Discord's 2000-character message limit.
   - `/verbose` mode displays real-time tool execution badges and status.

**Why**: Provides a high-fidelity, multi-agent interactive interface for human users and external platforms to monitor, drive, and converse with folder-based agents in real-time across Discord channels, with seamless persona presentation, instant live updates from background executions, and reliable state sync across compaction events.

## D56: `run_command`'s `args` gets an explicit, hand-written schema override - not global env var, not just `omitempty` removal

Implemented in `pkg/agent/agent_folder.go`.

**The problem, confirmed at the wire level, not inferred.** Testing `wackyproc` against MiniMax-M2.7 (`testws/clerk`) surfaced a real tool-calling failure: `run_command` calls with `args` consistently came back as `"args": null` in the raw API response, even though the model's own `reasoning_content` in the same response showed clear intent to pass specific args. Captured via a local proxy between wackypub and MiniMax's real endpoint (not trusting wackypub's own logs) - the model's own generated JSON, not something wackypub mis-parsed.

**Isolated to exactly one thing, by testing four schema variants directly against MiniMax's API, bypassing wackypub entirely:** required-vs-optional makes no difference; the only thing that breaks it is `args`'s type being the union `["null", "array"]` rather than a plain `"array"`. A required, non-nullable array works. An *optional*, non-nullable array (no `null` in the type, and not in `required` either) also works. Only the null-union fails. This isn't likely to be MiniMax-specific either - nullable/union types are a known rough edge for providers doing schema- or grammar-constrained tool-call decoding generally, so treating this as a real compatibility fix rather than a narrow one-off patch.

**Root cause**: `run_command`'s `args []string` field gets `type: ["null", "array"]` from ADK's own schema auto-inference (`github.com/google/jsonschema-go`, pulled in transitively via `google.golang.org/adk/v2`, not a wackypub dependency directly) - confirmed by reading `infer.go` directly: any Go field of `reflect.Slice` kind gets the null-union unconditionally, regardless of `omitempty`/required status (those are two separate, independently-computed things in this library - "required" only controls whether the key must be present, not whether its value can be `null`). Dropping `omitempty` alone would *not* fix this, confirmed by tracing the exact function boundaries: the null-decision happens inside `forType(reflect.Type, ...)`, which never receives the enclosing struct field's tags at all.

**Two fixes considered for suppressing the null-union; only one chosen.** The library has a `JSONSCHEMAGODEBUG=typeschemasnull=1` env var that does suppress it - verified working with a live, wire-captured test (a clean session, no prior-turn contamination, first attempt correct). Rejected anyway as the actual fix, for reasons found by checking scope before committing to it, not assumed: it's process-wide (any tool's slice-typed field, not just this one), the underlying library is used more broadly inside ADK than just tool schemas (`session.go`, `internal/typeutil/convert.go` - both in wackypub's live execution path; the `workflow` package too, though wackypub doesn't use that today), it's an undocumented debug flag on a transitive dependency wackypub doesn't version-pin directly (a routine ADK upgrade could silently change or drop it), and it leaks into every spawned tool subprocess's environment (`executeTool`'s `cmd.Env = os.Environ()`).

**Chosen instead: an explicit `InputSchema` override on `run_command`'s `functiontool.Config`, bypassing auto-inference entirely for just this one tool.** Confirmed via ADK's own source (`resolvedSchema[T]` in `tool/functiontool/function.go`): if `Config.InputSchema` is non-nil, `jsonschema.For[T](nil)` (the reflection-based inference that produces the null-union) never runs at all - the hand-written schema is used as-is. Zero global behavior change, no dependency on an undocumented flag, and if the `jsonschema.Schema` type itself ever changes shape in a future ADK version, that's a compile error, not a silent runtime regression.

**Real tradeoff, not free**: ADK's own code has a `// TODO: check if override schema is compatible with T` comment - it does not validate that a hand-written override actually matches `RunCommandArgs`'s real fields. Keeping the two in sync is now a manual responsibility. Mitigated by a regression test asserting the actual generated `run_command` schema has `args` typed as a plain `array` (not a null union) - not just that the tool still works, so a future edit to `RunCommandArgs` that silently drifts from the hand-written schema gets caught, and so a hypothetical future upstream fix to the auto-inference behavior doesn't quietly go unnoticed either.

**Also, independent of the null-union question**: `args` becomes a required, always-present field (`omitempty` dropped from the JSON tag, `[]string{}` used instead of `nil` for "no args") rather than an optional/omittable one. Not a substitute for the schema override - confirmed this alone doesn't fix the null-union - but a genuinely cleaner contract on its own: there's no meaningful difference between "no args given" and "an empty args list" for a command, so collapsing that distinction removes one more axis of ambiguity for *any* model to reason about, not just this one.

**Why**: fixes a real, wire-confirmed tool-calling failure for at least one real provider (MiniMax-M2.7) and probably others with similar constrained-decoding behavior, with a fix scoped precisely to the one field that needs it rather than a process-wide toggle whose full blast radius wasn't fully auditable, and backed by a regression test specific enough to catch either side silently drifting apart later.

## D57: Deterministic tool slice ordering for prompt cache stability

Implemented in `pkg/agent/agent_folder.go`.

**Root cause**: In `LoadFolderAgent`, ADK tools were extracted from `adkToolsMap` (a `map[string]tool.Tool`) via `for _, t := range adkToolsMap { toolsList = append(toolsList, t) }`. Because Go map iteration order is randomized by the Go runtime on every execution, `toolsList` was shuffled into a pseudo-random order on every turn generation. This caused Google ADK and downstream LLM wire adapters (OpenAI, Anthropic, OpenRouter, Gemini) to emit the `tools: [...]` schema array in an arbitrary order from turn to turn, completely invalidating the prompt cache prefix on every single request.

**Fix**:
1. Sort `toolsList` deterministically by tool name (`sort.Slice(toolsList, func(i, j int) bool { return toolsList[i].Name() < toolsList[j].Name() })`) before passing to `BuildADKAgentWithConfig`.
2. Ensure `BuildFolderAgentTools` and all prompt/tool assembly paths remain 100% deterministic.
3. Regression test asserting that repeated calls to `LoadFolderAgent` produce identical, stably-sorted tool lists and request tool declarations.

**Why**: Fixes a critical prompt-caching regression where multi-turn agent sessions suffered ~0% prompt cache hit rates due to shuffled tool schemas, restoring full prefix prompt caching across all supported LLM backends (OpenAI, Anthropic, DeepSeek, Minimax, OpenRouter, Gemini).

## D58: `wackyproc` adopts D14 recursive tool discovery & symlink resolution

Implemented in `tools/wackyproc/proc/manager.go`.

**Context**: In WackyPub, D14 established that tools in `<agentDir>/tools/` are discovered recursively (`DiscoverAgentToolsMap`), resolving and following directory symlinks (such as shared toolpacks or nested tool directories) with cycle detection, while strictly denying `$PATH` fallback.

**The problem**: `wackyproc` initially resolved tools using a flat `filepath.Join(cwd, "tools", toolName)` lookup. While direct file symlinks worked, directory symlinks (e.g. `./tools/toolpack -> /opt/toolpack/`) and nested tool directories (e.g. `./tools/nested/sub/subtool`) were not discoverable by their base command name, forcing callers to provide explicit relative subpaths or failing altogether.

**Fix**:
1. Added `ResolveToolPath(cwd, toolName)` to `wackyproc/proc`:
   - Checks direct relative paths under `./tools/` first (with boundary checks preventing `../` path traversal escapes outside `./tools/`).
   - Performs a recursive walk under `<cwd>/tools/` following directory symlinks (`filepath.EvalSymlinks`) with cycle detection, identical to WackyPub's D14 mechanics.
   - Preserves strict isolation: only executable files under `./tools/` are resolved; commands outside `./tools/` fail with "no PATH fallback".
2. Added unit tests in `tools/wackyproc/proc/proc_test.go` covering nested directories, directory symlinks, file symlinks, and path traversal attempts.

**Why**: Ensures `wackyproc` and `wackypub` share identical tool resolution semantics, so an agent can spawn any tool via `wackyproc run <tool>` that it can run synchronously via `run_command command="<tool>"`.

## D59: Elimination of `os.Setenv` process-global mutation for `AGENT2AGENT` / `WACKYPUB_CALL_CHAIN`

Implemented in `pkg/agent/workspace.go`, `pkg/agent/agent_folder.go`, `pkg/agent/sdk.go`.

**The problem**: `ValidateAgentTarget` historically used `os.Setenv(Agent2AgentEnvVar, ...)` and `os.Setenv(CallChainEnvVar, ...)` to mutate the process-global environment during turn execution, returning a `cleanup` callback that attempted to restore previous values via `os.Setenv(..., orig)`. This caused severe issues in long-lived or multi-threaded environments (such as `wackydiscord`, background session watchers, and concurrent SDK consumers):
1. `os.Setenv` is process-global in the Go runtime. Concurrent goroutines handling turns for different agents or watching sessions collided on the shared environment variables.
2. If `orig` was empty, `os.Setenv(key, "")` left empty environment variables rather than calling `os.Unsetenv`.
3. Read-only SDK methods (`ReadSession`, `ReadMemory`, `RenderSystemPrompt`, `GetScratchpad`, `ListScratchpads`, `SearchScratchpad`) were erroneously calling `ValidateAgentTarget`, causing background reads (such as `wackydiscord`'s file watcher checking turn counts) to mutate the process environment and fail with false-positive deadlock errors if an agent was already in the call chain.

**Core insight**: `AGENT2AGENT` / `WACKYPUB_CALL_CHAIN` only ever needs to exist in the environment of child subprocesses spawned via `run_command` (`cmd := exec.Command(...)`). The host Go process does not need to mutate its own OS environment at all.

**Fix**:
1. **Refactored `ValidateAgentTarget(targetAgentID string) (*A2AMetadata, error)`**:
   - Reads incoming `AGENT2AGENT` (or fallback `WACKYPUB_CALL_CHAIN`) from the process environment to inspect the parent caller chain.
   - Enforces authorization (`WACKYPUB_ALLOWED_AGENTS`) and deadlock cycle prevention (`targetAgentID` in `meta.CallChain`).
   - Calculates and returns the updated `*A2AMetadata` (appending `targetAgentID` to `CallChain`, computing `workspace_revision`, etc.).
   - Performs **zero `os.Setenv` / `os.Unsetenv` calls** and requires no cleanup callback.
2. **Subprocess Scoping in `FolderAgent.executeTool`**:
   - `FolderAgent` stores the computed `A2AMeta *A2AMetadata`.
   - When `executeTool` builds `cmd.Env`, it explicitly injects `AGENT2AGENT=<denseJSON>` and `WACKYPUB_CALL_CHAIN=<csv>` directly into the child process environment slice.
3. **Exempted Read-Only SDK Methods**:
   - Removed `ValidateAgentTarget` from all read-only inspection methods in `AgentSDK`, aligning strictly with D16 (which established that read-only inspection carries neither side effects nor deadlock risk).
4. **Data Deduplication**:
   - Clarified that `WACKYPUB_CALL_CHAIN` is a legacy subset of `AGENT2AGENT.call_chain`, exported only to `cmd.Env` for backward compatibility with external scripts.

**Why**: Guarantees 100% goroutine-safe, stateless in-process execution across multi-agent daemons (`wackydiscord`) and SDK consumers without mutexes, environment pollution, or false-positive deadlock errors.
## D60: Distinction between Cross-Agent Authorization (`WACKYPUB_ALLOWED_AGENTS`) and Deadlock Cycle Prevention (`CallChain`)

Implemented in `pkg/agent/workspace.go`, `pkg/agent/sdk.go`.

**Context**: D16 established two safety boundaries for cross-agent calls:
1. Authorization: `WACKYPUB_ALLOWED_AGENTS` gates which peer agents an agent is allowed to invoke.
2. Deadlock cycle prevention: `WACKYPUB_CALL_CHAIN` / `AGENT2AGENT.call_chain` rejects recursive re-entry (A -> B -> A).
D16 explicitly exempted `InspectAgent` (and `wackypub workspace`) because diagnostic introspection only returns high-level structural metadata (turn count, tool list, model name, parse status), carrying no side effects or deadlock risks.

**The problem**: D59 inadvertently exempted all read-only methods (`ReadSession`, `ReadMemory`, `RenderSystemPrompt`, `GetScratchpad`, `ListScratchpads`, `SearchScratchpad`) from `ValidateAgentTarget` in order to prevent background watchers from tripping `CallChain` cycle errors. However, doing so eliminated the `WACKYPUB_ALLOWED_AGENTS` authorization check entirely for read-only content operations. This created a severe cross-agent exfiltration vulnerability: an untrusted or prompt-injected agent could execute `run_command command="wackypub" args=["agent", "victim", "read-session"]` or `read-memory` to read any peer agent's private session history, long-term memory, or scratchpad files without authorization.

**Fix**:
1. Decoupled the checks into two distinct functions in `pkg/agent/workspace.go`:
   - `AuthorizeAgentTarget(targetAgentID string) error`: Enforces `WACKYPUB_ALLOWED_AGENTS` against the caller's CWD (denies if caller is an agent directory and `targetAgentID` is not in its allowlist).
   - `ValidateAgentTarget(targetAgentID string) (*A2AMetadata, error)`: Calls `AuthorizeAgentTarget`, enforces `CallChain` cycle prevention, and computes updated `*A2AMetadata`.
2. Gated all private content read operations in `AgentSDK` with `AuthorizeAgentTarget(agentID)`:
   - `ReadSession`, `ReadMemory`, `RenderSystemPrompt`, `GetScratchpad`, `ListScratchpads`, `SearchScratchpad` enforce `WACKYPUB_ALLOWED_AGENTS` authorization, while skipping `CallChain` checks.
   - Background file watchers and non-agent daemons (like `wackydiscord`) running from the workspace root remain authorized and will never trip cycle errors.
3. Gated all mutating and turn-generating operations in `AgentSDK` with `ValidateAgentTarget(agentID)`:
   - `GenerateTurn`, `AddAndGenerateTurn`, `Prompt`, `CompactSession`, `AddUserTurn`, `AddMedia`, `CreateScratchpad`, `DeleteScratchpad`, `StripSignatures`.
4. Kept `InspectAgent` and `ListAgents` as the sole exemptions, preserving D16's narrow exemption for structural workspace health diagnostics.

**Why**: Restores complete cross-agent boundary security against unauthorized session/memory/scratchpad reads via `run_command`, while preserving full goroutine safety and zero-deadlock background reading.

## D61: `<SCRATCHPAD_EXPAND id="X" />` - an output-side, `wackydiscord`-only display-expansion sentinel

Implemented in `tools/wackydiscord/bot/sync.go`, `tools/wackydiscord/bot/handlers.go`, `tools/wackydiscord/bot/commands.go`, `skills/scratchpad-efficiency/SKILL.md`.

**The problem**: A beta RP framework built on `wackypub` has a "narrator" agent that generates a large prose payload each turn. Storing that prose in a scratchpad (`create_scratchpad(text=<prose>)`) and *also* re-emitting it as the turn's own final plain-text response (so a human sees it, via `wackydiscord`) forces the model to generate the same content twice in one turn - directly against the efficiency pattern `skills/scratchpad-efficiency/SKILL.md` already teaches for every other large-payload case.

**Rejected approach**: reusing `<SCRATCHPAD_DATA id="X" />` (the existing tool-input macro, expanded server-side inside `run_command`'s `args`/`stdin` fields per D18) directly inside an assistant's final response text. Rejected because that syntax already carries a specific, narrower meaning inside `wackypub` core - triggering subprocess-input expansion with real side effects - and overloading the same tag to *also* mean "a downstream display consumer should substitute this" blurs turn IO: it becomes ambiguous, from the tag alone, whether a given occurrence is something `wackypub` core is expected to expand (tool args) or just inert text a model happened to produce (everywhere else, including turn output).

**Chosen design**:
1. **A new, distinctly-named tag - `<SCRATCHPAD_EXPAND id="X" />`** - self-closing, same `id`-attribute shape as `<SCRATCHPAD_DATA>` for visual/vocabulary consistency with the rest of the scratchpad macro family, but a different verb (`EXPAND` vs `DATA`) signaling a different, narrower meaning: "a downstream display consumer *may* substitute this inline for human-facing rendering." `wackypub` core (CLI, SDK, `session.jsonl`, compaction, `ReadSessionTurns`, etc.) does **not** recognize this tag at all - it's inert, literal text as far as core is concerned, same as any other string a model might emit. This preserves the turn-IO boundary: nothing rewrites what the model actually said before persisting it.
2. **Expansion lives entirely in `wackydiscord`**, not `wackypub` core - a new shared helper (alongside `FormatAssistantBackfillMessage`/`FormatToolTurnSummary` in `tools/wackydiscord/bot/sync.go`) that scans outgoing message text for `<SCRATCHPAD_EXPAND id="X" />` occurrences, resolves each via `b.SDK.GetScratchpad(binding.AgentID, id, nil, nil)` (same-agent access, already authorized post-D60 regardless of `WACKYPUB_ALLOWED_AGENTS` state since `wackydiscord` runs from the workspace root, not inside any single agent's directory), and substitutes the resolved text inline before the message is handed to `SplitDiscordMessage`/`SendAgentMessage`. The pattern itself should be generic in `wackydiscord`'s implementation (not hardcoded to "narration" or this specific RP framework), so any agent's own convention can opt into it.
3. **Applied uniformly across every text-formatting site that shows assistant output** - the live path (`respText` in `HandleMessageCreate`) and the backfill/resync paths (`autoFillUnsyncedTurns`, `handleFillCommand`'s use of `FormatAssistantBackfillMessage`) - not just the live path, so a restart or `/fill` shows the same expanded content a live turn would have.
4. **Graceful, silent fallback if the referenced scratchpad entry is gone** (evicted past the 300-entry cap, wrong ID, etc.): leave the raw `<SCRATCHPAD_EXPAND id="X" />` text as-is rather than erroring or blocking the rest of the message. No special-casing needed beyond that - deliberately accepted, not a bug to fix later (see below).
5. **Document the new sentinel in `skills/scratchpad-efficiency/SKILL.md`** as a new numbered pattern (section 8, after the existing "Additional Advanced Swarm & Pipeline Patterns") - explicitly framed as *optional*, honored only by downstream consumers that choose to support it (`wackydiscord` today), not a `wackypub`-core mechanism, so a model doesn't assume it works when talking to a consumer that doesn't expand it.

**Deliberately accepted tradeoffs, not deferred as open questions**:
- **`session.jsonl` stores the raw, unexpanded reference, not the prose.** A human reading session history directly (outside `wackydiscord`) sees the sentinel, not the narration. Accepted: the model generated the actual prose exactly once already (inside the `create_scratchpad` tool call's own `args.text`, which *is* captured in `session.jsonl` as part of that turn's `FunctionCall` part) - it's recoverable from the transcript, just not as the final turn's own plain text.
- **The reference can go stale** if resolved long after the fact, once scratchpad churn evicts the entry (300-entry cap, oldest-evicted). Accepted for the same reason compaction is already accepted as lossy: compaction *already* replaces old turns' literal text with an LLM-generated summary over time, so "the literal original text isn't preserved forever in `session.jsonl`" is already true of the system generally, not a new property this introduces. `git`-backed agent history (D9-era workspace commits) means the byte-for-byte original is still recoverable from a past commit even if both the live scratchpad and the live session no longer carry it.
- **The `/fill` backfill case is explicitly not a special concern**: if a scratchpad referenced by a backfilled turn is already gone by the time `/fill` runs, showing the raw sentinel (per the graceful-fallback behavior above) is acceptable, not a bug.

**Why**: Lets the narrator model generate its prose exactly once per turn (matching the project's established scratchpad-efficiency pattern) while keeping `wackypub` core fully unaware of and unaffected by an application-specific (RP-framework) display convention - the turn-IO boundary stays exactly as clean as it was before, since nothing in core ever interprets or rewrites a model's own output.

## D62: `files-rw cat <path>` - raw binary read, completing the image-read pipeline

Implemented in `tools/files-rw/filesrw/ops.go`, `tools/files-rw/main.go`.

**The problem**: D47-D48 built a complete pipeline for getting binary/image content into an agent's multimodal context: `run_command`'s captured stdout is inspected via `DetectMediaType`, and binary output is *always* routed into a `.dat` binary scratchpad entry (`CreateBinaryScratchpad`, a raw on-disk file, not JSON-embedded - no UTF-8 corruption risk) regardless of size; that entry can then be piped into another tool's stdin via the existing `<SCRATCHPAD_DATA id="X" />` macro (e.g. straight into `wackypub agent <id> add-media`'s stdin), which handles resize/normalize/deferred-multimodal-queueing from there. `files-rw` is the only broken link in that chain: `read`/`tail` both unconditionally refuse any content with a NUL byte in the first 8KB (`isBinary` in `filesrw/ops.go`), with no raw/binary-safe read path at all - so an agent cannot get an image file already sitting in its own workspace (e.g. a reference photo, a generated asset) into the pipeline at all. `files-rw cat photo.jpg` currently just errors with "looks like a binary file - refusing to read it as text."

**Fix**: A new `files-rw cat <path>` command that streams the file's raw bytes to stdout unconditionally, bypassing the `isBinary` check entirely - explicitly for piping into another tool's stdin (`run_command`'s existing `DetectMediaType` auto-capture takes it from there), not for a model to read directly as text. Goes through the exact same `Access`/permission boundary as `read` (same root-scoping, same `Access.OpenFile` call) - only the binary-content check is skipped, no change to what paths are reachable at all.

**Size limit**: NOT `read`'s existing `MaxReadSizeBytes` (200KB) - that cap exists specifically to avoid wasting LLM context tokens on oversized text, which doesn't apply here since these bytes never become text tokens; they're piped straight to another process's stdin. `cat`'s own limit is 30MB, generous enough for real photos/generated images without being unbounded.

**Must not repeat the existing `TODOS.md` gap** ("`files-rw` reads a file's full contents into memory before ever checking its size"): `cat` needs its own `os.Stat`-based size check *before* `io.ReadAll`/copying the file, rejecting early if the file already exceeds the 30MB cap - not read-then-check, which would inherit the same unbounded-memory-read pattern that gap already flags elsewhere in this tool, just with a new command reproducing it.

**Why**: Completes an otherwise-fully-built image pipeline with the smallest possible change - `files-rw` stays a generic, wackypub-agnostic byte-I/O tool (no image/MIME-type awareness added to it at all), and every bit of "smart" handling (detecting it's an image, routing to a binary scratchpad, later deferring as a real multimodal part) stays entirely on the `wackypub` side, already built and already working.

## D63: `get_scratchpad` size cap, mid-turn context short-circuit, and token-weighted compaction

Implemented in `pkg/agent/scratchpad.go`, `pkg/agent/adk_agent.go`, `pkg/agent/compaction.go`, `pkg/agent/session_store.go`.

**The incident**: An agent pulled a huge scratchpad entry into its own context via an unpaginated `get_scratchpad` call, wrecking the session - and compaction couldn't save it, because the resulting turn was already at or beyond the model's `contextWindow` on its own.

**Root cause, traced through the actual code**: `GetScratchpad` (`pkg/agent/scratchpad.go`) has *no* size cap when called without `skip_lines`/`num_lines` - it just returns the entire entry, however large. Every other read-shaped tool in this project already has one (`files-rw read` refuses anything over its 200KB `MaxReadSizeBytes` unless paginated; `search_scratchpad` defaults `max_results` to 50) - `get_scratchpad` is the sole exception. Compaction couldn't recover because it only ever archives the *older* portion of a session (`CheckAndCompactSession`, `pkg/agent/compaction.go`) - a huge pull is by definition the most recent turn when it happens, so compaction can't touch it until it eventually ages into the archived portion, at which point *that* compaction call has to include it in its own request too and hits the same wall.

Also traced (in response to "does compaction happen mid-turn?"): it does not, and structurally can't cheaply. `CheckAndCompactSession` is checked exactly once, in `FolderAgent.GenerateTurn` (`agent_folder.go`), *before* `r.Run(...)` (ADK's internal tool-calling loop) starts - nothing re-checks size once that loop is running. Actually compacting mid-loop isn't a small addition: `CheckAndCompactSession` is itself a full nested LLM call via a disposable in-memory session (D45), and whatever context ADK's runner has already accumulated in memory for the in-flight turn doesn't retroactively shrink just because `session.jsonl` changes on disk underneath it - you'd need to abort and restart the tool loop against newly-compacted state, not just interleave a compaction pass between iterations.

**Fix, three parts, same root cause**:

1. **Cap `get_scratchpad`'s output.** Same shape as `files-rw read`'s existing pattern (hard cap, forced pagination via `skip_lines`/`num_lines` or `search_scratchpad` instead, no bypass flag), with one deliberate difference: the cap applies to the *actual resulting output* regardless of whether `skip_lines`/`num_lines` were given, not just the fully-unpaginated case (unlike `files-rw read`, which only checks size when no range is given at all) - scratchpads are specifically the zero-token hand-off mechanism for genuinely huge dumps, so an explicit `num_lines=999999` needs to be caught too, not just a bare call. Threshold is relative to the agent's own `runtime.json` `contextWindow` (reusing `EstimateTokens`'s existing `~4 chars/token` heuristic) rather than a flat byte size - a flat cap tuned for one model could be far too generous or too stingy for another, and this incident was specifically "bigger than the model's own context." Default: refuse a single call's resulting output if it's estimated to exceed 25% of `contextWindow`, with a flat-byte fallback (matching `files-rw`'s 200KB) for agents that don't set `contextWindow` at all. Error message reports the actual size/estimated tokens vs. the cap, same pattern as `files-rw read`'s existing error text.

2. **Mid-turn context short-circuit, as a general, complementary safety net - not a substitute for (1).** Even with `get_scratchpad` capped, several medium tool calls in one turn's loop could still cumulatively exceed budget without any single call tripping the per-call cap. Extend the existing `BeforeModelCallbacks` hook (`BuildADKAgentWithConfig`, `pkg/agent/adk_agent.go`) - which already short-circuits the loop early on `maxToolTurns` via a synthetic stop-here response - to *also* estimate accumulated context size before each model call within the loop, not just count calls, and stop the turn early the same way if something mid-flight pushed it over budget. This doesn't compact anything mid-turn (per the "not structurally cheap" finding above) - it just ends the *current* turn early so the *next* top-level turn's existing pre-generation compaction check runs against a session that's already been given a chance to stop growing, instead of letting the loop keep stacking more model calls on top of an already-oversized turn. Deliberately safe to just stop-and-defer rather than needing to salvage anything: in practice the dominant vector for a turn ballooning mid-flight is a scratchpad read (anything large out of `run_command` already funnels into a scratchpad entry per D48 automatically), and scratchpad reads are idempotent - stopping early and letting the agent redo the read on its next turn (against a session `wackypub` has now had a chance to compact) loses nothing.

3. **Reinterpret `compact-pct` as a percentage of estimated tokens, not turn count.** `CheckAndCompactSession`'s current boundary computation (`numToCompact := int(float64(len(turns)) * (pct / 100.0))`) is purely turn-count-based, blind to how much any individual turn actually weighs - inconsistent with the compaction *trigger* itself, which is already token-based (`EstimateTokens(turns, ...) >= runtimeCfg.ContextWindow`). A count-based archival amount can easily fail to relieve real context pressure when turn sizes are uneven (the exact shape of this incident): archiving the oldest 50% *by count* could mean archiving a pile of tiny old turns while a single still-huge turn sits just past the boundary in `remainingTurns`, having done almost nothing to reduce actual token load. Fix: walk turns oldest-to-newest accumulating each turn's own estimated token count (same heuristic as `EstimateTokens`, applied per-turn), and set the initial boundary where that running total first reaches `pct%` of the *whole session's* estimated tokens - then still extend the boundary forward to the next model-turn boundary exactly as today (that part is orthogonal to how the initial boundary was computed). Reinterprets the existing `compact-pct` frontmatter field in place rather than adding a new mode/field - simplest option, matches the project's minimal-config-surface preference elsewhere, and low-impact given `wackypub`'s early-stage/small-deployment status.

**Why**: All three address the same underlying gap - nothing in the tool-loop path currently bounds how much a single turn's context can grow before compaction gets a chance to run - from three complementary angles: capping the single largest realistic growth vector at the source (1), catching any other way a turn balloons mid-flight as a general backstop (2), and making the compaction that eventually runs actually proportional to the thing it's meant to relieve (3), rather than blind to it.

## D64: `wackydiscord`: Session Pruning Sync Resets & Webhook Self-Trigger Loop Prevention

Implemented in `tools/wackydiscord/bot/sync.go`, `tools/wackydiscord/bot/handlers.go`, `tools/wackydiscord/bot/commands.go`.

**The problem**: If a user pruned one or more turns from the end of `session.jsonl` (e.g. via hand edit or stripping signatures), `wackydiscord` entered an infinite loop of re-filling turns and generating runaway responses in Discord.

**Root causes**:
1. **Historic Turn Dumping in `DiffUnsyncedTurns`**: When the tail of a session was pruned, `lastHash` (the hash of the pruned turn) no longer existed in `session.jsonl`. Case 4 in `DiffUnsyncedTurns` erroneously assumed any missing hash meant "compaction truncated history past the last synced turn" and returned **all surviving turns** (`turns`) from index 0. This caused `wackydiscord` to re-emit the entire historical session prefix to Discord.
2. **Webhook Self-Triggering in `HandleMessageCreate`**: `HandleMessageCreate` checked `if m.Author == nil || m.Author.Bot { return }`. However, Discord API delivers messages created via webhooks with `m.WebhookID != ""` and `m.Author.Bot == false`. Consequently, every webhook message posted by `wackydiscord` during auto-sync was treated as a brand new human prompt, invoking `AddAndGenerateTurn`, which generated a new model turn, modified `session.jsonl`, fired the file watcher, and triggered another round of webhook emissions in an infinite loop.

**Fix**:
1. **Case 4 Safe Reset**: In `DiffUnsyncedTurns` (`tools/wackydiscord/bot/sync.go`), if `lastHash` is not found, return `nil` unsynced turns and reset the sync markers (`newLastIdx`, `newLastHash`) to the new session tail. Never dump historical prefix turns into the channel during background auto-sync.
2. **Webhook Message Filtering**: In `HandleMessageCreate` (`tools/wackydiscord/bot/handlers.go`), filter out `m.WebhookID != ""` alongside `m.Author.Bot` to prevent bot/webhook self-echoing.
3. **Explicit `/fill limit:N` Support**: In `handleFillCommand` (`tools/wackydiscord/bot/commands.go`), allow explicit `limit` requests to backfill the trailing $N$ turns even when the diff shows no new turns.

**Why**: Guarantees that rewinding or pruning session turns cleanly resets channel synchronization markers without replaying old conversation turns or triggering runaway webhook loops.

## D65: Self-driving "director" bootstrap container

Implemented in `Dockerfile`, `docker-entrypoint.sh`, `docker-compose.yml`, `agents/director/AGENTS.md`, `examples/runtimes/`.

**The problem**: The existing `Dockerfile` at the repo root is deliberately minimal - it copies a pre-built `wackypub` binary onto Ubuntu with a few common tool runtimes (python3, golang-go, nodejs, npm) and drops straight into `bash`. It has no workspace, no bundled skills, nothing agent-shaped at all - a human has to build a workspace by hand from scratch every time, following `skills/wackypub-ws/SKILL.md` themselves rather than an agent doing it for them.

**Design**:

1. **A self-bootstrapping "director" agent**, seeded inside the image's own default workspace and copied out to a bind-mounted workspace directory on first run. Has `run_command`/bash access (a setup wizard legitimately needs broad capability - installing things, running arbitrary setup scripts, cloning skillpacks - a different risk profile than a general-purpose agent default). Preloads `wackypub-ws` (workspace setup guidance) and whichever other bundled skills are broadly useful. `AGENTS.md` directives: help the user set up a workspace, configure runtimes, ask what they want the environment to do, stay aware that the workspace is a bind mount the user can drop files/skillpacks into directly.
2. **Bind mount, not a named Docker volume**, for the workspace - `${WORKSPACE_DIR:-./workspace}:/ws` in the compose file's `volumes:`, overridable via `.env`/shell. A named volume is opaque (lives in Docker's own storage, not directly browsable) - directly at odds with this project's core "config as plain files a human can read/edit directly" tenet (AGENTS.md/MEMORY.md/runtime.json are all meant to be hand-editable), and the director's whole premise (user drops files in for it to see) requires host-visible files in the first place.
3. **Bundle skills + runtime examples, not the whole source tree.** The skills are already written *for* an agent to consume (curated, task-oriented) - a better source of truth for "how do I use this" than raw Go source, which would mostly add image bloat for a speculative need already served by the skills.
4. **`examples/runtimes/*.json` gets fixed at the source to use `${PROVIDER_API_KEY}`-style placeholders instead of the current literal blank `"apiKey": ""`.** `runtime.json` env-var expansion is already a real, documented capability (`skills/wackypub-ws/SKILL.md`) - the example files just never actually demonstrated it. This is a strict improvement for the existing non-container `examples/runtimes/` workflow too, not something container-specific, so it's the source-of-truth files that change, not a divergent container-only copy.
5. **Compromise on source access**: no bundled source tree, but the director's `AGENTS.md` includes the `wackypub` GitHub URL so it can `git clone` and read source directly if something is genuinely undocumented and it needs to debug that way (modern LLMs can do this capably, and it's a real, if rare, legitimate need). Paired with an explicit nudge: if the director *does* have to fall back to source because something wasn't well-documented, it should tell the user their situation would make a good GitHub issue and give them enough detail to file one - a free real-world documentation-gap feedback loop.
6. **`tools/` symlink convention** matches what `skills/wackypub-ws/SKILL.md` already documents (`toolsets/` at the workspace root, agents symlinking in) - not a new pattern, just applying the existing one: a canonical tools directory in the image, each seeded agent's `tools/` symlinks into it.
7. **Boot-mode handoff mechanism**: the container's real `ENTRYPOINT` is a small shell script, not `wackypub` directly. On start, it checks for an executable `/ws/desired-entrypoint.sh` the director may have written (`chmod +x`-ing it first so the director doesn't have to remember that step itself); if absent, it launches the director's own `wackypub agent director repl` directly. If present, it runs it - a clean (exit 0) exit ends the container normally, same as any Docker container whose foreground process finishes (`docker start` re-runs the entrypoint and picks the same script back up); a *nonzero* exit falls back to the director repl so repairs can be made. This deliberately distinguishes a REPL session ending normally (exit 0, not a failure) from an actual crash (nonzero), and correctly handles a long-running foreground process (e.g. `wackydiscord`) that's *meant* to never return until killed. No new escape-hatch mechanism needed for a human to force a reset back to the director either - the workspace is a bind mount, so `rm`/renaming `desired-entrypoint.sh` from the host works directly.
8. **Strong "don't brick yourself" directives required in the director's `AGENTS.md`** - the whole failsafe depends on the director not writing a script that both exits 0 *and* doesn't actually do what was intended (a script that's "successful" but wrong wouldn't trigger the fallback at all, unlike a script that fails outright).

**Why**: Turns `skills/wackypub-ws/SKILL.md`'s manual, human-driven workspace bootstrapping into something an agent can do *for* a new user interactively, without inventing new wackypub-core mechanisms - everything here is either an existing convention applied to a new context (skills, `toolsets/`, env-var expansion) or a container/shell-level concern (bind mount, entrypoint script) that doesn't touch `wackypub` itself at all.

## D66: One master container - retire the `main`/`sub1`/`sub2` demo, streamline the quick start, scoped `sudo` for the director

Implemented in `Dockerfile`, `agents/director/AGENTS.md`, `README.md`.

**The problem, found during D65 review**: the repo already had a second, separate container quick start - `scripts/run_container.sh`/`init_container_env.sh`/`destroy_container.sh` plus `agents/container/` (`MAIN.md`/`SUB.md`), a minimal "give an agent root and see what happens" demo (a `main` agent delegating to `sub1`/`sub2`). It builds from the same root `Dockerfile` D65 rewrote. Confirmed live: this is now broken - `init_container_env.sh` hardcodes `ln -sf /bin/wackypub main/tools/wackypub`, but D65's multi-stage build installs binaries to `/usr/local/bin/`, not `/bin/` - `/bin/wackypub` doesn't exist in the image at all anymore, so `main`'s ability to reach `wackypub` as a tool (needed to delegate to `sub1`/`sub2`, the entire point of the demo) is a dangling symlink. Root cause: the root `Dockerfile`'s purpose fundamentally changed under D65 (single Ubuntu+binary+bash-entrypoint demo -> multi-stage build compiling the whole suite, non-root user, a director-specific template workspace, an entrypoint that defaults to launching `director`), and both flows now collide on the same file.

**Decision: don't maintain two container flows - retire the old demo entirely** rather than patching it to coexist. Remove `scripts/run_container.sh`, `scripts/init_container_env.sh`, `scripts/destroy_container.sh`, `agents/container/MAIN.md`, `agents/container/SUB.md`. The demo's one distinguishing feature - an agent with bash and nothing else set up - isn't actually lost, it's reachable by telling the director that directly ("just give me root, don't set anything else up") instead of via a separate scripted path. One Dockerfile, one `docker-compose.yml`, one story.

**README Quick Start replaced.** The current two-part Quick Start (`run_container.sh` demo + "Bring Your Own Agent") becomes: `docker compose up`, then describe what you want to build to the director in the attached REPL - it sets up the workspace and hands off. "Quick Start: Bring Your Own Agent" (the non-Docker CLI-install path for an existing coding agent) is unrelated and stays as-is.

**Director `AGENTS.md` needs a posture shift to actually deliver "describe it, it goes."** As currently written it reads as an open-ended interactive wizard - ask what they want, guide them through configuring runtimes, ask about API keys, a lot of back-and-forth. For the new quick start framing to hold, the director needs to lean toward reasonable default choices and moving to execution quickly rather than a long Q&A: gather the goal, pick sensible defaults without belaboring it, build it, then write `/ws/desired-entrypoint.sh` and tell the user to exit to launch it - treated as the natural conclusion of the setup conversation, not a separate feature buried at the end of the instructions. The underlying exit-to-relaunch handoff mechanism (D65.7) is unchanged - this is about the director's conversational posture, not the technical mechanism.

**Scoped `sudo` for the director, not blanket root.** D65 already fixed the actual danger (a root container process has unrestricted access to whatever host path is bind-mounted, since there's no UID translation without `userns-remap`) by running as non-root. That risk is specifically about the bind-mounted path (`/ws`), not root-ness in the abstract - so re-granting narrowly-scoped `sudo` for system package installation doesn't reopen it, as long as the grant can't reach `/ws`. Add to `/etc/sudoers`:
```
wackypub ALL=(root) NOPASSWD: /usr/bin/apt-get, /usr/bin/apt
```
Not a blanket `NOPASSWD: ALL` - a general root shell (via unrestricted `sudo`) could still `chown`/`rm`/otherwise touch the bind-mounted `/ws` and reintroduce exactly the host-file-ownership problem D65 fixed, and there's no way to guarantee an LLM-driven bash session never does that beyond an `AGENTS.md` request not to - an OS-level restriction is the actual guardrail, not a prompt instruction. Other package managers already on the image (`npm`, `pip`, `go install`) don't need root for normal per-user installs, so the sudoers scope stays limited to `apt-get`/`apt`. One concrete gotcha to put directly in the director's `AGENTS.md`: the Dockerfile's build step runs `apt-get install ... && rm -rf /var/lib/apt/lists/*` to keep the image lean, so the package index is empty by the time the director runs - its first `sudo apt-get install <x>` will fail with "unable to locate package" unless it runs `sudo apt-get update` first.

**Why**: Two container flows sharing one Dockerfile is what caused the D65 regression in the first place and will keep causing friction as either evolves - collapsing to one, better-matches "our quick start was kind of random anyway." The director's "ask a lot, then maybe get to work" posture undersells what D65 actually built; a decisive "describe it, it goes" framing is a better match for both the feature and the new single quick start. Scoped `sudo` gives the director a real, common setup need (installing system tools) without re-opening the exact host-write risk D65's non-root fix just closed.

## D67: `docs/SWARM_TESTING.md` reconciled with D66's single container - privilege level becomes an explicit test parameter ("access bands")

Implemented in `docs/SWARM_TESTING.md`.

**The problem, found during D66 review**: `docs/SWARM_TESTING.md` (the swarm-based security-testing methodology behind `files-rw`'s pen-test reports and similar) references the now-deleted `scripts/run_container.sh`/`init_container_env.sh`/`destroy_container.sh` and the `main`/`sub1`/`sub2` pattern as its documented setup mechanism - all removed by D66. It also states outright that "the container runs as root, so anything the swarm creates in the bind-mounted host workspace... ends up root-owned on the host," describing the old container's root-by-default behavior as an accepted, load-bearing property, not just an incidental annoyance - now stale, since D65 made the container non-root by default.

**Resolved: swarm testing uses the same single director-based container, no separate Dockerfile.** Host protection against a swarm's fixture-building/probing is a property of the Docker `USER`/bind-mount permission boundary, not of whether the in-container user happens to be root - a non-default `USER` doesn't change what's reachable on the host side of the mount either way. Swarm workers "probably need to install tools sometimes" too, same as the director - covered by the same scoped `sudo` (`apt-get`/`apt`) D66 already added, or a broader grant for a specific test run (below).

**New methodological framing: privilege level is itself an explicit test parameter, not a fixed environment property.** D65/D66's own process directly demonstrated that available access changes the reachable attack surface in ways that are easy to miss (root-in-container + bind-mount = host-level root; non-root + scoped `sudo` = a materially different, much narrower surface). Rather than picking one fixed privilege tier for swarm testing, `SWARM_TESTING.md` should document running the same swarm methodology at different **access bands** and treating divergent findings across bands as signal, not noise. Mechanically, this needs no new infrastructure: `USER` is overridable per-run against the same image (`docker compose run --user root wackypub` for a root-band run, the default `wackypub` user for the normal band, etc.), so varying the band is just a run-time flag, not a second Dockerfile - consistent with D66's "one master container."

**Concrete doc fix**: replace the dead script references with the `docker compose up`/root-`Dockerfile` setup; replace the stale "container runs as root" passage with guidance on choosing (and documenting) which access band a given swarm run is testing, and how to invoke each band (default non-root+scoped-sudo, and an explicit root override for when root-in-container is the thing under test).

**Why**: Keeps D66's "one master container" intact rather than reopening it for a special case, and turns a doc-staleness bug into a genuine methodology improvement this project already has direct, first-hand evidence for - access level materially changes what's findable, so testing across bands finds more than testing at just one.

## D68: Real-usage-based compaction (post-turn + mid-turn), configurable trigger overhead, `compact` CLI always forces

Implemented in `pkg/agent/compaction.go`, `pkg/agent/agent_folder.go`, `pkg/agent/adk_agent.go`, `cmd/agent.go`, `examples/COMPACT.md`, `docs/agents.md`.

**The problem**: Two independent gaps in the compaction trigger, found while workshopping a "full" (`append-only: false`, memory-rewriting) compaction mode:
1. **Real provider usage stats already reach `wackypub`, unused.** `model.LLMResponse.UsageMetadata` (`*genai.GenerateContentResponseUsageMetadata` - `PromptTokenCount`, `CandidatesTokenCount`, `CachedContentTokenCount`) is already populated by the OpenAI-compatible adapter from the real `usage`/`prompt_tokens_details.cached_tokens` fields on every response - confirmed by reading `adk-utils-go`'s `convertUsageMetadata`. But a repo-wide grep for `Usage`/`UsageMetadata` in `pkg/agent/*.go` turns up nothing - this data is silently discarded on every single call, and compaction relies entirely on `EstimateTokens`'s `~4 chars/token` heuristic instead, which D63 already proved can meaningfully undercount (it missed `FunctionCall`/`FunctionResponse` payloads entirely until that fix).
2. **The compaction trigger has zero safety margin.** `CheckAndCompactSession`'s check is `if estimatedTokens < contextWindow { return false }` - compaction is only attempted once the session is already *at or over* the full context window, with no cushion for any estimation error, real or heuristic. A heavier "full" rewrite-mode compaction call (summarizing *and* re-synthesizing all of `MEMORY.md`, not just appending a short addendum) needs more generation headroom than the default append-only mode does, and neither mode currently gets any.

**Fix, four parts**:

1. **New post-turn compaction check, using real usage data - additive, not a replacement for the existing pre-turn check.** After `r.Run(...)` finishes inside `FolderAgent.GenerateTurn`, check the just-completed turn's final `LLMResponse.UsageMetadata.PromptTokenCount` against `contextWindow` (with the new overhead margin, below) and compact immediately if over, before returning. Needs no new persisted state: the real number is already in memory from the response that was just generated, since this runs in the same process, not a separate later invocation. Considered (and rejected) persisting the last real number to a new file so the *existing pre-turn* check could use it instead - real, but simpler and with a genuine correctness gap: a stored number from the agent's own last generation wouldn't reflect turns added by something else since (a human `add`, a cross-agent write, a background sync), which the current estimate-based pre-turn check already handles correctly today by re-scanning whatever's actually in `session.jsonl`.
2. **Existing pre-turn `EstimateTokens`-based check stays exactly as-is.** It's the right tool for "did the session grow from something other than my own last generation" - the new post-turn real-number check and this one are complementary, not competing: one catches growth from the agent's own turn immediately and precisely, the other catches everything else at the top of the next turn, same as today.
3. **D63's mid-turn short-circuit (`BeforeModelCallbacks` in `adk_agent.go`) switches to real usage data too, where available.** It already re-runs before every model call within a turn's tool loop (`modelCalls > 1`), so the *previous* call's real `UsageMetadata.PromptTokenCount` (from earlier in the same turn) is available and gets fresher with every loop iteration - strictly better than `EstimateTokens(req.Contents, ...)`, which it currently uses unconditionally. Falls back to the estimate only when no real number exists yet for this turn (i.e. the check right after the very first model call).
4. **New `COMPACT.md` frontmatter field: `compact-overhead-pct`** (sibling to the existing `compact-pct`), a configurable safety margin - compaction triggers at `(100 - compact-overhead-pct)%` of `contextWindow` instead of the current, always-zero-margin 100%. Default 20%, matching the number floated when this was first raised. Applies uniformly to all three "how close to the wall" checks above (pre-turn estimate, post-turn real, mid-turn real) for consistency - it would be confusing for compaction to respect a margin that the mid-turn short-circuit doesn't. Kept orthogonal to the existing `compact-pct` (how much gets archived per pass, already token-weighted per D63) - one field controls *when* to trigger, the other controls *how much*, and a "full" rewrite-mode config is expected to want a larger `compact-overhead-pct` than the default append-only mode, without needing a separate mode-specific mechanism.
5. **`wackypub agent <id> compact` always forces - remove its `--force` flag and the check it gates.** Explicitly invoking the command already expresses the intent to compact now; making the human/agent also remember a separate `--force` flag to get that to actually happen is redundant. Scoped narrowly to the CLI command's own `RunE` (`cmd/agent.go`'s `agentCompactCmd`, currently `sdk.CompactSession(ctx, agentID, forceCompact)` - change to always pass `true`) - `AgentSDK.CompactSession`'s own `force bool` parameter stays as-is, since the *automatic* pre-turn/post-turn checks inside `GenerateTurn` must remain non-forced (forcing every single turn to compact regardless of need would defeat the whole point of a threshold).

**Why**: Uses data `wackypub` already receives on every call instead of an admittedly-imperfect heuristic, closes a real zero-margin gap in the trigger with no dependency on getting `EstimateTokens` perfectly accurate, and does all of it without introducing new persisted state - the post-turn and mid-turn checks both source their real numbers from data already in memory within the same process.

## D69: Turn generation becomes iterator-first (`iter.Seq2[string, error]`) - fixes silently-dropped multi-part text, adds real streaming to CLI and `wackydiscord`

Implemented in `pkg/agent/agent_folder.go`, `pkg/agent/sdk.go`, `cmd/agent.go`, `tools/wackydiscord/bot/handlers.go`.

**The bug, confirmed live before any design work started**: `FolderAgent.GenerateTurn` (`agent_folder.go`) loops over `r.Run(...)`'s event stream and does `finalResponse = text` (assignment, not append) every time an event carries non-empty text. Reproduced directly: a model that narrates before calling a tool ("Let me check that for you." + a tool call), then gives its final answer ("The result is 42.") in a separate event - `session.jsonl` correctly captures *both* as separate turns (`AppendEvent` is wired into ADK's runner independently of `GenerateTurn`'s own bookkeeping, so nothing is lost from the durable record), but the string `GenerateTurn` actually *returns* is only `"The result is 42."` - the narration is silently dropped. `ExtractTextFromEvent` itself is fine (it correctly concatenates all parts *within* one event, filtering `Thought`-marked parts) - the bug is specifically across events, at the outer accumulation.

Single root cause, identical impact across every consumer: `GenerateTurn` is the one shared function underneath `wackypub agent prompt`/`generate` (CLI), `AgentSDK.GenerateTurn`/`AddAndGenerateTurn` (SDK), and `wackydiscord` (which posts the SDK's return value directly, no extraction logic of its own).

**Fix: make the iterator the primary API, not an alternative one.** `FolderAgent`/`AgentSDK`'s turn-generation core gets rebuilt around a new primitive yielding `iter.Seq2[string, error]` - each yielded string is one text-bearing event's extracted text (reusing `ExtractTextFromEvent`'s existing correct within-event concatenation), in order; tool-call-only events yield nothing, matching current filtering. Mirrors `r.Run(...)`'s own `(value, error)` iteration shape rather than introducing a new convention - this codebase already uses that pattern throughout. The existing `(string, error)`-returning methods become thin wrappers that range over the iterator and join with `\n\n` - every existing caller gets the dropped-text bug fixed for free without touching anything, and only callers that want to actively consume the stream (CLI, `wackydiscord`) need to change. Chosen over a channel-based streaming API specifically because it's easy to go from an iterator to a collected string/slice and not the other way around - exposing the more general primitive by default costs nothing for callers who just want the old blocking behavior.

**Resource cleanup needs no new API surface.** Go's `range-over-func` (`iter.Seq`/`iter.Seq2`) guarantees that when a caller's loop breaks early (a `break`, an early `return`, or a panic), the `yield` call inside the iterator function returns `false` and control resumes right there, inside the iterator's own code - exactly like any other function call. Ordinary `defer` around the yielding loop is sufficient for both the session lock release and the post-turn compaction check (D68) to correctly fire on early termination, not just full drain - no separate `Close()`/closer function needs to be returned alongside the iterator. Not a novel property being relied on here either: `r.Run(...)` (ADK's own runner, which `GenerateTurn` already ranges over today) is already exactly this shape.

**CLI (`prompt`/`generate`/`repl`)**: ranges over the streaming variant and prints each chunk to stdout as it arrives, rather than collecting everything and printing once at the end. No flag needed - strictly better than current behavior (nothing prints until the whole turn finishes today, so this isn't a change anyone could be relying on), and gives any consumer of the CLI - human or another tool piping stdout - real-time visibility into a turn's progress without a special protocol, just reading stdout as it's written.

**`wackydiscord`**: posts one Discord message per yielded chunk (not `\n\n`-joined into a single message) - real per-chunk streaming, same as the CLI, not just a dropped-text fix. Each individual chunk still goes through the existing `SplitDiscordMessage` if it's oversized on its own.

**Why**: Fixes a real, live-confirmed data-loss bug (in the returned/displayed text specifically, not in `session.jsonl`) for every consumer at once by centralizing on one corrected code path, and turns "fix the bug" into "also gain real streaming" for free - the more general iterator-first primitive was needed to fix the bug correctly regardless, and CLI/`wackydiscord` streaming falls out of consuming it directly rather than requiring separate work.

## D70: Deferred scratchpad-image queueing surfaces its own failures instead of silently swallowing them

Implemented in `pkg/agent/agent_folder.go`.

**The problem**: Investigating a live report of a deferred image (`get_scratchpad` on a binary/image entry, per D49) never actually appearing in a follow-up turn. The specific incident turned out to be user/agent error, not a `wackypub` bug (the agent piped `files-rw`'s output through `head` before it reached the scratchpad, truncating the image) - but tracing it surfaced a real, general robustness gap independent of that specific cause: `FolderAgent.GenerateTurnStream`'s deferred-image-append loop (`agent_folder.go`) silently `continue`s past any failure in `findScratchpadFile`/`os.ReadFile`/`NormalizeAndResizeImage`, with zero logging anywhere. Worse, the "Image has been queued... available in your next turn" confirmation text is generated *speculatively*, by a `BeforeModelCallbacks` short-circuit that fires as soon as `get_scratchpad` returns `deferred: true` - before the actual append is ever attempted, later, in the turn's own cleanup. So any failure in that append - a truncated/corrupted file, an unsupported image format Go's decoder can't handle, anything - produces a confirmation message that's simply false, with no path to walk it back and nothing telling the agent (or the human) that anything went wrong.

**Fix**: on a failed append, don't silently `continue` - append a distinct failure-notice user turn instead of the image turn (clear text naming the scratchpad ID and the failure reason), so the agent's next generation call sees an honest "this failed" turn in context and can relay that to the user directly, instead of the user waiting on an image reaction that will never come. Pair with basic operator-facing logging (stderr) for the failure too, matching how other swallowed-but-logged warnings already work elsewhere in this same function (e.g. the post-turn compaction error path already does this).

**Why**: A queueing mechanism that can silently lie about its own success is a real robustness gap independent of any one root cause - this specific incident happened to be user error, but the exact same silent-failure code path would produce the identical confusing symptom for a genuinely corrupted upload, an unsupported format, or any other real failure in the future.

## D71: `files-rw write` refuses empty stdin content unless `--allow-empty` is passed

Implemented in `tools/files-rw/main.go`.

**The problem**: Found live - an agent exploring `files-rw` invoked `write <path>` without providing `stdin` (or via a macro that resolved to empty), and `WriteFile` (`tools/files-rw/filesrw/ops.go`) did exactly what it was told: atomically replaced the target file's content with an empty string. No warning, no confirmation, no distinction between "the caller genuinely wants a zero-byte file" and "the caller forgot to provide content" - both look identical by the time `WriteFile` sees them, and `write` is otherwise a normal, unremarkable, frequently-used command, not something an agent is likely to double-check output for the way it might for a destructive-sounding one like `delete`.

**Fix**: `write`'s `RunE` (`tools/files-rw/main.go`) rejects zero-length stdin content by default, with a clear error (`refusing to write empty content to <path> - pass --allow-empty to write an empty file intentionally`). A new `--allow-empty` bool flag on `write` bypasses the check for the genuine, intentional case. `append` is not affected - empty-stdin `append` is already a harmless no-op (nothing gets added to the target either way), not a truncation, so it has no equivalent failure mode to guard against.

**Why**: Matches the project's existing pattern of requiring explicit opt-in for a risky-but-sometimes-legitimate operation rather than silently permitting it (same shape as `files-rw`'s scoped-access-by-default posture generally) - closes a real, live-demonstrated accidental-data-loss path at essentially zero cost to the legitimate use case, which just needs one extra flag.

## D72: Skill reference files and bundled scripts - `load_skill_extra`, `list_skill_extra`, `run_skill_script`

Implemented in `pkg/agent/skill.go`, `pkg/agent/agent_folder.go`, and `pkg/agent/adk_agent.go`.

**The problem**: Real-world skill folders (the convention this project's own `SKILL.md` format is descended from) routinely ship more than one file - reference docs, example data, images, and executable scripts - referenced from the skill body via relative paths, alongside `SKILL.md` itself. `DiscoverAgentSkillsMap`/`load_skill` (`pkg/agent/skill.go`, `agent_folder.go`) only ever expose `SKILL.md`'s own parsed body - there's no way for an agent to reach anything else sitting in that same folder. This was already flagged as a TODO (`load_skill_extra for skill reference files`); resolving it surfaced two further, harder sub-questions - how binary reference files should be handled, and what to do about bundled scripts a skill expects the agent to actually run - both worked through below.

**`load_skill_extra(skill_name, relative_path)`**: reads one file from inside a skill's own folder. `relative_path` resolves recursively (skills can nest refs in subfolders, e.g. `reference/schema.md`), boundary-checked against that skill's own directory the same way `files-rw`'s access model bounds a path to an allowed root - no traversal outside the skill folder, no allowlist needed since the boundary is just "this one skill's own directory." Text content returns directly into the tool response, with no scratchpad auto-capture the way large `run_command` output gets - unlike a command's stdout, a reference file's content is exactly what the agent asked to see, not a side-effect byproduct, so there's no reason to make it fetch its own answer back out of a scratchpad entry immediately after asking for it. Binary content (detected the same way `NormalizeAndResizeImage`'s callers already detect it) is queued into a binary scratchpad entry instead of inlined, reusing the exact same mechanism `get_scratchpad`'s deferred-image path already uses (D47/D49) rather than inventing a second one.

**`list_skill_extra(skill_name)`**: recursive listing of every file in a skill's folder other than `SKILL.md` itself. Exists specifically as a fallback for when a skill's own body gets the relative path wrong or phrases it ambiguously - rather than an agent guessing at `load_skill_extra` calls until one works, it can just ask what's actually there.

Both tools work for any known skill name, including `always_load: true` skills - unlike `load_skill` itself (which refuses those, since the body's already sitting in context), an always-loaded skill's *extra* files still aren't injected anywhere, so they need to stay reachable the same way an on-demand skill's do.

**Bundled scripts - the harder question, resolved as a separate on-demand tool rather than folding into `run_command`.** A skill that ships a script the agent is meant to actually execute is a real, different capability from reading a reference file - it's a code-execution surface, the same trust level as anything already in `tools/`, just reached through a different door. Two shapes were considered:
1. **Widen `tools/`'s existing executable-bit discovery (`DiscoverAgentToolsMap`, `workspace.go`) to also walk `skills/*/`**, merging any executable file found there into the same `run_command` surface `tools/` already populates - reuses 100% of existing discovery/shadowing/exec code, but every skill's scripts inflate `run_command`'s tool description (already sent in full on every single call) from turn 1, whether or not that skill was ever loaded or is even relevant to the current task.
2. **Chosen: a sibling tool, `run_skill_script(skill_name, relative_path, args, stdin)`**, reusing `executeTool`'s exact machinery (macro expansion, stdout/stderr scratchpad auto-capture, timeout handling) but resolving `relative_path` against the skill's own folder the same bounded way `load_skill_extra` does, instead of going through the flat `tools/` discovery map. Keeps skill scripts genuinely opt-in and discovered the same way extras are - by reading the skill's own body after loading it - rather than statically bloating `run_command`'s always-visible command list with scripts from skills that were never loaded. Costs a second execution entry point rather than one unified one, accepted specifically to avoid the static-list-bloat problem, the same category of issue just trimmed from `load_skill`/`run_command`'s own error text (see the `agent_folder.go` fix logged alongside D71's review).

A bundled script must already be marked executable with its own shebang line to be runnable, identical to the existing `tools/` convention - no interpreter-sniffing or special-casing added on top of what `run_command` already does today.

**Why**: Keeps the read-only, low-trust surface (`load_skill_extra`/`list_skill_extra`) and the code-execution surface (`run_skill_script`) clearly separated rather than blurring skills into tools, while still reusing existing, already-reviewed machinery (bounded path resolution, `executeTool`, the binary scratchpad path) instead of building parallel implementations for each. Matches the project's established pattern of not inflating an always-sent tool description with content that's only relevant once something else has already happened (loading the skill, in this case) - the same principle behind D71's sibling fix to `load_skill`/`run_command`'s error text.

## D73: A second git commit right after `MEMORY.md` is updated during compaction, before `session.jsonl` is pruned

Implemented in `pkg/agent/compaction.go`.

**The problem**: `CheckAndCompactSession` (`pkg/agent/compaction.go`) currently produces exactly one git commit per compaction pass - `CommitWorkspaceEvent(wsDir, agentID, "compact")` at the very end, after `WriteSessionTurns` has already overwritten `session.jsonl` down to just the surviving turns (`WriteSessionTurns` does a full `os.Create` truncate-and-rewrite, not an append - the pre-compaction turns aren't recoverable from the *working tree* after this point, only from git history). So the one commit that exists conflates two genuinely separate events - "memory was updated" and "session was pruned" - into a single snapshot, and nothing in git history ever shows the full pre-prune session sitting alongside the newly-written memory addendum that summarizes it.

This is also the concrete cause of an already-logged TODO: `wackypub trace`'s "fog of war" at a compaction step, where `extractTurnDiff` can't show what a compaction step actually did (it compares turn *counts*, which is all it has, since there's no commit boundary separating "memory changed" from "session shrank").

**Fix**: add a second commit, immediately after `WriteMemoryFile` succeeds (`compaction.go`, right after the existing `if err := WriteMemoryFile(...)` block, before the compaction-notice turn is prepended and before `WriteSessionTurns` runs) - `CommitWorkspaceEvent(wsDir, agentID, "compact (memory)")`. At that point `MEMORY.md` already has the new addendum on disk, but `session.jsonl` is still completely untouched - full pre-compaction history, nothing pruned yet. The existing end-of-function `CommitWorkspaceEvent(..., "compact")` call is unchanged and still fires after the prune, so a compaction pass now normally produces two commits in sequence: `compact (memory)` (memory updated, full session still present) then `compact` (session pruned to the surviving turns).

Only fires if `MEMORY.md` actually changed - i.e. `addendum != ""` after the compaction directive call, same condition already gating the existing `WriteMemoryFile` call. If the directive produces no addendum (a failed/empty compaction generation), nothing new is committed at this step - no separate opt-out needed, since `CommitWorkspaceEvent`'s existing `AllowEmptyCommits: false` already no-ops a commit with nothing staged, this is just making that the deliberate behavior rather than an accident of it never being reachable.

**Why**: Gives git history an honest, separately-diffable boundary between "what compaction decided to summarize" and "what got pruned as a result" - a future `trace` improvement (or just a human doing `git log`/`git show` by hand) can now see exactly which turns were archived and what they became, instead of inferring it from a turn-count delta across a single conflated commit. Small, additive change - no behavior changes for anything that isn't specifically reading git history around a compaction event.

## D75: `AppendSessionContent` heals a missing trailing newline before appending

Implemented in `pkg/agent/session_store.go`.

**The problem**: `ReadSessionTurns` silently skips any line it can't unmarshal (by design - one bad line shouldn't take down the whole session). If `session.jsonl`'s last line has no trailing newline (e.g. after a hand-edit) and `AppendSessionContent` then appends a new turn, the two JSON objects land on one line. That merged line can't be unmarshaled, so both turns silently drop from history on the next read - a coherence gap with no error signal. Documented in AGENTS.md's Gotchas section and in TODOS.md.

**Fix**: `AppendSessionContent` now opens the file with `O_RDWR|O_APPEND` (previously `O_WRONLY|O_APPEND`), calls `Stat` to check the file size, and if the file is non-empty uses `ReadAt` to inspect the last byte. If the last byte is not `'\n'`, it writes a single healing newline before the new content. `ReadAt` reads from an absolute offset without moving the file position, so the subsequent append-mode write still lands at end-of-file correctly. The check is O(1) - one `Stat` and one `ReadAt` per append call; no seek needed.

**Why not fix this in `ReadSessionTurns` instead**: the corruption happens at write time and is visible on disk immediately after. A read-side fix (e.g., splitting on `}{` to recover merged objects) would be fragile, format-specific, and invisible to any other tool reading the file directly. Healing at write time is the minimal, correct boundary.

**Consequence**: the first `AppendSessionContent` call after a trailing-newline-stripping hand-edit now transparently repairs the file. The session lock (held by all callers of the mutating SDK methods that call `AppendSessionContent`) serializes concurrent appenders, so there is no new race condition introduced.

## D74: Bundled `openrouter-auto` becomes the default `runtime.json` when an agent has none

Implemented in `pkg/agent/runtime.go` and `main.go`.

**The problem**: `LoadRuntimeConfig` (`pkg/agent/runtime.go`) hard-fails whenever `<agentDir>/runtime.json` doesn't exist - a brand-new agent directory with just an `AGENTS.md` can't generate a single turn until a human manually copies or symlinks in one of the templates from `examples/runtimes/` (per that directory's own README - "copy one, configure your `.env`... and either point `runtime.json` directly at it or symlink it in"). There's no working-out-of-the-box path.

**Fix**: mirror `COMPACT.md`'s existing embedded-default pattern (D44/D45) exactly. `examples/runtimes/openrouter-auto.json` (already env-var-templated - `"apiKey": "${OPENROUTER_API_KEY}"`, already expanded today via `LoadRuntimeConfig`'s existing `os.ExpandEnv` call, no new plumbing needed there) gets embedded in `main.go` via `//go:embed` and assigned into a new `pkg/agent.DefaultRuntimeJSON` var, the same indirection `DefaultCompactMD` already uses and for the same reason - `examples/` isn't reachable by a `//go:embed` directive living inside `pkg/agent` itself.

`LoadRuntimeConfig` falls back to `DefaultRuntimeJSON` only when `runtime.json` is completely absent (`os.IsNotExist` on the resolved path - already covers both "never created" and "dangling symlink to a since-removed example," since `EvalSymlinks` fails identically for both cases today). A `runtime.json` that exists but fails to parse still hard-errors exactly as it does now - swapping in a default for a *broken* config would mask a real mistake instead of surfacing it, which is a materially different situation from "never configured at all."

`InspectAgent`/`wackypub workspace <id>` is unaffected - it already checks `runtime.json`'s existence directly via `pathExists`, not through `LoadRuntimeConfig`, so it keeps correctly reporting "no `runtime.json` present" as a diagnostic fact rather than being fooled by the fallback silently succeeding underneath it.

**Fail-fast on a missing API key, specifically for this fallback path**: when `DefaultRuntimeJSON` is the config actually in use and the expanded `apiKey` is still empty (i.e. `OPENROUTER_API_KEY` isn't set in the environment or workspace `.env` either - likely for exactly the brand-new-agent case this feature targets), `LoadRuntimeConfig` returns a clear error naming the missing env var and pointing at `examples/runtimes/README.md`, instead of letting an empty key reach OpenRouter and surface as a generic 401 several layers downstream.

**Why**: Gets a brand-new agent directory working the instant `AGENTS.md` exists, with zero required setup beyond exporting one API key - `openrouter-auto` specifically chosen (over pinning a single model) because "auto" routing degrades gracefully as a sane default rather than committing a new agent to one specific backend nobody chose on purpose. Reuses the exact embedding mechanism and fallback-only-on-absence semantics `COMPACT.md`'s default already established, rather than inventing a second pattern for the same kind of problem.

## D76: A per-channel mutex in `wackydiscord` serializes `ChannelBinding` mutations across `HandleMessageCreate`/`autoFillUnsyncedTurns`/`SyncAgentToChannels`

Implemented in `tools/wackydiscord/bot/state.go`, `tools/wackydiscord/bot/handlers.go`, `tools/wackydiscord/bot/bot.go`, `tools/wackydiscord/bot/commands.go`.

**The problem**: Already flagged as a TODO (`wackydiscord`'s per-channel binding state has no concurrency guard) and now live-confirmed. `ChannelBinding` (`IsGenerating`, `PendingUserHash`/`PendingUserText`, `LastTurnIndex`/`LastTurnHash`) is mutated via a get-copy-mutate-set pattern across three independent call paths that `discordgo`'s concurrent gateway dispatch lets run at the same time for one channel: `HandleMessageCreate` itself, that same function's own initial `autoFillUnsyncedTurns` call, and `SessionWatcher`'s debounced `SyncAgentToChannels` -> `autoFillUnsyncedTurns`. `State`'s own mutex (`state.go`) only protects each individual `GetBinding`/`SetBinding` call atomically - it does nothing to protect the much longer read-modify-...-modify-write sequence a single `HandleMessageCreate` invocation spans, from before generation starts through to its post-generation bookkeeping.

Traced through a real, live-reported incident (a message sent while the same channel's agent was already mid-generation on an earlier message) to a concrete mechanism: goroutine 2 (the second message) reads a `ChannelBinding` snapshot and sets its own `PendingUserHash`/`IsGenerating=true`, then blocks on the real session lock (`AcquireSessionLock`, held by goroutine 1's in-flight generation) inside `AddAndGenerateTurnStream`. When goroutine 1 finishes, its own end-of-function bookkeeping (`handlers.go`, after the streaming loop) writes back *its own stale local copy* of the binding - unconditionally resetting `PendingUserHash`/`PendingUserText` to `""` and `IsGenerating` to `false` - clobbering what goroutine 2 had just set moments earlier, with no signal that a second write had happened in between. Two concrete, observed consequences: (1) goroutine 2's own message later fails its `PendingUserHash` echo-suppression check once processed (the hash was wiped out from under it), so it gets posted back as if it were an unrelated "background" turn instead of being recognized as the channel's own; (2) with `IsGenerating` incorrectly flipped back to `false` while goroutine 2 is still actually generating, the file watcher's debounced sync fires unsuppressed and starts re-posting turns goroutine 2 is *simultaneously* posting itself live - the duplicate messages.

**Fix**: give `State` a per-channel mutex - `chanLocks map[string]*sync.Mutex` guarded by its own small map-mutex, lazily creating an entry per channel ID on first use - exposed as `func (s *State) LockChannel(channelID string) (unlock func())`. Call-site changes:
1. `HandleMessageCreate` performs a pre-lock fast-path existence check, acquires the channel's lock once (`LockChannel`), reads a fresh `GetBinding` snapshot under lock, and holds it via `defer unlock()` for the entire function body - spanning its own binding mutations, the full generation call, and the post-generation bookkeeping that resets `IsGenerating`/`PendingUserHash`. Its own internal `autoFillUnsyncedTurns` call at the top stays as a plain, lock-free helper call, since the caller already holds the channel's lock for the whole duration - `autoFillUnsyncedTurns` itself is refactored to assume its caller already holds the relevant channel's lock, rather than acquiring anything itself, to avoid needing a reentrant mutex.
2. `SyncAgentToChannels` (the watcher-driven path, which is outside `HandleMessageCreate`) acquires the same per-channel lock around its own call to `autoFillUnsyncedTurns` for each bound channel.
3. `handleFillCommand` (`/fill` slash command) acquires the same per-channel lock, refreshes the binding snapshot under lock, checks `IsGenerating`, and delegates to `autoFillUnsyncedTurns(..., limit)` rather than maintaining a duplicate, unguarded turn-diffing implementation.
4. `handleUnbindCommand` (`/unbind` slash command) acquires the same per-channel lock before calling `RemoveBinding`, ensuring it waits for any in-flight message generation to finish and preventing subsequent post-generation `SetBinding` writes from resurrecting an unbound channel.
5. Defensive `AgentID` guards in `HandleMessageCreate` (post-generation and deferred cleanup) verify that the channel's `AgentID` has not diverged before writing sync markers, protecting newly bound agents if `/bind` executes mid-generation before `/bind`'s own locking is completed.
6. No change needed to the lock's actual duration semantics beyond "one critical section at a time per channel" - a second message arriving mid-turn now genuinely blocks (queues) until the first message's entire cycle (generation, posting, bookkeeping) has completed and left the binding in a fully consistent state, rather than racing a stale copy in behind it. This doesn't add any new blocking that wasn't already happening at the wackypub layer anyway - the real session lock (`AcquireSessionLock`) already fully serializes the actual generation calls for one agent; this just makes the Discord-side bookkeeping wait for its turn too, instead of mutating shared state concurrently with it.

Locking is per-*channel*, not per-*agent* - two different channels bound to the same underlying agent still don't contend with each other, since each channel's own Discord-side view (webhook, sync marker, pending-echo tracking) is genuinely independent even when they share one agent's session.

**Why**: Closes the root cause rather than a symptom - the flag *was* already designed to suspend the watcher during generation (`IsGenerating`), but the flag itself had no protection against being clobbered by a second concurrent writer holding a stale copy. A coarse-but-correct per-channel mutex fixes both observed failure modes (the mis-labeled "background" echo, and the duplicate re-posts) at their actual source, without weakening any of the deliberate real-generation concurrency (different channels, different agents) the current design already allows.

## D77: Mid-turn short-circuit no longer forces an immediate compaction pass on the turn it just interrupted

Implemented in `pkg/agent/adk_agent.go` and `pkg/agent/agent_folder.go`.

**The problem**: Live-reported - a long tool-calling turn hit D63/D68's mid-turn context budget check (`adk_agent.go`'s `BeforeModelCallbacks`), which stops the turn early with a synthetic "stopping turn early... send another message to continue" response once accumulated tool context crosses the compaction threshold. `GenerateTurnStream`'s post-turn compaction check (`agent_folder.go`, D68) then sees real usage still over threshold and immediately force-compacts, right at that exact moment. Every tool call from the now-interrupted, still-unresolved task has already streamed into `session.jsonl` live (`AppendEvent` fires per-event during the turn, independent of whether the turn ever reaches a real conclusion) - so `CheckAndCompactSession`'s always-from-the-oldest turn-selection logic, when the current interrupted task itself accounts for a large share of the session's tokens (plausible for exactly the "had full context" case that triggered this), can land its archive boundary inside or at the unresolved task. The compaction directive then dutifully summarizes exactly what it's handed - a task with no outcome yet - producing the observed "just dumped what it was most recently working on" instead of a clean memory amendment. Nothing about the directive prompt is at fault; it's being handed the wrong material at the wrong moment.

**Considered and rejected**: precisely capping the archive boundary to never reach into the current turn's own new turns (tracking `len(turns)` at the start of `GenerateTurnStream`, threading it into `CheckAndCompactSession` as a hard limit). Correct in principle, but adds real complexity (a new parameter threaded through multiple call sites, boundary-invariant interactions with the existing "always end on a model turn" extension logic) for a mechanism the user considers still fundamentally brittle - interrupting a task specifically to force compaction, then immediately trying to compact right at that interruption, is the wrong shape regardless of how precisely the boundary is capped.

**Fix, a middle ground**: keep the mid-turn short-circuit itself exactly as-is (still stops a turn early when tokens exceed threshold mid-loop - this remains the only defense against a single turn's own tool-calling activity hitting a hard context-length API failure before it ever gets a chance to finish). But decouple it from the automatic *forced* compaction pass that currently runs immediately afterward. `TurnUsageTracker` (`pkg/agent/adk_agent.go`) gains a new bool field - `StoppedEarlyForCompaction` - set to `true` at the exact point the mid-turn short-circuit fires, alongside the existing `Reset()` call already zeroing it at the start of every `GenerateTurnStream` invocation. `GenerateTurnStream`'s post-turn compaction check (`agent_folder.go`) skips its forced `CheckAndCompactSession(..., true)` call specifically when `fa.UsageTracker.StoppedEarlyForCompaction` is true - no compaction attempt happens right at the interruption at all. The interrupted turn's own trailing turns simply stay in `session.jsonl` as-is; whether compaction ever runs against them is left entirely to the ordinary, unforced pre-turn check (unchanged, already existed before D68) the next time generation is invoked - the same organic path that handled this before the D68 forced/immediate mechanism was ever added. The `maxToolTurns` cap's own separate short-circuit (a different, pre-existing mechanism for runaway tool loops, not part of the compaction work) is untouched - it can still be followed by a forced post-turn compact if usage happens to be over threshold at that point, since that's not the behavior in question here.

**Why**: Removes the single most aggressive, worst-timed trigger point (compacting immediately at the moment of interruption, guaranteed to hand the directive an unresolved task) without abandoning the safety net the short-circuit exists for, and without introducing new boundary-tracking complexity for a mechanism already agreed to be too brittle to invest further in right now. A proper fix - if one turns out to still be needed after this - is deferred to a TODO rather than attempted here.

## D78: `wackyproc wait` can target a process ID and ignores processes already terminal when the call began

Implemented in `tools/wackyproc/main.go` (`waitCmd`) and `tools/wackyproc/proc/manager.go` (`Wait`, plus the `isTerminal` helper), and documented in that tool's `README.md`. The bundled `skills/wackyproc/SKILL.md` still documents only the bare `wait <seconds>` form - tracked in TODOS.md.

`wackyproc wait [seconds]` blocks until a process that was still running when the call began reaches a terminal state, exiting 0 and printing its ID. Before polling, it snapshots the set of already-terminal process IDs once and never reports a member of that set. With no tracked processes it blocks until the timeout. The timeout defaults to `MaxWaitSeconds`.

`wackyproc wait --for <id> [seconds]` blocks until the named process reaches a terminal state, sharing the same `MaxWaitSeconds` default. Because IDs are drawn from a charset containing every digit (`id.go:13`, `IDLength = 4`), a bare positional argument cannot be resolved between an ID and a timeout, so ID selection is a flag rather than a positional: `wait 1234` means 1234 seconds, `wait --for 1234 30` waits up to 30 seconds for process `1234`. A `--for` target with no corresponding `.proc/<id>` directory fails immediately with the same `process %q not found` error `Get` and `Stop` return (`manager.go:248`, `manager.go:321`), rather than blocking until the timeout. Baseline exclusion does not apply to `--for`: a target already terminal at entry is reported immediately.

`waitCmd` sets `SilenceUsage`, so a timeout prints one diagnostic line instead of the full usage block.

**Why**: the previous contract scanned every tracked process and returned the first terminal one (`manager.go:303-306`), ordered by `StartedAt` ascending (`manager.go:234`), so any terminal entry satisfied the call immediately and permanently, and the `deadline` check, positioned after the scan, was unreachable. Targeting an ID makes the contract match caller intent. The baseline snapshot makes the remaining any-mode correct while keeping the documented `wait [seconds]` form working; it is local to one invocation, so no new on-disk state and no new concurrency surface. Fail-fast on an unknown target matches `Get`/`Stop`, and is the only safe reading — a mistyped ID must not silently become a timeout.

**Rejected:** skipping only consumed records (the `consumed` flag, D79) as the fix, which still returns the oldest matching record rather than the one the caller awaits; and any wall-clock behavior in either mode, since agent turns may be seconds or days apart.

## D79: `wackyproc` disposes of process records by consumption order, never by wall clock

Not yet implemented. Touches `tools/wackyproc/proc/types.go` (new fields, `MaxTerminalEntries`), `tools/wackyproc/proc/manager.go` (`Get`, `List`, `Run`, disposal), `tools/wackyproc/proc/id.go` (sequence lock), and `tools/wackyproc/main.go` (new `prune`/`unconsume` commands).

Nothing removes a process record today. The only `os.RemoveAll` calls in the codebase (`manager.go` 138/155/159/166/181) are spawn-path rollback, so `.proc/` grows without bound and every `wait` poll runs `CheckLiveness` over every entry.

**`consumed` is a flag on the record, not a process status.** `status` stays derived: `CheckLiveness` (`liveness.go:46`) computes `RUNNING`/`COMPLETED`/`FAILED`/`CRASHED` from `pid` and `exit_code` on disk and never persists it. Folding consumption into that enum would force the one function `list` and the `wait` poll depend on to merge a persisted, externally-mutated bit into a freshly derived value, and would lose information: a `CRASHED` process that is read must stay identifiable as crashed, and a `RUNNING` process whose partial output was read must be representable at all.

**`get` marks a record consumed only if the process is already terminal, and never deletes.** Reading a still-running process returns its current output and leaves `consumed` unset, because a peek at partial output is not consumption of the final output. Marking is set-once: `consumed_seq` is assigned only on the unconsumed-to-consumed transition, and later reads of a consumed record are no-ops. `wackyproc unconsume <id>` clears both.

**Ordering is a monotonic sequence, not a timestamp.** `consumed_seq` gives first-consumed ordering and `gen` gives creation order. Both are incremented while holding a single lock, `.proc/.seq.lock`, built from the same `os.Mkdir`/`EEXIST` retry primitive `ClaimUniqueProcessDir` uses for name collisions (`id.go:32-44`) — that primitive is reused, but the lock itself is new code, since no acquire/release construct exists today. A shared unlocked read-increment-write would let two concurrent calls claim one value, destroying the total order the fields exist to provide. `gen` cannot be replaced by `StartedAt`: `StartedAt` is `time.Now().Unix()` (`manager.go:151`), so back-to-back spawns within one second tie, and a tie defeats lowest-`gen` eviction ordering just as an unlocked counter would.

**Disposal happens in `run` and via explicit `prune`, never in a read path.** `MaxTerminalEntries` in `proc/types.go` caps how many terminal records are retained. When terminal records exceed it, `run` disposes consumed terminal records in ascending `gen` order, one at a time, stopping when the count is at or below the cap or no consumed terminal record remains. Unread terminal records are never disposed automatically. When the cap is exceeded and nothing is disposable, `list` writes one warning line to stderr naming the counts and suggesting `prune`. `wackyproc prune` disposes terminal records on demand and reports what it removed. A disposal racing a concurrent reader is benign: `Get`'s `os.Stat` and the per-ID `CheckLiveness` surface the existing `process %q not found` error rather than a partial read or a crash. This is the first case where one command removes a record another may be reading, since every previous `RemoveAll` ran inside the spawning process's own rollback path.

**Why**: consumption is an event, so it yields a total order without a clock, which expiry cannot provide under real agent cadence. Disposal lives in `run` because `run` is the only existing writer to `.proc/`; disposal in `list` or `wait` would make a read path a silent writer. A warning beats silently evicting unread output, because losing an unread crashed process's output to a cap is a worse failure than the disk cost. `MaxTerminalEntries` is a retunable starting default, not a derived figure; it is set below the 300 that caps scratchpad entries (`EvictOldestScratchpad`) because a process record carries captured stdout and stderr and is typically larger than a scratchpad text entry.

**Rejected:** `prune --max-age`, the original design doc's proposal and the shape the open TODO at `TODOS.md:338` still sketches — wall-clock assumptions do not survive agent turns that are seconds or days apart. Also rejected: treating a fully-drained stdout or a zero exit status as consumption, since `tail` drains completely and exits 0 while returning a fraction of its input.
