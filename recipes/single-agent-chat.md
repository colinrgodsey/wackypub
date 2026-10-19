---
name: single-agent-chat
title: One Agent, Minimal Toolset
confinement: safe-unconfined
skills: [wackypub-ws]
tools: []
---

# Single-Agent Chat Workspace

The smallest workspace that is still a real WackyPub agent: one agent directory, one
runtime, no external tools, no skills beyond the bundled workspace guide. The agent
can hold a conversation and use the built-in scratchpad and skill-loader — nothing
else. Safe to run unconfined on the host: no bash, no file access, no process tools.

> Prerequisite: `wackypub` on your PATH. If not, see the root README Installation
> section (`go install github.com/colinrgodsey/wackypub@latest`).

## 1. Create the workspace + agent

Run this from your wackypub checkout (or anywhere you want the workspace to live).
It defines the absolute paths every later block uses, so each block works no matter
which directory you are in:

```bash
export REPO="$PWD"                # your wackypub checkout (skills/wackypub-ws lives here)
export WS="$PWD/ws"               # absolute workspace dir
mkdir -p "$WS/chat" && touch "$WS/WACKYPUB_ROOT"
```

Adjust `REPO` if you are not running from the checkout (e.g. `export REPO=/path/to/wackypub`).

## 2. Wire a runtime

Every agent needs a `runtime.json` naming a provider, model, and API key. The key is
read from the workspace `.env` (step 3), so it never sits in plaintext in the recipe.

```json
{
  "provider": "openai",
  "endpoint": "https://openrouter.ai/api/v1",
  "model": "anthropic/claude-3.5-sonnet",
  "apiKey": "${OPENROUTER_API_KEY}"
}
```

Write that as `$WS/chat/runtime.json`. (See `examples/runtimes/` in the repo for
other provider shapes, and the `wackypub-ws` skill for the full runtime schema.)

## 3. Stash the API key

```bash
cat > "$WS/chat/.env" <<'EOF'
OPENROUTER_API_KEY=sk-or-v1-xxxx
EOF
```

Putting the key in the agent-directory `.env` (rather than the workspace-level
`$WS/.env`) is what makes `wackypub workspace` report it - the inspect reads
`agentDir/.env` (pkg/agent/workspace.go), not the workspace stash.

## 4. Install the bundled skill

```bash
mkdir -p "$WS/chat/skills"
ln -s "$REPO/skills/wackypub-ws" "$WS/chat/skills/wackypub-ws"
```

`$REPO` is your wackypub checkout — the directory whose `skills/` folder contains
`wackypub-ws` (defined in step 1).

## 5. Verify

```bash
cd "$WS" && wackypub workspace chat
```

Expect `tools/` missing (none configured), `skills/` present with 1 discovered
skill, `runtime.json` present + valid, and `.env` present. `AGENTS.md`/
`MEMORY.md`/`session.jsonl` are reported missing until you run a turn — expected.

## 6. Talk to it

```bash
cd "$WS" && wackypub agent chat prompt "Say hello and tell me what skills you can load."
```

The agent has the built-in `load_skill` tool, so it can pull up `wackypub-ws` itself
and explain the workspace model to you.

## What you have

- One agent with a real model backend and persistent `session.jsonl` history.
- The built-in in-process tools: scratchpad (`create`/`get`/`list`/`search`/`diff`),
  `load_skill`, and the skill catalog (`wackypub skill`).
- No external capability: no bash, no file access, no subprocess tool, no A2A.

## Next steps

- Add file access: follow the [coding-agent](coding-agent.md) recipe's file-access
  section (needs `container-recommended` confinement once bash is in).
- Give it a research corpus: follow [research-reader](research-reader.md).
- Let it call other agents: follow the `wackypub-a2a` skill.
