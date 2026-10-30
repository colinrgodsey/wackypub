# 🏛️ WackyPub Architecture: Agent Session Management

This document details the architecture, directory specs, lifecycle, compaction mechanics, and Google Agent Development Kit (ADK) integration for agent sessions in **WackyPub**.

> This is the deep reference: full schemas, diagrams, and mechanics. For quick orientation when working in this repo, and for the *why* behind specific design choices, see [`.agents/AGENTS.md`](../.agents/AGENTS.md) and [`.agents/DECISIONS.md`](../.agents/DECISIONS.md).

---

## 📑 Table of Contents

1. [Overview](#1-overview)
2. [Workspace & Directory Structure](#2-workspace--directory-structure)
3. [File Specifications](#3-file-specifications)
   - [runtime.json](#runtimejson)
   - [AGENTS.md & Macro Expansion](#agentsmd--macro-expansion)
   - [MEMORY.md](#memorymd)
   - [session.jsonl](#sessionjsonl)
   - [WACKYPUB_ALLOWED_AGENTS](#wackypub_allowed_agents)
   - [tools/ Directory](#tools-directory)
   - [scratchpad/ Directory](#scratchpad-directory)
   - [skills/ Directory](#skills-directory)
   - [hooks/ Directory](#hooks-directory)
4. [Google ADK Integration Layer](#4-google-adk-integration-layer)
5. [Execution Lifecycle](#5-execution-lifecycle)
6. [Hooks](#6-hooks)
7. [Session Compaction Mechanics](#7-session-compaction-mechanics)
8. [Reasoning / Thinking Support](#8-reasoning--thinking-support)
9. [CLI Command Pipeline](#9-cli-command-pipeline)
10. [Session Locking](#10-session-locking)
11. [Programmatic Go SDK API](#11-programmatic-go-sdk-api-pkgagent)

---

## 1. Overview

WackyPub manages agents using a file-system-first architecture. Each agent operates within a dedicated directory located inside a workspace directory (`<ws_dir>`).

```
<ws_dir>/
├── WACKYPUB_ROOT          # Empty marker file designating workspace root
└── <agent_id>/
    ├── runtime.json       # LLM Endpoint, Model & Compaction Settings (or Symlink)
    ├── .env               # Optional Agent Environment Variables for tool execution (D32)
    ├── AGENTS.md          # System Prompt with @<FILE_PATH> Macro Inclusions
    ├── MEMORY.md          # Long-term Compacted Memories
    ├── session.jsonl      # Sequential Turn History Log (JSON Lines)
    ├── session.lock       # PID-based Exclusive Process Lock
    ├── WACKYPUB_ALLOWED_AGENTS # Opt-in allowlist of target agents for cross-agent calls
    ├── scratchpad/        # Persistent session scratchpad entries (1 file per entry, D30)
    ├── tools/             # Discovered executable tool binaries / scripts
    ├── skills/            # Discovered skill folders containing SKILL.md
    └── hooks/             # Optional per-event lifecycle hook scripts (<event>/NN-name)
```

By decoupling runtime configuration, system prompts, memory, turn logs, tools, and cross-agent authorization into discrete files, agent state is human-readable, source-controllable, and easily inspectable.

---

## 2. Workspace & Directory Structure

- **Workspace Root Marker (`WACKYPUB_ROOT`)**: Every valid workspace directory must contain an empty `WACKYPUB_ROOT` marker file directly at its root.
- **Workspace Discovery**:
  - **Unspecified `--ws`**: Walks up directory ancestors from the current working directory (`CWD`) to find the nearest directory containing `WACKYPUB_ROOT`. Errors if no marker file is found.
  - **Explicit `--ws <path>`**: Validates that `<path>` directly contains `WACKYPUB_ROOT`.
- **Agent Directory (`<ws_dir>/<agent_id>/`)**: Contains all runtime configuration, prompt templates, memory, session history, allowlists, scratchpads, and tools for `<agent_id>`.

---

## 3. File Specifications

### `runtime.json`

Specifies the LLM provider configuration and session compaction parameters for the agent.

> 💡 **Symlink Support**: `runtime.json` may be a symbolic link to a shared global configuration file (e.g. `../shared_runtime.json`). The runtime loader evaluates symlinks automatically via `filepath.EvalSymlinks`.

#### Schema
```json
{
  "endpoint": "https://api.openai.com/v1",
  "model": "gpt-4o",
  "apiKey": "sk-...",
  "sessionCompactPct": 50.0,
  "contextWindow": 128000,

  "preserveThinking": false,
  "reasoningEgress": "",
  "reasoningField": "",
  "supportsReasoningDetails": false,
  "extraBody": {}
}
```

#### Fields
| Field | Type | Description |
|---|---|---|
| `provider` | `string` | Model provider selection: `"openai"` (default when `endpoint` is set), `"gemini"` (default when `endpoint` is empty), or `"anthropic"`. |
| `endpoint` | `string` | Base HTTP URL endpoint (e.g. `https://api.openai.com/v1`, `https://api.anthropic.com`, `http://localhost:11434/v1`). |
| `model` | `string` | Target LLM model identifier (e.g., `gpt-4o`, `claude-3-7-sonnet-20250219`, `gemini-2.5-flash`). |
| `apiKey` | `string` | Bearer token / API key for authentication. |
| `anthropicThinkingBudgetTokens` | `int` | Anthropic classic thinking token budget (e.g. `1024`). Must be >= 1024 and less than output token limits. |
| `anthropicThinkingEffort` | `string` | Anthropic reasoning effort (`"low"`, `"medium"`, `"high"`, `"max"`) for adaptive thinking mode. |
| `anthropicThinkingMode` | `string` | Anthropic thinking mode: `"enabled"` (classic budget), `"adaptive"` (effort-based), or empty for auto-detection. |
| `geminiThinkingBudget` | `int` | Gemini token budget for reasoning (e.g. `2048`). |
| `geminiThinkingLevel` | `string` | Gemini reasoning level (`"low"`, `"medium"`, `"high"`, `"minimal"`). |
| `geminiIncludeThoughts` | `bool` | Whether thoughts are included in output parts for native Gemini models (default: `true`). |
| `reasoningEffort` | `string` | OpenAI / OpenRouter reasoning effort (`"low"`, `"medium"`, `"high"`). |
| `contextWindow` | `int` | Optional maximum token threshold triggering auto-compaction. `0` disables auto-compaction. |
| `timeoutSeconds` | `int` | HTTP client timeout in seconds for API calls to the LLM backend (default: `900` seconds / 15 minutes). |
| `turnDeadlineSeconds` | `int` | Turn-watchdog bound, in seconds, for this runtime level's stream: if it doesn't terminate in time, the level fails with a named terminal error naming the backend (session lock released, no hang). Defaults to `3600` (1h) when unset or `<= 0`. One fixed default for every endpoint - set this per fallback level if a particular backend needs a lower (or higher) bound. |
| `preserveThinking` | `bool` | Set for backends that resend and bill for prior reasoning text on every turn (e.g. Kimi K2 Thinking, DeepSeek V4 thinking mode). When true, the compaction token estimate counts `Thought`-marked part text, since it's actually replayed to the model on every subsequent request. Leave `false` for backends that drop/ignore replayed reasoning by default (e.g. Qwen3). See [§7](#7-reasoning--thinking-support). |
| `reasoningEgress` | `string` | Wire shape used to send reasoning back as history: `""`/`"native"` (own field, required by DeepSeek V4 thinking mode & Kimi K2 Thinking), `"think_tags"` (folded into `content` as a `<think>` block, for backends that 400 on an unknown field), or `"omit"` (send no reasoning at all). |
| `reasoningField` | `string` | Name of the provider's plain-text reasoning field, read on ingest and written on egress. Empty means `"reasoning_content"`. OpenRouter uses `"reasoning"` instead. |
| `supportsReasoningDetails` | `bool` | Allows OpenRouter's structured `reasoning_details` block array (including encrypted/signed reasoning) to be replayed as history. Only safe with a **pinned** `model` — see [§7](#7-reasoning--thinking-support) for why `"auto"` routing breaks this. |
| `extraBody` | `map[string]any` | Provider-specific fields merged into the root of every request body, for extensions Chat Completions doesn't define — e.g. `{"reasoning": {"effort": "high"}}` to request extended thinking from OpenRouter-routed models that don't emit it by default. |
| `extraHeaders` | `map[string]string` | Overrides the default identifying HTTP headers sent on every request (`X-Title: WackyPub`, `HTTP-Referer: https://github.com/colinrgodsey/wackypub`, plus `X-OpenRouter-Title`/`X-OpenRouter-Categories` when the endpoint is detected as OpenRouter — together what stops a client from showing up as "unknown" on OpenRouter's dashboard/rankings). A key present here replaces the default of the same name; useful for anyone embedding wackypub under their own product name. `User-Agent` is not overridable this way — see DECISIONS.md D43. |

---

### `AGENTS.md` & Macro Expansion

`AGENTS.md` defines the base system prompt instructions for the agent.

#### `@<FILE_PATH>` Macro Expansion
To promote modular prompts, `AGENTS.md` supports embedding secondary files using the `@<FILE_PATH>` macro syntax:

```markdown
# Role: Archmage Ignis
You are Ignis, an ancient wizard.

@rules/spells.md
@prompts/personality.txt
```

#### Macro Resolution Rules:
1. **Boundary Matching**: An `@` include directive must be preceded by a boundary delimiter — start of line (`^`), whitespace, or enclosing punctuation (`(`, `[`, `{`, `<`, `"`, `'`, or `` ` ``) — and must end with an alphanumeric character or path separator (`[a-zA-Z0-9_\-/]`). Text without leading boundaries (such as email addresses like `user@agentmail.to` or `crgodsey@gmail.com`) is never matched or mangled. Trailing sentence punctuation (such as `.` or `,`) and closing brackets/parentheses are preserved.
2. **Workspace Containment**: File paths after `@` are resolved relative to the agent's directory (`<ws_dir>/<agent_id>/`) and must remain contained within the workspace root (`wsDir`, identified by `WACKYPUB_ROOT` or defaulting to `filepath.Dir(agentDir)`). Path traversal attempts escaping the workspace root (e.g. `@../../../etc/passwd`) are rejected and left literal without reading. Cross-agent or shared includes staying within the workspace root (e.g. `@../shared/rules.md`) are permitted. Symlinks are evaluated and verified to remain within the workspace root.
3. **Existence Gating (D90 Parity)**: Inclusion only fires if the resolved file actually exists and is not a directory. Missing files, directories, and social mentions (e.g. `@DranboF`, `@here`) pass through verbatim as literal text without generating error comments.
4. **Escaping**: Prefixing with a backslash or double-at (`\@path` or `@@path`) bypasses expansion immediately and emits `@path` literally, stripping the escape marker.
5. **Stack-Scoped Cycles & Recursion**: Macro expansion works recursively (files included via `@` can themselves contain `@` macros). Inclusions are tracked on an active call stack: repeated non-circular inclusions (e.g. `@rules.md ... @rules.md` or diamond includes) expand cleanly, while true circular import cycles (e.g., File A imports File B which imports File A) are detected while on the stack and omitted safely with `<!-- Circular macro import omitted: <path> -->`.
6. **Depth Limit**: Maximum expansion recursion depth is capped at 10 to prevent runaway recursion or stack overflow, bubbling an error up to the caller if exceeded.

---

### `MEMORY.md`

`MEMORY.md` contains long-term, compacted memory facts and relationship state for the agent.

#### Key Mechanics:
- If `MEMORY.md` does not exist in `<agent_id>/`, it is treated as empty (`""`).
- **Hardcoded Context Position (User Turn 1)**: There is no separate "system" role message. Instead, the fully rendered `AGENTS.md` system prompt and the current contents of `MEMORY.md` are combined into a **single first user turn**, sent as plain user-role text (not a `system`/`developer` role message) for broad compatibility across OpenAI-compatible backends — some local model chat templates don't handle a `system` turn correctly, so folding it into the first user message is the one behavior that works everywhere:
  ```
  <rendered AGENTS.md system prompt>

  <PERSISTENT_MEMORY>
  <contents of MEMORY.md>
  </PERSISTENT_MEMORY>
  ```
- **Prefix Consistency**: Both normal generation (`GenerateTurn`) and compaction (`CheckAndCompactSession`) build this exact same first-turn text, so the conversation prefix stays identical between the two, maximizing LLM prompt cache performance.

---

### `session.jsonl`

`session.jsonl` stores the conversation turn history log in JSON Lines format (one JSON object per line). Each line is a serialized [`genai.Content`](https://pkg.go.dev/google.golang.org/genai#Content) — not a custom struct — so it natively supports multi-part messages, including reasoning/thinking, images, and other multimedia, with no lossy round-trip through a text-only format.

#### Schema per line
```json
{"role": "user", "parts": [{"text": "Hello, traveler!"}]}
{"role": "model", "parts": [{"text": "Let me think about how to greet them...", "thought": true}, {"text": "Greetings! What brings you to my tavern?"}]}
```

#### Rules:
- Roles are `"user"` and `"model"` (the ADK/`genai` convention), not `"user"`/`"assistant"`.
- There is no `timestamp` field — `genai.Content` doesn't have one, and none is added.
- The system prompt is **never stored** inside `session.jsonl` — it's re-rendered from `AGENTS.md` and injected fresh into the first turn on every generation (see [MEMORY.md](#memorymd) above).
- A `Part` with `"thought": true` holds reasoning/chain-of-thought text captured from the model, kept separate from the final-answer part(s). See [§7](#7-reasoning--thinking-support).
- A part can also carry a `partMetadata` object holding an opaque, provider-specific block (e.g. OpenRouter's `reasoning_details`, including encrypted/signed reasoning) — see [§7](#7-reasoning--thinking-support).

---

### `WACKYPUB_ALLOWED_AGENTS`

`WACKYPUB_ALLOWED_AGENTS` is an opt-in plain-text allowlist file in an agent's directory (`<agent_id>/WACKYPUB_ALLOWED_AGENTS`) listing target agent IDs that the current agent is authorized to invoke via cross-agent calls (e.g. messaging tools).

#### Key Rules:
- Each non-empty line (ignoring `#` comments) specifies one allowed target agent ID.
- **Default Deny-All**: If `WACKYPUB_ALLOWED_AGENTS` is absent from an agent's directory, all cross-agent target invocations and private content reads (`read-session`, `read-memory`, `render-prompt`, scratchpads) from that agent directory are denied (D16, D60). Diagnostic inspection (`wackypub workspace` / `InspectAgent`) remains exempt as it only exposes structural metadata.
- **Deadlock Safety & A2A Metadata (`AGENT2AGENT` / `WACKYPUB_CALL_CHAIN`)**: Cross-agent calls propagate Agent2Agent (A2A) protocol context via the `AGENT2AGENT` environment variable (carrying `caller_id`, `call_chain`, `trace_id`, and `metadata` as dense JSON - D33), falling back to `WACKYPUB_CALL_CHAIN` CSV strings for legacy callers. Re-targeting an agent already in `call_chain` is rejected immediately on mutating operations to prevent deadlock cycles (D16, D59, D60).

---

### `tools/` Directory

`<agent_id>/tools/` contains executable binaries or scripts discovered and registered as tools during generation turns (see DECISIONS.md D14, D17).

#### Discovery & Invocation:
- Recursively walked for executable files (`mode & 0111 != 0`).
- Discovered executables under `tools/` are dispatched through a single generic `run_command` tool (`genai.FunctionDeclaration`), rather than N individual declarations.
- `run_command`'s description is constructed dynamically at agent-load time, embedding an alphabetically sorted list of available commands and general execution guidance (working directory, argv conventions, scratchpad usage, and `--help` exploration).
- `run_command` accepts:
  - `command`: Name of the command executable to run from `tools/`.
  - `args`: Array of positional CLI command arguments (supports inline `<SCRATCHPAD_DATA id="X" />` macro expansion).
  - `env`: Key-value map of environment variables (not macro-expanded).
  - `stdin`: Optional stdin template string (supports inline `<SCRATCHPAD_DATA id="X" />` macro expansion).
- Executed in a multi-turn tool loop within `GenerateTurn` up to `--max-tool-turns` (default `10`).

#### Optional: Sandboxing `bash` in a Container

`tools/bash` doesn't have to be the real host shell. `scripts/sandbox/` ships a reference
setup that runs an agent's `bash` tool inside a persistent, per-agent Docker container
instead: `agent-sandbox.sh <ws_dir>/<agent_id>` starts one detached container per agent
workspace (`--restart unless-stopped`, survives host reboots), and `tools/bash` becomes a
symlink to `scripts/sandbox/bashc` - a thin `docker exec` wrapper whose only job is deriving
*which* container to exec into from its own CWD (wackypub always runs tools from the agent's
own directory, so CWD is a reliable, env-free way to recover agent identity).

Three host directories are bind-mounted into the container at **identical absolute paths**
on both sides (no path translation): the agent's own `container-ws/` scratch space, and -
same-path, so worktree `.git` pointers keep resolving - the workspace's shared `projects/`
and `worktrees/` directories, if present. Everything else on the host, including other
agents' folders, is invisible from inside the container. See `scripts/sandbox/README.md`
for the full setup sequence and `skills/bashc/SKILL.md` for the agent-facing explanation
(etiquette: own worktree only, `projects/` read-mostly, coordinator-side pushes - agents can
make local commits in their own worktree since the same-path mount keeps the `.git` link
intact, but credentials and `git push` stay host-side).

**Known gap, not solved by this design**: a command that `run_command` defers to an
out-of-process background dispatcher (e.g. a `wackyproc`-style job runner) runs on the bare
host if the dispatcher re-execs the command directly rather than re-entering through
`tools/bash` - it never passes through `bashc`, so it escapes the sandbox entirely. There is
no enforcement point here that catches that case; it's a property of how a given background
dispatcher is wired, not something `bashc`/`agent-sandbox.sh` can detect or prevent.

---

### `scratchpad/` Directory

`scratchpad/` is a persistent directory (`<agent_id>/scratchpad/`) storing one raw text file per live entry (`<id>-<created_by>.txt`) with collision-safe 4-character IDs and mtime-based eviction at a capacity of 300 entries (see DECISIONS.md D18, D30).

#### Built-in Tools & I/O Integration:
- **`create_scratchpad(text: string)`**: Stores `text` under a freshly generated 4-character ID (`[0-9a-z]`), returning `{id, size}`. Automatically evicts the entry with the oldest file mtime when live entries exceed cap (300). Atomic and collision-safe across separate OS processes via `O_CREATE|O_EXCL`.
- **`get_scratchpad(id: string, skip_lines?: int, num_lines?: int)`**: Retrieves stored text by ID, optionally paginated by line range.
- **`list_scratchpads()`**: Lists metadata for all currently-live scratchpad entries (id, size, lines, created_by) ordered by file mtime ascending, and reports current capacity usage (D39).
- **`search_scratchpad(id: string, query: string, case_sensitive?: bool, regex?: bool, max_results?: int)`**: Searches a specific scratchpad entry by ID for matching lines (see DECISIONS.md D25). Returns 1-indexed line numbers, precomputed `skip_lines` for `get_scratchpad` pagination, and truncated line text (~200 chars), with total match counts reported separately.
- **Inline Macro Expansion**: Positional arguments and `stdin` template string in `run_command` expand `<SCRATCHPAD_DATA id="X" skip_lines="N" num_lines="M" json_escape="true" />` server-side before process execution. When `json_escape="true"` is set, content is substituted as JSON-escaped text (quotes, newlines, and backslashes escaped per RFC 8259) without adding surrounding quotes (D37). Arguments exceeding 500,000 bytes after expansion fail fast.
- **`diff_scratchpad(before_id: string, after_id: string)`**: Renders a unified diff between two text entries as an agent-callable tool, returning `{diff, identical}`. Identical entries yield an empty diff with `identical: true`; an unknown or malformed ID is an error rather than an empty diff, so a typo cannot read as "nothing changed". Unlike the CLI form, the patch is returned into context directly, so slice or shell out for a diff expected to be huge.
- **`wackypub agent <agent_id> scratchpad diff <before_id> <after_id>`**: Renders a unified diff between two text entries, backed by the SDK `DiffScratchpadEntries(wsDir, agentID, beforeID, afterID)` and its authorized method `(*AgentSDK).DiffScratchpadEntries(ctx, *agentv1.DiffScratchpadEntriesRequest)` (request fields `agent_id`, `before_entry_id`, `after_entry_id` and an optional `workspace_dir`; response field `diff`). Identical entries print nothing and exit 0; an unknown or malformed entry ID exits 2 without writing; binary entries are refused. A diff larger than the output threshold stays ordinary stdout, so the Automatic Output Redirection below is what turns it into a new entry.
- **Automatic Output Redirection**: Subprocess stdout/stderr exceeding 4,000 bytes are automatically captured into fresh scratchpad entries, returning structured tags like `<STDOUT><SCRATCHPAD_DATA id="k3p1" size="5001" lines="42" /></STDOUT>` (D39).

---

### `skills/` Directory

`<agent_id>/skills/` contains pre-written, distilled skill guidance folders holding `SKILL.md` files with YAML frontmatter (see DECISIONS.md D20).

#### Discovery & Loading Modes:
- Recursively walked for folders containing `SKILL.md`.
- `SKILL.md` frontmatter fields:
  - `name`: Unique skill identifier (defaults to folder name).
  - `description`: Short description of the skill.
  - `always_load`: Optional boolean (`true` or `false`).
- **On-demand Skills** (`always_load: false`): Registered with the `load_skill(name: string)` tool. Available skill names and descriptions are embedded in `load_skill`'s dynamically built description (alphabetically sorted). Invoking `load_skill` returns the skill body as a `FunctionResponse` turn.
- **Always-loaded Skills** (`always_load: true`): Excluded from `load_skill`. Concatenated onto the rendered system prompt (`Instruction`), sorted alphabetically by skill name, wrapped in `<AUTOLOADED_SKILLS><SKILL name="...">...</SKILL>...</AUTOLOADED_SKILLS>`.

---

### `hooks/` Directory

`<agent_id>/hooks/<event>/` contains optional executable lifecycle hook scripts (`NN-name`) discovered and executed in ascending numeric order for each lifecycle event (`on-user-message`, `pre-compact`, `post-compact`, `compact-failed`, `pre-run-command`, `end-of-turn`, `turn-completed`, `turn-failed`). For the complete event catalog, payload schemas, `HookOutput` / `HookAction` contract, and precedence rules, see [§6 Hooks](#6-hooks).

---

## 4. Google ADK Integration Layer

WackyPub integrates with **Google Agent Development Kit v2** (`google.golang.org/adk/v2`) for core types (`model.LLM`, `model.LLMRequest`/`LLMResponse`, `agent.Agent`, `session.Event`, `tool.Tool`), and executes generation turns directly via ADK's `runner.Runner` powered by a custom file-backed `session.Service` (`FileSessionService` in `pkg/agent/file_session_service.go`).

```
┌─────────────────────────────────────────────────────────────┐
│                      WackyPub CLI                         │
└──────────────────────────────┬──────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│         FolderAgent.GenerateTurn (pkg/agent/agent_folder.go) │
│    runs ADK runner.Runner with custom FileSessionService    │
└──────────────────────────────┬──────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────┐
│              OpenAI-Compatible ADK Model Adapter             │
│   pkg/agent/openai_model.go -> adk-utils-go's genai/openai   │
│      (official github.com/openai/openai-go/v3 SDK underneath) │
└──────────────────────────────┬──────────────────────────────┘
                               │ HTTP POST /chat/completions
                               ▼
┌─────────────────────────────────────────────────────────────┐
│               OpenAI-Compatible LLM Provider                │
│  (OpenAI, OpenRouter, DeepSeek, Kimi, vLLM, Ollama, llama.cpp) │
└─────────────────────────────────────────────────────────────┘
```

Agent generation turns are executed using ADK's `runner.Runner` pipeline with `FileSessionService` handling reading and appending session history directly to `<agent_id>/session.jsonl`.

### OpenAI Adapter: Upstream `github.com/achetronic/adk-utils-go`

`pkg/agent/openai_model.go`'s `NewOpenAIModel` is a thin wrapper around [`achetronic/adk-utils-go`](https://github.com/achetronic/adk-utils-go)'s `genai/openai` package, which itself wraps the official `openai-go/v3` SDK. `go.mod` tracks upstream `achetronic/adk-utils-go` at commit `1f0a646bcdfd07ad5f09363d6cbca3b5c58bd764` (which incorporated the `Dialect` interface for OpenRouter and reasoning handling), eliminating the need for a fork replace directive.

### ADK `llmagent.Config` Mapping

1. **`Name`**: Set directly to `agentID` (which is already unique within the workspace directory).
2. **`Instruction`**: Set to the fully rendered system prompt string loaded from `AGENTS.md` (after processing `@<FILE_PATH>` macro inclusions).
3. **`Model`**: The configured `model.LLM` instance (OpenAI-compatible adapter or native Gemini).

```go
ag, err := llmagent.New(llmagent.Config{
    Name:        agentID,
    Description: fmt.Sprintf("Agent %s", agentID),
    Instruction: renderedPrompt, // Fully expanded AGENTS.md
    Model:       llmModel,
})
```

---

## 5. Execution Lifecycle

When running `wackypub agent <agent_id> generate`:

```mermaid
sequenceDiagram
    autonumber
    participant CLI as WackyPub CLI
    participant Agent as FolderAgent Manager
    participant Compactor as Compaction Engine
    participant LLM as OpenAI-Compatible Model (ADK)
    participant Disk as Filesystem (<ws_dir>/<agent_id>)

    CLI->>Agent: LoadFolderAgent(wsDir, agentID)
    Agent->>Disk: Read runtime.json, AGENTS.md, MEMORY.md
    Disk-->>Agent: Raw files & configs
    Agent->>Agent: Expand @<FILE_PATH> macros in AGENTS.md

    CLI->>Agent: GenerateTurn(ctx)
    Agent->>Compactor: CheckAndCompactSession(contextWindow)
    alt Token Count >= contextWindow
        Compactor->>LLM: Generate Memory Summary
        LLM-->>Compactor: Updated Memory Text
        Compactor->>Disk: Overwrite MEMORY.md & Truncate session.jsonl
    end

    Agent->>Disk: Read updated MEMORY.md & session.jsonl
    Disk-->>Agent: Memory content & turn history

    Agent->>LLM: Issue LLMRequest (SystemPrompt+MemoryTurn combined as User Turn 1, then SessionTurns)
    LLM-->>Agent: Generated model Content (final answer, plus a Thought part if the model reasoned)

    Agent->>Disk: Append full model Content to session.jsonl (strips reasoning_details block metadata if supportsReasoningDetails is false)
    Agent->>CLI: Return final-answer text (Thought parts excluded)
    CLI->>stdout: Print generated response
```

---

<a id="hooks"></a>

## 6. Hooks

WackyPub supports per-agent, filesystem-discovered lifecycle hooks under `<ws_dir>/<agent_id>/hooks/<event>/`.

> **Strictly additive by design**: When an agent has no hooks registered for an event (or `WACKYPUB_HOOKS=0` is set in the environment), `wackypub` returns immediately before marshaling payloads or allocating subprocess state — runtime behavior is byte-for-byte unchanged. Any hook failure (missing directory, non-zero exit status, timeout, empty stdout, or malformed JSON) logs a warning to `stderr` and degrades to normal execution without wedging the turn or losing a command.

### Conventions & Execution Chain (`DiscoverHooks` / `RunHookChain`)

- **Directory layout**: `<ws_dir>/<agent_id>/hooks/<event>/<NN-name>` (inspectable read-only across the workspace via `wackypub agent hooks <agent_id>`).
- **Discovery**: `DiscoverHooks` selects only regular executable files (`mode & 0111 != 0`) directly inside `<agent_id>/hooks/<event>/`. Subdirectories and non-executable files are ignored.
- **Numeric ordering**: Scripts sort in ascending numeric order by leading digits (`00-first` before `10-second` before `100-third`), with lexicographical tie-breaking on the filename. Scripts with leading digits sort before scripts without leading digits.
- **Process isolation & timeout**: Each script executes with `CWD = <ws_dir>/<agent_id>` in its own process group (`Setpgid: true`). If a script exceeds the per-hook timeout (`DefaultHookTimeoutSeconds = 5`, configurable via `WACKYPUB_HOOK_TIMEOUT_SECONDS`), its entire process group (`-pid`) receives `SIGKILL`, a warning is recorded, and the chain continues.
- **Stdin / Stdout JSON wire contract**:
  - Each script receives the current text or JSON-encoded event payload on `stdin`.
  - Each script must print a single JSON object (`HookOutput`) on `stdout` (use `{}` for a no-op decline):
    ```json
    {
      "text": "optional replacement stdin text for the next hook in the chain",
      "env": {
        "SET_VAR": "value",
        "UNSET_VAR": null
      },
      "action": {
        "type": "short_circuit | continue",
        "output": "optional replacement stdout or continuation message",
        "stderr": "optional replacement stderr (short_circuit only)",
        "reason": "optional audit/diagnostic reason"
      }
    }
    ```
  - **Environment chaining**: Non-null entries in `env` set or override variables in the environment passed to subsequent hooks in the chain (and in `HookChainResult.MutatedEnv`); `null` values delete the variable from both.

### Unified `HookAction` Contract & Honored Action Sets

Active lifecycle hooks (`pre-run-command` and `end-of-turn`) share a single control-flow extension on `HookOutput`: the `action` field (`HookAction`).

- **Recognized `action.type` values**:
  - `"short_circuit"` (aliases `"short-circuit"` and `"replace"` normalize to `"short_circuit"`): skip executing the target command and substitute `action.output` (and optional `action.stderr`) as a normal successful tool response.
  - `"continue"`: request an automatic continuation turn at the end of the current turn, bounded by D88's `MaxAutoContinuations` budget.
- **Per-event honored action sets**: `RunHookChain` enforces each event's honored action set (`defaultAllowedActionsForEvent(event)`, overridable programmatically via `HookOptions.AllowedActions`):
  - Any unrecognized `action.type` emits a warning and is ignored.
  - Any recognized `action.type` that is **not** in the event's honored set (for example, `"short_circuit"` emitted by an `end-of-turn` or `on-user-message` hook, or `"continue"` emitted by a `pre-run-command` hook) emits a warning (`action type "<type>" which is not honored by event "<event>"`), is ignored, and **never** claims `HookChainResult.Action` or truncates the chain.
  - An empty action object (`{"action": {}}`) is ignored without warning.

### Event Catalog

| Event | Fire Point | Stdin Payload | Honored `HookOutput` Fields | Chain & Precedence Rule |
|---|---|---|---|---|
| `on-user-message` | `AddUserTurn` / `AddAndGenerateTurn`, before persisting the user turn | Raw user message text | `text` (rewrites user message), `env` (stored in-memory via `setLastHookEnv` / `getLastHookEnv` as a one-shot hand-off on the `AgentSDK` instance for the next generation turn) | Sequential pipeline; each script receives prior `text` and `env`. Honored `action` set: none |
| `pre-compact` | `CheckAndCompactSession`, after committing to compact, before compaction LLM runs | `PreCompactPayload` JSON (`agent_id`, `session_path`, `turn_count`, `token_estimate`, `trigger`: `"auto"` \| `"forced"`) | Observe-only (none applied) | Synchronously runs all scripts in order. Honored `action` set: none |
| `post-compact` | Async goroutine after compaction completes | `PostCompactPayload` JSON (`agent_id`, `session_path`, `turns_archived`, `tokens_before`, `tokens_after`, `compaction_model`, `tool_denials`) | Observe-only (none applied) | Best-effort async goroutine; runs all scripts in order. Honored `action` set: none |
| `compact-failed` | Synchronously when `CheckAndCompactSession` errors after reading session turns | `CompactFailedPayload` JSON (`agent_id`, `session_path`, `stage`, `error`) | Observe-only (none applied) | Synchronously runs all scripts in order. Honored `action` set: none |
| `pre-run-command` | `executeTool`, after the pre-dispatch workspace commit and before `cmd.Run()` | `PreRunCommandPayload` JSON (`agent_id`, `command`, `command_path`, `args`, `cwd`, `env`) | `env` (merged into tool's `childEnv`), `action` (`type: "short_circuit"`) | **First honored `short_circuit` wins and terminates the chain** |
| `end-of-turn` | `GenerateTurnStream` (and `GenerateTurn`, which drains it) when a turn iteration ends in `handleContinuationOrCompaction` | `EndOfTurnPayload` JSON (`agent_id`, `session_path`, `stop_reason`, `duration_ms`, `continuation_count`, `max_continuations`, `can_continue`) | `action` (`type: "continue"`, honored only when `can_continue: true`) | **All scripts run; any-wins** (multiple `continue` actions merge `reason` and `output`) |
| `turn-completed` | Single defer at the top of `runTurnWithRuntimeFallback` (`pkg/agent/sdk.go`), once per attempted turn, after the final session write and while the session lock is still held | `TurnCompletionPayload` JSON (`agent_id`, `agent_dir`, `event`, `reason`, `duration_ms`, `backend`) | Observe-only (none applied) | All scripts run in ascending order; no `action` is honored, so the chain never truncates |
| `turn-failed` | Same single defer, when the turn ended by surfacing an error | `TurnCompletionPayload` JSON (as above, plus `error`) | Observe-only (none applied) | Same; exactly one of `turn-completed` or `turn-failed` fires per turn |

### `pre-run-command`

Fires synchronously inside `executeTool` (`pkg/agent/agent_folder.go`) immediately before spawning an executable discovered under `<agent_id>/tools/`. Built-in in-process tools (`load_skill`, `create_scratchpad`, `get_scratchpad`, `list_scratchpads`, `search_scratchpad`, `diff_scratchpad`) do not spawn subprocesses and do not fire `pre-run-command`.

#### Stdin Payload (`PreRunCommandPayload`)
```json
{
  "agent_id": "coordinator",
  "command": "my-tool",
  "command_path": "/ws/coordinator/tools/my-tool",
  "args": ["--flag", "value"],
  "cwd": "/ws/coordinator",
  "env": {
    "PATH": "/usr/bin:/bin"
  }
}
```
The hook chain's starting process environment (`HookOptions.InitialEnv`) is seeded with the tool's resolved `cmd.Env` (base environment + `.env` + model-supplied `args.Env` + harness-owned variables).

#### Output Semantics & Precedence
- **Decline (run inline unchanged)**: Print `{}` on stdout and exit `0`.
- **Environment mutation**: Return `{"env": {"EXTRA_FLAG": "1"}}` without an `action`. `MutatedEnv` is merged into the tool subprocess environment via `childEnv` (harness-owned variables such as `AGENT2AGENT` still take precedence and cannot be spoofed).
- **Short-circuit replacement**: Return an honored `short_circuit` action:
  ```json
  {
    "action": {
      "type": "short_circuit",
      "reason": "custom-intercept",
      "output": "Replacement stdout shown to the model",
      "stderr": "Optional replacement stderr"
    }
  }
  ```
  - The target binary is **not** executed.
  - When `action.reason` is non-empty, a best-effort workspace git commit `tool call (<command>: short-circuited: <reason>)` is recorded for causal auditability.
  - `action.output` and `action.stderr` are formatted through `buildOutputBlocks` (`<STDOUT>` / `<STDERR>` tags, with automatic scratchpad redirection above 4,000 bytes or on binary data) and returned to the model as a **normal successful tool response (`err == nil`)**, so the model continues its turn with the substituted output rather than retrying a failed command.
  - **First-wins chain termination**: Short-circuiting is an exclusive claim. The first hook in numeric order to emit an honored `short_circuit` action claims the call and breaks the chain immediately so subsequent hooks cannot double-dispatch the same command.

### `end-of-turn`

Fires when a turn iteration reaches its end inside `handleContinuationOrCompaction` in `GenerateTurnStream` (which non-streaming `GenerateTurn` also drains).

#### Stdin Payload (`EndOfTurnPayload`)
```json
{
  "agent_id": "coordinator",
  "session_path": "/ws/coordinator/session.jsonl",
  "stop_reason": "completed",
  "duration_ms": 1420,
  "continuation_count": 0,
  "max_continuations": 4,
  "can_continue": true
}
```

| Field | Type | Description |
|---|---|---|
| `agent_id` | `string` | ID of the agent whose turn just ended. |
| `session_path` | `string` | Absolute path to `<agent_id>/session.jsonl`. |
| `stop_reason` | `string` | One of the five `TurnStopReason` constants (`pkg/agent/hooks.go`): `"completed"` (`TurnStopCompleted`), `"max_tool_turns"` (`TurnStopMaxToolTurns`), `"failure_breaker"` (`TurnStopFailureBreaker`), `"compacted_bail"` (`TurnStopCompactedBail`), or `"deferred_image"` (`TurnStopDeferredImage`). |
| `duration_ms` | `int64` | Elapsed wall-clock duration of the turn iteration in milliseconds. |
| `continuation_count` | `int` | Number of general D88 auto-continuations already consumed in this stream call. |
| `max_continuations` | `int` | Effective D88 general `MaxAutoContinuations` ceiling for this turn (`DefaultMaxAutoContinuations = 4` default, `DefaultMaxAutoContinuationsA2A = 2` when `A2AMeta` / `AGENT2AGENT` is set, `0` when auto-continuation is disabled, or `runtime.json`'s `maxAutoContinuations`). |
| `can_continue` | `bool` | Whether `wackypub` will honor a `continue` action returned from this invocation. |

#### The `can_continue` Contract (Notify-Only vs. Continuation-Capable Paths)
`end-of-turn` fires on **all** terminal turn paths, but only the normal turn-completion path in `GenerateTurnStream` (when auto-continuation is enabled, `continuation_count < max_continuations`, and neither mid-turn compaction bail nor deferred-image injection has already claimed the continuation slot) sets `"can_continue": true`. All other fire sites set `"can_continue": false` and discard any `continue` action as notify-only.

> **Important for stateful hooks**: A hook that performs a one-time side effect in exchange for a continuation turn (such as marking an external event or background job as delivered) **must** check `can_continue == true` before committing that state change. Otherwise the side effect will be consumed on a notify-only path whose continuation request is dropped.

#### Requesting a Continuation & D88 Budget Bound
- **Decline (let turn end)**: Print `{}` on stdout and exit `0`.
- **Request continuation**: When `can_continue` is `true`, return:
  ```json
  {
    "action": {
      "type": "continue",
      "reason": "pending-work",
      "output": "Context explaining why the turn is continuing."
    }
  }
  ```
- **Multi-hook any-wins aggregation**: All `end-of-turn` hooks run in ascending numeric order (`continue` does not truncate the chain). If one or more hooks emit `type: "continue"`, a continuation turn is requested:
  - Non-empty `reason` fields are joined in execution order with `"; "` (defaulting to `"end-of-turn-hook"` when all are empty).
  - Non-empty `output` fields are joined in execution order with `"\n"` (defaulting to `"Continuation requested by end-of-turn hook (<reason>)."` when `reason` was provided, or `"Continuation requested by end-of-turn hook."`).
- **Sentinel persistence**: The harness appends a user turn `<CONTINUATION reason="<reason>"><output></CONTINUATION>` to `session.jsonl` with workspace git commit label `user (end-of-turn continuation)` and starts the next turn iteration under the same session lock.
- **Hard D88 `MaxAutoContinuations` bound**: Hook-requested continuations draw from D88's existing general `maxAutoContinuations` budget alongside compaction-bail continuations (never a separate or unbounded loop). Even if a broken hook ignores `can_continue: false` and emits `"continue"` on every invocation, `handleContinuationOrCompaction` enforces `continuationCount < maxContinuations` and terminates the stream once the budget is reached.

### `turn-completed` and `turn-failed`

The post-everything turn events: they fire once a turn is terminal, which makes them the only safe place to delete or archive session state. `end-of-turn` fires while the continuation decision is still open, so work done there can be followed by a continuation turn that writes into what it just removed.

- **Fire point**: one `defer` at the top of `runTurnWithRuntimeFallback` (`pkg/agent/sdk.go`). That frame returns before either caller reaches its deferred session-lock release, so every write for the turn has landed (including post-turn compaction), the lock is still held exclusively, and no continuation can follow because the continuation loop lives inside the iterator that function consumes.
- **Exactly once per turn, not per level**: a turn that descends the runtime fallback chain still reports one terminal state, and `backend` names the endpoint/model that served it.
- **Not fired at all** for aside turns (`asideInternal` drives no fallback walk) or for turn-setup failures where no stream was attempted, such as a fallback backend that will not load or a chain in which every backend was skipped as failed-until-reset.
- **`reason` values**: `turn-completed` carries `completed` or `budget_exhausted` (the turn stopped because the D88 auto-continuation budget was spent, which surfaces no error and is therefore not a failure); `turn-failed` carries `stream_error`, `watchdog`, `consumer_stopped` or `cancelled`.
- **Observe-only**: the honored `action` set is empty. Any `text`, `env` or `action` a script returns is discarded and reported as a warning, and a broken script can only ever warn.
- **Timeout and context**: runs synchronously on a context detached from the turn's with the normal hook timeout, so a slow script taxes teardown by at most that timeout and long work should detach inside the script. Because it runs inside the session lock, a hook here must not call `wackypub` CLI verbs that take that lock, which would wait on its own turn.
- **Example**: `examples/hooks/turn-completed/erase-context` implements the always-fresh agent pattern. Installing that hook is itself the opt-in: there is deliberately no config gate, because placing the file in an agent's hook directory already states the intent.

---

## 7. Session Compaction Mechanics

Compaction prevents conversation history from exceeding LLM context boundaries while preserving prefix caching.

### Normal Session Context Layout
1. **User Turn 1**: Fully rendered `AGENTS.md` system prompt, followed by `<PERSISTENT_MEMORY>\n<contents of MEMORY.md>\n</PERSISTENT_MEMORY>` — combined into one plain user-role turn (no `system`-role message is sent at all; see [MEMORY.md](#memorymd)).
2. **Session Turns**: All turns from `session.jsonl` (`user` / `model`).

### Compaction Session Context Layout
Compaction is triggered when session token count reaches `(100 - compact-overhead-pct)%` of `contextWindow` (default 20% overhead margin, configurable in `COMPACT.md`):
- **Pre-turn**: Evaluates `EstimateTokens` over `session.jsonl` turns before generation begins.
- **Mid-turn**: Evaluates real provider `PromptTokenCount` before subsequent tool turns (`modelCalls > 1`) and short-circuits early if the threshold is exceeded.
- **Post-turn**: Evaluates the turn's final `PromptTokenCount` and triggers compaction immediately before returning.

When compaction runs:
1. **User Turn 1**: Same combined system-prompt + `<PERSISTENT_MEMORY>` text as normal generation *(identical prefix, for prompt caching)*.
2. **Archived Turns**: Oldest turns from `session.jsonl` (determined by `compact-pct`, default 50% token-weighted), extended forward until the boundary lands right after a `model` turn — so the surviving session (`remainingTurns`) always starts fresh on a `user` turn.
3. **Compaction Directive (User Turn)**: Instructs the model to generate a concise, chronological markdown ADDENDUM capturing new developments from the archived turns (without repeating what `<PERSISTENT_MEMORY>` already has).

### Memory Update & Session Pruning:
1. The LLM generates a bulleted markdown **ADDENDUM** (extracted via `ContentText`, which excludes `Thought`-marked parts — reasoning never leaks into `MEMORY.md`).
2. The addendum is appended to `<agent_id>/MEMORY.md` (or replaces it if `append-only: false`).
3. The archived turns (`compactTurns`) are removed from `session.jsonl`, keeping only `remainingTurns`.

### Token Counting & Usage Tracking
WackyPub tracks real provider usage stats (`PromptTokenCount`) across generation turns and uses `EstimateTokens` (~4 chars/token heuristic, with `preserveThinking` awareness) when estimating offline turn history.

---

<a id="7-reasoning--thinking-support"></a>

## 8. Reasoning / Thinking Support

Backends vary widely in how they expose and expect back a model's reasoning/chain-of-thought, and getting this wrong ranges from "wastes tokens" to "hard 400 error." This is handled by the OpenAI adapter (`pkg/agent/openai_model.go`, backed by the `colinrgodsey/adk-utils-go` fork — see [§4](#4-google-adk-integration-layer)) plus a few `runtime.json` knobs.

### Ingest

Reasoning is captured **unconditionally** on ingest, regardless of `runtime.json` settings — a plain-text reasoning field (`reasoningField`, default `"reasoning_content"`) becomes a `Thought`-marked `genai.Part`, and OpenRouter's structured `reasoning_details` blocks (including opaque encrypted/signed reasoning) are captured verbatim into `partMetadata` under the adapter's `ReasoningDetailMetadataKey`. This is why reasoning shows up in `session.jsonl` even for agents where egress is disabled — capture and replay are independently controlled.

### Egress

Controlled by `reasoningEgress` and `supportsReasoningDetails`:
- **`native`** (default): reasoning is sent back as its own field (named by `reasoningField`) on the assistant message, separate from `content`. Required by DeepSeek V4 thinking mode and Kimi K2 Thinking, which 400 if it's missing.
- **`think_tags`**: reasoning is folded into `content`, wrapped in a `<think>...</think>` block, for backends that reject an unrecognized field with a 400 (observed on Mistral, TensorRT-LLM, some gateways).
- **`omit`**: no reasoning is sent at all.
- **`supportsReasoningDetails: true`**: additionally replays OpenRouter's structured `reasoning_details` block array verbatim (unmodified, unreordered — the sequence has to match what the model produced). **Only safe with a pinned `model`** — encrypted/signed reasoning blocks are tied to the exact backend endpoint that produced them, and OpenRouter's `"auto"` router can pick a different endpoint on the next turn, which gets rejected with a 404 ("Encrypted payloads can only be replayed to the endpoint that created them"). If `supportsReasoningDetails` is `false`, `StripSignatures` removes any captured block metadata before a turn is persisted to `session.jsonl`, so a block captured while the setting was on doesn't sit around as dead weight (or a future stale-endpoint landmine) after it's turned off.

### Forcing extended thinking

Some models (e.g. Claude via OpenRouter) don't emit reasoning by default — it has to be explicitly requested via `extraBody`:
```json
"extraBody": {
  "reasoning": { "effort": "high" }
}
```

### Display vs. storage

`ContentText` (used for the CLI's printed/returned response, and for `MEMORY.md` addenda) always excludes `Thought`-marked parts — reasoning is preserved in `session.jsonl` for full fidelity, but never shown as if it were the character's actual dialogue.

### Manually stripping stale reasoning/thought signatures

If an agent's `session.jsonl` already contains `reasoning_details` block metadata from a prior backend (e.g. it was run against `"model": "auto"` on OpenRouter and picked up an encrypted block), or a Gemini `ThoughtSignature` from before the agent was switched to a different provider, that stale signature is a landmine — see [`wackypub agent <agent_id> strip-signatures`](#9-cli-command-pipeline) below. It removes only the signature/block metadata; readable `Thought` text is left in place.

---

<a id="8-cli-command-pipeline"></a>

## 9. CLI Command Pipeline

### Global Flags
- `--ws <path>`: Specifies workspace directory. Unspecified walks up from CWD looking for `WACKYPUB_ROOT`.
- `--max-tool-turns <int>`: Sets maximum consecutive tool-call turn limit per generation (default `300`).
- `--command-timeout-seconds <int>`: Sets maximum execution timeout in seconds for tool commands (`-1` to disable, default `900`). Precedence: CLI flag > `WACKYPUB_COMMAND_TIMEOUT_SECONDS` env var > default (`900`). Spawns each tool in an isolated process group (`Setpgid: true`) and terminates the entire process group upon timeout.

### Add User Turn (`add`)
```bash
wackypub agent <agent_id> add [message]
```
- Accepts message via positional argument, `-m / --message` flag, or piped stdin.
- Appends `{"role": "user", "parts": [{"text": message}]}` to `<ws_dir>/<agent_id>/session.jsonl`.

### Generate Assistant Turn (`generate`)
```bash
wackypub agent <agent_id> generate
```
- Loads agent from `<ws_dir>/<agent_id>`.
- Evaluates compaction triggers.
- Discovers executable tools in `<ws_dir>/<agent_id>/tools/` and registers them alongside built-in `set_scratchpad`/`get_scratchpad` tools.
- Executes multi-turn tool calling loop up to `--max-tool-turns` (default 300).
- Builds request contents and passes them through `CleanSessionTurns` (stripping dangling tool responses and merging consecutive user turns).
- Generates turn by calling the configured `model.LLM` directly.
- Prints final-answer text to `stdout` (`Thought` parts excluded).
- Appends generated turns (`genai.Content`) to `<ws_dir>/<agent_id>/session.jsonl`.

### Atomic Prompt Turn (`prompt`)
```bash
wackypub agent <agent_id> prompt [message]
```
- **Atomically** appends the user message and executes the generation turn loop under a **single session lock**.
- Prevents race conditions when multiple processes target the same agent.
- Accepts message via positional argument, `-m / --message` flag, or piped stdin.
- Prints generated assistant text to `stdout`.
- **Recommended over separate `add` + `generate`** for most use cases.

### Strip Provider Signatures (`strip-signatures`)
```bash
wackypub agent <agent_id> strip-signatures
```
- Permanently removes provider-specific opaque reasoning/thought signatures — OpenRouter `reasoning_details` block metadata and Gemini's `ThoughtSignature` field — from every turn in `<ws_dir>/<agent_id>/session.jsonl`, rewriting the file in place under the session lock.
- Readable plain-text `Thought` reasoning is left untouched.
- Run this before switching an agent's `runtime.json` to a different provider — a signature issued by the old provider is rejected outright if replayed to the new one (confirmed live: Anthropic 400s with `Invalid \`signature\` in \`thinking\` block` on a Gemini `ThoughtSignature`).

### Read Session (`read-session`)
```bash
wackypub agent <agent_id> read-session
```
- Prints every turn in `<ws_dir>/<agent_id>/session.jsonl` to stdout, one JSON-encoded `genai.Content` per line.
- Read-only.

### Read Memory (`read-memory`)
```bash
wackypub agent <agent_id> read-memory
```
- Prints current contents of `<ws_dir>/<agent_id>/MEMORY.md` to stdout.
- Read-only.

### Render System Prompt (`render-prompt`)
```bash
wackypub agent <agent_id> render-prompt
```
- Prints fully rendered system prompt (`AGENTS.md` after `@<FILE_PATH>` macro expansion).
- Read-only.

### Compact (`compact`)
```bash
wackypub agent <agent_id> compact
```
- Unconditionally performs session compaction on the agent's session history (summarizes oldest turns into MEMORY.md and prunes session.jsonl).

### Cancel In-Flight Turn (`cancel`)
```bash
wackypub agent <agent_id> cancel
```
- Requests cancellation of the turn now running for the agent and prints which one it reached: an in-process turn through the D85 registry, or a turn in another process by SIGTERM to whatever holds the agent's session lock.
- Enforces `WACKYPUB_ALLOWED_AGENTS` like `add`, `prompt`, and `generate`: cancelling ends another agent's work, so it is a mutating call, not a read-only diagnostic.
- A turn holds the session lock for its whole duration, which is how an outside process identifies it. The lock is probed with `LOCK_EX|LOCK_NB` so the command never queues behind the turn it is trying to stop, and only a holder that is a `wackypub` executable (or this same binary) is signalled. A lock file naming an absent or unrelated process is leftover metadata from a finished turn: nothing is signalled and the command reports that no turn is running.
- A cancelled turn stops at the existing `ctx.Err()` checkpoints, all before the assistant turn boundary, so no partial assistant text is committed. The user message that started the turn stays in `session.jsonl`, and the session lock is released, leaving the agent immediately usable.
- Exits non-zero when there is nothing to cancel. `wackypub workspace locks` shows who holds each lock and how long each session has been quiet.

### Inspect Hooks (`hooks`)
```bash
wackypub agent hooks <agent_id>
```
- Read-only listing of every hook installed under `<ws_dir>/<agent_id>/hooks/<event>/`, backed by `adkAgent.InspectAgentHooks`. Groups by event, in the same ascending numeric order `RunHookChain` executes them in, and shows each script's source.
- Works against any agent in the workspace, not just the caller's own - useful for a coordinator verifying hook installs across a swarm without raw filesystem access.
- Only lists files `DiscoverHooks` would actually execute (regular, executable files directly under an event directory), so the listing matches runtime behavior rather than everything sitting in that directory.
- Source is capped at `adkAgent.HookContentLineLimit` (200) lines per script; a longer script prints with a `+N more lines` suffix instead of its full body, since hooks are expected to be small (see examples/hooks/).
- Never creates, modifies, or executes a hook.

### Scratchpad Management (`scratchpad`)
```bash
wackypub agent <agent_id> scratchpad create [message]
wackypub agent <agent_id> scratchpad read <entry_id> [--skip-lines N] [--num-lines M]
wackypub agent <agent_id> scratchpad diff <before_id> <after_id>
wackypub agent <agent_id> scratchpad list
wackypub agent <agent_id> scratchpad search <entry_id> <query> [--regex] [--case-insensitive] [--max-results N]
```
- CLI-level access to an agent's persistent scratchpad (`<ws_dir>/<agent_id>/scratchpad/`) (see DECISIONS.md D27).
- `create`: accepts text via positional argument, `--message` flag, or piped stdin. Acquires session lock for atomic write.
- `read`, `list`, `search`: pure reads against atomic temp-file replacement, executing without session lock.
- Supports both `wackypub agent <agent_id> scratchpad <subverb>` and `wackypub agent scratchpad <subverb> <agent_id>` syntaxes for positional arguments - but flags (`--skip-lines`, `--regex`, etc.) only work in the second form (`wackypub agent scratchpad <subverb> <agent_id> ... --flag`); see `skills/wackypub-a2a/SKILL.md`'s Flag Ordering Caveat.

### Workspace Diagnostics & Git Versioning (`workspace`)
```bash
wackypub workspace
wackypub workspace <agent_id>
wackypub workspace init-git [agent_id]
wackypub workspace snapshot
wackypub workspace tag <name>
wackypub workspace push <remote>
wackypub workspace locks
```
- **Overview (`wackypub workspace`)**: Top-level diagnostic command inspecting workspace agents, git tracking status, presence of expected files (`WACKYPUB_ROOT`, `AGENTS.md`, `runtime.json`, `session.jsonl`, `MEMORY.md`, `WACKYPUB_ALLOWED_AGENTS`, `tools/`), discovered tools, shadowed tools, and issue warnings. Read-only: never creates or modifies any file.
- **Git Versioning Init (`wackypub workspace init-git [agent_id]`)**: Initializes a pure-Go git repository in an agent directory (`<ws_dir>/<agent_id>/.git`) or workspace root (`<ws_dir>/.git`) via `go-git`. When `.git` is present, every state event creates an isolated commit with embedded `AGENT2AGENT` JSON metadata and `workspace_revision` (D35).
- **Workspace Snapshot (`wackypub workspace snapshot`)**: Scans all workspace agent repositories, records each agent's active HEAD commit SHA in `<ws_dir>/MANIFEST.md`, and commits `MANIFEST.md` in the workspace root repository.
- **Workspace Tagging (`wackypub workspace tag <name>`)**: Tags the workspace root repository with `<name>` and tags each per-agent repository with `tag-<agent_id>`.
- **Remote Push (`wackypub workspace push <remote>`)**: Pushes each agent repository to `<remote>` under a remote branch matching its `agent_id`, and pushes all workspace and agent tags. Requires `--i-understand` flag confirmation to guard against accidental API key exfiltration.
- **Session Locks (`wackypub workspace locks`)**: Read-only table (backed by the `adkAgent.InspectAgentLocks` SDK function, which owns the lock, `/proc`, and session mtime reads) of every agent directory showing whether its `session.lock` is HELD, FREE, or STALE (recorded PID gone), the holder PID and shortened command line (credential flag values redacted), the lock file mtime, and the age of the last `session.jsonl` write with a verdict: OK, WEDGED (lock held, session quiet over 10 minutes), IDLE (no lock, quiet over 30 minutes), or STALE. Holder liveness uses `kill(pid,0)` and `/proc/<pid>/cmdline`, so holder details are Linux-specific. Never creates, truncates, locks, or deletes a lock file.

### Causal Swarm Tracing (`trace`)
```bash
wackypub trace <agent_id> <commit> [-n <steps>] [-v <0..4>]
wackypub trace <trace_id> [-n <steps>] [-v <0..4>]
```
- **Targeted Trace**: Step-by-step backward causal traversal starting from `<commit>` in `<agent_id>`'s repository. Traverses backward through turn commits and follows `metadata.workspace_revision` out to calling agent repositories across inter-agent boundaries (D36).
- **Global Correlation Trace**: Searches across all workspace agent repositories for commits matching `<trace_id>` and reconstructs the causal execution chain.
- **Verbosity Levels (`-v 0..4`)**:
  - `0`: Minimal (event types, function call names, user prompt text)
  - `1`: Compact Default (event type, tool names, user text, assistant text)
  - `2`: Clean Full (complete text, stripped of thinking blocks & signatures)
  - `3`: Full with Thinking (includes thinking blocks, stripped of provider signatures)
  - `4`: Raw JSONL (dumps raw commit messages & `AGENT2AGENT` payloads as-is)

---

## 10. Session Locking

All SDK operations acquire an exclusive POSIX file lock (`flock`) on `<agent_id>/session.lock` before reading or writing session state.

### Mechanics
- **Lock file**: `<ws_dir>/<agent_id>/session.lock`
- **Lock type**: `syscall.Flock(fd, LOCK_EX)` — blocking exclusive lock.
- **PID visibility**: The current process PID is written to the lock file for diagnostic inspection.
- **Scope**: The lock is held for the duration of the SDK method call and released automatically via `defer`.

---

## 11. Programmatic Go SDK API (`pkg/agent`)

All agent functionality is exposed as a Go SDK (`agent.AgentSDK`) in `pkg/agent` for programmatic orchestration.

### Initializing the SDK
```go
import "github.com/colinrgodsey/wackypub/pkg/agent"

// Initialize SDK for workspace directory
sdk := agent.NewSDK("./my_workspace")
sdk.MaxToolTurns = 300 // Optional cap over consecutive tool turns
sdk.CommandTimeoutSeconds = 900 // Optional tool command execution timeout in seconds (-1 to disable)
```

### SDK Methods
```go
// Add a user message turn to session.jsonl (acquires session lock, returns *UserTurnResult with any hook warnings)
res, err := sdk.AddUserTurn("wizard", "Greetings! Tell me a rumor.")

// Generate the agent's turn response (acquires session lock, evaluates compaction & runs tool execution loop)
respText, err := sdk.GenerateTurn(ctx, "wizard")

// Atomically add user message + generate assistant response under a single lock (returns *GenerateTurnResult with Text and any hook Warnings)
resp, err := sdk.AddAndGenerateTurn(ctx, "wizard", "Greetings! Tell me a rumor.")

// Read session history as []*genai.Content (acquires session lock)
turns, err := sdk.ReadSession("wizard")

// Read memory file (MEMORY.md) (acquires session lock)
mem, err := sdk.ReadMemory("wizard")

// Persistent session scratchpad management
msg, err := agent.SetScratchpad(agentDir, 1, "hello")
val, err := agent.GetScratchpad(agentDir, 1)

// Fully rendered system prompt (AGENTS.md + macro expansion) (acquires session lock)
prompt, err := sdk.RenderSystemPrompt("wizard")

// Manually trigger session compaction evaluation (acquires session lock)
compacted, err := sdk.CompactSession(ctx, "wizard")

// Permanently strip provider-specific reasoning/thought signatures from session.jsonl (acquires session lock)
modified, err := sdk.StripSignatures("wizard")

// List agent IDs found directly under the workspace directory
ids, err := sdk.ListAgents()

// Report an agent's on-disk state (files present/missing, runtime.json validity, session/memory stats, tools)
insp, err := sdk.InspectAgent("wizard")

// Access underlying FolderAgent for ADK runner customization
fa, err := sdk.GetAgent("wizard")
```

