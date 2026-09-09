---
name: wackypub-ws
description: Guide for setting up, structuring, and managing WackyPub AI agent workspaces, including git versioning, manifest snapshots, remote sync, symlink organization, and an AGENTS.md brace gotcha.
always_load: false
---
# WackyPub AI Workspace Setup & Management Guide

`wackypub` operates on workspace directories containing folder-based AI agents. This guide details workspace creation, agent scaffolding, git versioning management, remote synchronization, and recommended organizational patterns.

## Setting Up an Agent & Swarm From Scratch

Everything below is on-disk convention — `wackypub workspace <agent_id>` will report what is present/missing for an existing agent. Use `wackypub workspace` to check progress as you set each piece up.

1. **Workspace Root Marker**: A directory becomes a workspace when it contains a `WACKYPUB_ROOT` file:
   ```bash
   mkdir -p ws && touch ws/WACKYPUB_ROOT
   ```
2. **Workspace Environment & Secret Stashing (`<ws_dir>/.env`)**:
   Stash shared API keys and secrets in `<ws_dir>/.env`. Loaded automatically before per-agent `.env` files. `runtime.json` supports environment variable expansion (`${OPENROUTER_API_KEY}`).

3. **Agent Runtime Configuration (`<ws_dir>/<agent_id>/runtime.json`)**:
   Backend model configuration. Minimal example:
   ```json
   {
     "provider": "openai",
     "endpoint": "https://openrouter.ai/api/v1",
     "model": "anthropic/claude-3.5-sonnet",
     "apiKey": "${OPENROUTER_API_KEY}"
   }
   ```
   To share backend configs across agents, use symlinks (e.g. `ln -s ../runtimes/openrouter-sonnet.json ws/<agent_id>/runtime.json`).

4. **Tools Directory (`<ws_dir>/<agent_id>/tools/`)**:
   Any executable file (or symlink) placed recursively under `tools/` becomes a callable tool. To enable sub-agent calling, symlink the `wackypub` CLI binary into an agent's `tools/` directory:
   ```bash
   ln -s "$(command -v wackypub)" ws/coordinator/tools/wackypub
   ```

5. **Skills Directory (`<ws_dir>/<agent_id>/skills/<skill_name>/SKILL.md`)**:
   Provides skills with YAML frontmatter. To share common skills across multiple agents, maintain a workspace `skillsets/` folder and symlink into agent directories.

6. **Agent Authorization (`<ws_dir>/<agent_id>/WACKYPUB_ALLOWED_AGENTS`)**:
   Plain text file listing target agent IDs allowed for cross-agent calling (one per line). Missing file defaults to deny-all.

7. **System Prompt (`<ws_dir>/<agent_id>/AGENTS.md`)**:
   Agent persona and instructions. Supports `@<FILE_PATH>` macro expansion.

---

## Workspace Git Management & Versioning (D35)

WackyPub supports pure-Go git versioning via `go-git`:

```bash
# Initialize git tracking for workspace or agent
wackypub workspace init-git             # Workspace root coordinator repo (<ws_dir>/.git)
wackypub workspace init-git <agent_id>  # Per-agent isolated repo (<ws_dir>/<agent_id>/.git)

# Workspace Snapshot
wackypub workspace snapshot             # Updates MANIFEST.md with agent commit SHAs

# Workspace Tagging
wackypub workspace tag <name>           # Tags root repo with <name> and agents with tag-<agent_id>

# Remote Synchronization
wackypub workspace push <remote>        # Pushes root repo and agent repos (to branch <agent_id>)
```

> [!WARNING]
> **Credential Exfiltration Risk**: `wackypub workspace push` pushes full agent history, including `runtime.json` and `.env` files which may contain sensitive API keys or credentials. Always verify that the target remote is private and trusted before pushing, and notify your user before executing remote pushes.

### Gitignore Rules
- **Workspace Root**: Excludes everything (`*`) by default, tracking only root metadata (`.gitignore`, `WACKYPUB_ROOT`, `MANIFEST.md`).
- **Agent Directory**: Excludes everything (`*`) by default, tracking core agent files (`AGENTS.md`, `IDENTITY.md`, `MEMORY.md`, `runtime.json`, `.env`, `session.jsonl`, `scratchpad/`, `skills/`, `tools/`).

### Commit Cadence

Git tracking is entirely opt-in (nothing commits until `workspace init-git` has been run for that agent/workspace) and, once enabled, commits happen at:
- **Turn boundaries**: once when a user turn is added, once when the assistant's turn finishes generating.
- **Every `run_command` dispatch**: once, synchronously, immediately before the subprocess is spawned - uniformly for every tool call, not just cross-agent ones. This is what lets an A2A hop mid-turn carry a `workspace_revision` reflecting everything the calling agent did up to that point, not just its state from the start of the turn.
- **Compaction**, when it runs.

Built-in in-process tools (`create_scratchpad`, `get_scratchpad`, `list_scratchpads`, `search_scratchpad`, `load_skill`) do **not** get their own commit - they never spawn a subprocess, so whatever surrounding commit already exists covers them.

---

## Causal Swarm Tracing (D36)

Step-by-step causal tracing across multi-agent commit graphs is supported via `wackypub trace`:
```bash
# Targeted trace starting from a commit in an agent's repository
wackypub trace <agent_id> <commit> [-n <steps>] [-v <0..4>]

# Global correlation trace searching across all workspace agent repositories
wackypub trace <trace_id> [-n <steps>] [-v <0..4>]
```

### Options & Verbosity Levels
- `-n, --max-steps <int>`: Maximum trace steps to traverse (default 20).
- `-v, --verbosity <int>`: Verbosity level 0..4 (default 1):
  - `0`: Minimal (event types, function call names, user prompt text)
  - `1`: Compact Default (event type, tool names, user text, assistant text)
  - `2`: Clean Full (complete text, stripped of thinking blocks & signatures)
  - `3`: Full with Thinking (includes thinking blocks, stripped of provider signatures)
  - `4`: Raw JSONL (dumps raw commit messages & `AGENT2AGENT` payloads as-is)

---

## Recommended Workspace Organization & Symlinks

To keep workspaces clean and avoid duplicating configurations or scripts across agents, organize shared resources at the workspace root and symlink them into individual agent folders:

```text
my_workspace/
├── WACKYPUB_ROOT
├── .env                        # Shared API keys (git-ignored)
├── MANIFEST.md                 # Agent commit SHA manifest
├── runtimes/                   # Shared runtime.json configurations
│   ├── openrouter-sonnet.json
│   └── gemini-flash.json
├── toolsets/                   # Shared executable tools
│   └── files-rw
├── skillsets/                  # Shared skill folders
│   └── expert-poet/
├── agent_a/
│   ├── runtime.json -> ../runtimes/openrouter-sonnet.json
│   ├── tools/
│   │   └── wackypub -> ../../tools/wackypub
│   └── skills/
│       └── expert-poet -> ../../skillsets/expert-poet
└── agent_b/
    └── runtime.json -> ../runtimes/gemini-flash.json
```

## Gotcha: Braces in `AGENTS.md` Are Session-State Lookups

Everything in `AGENTS.md` becomes the agent's ADK instruction, and ADK scans that whole string for
`{...}` placeholders before the model ever sees it. A braced span whose contents look like an
identifier is resolved against session state, and a missing key fails the turn outright:

```text
failed to append instructions: failed to inject session state into instruction: state key does not exist
```

So a placeholder you wrote for human readers is not inert text. Shell style does not save you: in
`${OPENROUTER_API_KEY}` the `$` is an ordinary character, and `{OPENROUTER_API_KEY}` is still matched
and looked up.

| Written in `AGENTS.md` | What actually happens |
|---|---|
| `{OPENROUTER_API_KEY}`, `${OPENROUTER_API_KEY}` | State lookup for `OPENROUTER_API_KEY`; turn dies if the key is not in session state |
| `{HOME}`, `${HOME}` | Same trap; the `$` changes nothing |
| `{app:theme}`, `{user:lang}` | Prefixed state lookup (`app:` / `user:` / `temp:`); same failure if unset |
| `{nickname?}` | No error, but the span renders as an empty string, silently eating your sentence |
| `{artifact.summary}` | Loads an artifact by that name, and errors if the artifact service is unset or the file is missing |
| `{"a": 1}`, `{my-var}` | Not a valid state name, so passed through unchanged |
| `OPENROUTER_API_KEY`, `$HOME` (unbraced) | Safe, reaches the model verbatim |

Mechanism, for anyone chasing the error: the rendered prompt (`RenderAgentSystemPrompt`,
`pkg/agent/macro.go:49`) is handed to ADK as the agent instruction
(`BuildADKAgentWithConfigAndTracker`, `pkg/agent/adk_agent.go:380`), and ADK v2.0.0 applies
the placeholder regex `{+[^{}]*}+` (`internal/llminternal/instruction_processor.go:70`) across it in
`InjectSessionState` (`instruction_processor.go:204`). Brace stripping, the `?` optional suffix, the
`artifact.` prefix, and the fall-through for invalid names all live in `replaceMatch`
(`instruction_processor.go:121`); the error is `session.ErrStateKeyNotExist`
(`session/session.go:272`). `@`-included files are expanded before this point, so braces in
*them* are resolved too.

Always-loaded skills count as instruction text too: `RenderAgentSystemPrompt` appends the
`<AUTOLOADED_SKILLS>` block, and that block embeds each always-load skill's **body**
(`pkg/agent/skill.go:199`) onto the end of the prompt (`pkg/agent/macro.go:72`). A brace-delimited
identifier anywhere in an `always_load: true` skill fails every turn for that agent, not just turns
that mention it. The file you are reading is `always_load: false`, which is why the examples above
are inert here.

**Safe pattern:** name environment variables unbraced in prose ("the `OPENROUTER_API_KEY` environment
variable"). Reserve braces for real session-state injection, which is what they mean, and add `?`
only when an empty substitution is genuinely what you want. Before committing, check the file:

```bash
grep -oE '\{+[^{}]*\}+' AGENTS.md | tr -d '{}' | sed 's/?$//' \
  | grep -E '^([A-Za-z_][A-Za-z0-9_]*|(app|user|temp):[A-Za-z_][A-Za-z0-9_]*|artifact\..*)$'
```

Every line of output is a span ADK will try to resolve. Empty output means nothing in the file will
be treated as a lookup.
