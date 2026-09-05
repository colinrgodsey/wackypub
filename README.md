# 🎭 WackyPub

A CLI and Go SDK for folder-based AI agents — built on Google's **Agent Development Kit (ADK) v2** — where every agent is just a directory, every capability is a text file, every tool is an executable, and the same command interface an agent uses to explore its own tools is the one you use from your terminal or personal agent. Agents can call each other directly, turning a single assistant into a coordinated swarm.

---

## What it is

An agent in WackyPub is a folder. `AGENTS.md` is its system prompt, `MEMORY.md` is what it remembers long-term, `session.jsonl` is its turn history, `runtime.json` says which model backend it talks to. That's the whole foundation — no database, no bespoke config DSL, nothing that isn't a file you could open in a text editor and understand immediately.

On top of that foundation, an agent can be handed capabilities the same way: a `tools/` folder full of executables it can run, a `skills/` folder full of distilled knowledge it can load on demand, and a persistent scratchpad for passing data between tool calls without ever paying to regenerate it. And because every agent is just a workspace directory next to every other agent, one agent can call another — with real authorization and deadlock safety, not just crossed fingers — turning a lone assistant into a small swarm of specialty agents, or a single roleplay agent into a coordinating cast.

This came up almost by accident in an actual working session building this project - it's a better encapsulation of the idea than anything either of us would have written on purpose:

> **Me:** heh yea its been a trend with all these new agent platforms- all of them know how to run commands, so its easier to make tools that are just a command/executable. we're taking it a step further by making the whole agent platform a command/executable as well lol
>
> **My faithful assistant:** And yeah, I'll cop to the joke writing itself here: I'm an agent that mostly runs commands, currently running commands to help build a thing whose entire pitch is "agents that run commands," some of which will probably end up running `wackypub` itself as a command. It's recursive in a way that's either elegant or mildly unsettling depending on how long I stare at it. I'm going with elegant.

---

## Quick Start

**Prerequisites:** Docker and Docker Compose (and an API key for your favorite provider).

1. **Configure your environment**:
   ```bash
   cp .env.example .env
   # Edit .env and set your OPENROUTER_API_KEY (or GEMINI_API_KEY / ANTHROPIC_API_KEY)
   ```

2. **Launch the Director**:
   ```bash
   docker compose up
   ```

That's it — this builds the multi-tool suite (`wackypub`, `files-rw`, `wackyproc`, `wackydiscord`), seeds a host-mounted workspace (`./workspace`), and drops you straight into an interactive REPL talking to the **Director** agent.

Describe what you want to build (a coding assistant, a Discord bot persona, a multi-agent research swarm), and the Director scaffolds your agents, configures backend runtimes, links tools/skills, and writes an operational entrypoint (`/ws/desired-entrypoint.sh`). When you type `exit` in the REPL, the container automatically hands off execution to your new service.

If your service ever crashes or errors, the container's bootloader automatically falls back to the Director REPL for interactive inspection and recovery. And because `./workspace` is a standard host bind mount, you can inspect, edit, or reset files directly from your host at any time.

---

## Quick Start: Bring Your Own Agent

Already driving this from a coding agent (Claude Code, or whatever you're using to read this)? Skip the container - install the binary and let your agent bootstrap the rest itself.

```bash
go install github.com/colinrgodsey/wackypub@latest
```

Then hand your agent a prompt along these lines:

> I've installed a CLI called `wackypub` (it's on my PATH). Run `wackypub skill` to see what's bundled, then `wackypub skill ws` to learn how workspaces and agents are structured, and set one up for me right here - a workspace with one agent I can start talking to.

That's the whole point of the bundled skills (D34, D40): the CLI teaches your agent how to use itself. Nothing in this README is required reading for it to get started.

---

## Philosophy

**The CLI is the interface — for you and for the agent.** Most agent frameworks hide the CLI behind a bespoke tool schema and an SDK. WackyPub doesn't: the thing an agent gets access to *is* `wackypub` itself, one command at a time. If `--help` alone is enough for a human to drive it correctly, it's enough for a model too — and holding to that constraint has caught real bugs (a `--help` routing gap, a misleading error label) an SDK-only design would never have surfaced.

**Plain files over infrastructure.** Every piece of agent state — identity, memory, history, config, tools, skills, scratchpad — is something you can `cat`, edit by hand, `git diff`, or `symlink`. Swapping a model backend is repointing one symlink. Sharing a toolset or a skill across agents is one more symlink. Nothing here needs a server, a database, or a special editor to inspect or modify.

**Every tool is a command.** There's no plugin system, no capability-registration API. Past the handful of built-ins (mostly scratchpad and skill loading), everything an agent can do is an executable linked into its `tools/` folder. Link in only the specific commands you want it to have, or link in `bash` for everything at once — that's YOLO mode, not a recommendation: no guardrails, no limits, full power at the cost of trusting whatever it decides to run. Link a few specific commands directly alongside `bash` if you want the efficiency of named commands with a fallback for everything else. Want it to orchestrate other agents? Link `wackypub` itself back in - it's not special-cased, it's just another executable an agent happens to invoke.

**Capabilities are composable primitives, not a monolith.** `run_command` is one generic tool that dispatches to anything in `tools/` — drop in any executable and it's usable, no custom schema required. Skills follow the same shape other agent harnesses already use (`SKILL.md` with YAML frontmatter), so skills written elsewhere work here with no translation. The scratchpad exists because generation is the expensive part of a token budget, not consumption — an agent can pipe one command's output directly into another's input, or fork one payload out to several downstream calls, without a single one of those bytes ever being generated or re-read by the model itself, and when it does need to look, it can pull just a line range or search for a match instead of re-reading the whole thing.

**Trust, but verify — even for agents.** Cross-agent access is default-deny: an agent can only reach the peers explicitly listed in its own `WACKYPUB_ALLOWED_AGENTS`, and a separate call-chain mechanism refuses any cycle before it can deadlock, regardless of what an allowlist would otherwise permit. Two different concerns, two different mechanisms, neither one standing in for the other. And true to the "every tool is a command" point above, none of this required special-casing `wackypub` itself as a tool - the only wackypub-specific machinery anywhere in the codebase is a couple of carefully scoped environment variables carrying the authorization/cycle-detection state between invocations, not a bespoke integration. One of those, `AGENT2AGENT`, is a plain environment variable, not a private API - any command in the chain can read it to verify lineage for itself, `wackypub` has no special access to it that another tool wouldn't.

**Every action is auditable, even across agents — once you turn it on.** Per-agent git versioning is opt-in (`wackypub workspace init-git`), but once it's on, every meaningful state change becomes a commit, and every commit carries the same `AGENT2AGENT` metadata above - caller, call chain, trace ID. That's a full causal history sitting on disk, walkable with plain `git log` even without any wackypub-specific tooling. `wackypub trace` is a first attempt at automating that walk across agent boundaries - it's simple today, with known rough edges, but the underlying data it needs is already there, not something bolted on after the fact.

**A small tool surface is easier to secure — and easier to test.** Every capability is one small, independent command rather than a sprawling do-everything API, which means each one can be reasoned about — and attacked — in isolation. That composability turns out to double as a testing methodology - and this isn't a design aspiration, there's a real, dogfooded protocol for it, with a tracked pass/fail record per tool. See [Security](#security) below.

---

## Why it's simple

- An agent's entire identity and behavior lives in files you already know how to read: Markdown for prompts and memory, JSON Lines for history, JSON for config.
- Adding a tool is dropping or linking an executable in a folder. Adding a skill is dropping or linking a `SKILL.md` folder in the same way. No registration step, no schema to hand-author.
- The CLI *is* the SDK's surface — every `wackypub agent ...` subcommand has a matching `AgentSDK` Go method, so there's exactly one behavior to learn, not two.
- Nothing about the system depends on a specific model provider. The same OpenAI-compatible adapter talks to OpenAI, OpenRouter, DeepSeek, Kimi, vLLM, Ollama, llama.cpp, or LM Studio, and reconciles their different ways of expressing reasoning/thinking content.

## Why it's great

- **Agents can talk to agents, for real.** By using `wackypub` just like it uses any other command - not a scripted illusion, an agent can autonomously chain multiple calls to a peer agent within a single turn, using the peer's actual response to formulate its own follow-up, with the full exchange persisted and inspectable afterward.
- **Data can move without ever costing tokens to move it.** Pull a large command's output into a scratchpad entry, then pipe that entry straight into three different downstream calls — the model never regenerates or re-reads the payload itself, it just references it.
- **It's self-describing enough to actually work unattended.** A model with nothing but `--help`, tool descriptions, and a workspace has been shown, live, to explore its own environment, discover the correct invocation syntax through trial and correction, invoke a peer agent, and pipe data between two separate tool calls — all without being told the exact mechanics in advance. Skills aren't required for any of this - they're a refinement on top, for making a specific tool or task faster and more reliable to use than figuring it out cold every time, not a prerequisite for an agent doing anything at all.
- **A session isn't locked to the model that started it.** `session.jsonl` is a plain, model-agnostic wire format - moving an agent's entire history to a different backend is repointing `runtime.json` (often just one symlink), nothing about the conversation itself has to change.
- **Nothing here is a black box.** Every claim above has been verified by literally reading the session transcript afterward, not just trusting the summary — and the honest gaps (a `--help` ordering quirk, a lock that needed to not exist) get written down and fixed, not glossed over.

---

## Security

Security testing here is agent-driven, not just aspirational documentation. `wackypub` orchestrates a coordinator-and-worker swarm of its own agents to red-team a target tool's actual live build — propose → dedupe → execute → report rounds against a real binary running inside a disposable Docker container ([`docs/SWARM_TESTING.md`](docs/SWARM_TESTING.md)), not a hypothetical threat model on paper. It's also a legitimate standalone use case for `wackypub` itself, distinct from roleplay or orchestration - point the swarm at anything with a CLI surface, including `wackypub`'s own companion tools.

It's already found real things. A swarm run caught a critical cross-agent hardlink bypass in [`files-rw`](https://github.com/colinrgodsey/files-rw) (a companion filesystem access-control tool) - one agent could read another's supposedly walled-off files by hardlinking to them first. Found, documented, fixed, and re-verified against the fix - not a paper finding. The original report is preserved at the commit where it was written, before the fix it drove made the report itself obsolete: [`docs/files-rw-security-test.md`](https://github.com/colinrgodsey/wackypub/blob/3b65cdcd6b3322c540e4b0950de5232408f4e711/docs/files-rw-security-test.md).

Every security-relevant tool is tracked in a 3-state checklist ([`.agents/SECURITY_TESTING.md`](.agents/SECURITY_TESTING.md)): untested, tested-and-clean, or tested-with-a-finding. A tool's state resets to untested the instant its enforcement logic changes, so a passing grade can never silently go stale, and a finding stays on record - report and all - even after it's fixed, superseded by a dated follow-up rather than quietly erased.

---

## Use cases

- **Agent swarms** — a coordinator agent delegating to specialist agents (a researcher, a writer, a critic), each with its own tools and skills, talking to each other through the same authorized-invocation mechanism.
- **Tool-calling evaluation and coherency testing** — stand up an agent whose entire job is stress-testing your own tools and reporting back on what confused it (this is genuinely how several real bugs in this project were found).
- **Personal automation with real system tools** — symlink a toolset of read-only (or read-write, if you trust it) system utilities into an agent's `tools/` folder and let it operate your machine within whatever boundary you've drawn.
- **Multi-character roleplay and narrative campaigns** — each character is its own agent with its own memory and voice; a narrator or player can interview them, and they can interview each other.
- **Distilled, reusable knowledge across agents** — write a skill once (how to use a particular CLI, a house style guide, domain-specific guidance), symlink it into every agent that needs it, and update it in one place.

---

## Recommended Tooling

A few tools worth linking into `tools/` for a real workspace, not just the demo:

- **[files-rw](https://github.com/colinrgodsey/files-rw)** (recommended) — our own file access/editing suite, gated by a per-directory allowlist. Vendored here as a git submodule at `tools/files-rw`; see [Security](#security) above for its actual track record, not just a claim.
- **[wackyproc](https://github.com/colinrgodsey/wackyproc)** (recommended) — our own zero-daemon background process manager, for spawning long-running work (builds, test suites, servers) that would otherwise block a whole turn on `run_command`, and checking back on it across turns. Vendored here as a git submodule at `tools/wackyproc`.
- **`wackypub` itself** (recommended) — yes, really. Linking `wackypub` into an agent's own `tools/` is what makes agent-to-agent calling possible in the first place (see [Philosophy](#philosophy)) - not a special integration, just another executable an agent happens to invoke. Necessary for any workspace that wants real cross-agent orchestration, not optional the way the rest of this list is.
- **[QMD](https://github.com/tobi/qmd)** — on-device search built by Tobi Lütke (Shopify), giving agents local RAG over Markdown, notes, and docs: BM25 keyword search, vector semantic search, and LLM re-ranking, all in one local binary.
- **[ast-grep](https://github.com/ast-grep/ast-grep)** — structural code search and rewriting via AST matching instead of regex.
- **[playwright-cli](https://github.com/microsoft/playwright-cli)** — a CLI-native wrapper around Playwright browser automation, built specifically for terminal coding agents.
- **[mcporter](https://github.com/openclaw/mcporter)** — auto-discovers and invokes MCP tools directly from the command line. Since `tools/` will run anything that's an executable, this means the entire MCP ecosystem becomes usable here with zero wackypub-side integration work.
- **[acpx](https://github.com/openclaw/acpx)** — runs headless ACP coding sessions from the CLI, so an agent can delegate whole coding tasks to another coding agent (Claude Code and friends) without a human in the loop.
- **[GitKB](https://gitkb.com/)** — git-backed knowledge base for decision records and task tracking, with CLI integrity checking (`git-kb fsck`). This repo's internal decision log runs on it; recommended for contributors whose agents can use it. If you don't have GitKB, document your decisions fully in the PR description instead.
- **[roller](https://github.com/dice-roller/cli)** — dice-notation CLI. Good for RP use cases, but also just genuine random decision-making in general, which LLMs can be surprisingly bad at on their own.

All tools execute from the agent's own workspace directory as the CWD.

---

## Repository Architecture

`cmd/` holds the CLI (Cobra subcommands, a thin wrapper), `pkg/agent/` holds the actual SDK (`AgentSDK`, `FolderAgent`, the OpenAI-compatible model adapter, tools, skills, scratchpad, macros, compaction, git versioning), `pkg/config/` handles `wackypub.yaml`.

For the full architecture reference (schemas, lifecycle, compaction mechanics, reasoning handling), see [`docs/agents.md`](docs/agents.md). For orientation when working in this repo, see [`.agents/AGENTS.md`](.agents/AGENTS.md) and the numbered design decisions in [`.agents/DECISIONS.md`](.agents/DECISIONS.md).

---

## Installation & Prerequisites

* **Go**: `go 1.25.7+`

```bash
go build -o wackypub .
```

---

## Agent Folder Structure (`<ws_dir>/<agent_id>/`)

Workspace and agent folder layout, setup steps, and git versioning are all covered in [`skills/wackypub-ws/SKILL.md`](skills/wackypub-ws/SKILL.md) - it's written to be agent-readable, but it's plain Markdown and just as readable by you.

For a working `runtime.json` to copy, see [`examples/runtimes/`](examples/runtimes); for the full field list (including reasoning/thinking-related settings), see [`docs/agents.md`](docs/agents.md).

`AGENTS.md`:

```markdown
# Character Agent
You are a character in a medieval setting.

@IDENTITY.md

@../rules/conduct.md
```

---

## CLI Usage

Every command is self-documenting — run `wackypub [command] --help` at any level for exact arguments, flags, and preconditions.

```
$ wackypub --help
```

```bash
# Diagnose the whole workspace, or one agent, without changing anything
./wackypub --ws my_workspace workspace
./wackypub --ws my_workspace workspace my_agent

# Add a user turn, or add-and-generate atomically (the recommended way to drive a turn)
./wackypub agent add my_agent "Greetings! What rumors have you heard?"
./wackypub agent prompt my_agent "Tell me about the hidden treasure."

# Inspect without mutating anything
./wackypub agent read-session my_agent
./wackypub agent read-memory my_agent
./wackypub agent render-prompt my_agent

# Manually trigger compaction (normally automatic during generate/prompt)
./wackypub agent compact my_agent

# Strip stale reasoning/thought signatures (OpenRouter encrypted blocks, Gemini
# ThoughtSignature) after switching models or providers
./wackypub agent strip-signatures my_agent

# Stash data out-of-band instead of paying tokens to move it through a turn -
# positional argument, --message flag, or stdin all work the same way
echo "a large log or payload that doesn't need to cost tokens to move around" | ./wackypub agent scratchpad create my_agent
./wackypub agent scratchpad read my_agent <entry-id>
```

`wackypub agent <cmd> <agent_id>` and `wackypub agent <agent_id> <cmd>` are both supported for real invocation (though `--help` only resolves correctly with the subcommand name first — put it right after `agent`).

This is a curated slice, not the full surface — `workspace`, `trace`, and `scratchpad` all keep growing. `--help` at any level is the actual source of truth, not this list.

---

## Testing

```bash
go test ./...
```

For live/manual testing against a real backend and the techniques developed for it (tracing tool-calling exchanges, reproducing lock contention, verifying wire payloads without spending API credits), see [`.agents/LOCAL_TESTING.md`](.agents/LOCAL_TESTING.md).

---

## License

[MIT](LICENSE)
