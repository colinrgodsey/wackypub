---
name: wackypub-ws
description: Guide for setting up, structuring, and managing WackyPub AI agent workspaces, including capability wiring (files-rw, A2A, wackyproc), git versioning, manifest snapshots, remote sync, symlink organization, and an AGENTS.md brace gotcha.
always_load: false
---
# WackyPub AI Workspace Setup & Management Guide

`wackypub` operates on workspace directories containing folder-based AI agents. This guide details workspace creation, agent scaffolding, capability wiring (file access, cross-agent calling, background processes), git versioning management, remote synchronization, and recommended organizational patterns.

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
     "model": "anthropic/claude-sonnet-5.5",
     "apiKey": "${OPENROUTER_API_KEY}"
   }
   ```
   Model ids go stale. Confirm the id against your provider's live model list
   (`curl -s https://openrouter.ai/api/v1/models`) before copying one from this page.
   To share backend configs across agents, use symlinks (e.g. `ln -s ../runtimes/openrouter-sonnet.json ws/<agent_id>/runtime.json`).

4. **Tools Directory (`<ws_dir>/<agent_id>/tools/`)**:
   - **Discovery trigger set**: Tools are discovered recursively under `<agent_id>/tools/`. Any regular file or symlink with the executable bit set (`0111`, e.g. `chmod +x`) is registered as a callable tool. Non-executable files and broken symlinks are ignored. The executable name determines the tool name.
   - **External tool wiring**: To enable capabilities like cross-agent calling, file access, or process supervision, symlink the corresponding binary into `tools/`:
     ```bash
     ln -s "$(command -v wackypub)" ws/coordinator/tools/wackypub
     ln -s "$(command -v files-rw)" ws/coordinator/tools/files-rw
     ln -s "$(command -v wackyproc)" ws/coordinator/tools/wackyproc
     ```
     *Note on files-rw*: Symlinking the binary is only half the setup — `files-rw` also requires its own `FILES_RW_ACCESS` grant file in the agent directory (see the [File Access (files-rw) & Capability Setup](#file-access-files-rw--capability-setup) section below).
   - **Embedded tool note**: Built-in tools (`load_skill`, `create_scratchpad`, `get_scratchpad`, `list_scratchpads`, `search_scratchpad`, `diff_scratchpad`) are provided directly in-process by the wackypub runtime. Their schemas and descriptions are injected into the system prompt on every turn as native function declarations. They do **not** need binaries in `tools/` and execute in-process without spawning subprocesses.

5. **Skills Directory (`<ws_dir>/<agent_id>/skills/<skill_name>/SKILL.md`)**:
   - **Discovery trigger set**: Discovered recursively under `<agent_id>/skills/`. Any directory or symlink containing a `SKILL.md` file with valid YAML frontmatter (`name:`, `description:`) is discovered on every turn and agent load.
   - **Loading modes**:
     - `always_load: true`: The skill's entire markdown body is automatically appended to the agent's rendered system prompt (`<AUTOLOADED_SKILLS>`) on every turn.
     - `always_load: false` (default): The skill is registered in the on-demand catalog presented to the model via the built-in `load_skill` tool, loaded only when relevant.
   - To share common skills across multiple agents, maintain a workspace `skillsets/` folder and symlink into agent directories.

6. **Agent Authorization (`<ws_dir>/<agent_id>/WACKYPUB_ALLOWED_AGENTS`)**:
   Plain text file listing target agent IDs allowed for cross-agent calling (one per line).
   - **Outbound-only directionality**: This file gates this agent's **outbound** calls only. Listing `agent_b` here allows this agent to call `agent_b`. Being listed in another agent's allowlist has no effect on whether that agent can reach you — inbound receipt is governed entirely by the *caller's* own `WACKYPUB_ALLOWED_AGENTS`. No symmetric pairing or mutual listing is required.
   - Missing or empty file defaults to deny-all (outbound A2A calls denied).

7. **System Prompt (`<ws_dir>/<agent_id>/AGENTS.md`)**:
   Agent persona and instructions. Supports `@<FILE_PATH>` macro expansion. (See the gotcha below regarding `{...}` braces).

8. **Verifying a New Agent (Live Smoke Test)**:
   Never consider an agent setup complete based solely on static file presence.
   - **Static inspection**:
     ```bash
     wackypub workspace <agent_id>
     ```
     Verifies on-disk directory layout, runtime presence, discovered tools, discovered skills, and authorization files.
   - **Live smoke test**:
     ```bash
     wackypub agent prompt <agent_id> "confirm your role, one sentence"
     ```
     Executes a real one-turn prompt against the backend. This validates runtime authentication (e.g. valid `${OPENROUTER_API_KEY}` in `.env`), endpoint connectivity, prompt macro expansion, and session creation. If the agent uses `files-rw`, follow up with a quick `files-rw access` check from the agent directory.

---

## File Access (files-rw) & Capability Setup

### The Capability Wiring Principle

**A capability is not enabled until its tool, its configuration, AND its skill are all wired.**

Partial enablement is a common silent-failure trap:
- **Tool without config**: Symlinking `files-rw` into `tools/` without creating `FILES_RW_ACCESS` results in every file operation silently denying with no obvious hint as to why.
- **Tool without skill**: The agent has an executable binary in `tools/`, but lacks prompt instructions or usage examples explaining its command line flags and guardrails.
- **Skill without tool**: The agent tries to invoke a tool mentioned in its instructions that does not exist in `tools/` or PATH.

### Capability Checklist

| Capability | 1. Tool (`<agent_id>/tools/`) | 2. Configuration / ACL | 3. Skill (`<agent_id>/skills/`) |
|---|---|---|---|
| **File Access** | `tools/files-rw` symlink | `<agent_id>/FILES_RW_ACCESS` grant file | `skills/files-rw` symlink |
| **A2A Calling** | `tools/wackypub` symlink | `<agent_id>/WACKYPUB_ALLOWED_AGENTS` | `skills/wackypub-a2a` symlink |
| **Process Manager** | `tools/wackyproc` symlink | Background supervisor environment | `skills/wackyproc` symlink |

### Setting Up File Access (`files-rw`)

`files-rw` is the preferred per-directory, allowlist-gated filesystem tool for agent file operations.

1. **The Grant File (`<agent_id>/FILES_RW_ACCESS`)**:
   A plain text file placed directly in the agent's directory (`<ws_dir>/<agent_id>/FILES_RW_ACCESS`), containing one grant rule per line.
   - **Deny-all default**: If `FILES_RW_ACCESS` is missing or empty, `files-rw` denies all file operations by default (fail-closed).
   - Lines starting with `#` and blank lines are ignored.

2. **Grant Syntax**:
   - `w: <path>`: Grants write access (which automatically implies read access).
   - `r: <path>`: Grants read-only access. Write, append, delete, and mkdir operations will be denied.

3. **Path Resolution Gotcha**:
   - **Relative paths resolve against the agent's CWD** (the agent directory `<ws_dir>/<agent_id>`), which is often not the workspace or project root you intended. For example, `w: ./output` inside `ws/editor/` refers to `ws/editor/output`.
   - **Prefer absolute paths** for anything outside the agent directory (e.g. project repositories or shared paths).
   - When a path fails or is denied, `files-rw` prints the resolved absolute path in the error message — read it to see exactly where the tool attempted to resolve.

4. **Common Starter Recipes**:
   - **Isolated Agent Sandbox (Starter)**:
     ```text
     # Read and write inside own agent directory and temporary scratch
     w: ./
     w: /tmp/scratch
     ```
   - **Asymmetric Swarm (Coordinator writes, Critics read-only)**:
     Coordinator agent (`ws/editor/FILES_RW_ACCESS`):
     ```text
     w: ./output
     ```
     Critic/Reviewer agent (`ws/critic/FILES_RW_ACCESS`):
     ```text
     # Read-only access to editor's output; write only to own directory
     r: ../editor/output
     w: ./
     ```
   - **Workspace Read + Project Write**:
     ```text
     # Read workspace configs and documentation
     r: ../
     # Write to target project repository
     w: /home/user/workspace/projects/my-project
     w: /tmp/scratch
     ```

5. **Tool Wiring**:
   Symlink the `files-rw` binary into the agent's `tools/` directory and ensure it has executable permissions:
   ```bash
   ln -s "$(command -v files-rw)" ws/<agent_id>/tools/files-rw
   chmod +x ws/<agent_id>/tools/files-rw
   ```
   Remember: having `FILES_RW_ACCESS` without the `files-rw` binary in `tools/` means the agent cannot call the tool; having the binary in `tools/` without `FILES_RW_ACCESS` means every call denies.

6. **Robustness Notes**:
   - **Non-existent paths**: Granting access to a path that does not yet exist is completely valid — it allows the agent to create that directory or file.
   - **Malformed lines**: Syntax errors in `FILES_RW_ACCESS` emit a warning and are skipped; they never crash the tool and never silently corrupt existing rules.
   - **ACL Immutability**: `FILES_RW_ACCESS` is always read-only to `files-rw`, regardless of any write rules in the file. An agent cannot widen its own permissions.
   - **Empty write guardrail**: `files-rw write` refuses zero-byte writes by default (requiring `--allow-empty`) to prevent accidental file truncation.

### Setting Up Background Processes (`wackyproc`)

`wackyproc` is a self-supervising process manager for background tasks, daemons, test runs, and build monitoring.
1. Symlink the `wackyproc` binary into `<agent_id>/tools/wackyproc`:
   ```bash
   ln -s "$(command -v wackyproc)" ws/<agent_id>/tools/wackyproc
   chmod +x ws/<agent_id>/tools/wackyproc
   ```
2. The agent executes `wackyproc` commands via `run_command` (e.g. `wackyproc run -- <command>`, `wackyproc ps`, `wackyproc log <task_id>`).
3. Install the `wackyproc` skill (`skills/wackyproc/SKILL.md`) to provide the agent with full command and lifecycle guidance.

### Skillset Wiring: Bundled vs. Curated

- **Bundled Shared Skillset (All skills)**:
  If all agents should have access to all workspace skills, symlink the whole `skillsets/` tree:
  ```bash
  ln -s ../../skillsets/wackypub ws/<agent_id>/skills/wackypub
  ```
- **Curated Per-Agent Skills**:
  To keep an agent's context focused and capabilities aligned, symlink only the skills matching the agent's enabled tools:
  ```bash
  ln -s ../../skillsets/wackypub/files-rw ws/<agent_id>/skills/files-rw
  ln -s ../../skillsets/wackypub/wackypub-a2a ws/<agent_id>/skills/wackypub-a2a
  ln -s ../../skillsets/wackypub/wackyproc ws/<agent_id>/skills/wackyproc
  ```

---

## Workspace Git Management & Versioning (D35)

> [!NOTE]
> **Git tracking is entirely opt-in.** A workspace or agent operates normally without git. Nothing commits or initializes automatically until `wackypub workspace init-git` is explicitly executed for that workspace or agent.

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

Built-in in-process tools (`create_scratchpad`, `get_scratchpad`, `list_scratchpads`, `search_scratchpad`, `diff_scratchpad`, `load_skill`) do **not** get their own commit - they never spawn a subprocess, so whatever surrounding commit already exists covers them.

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
│   ├── files-rw
│   └── wackyproc
├── skillsets/                  # Shared skill folders
│   └── wackypub/
│       ├── files-rw/
│       ├── wackypub-a2a/
│       └── wackyproc/
├── agent_a/                    # Coordinator / Writer
│   ├── runtime.json -> ../runtimes/openrouter-sonnet.json
│   ├── WACKYPUB_ALLOWED_AGENTS # Outbound target allowlist: agent_b
│   ├── FILES_RW_ACCESS         # w: ./ (writes own directory and output)
│   ├── tools/
│   │   ├── wackypub -> ../../tools/wackypub
│   │   └── files-rw -> ../../toolsets/files-rw
│   └── skills/
│       ├── wackypub-a2a -> ../../skillsets/wackypub/wackypub-a2a
│       └── files-rw -> ../../skillsets/wackypub/files-rw
└── agent_b/                    # Critic / Read-Only Reviewer
    ├── runtime.json -> ../runtimes/gemini-flash.json
    ├── FILES_RW_ACCESS         # r: ../agent_a/output (asymmetric read-only)
    ├── tools/
    │   └── files-rw -> ../../toolsets/files-rw
    └── skills/
        └── files-rw -> ../../skillsets/wackypub/files-rw
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
