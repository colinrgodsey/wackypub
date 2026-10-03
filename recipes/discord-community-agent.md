---
name: discord-community-agent
title: Long-Lived Discord Community Agent
confinement: container-required
skills: [wackypub-ws, wackypub-a2a, wackyproc, scratchpad-efficiency]
tools: [wackypub, wackyproc, bash, files-rw]
---

# Long-Lived Discord Community Agent

A persistent agent bound to a Discord channel: it listens for messages, responds with
its persona, and can dispatch async work to peer agents in the same workspace. This
is the shape the swarm itself runs as a community bot.

> **Confinement: container-required.** This agent is long-lived, talks to the world,
> and (via its peers) can exercise programming tools. Run the whole workspace inside
> the repo's Docker image (or a bespoke container with `wackypub`, `wackyproc`,
> `files-rw`, and the Discord bridge). The container holds the bindings, the cron
> wiring, and the API keys.

> Prerequisite: the wackypub Docker image (see root README `docker compose up`), or a
> host with `wackypub`, `wackyproc`, `files-rw`, and the Discord bridge binary.

## 1. Create the workspace + two agents

Run this from your wackypub checkout (or anywhere you want the workspace to live).
It defines the absolute paths every later block uses, so each block works no matter
which directory you are in:

```bash
export REPO="$PWD"                # your wackypub checkout (skills/ lives here)
export WS="$PWD/ws"               # absolute workspace dir
export FILESRW_REPO="$HOME/files-rw"      # your files-rw CHECKOUT (skills/files-rw)
export WACKYPROC_REPO="$HOME/wackyproc"  # your wackyproc CHECKOUT (skills/wackyproc)
mkdir -p "$WS/community" "$WS/worker" && touch "$WS/WACKYPUB_ROOT"
```

A community agent usually needs a peer to hand long jobs to (the community agent
stays quick to respond; the worker does the heavy lifting).

## 2. Runtimes + API key

`$WS/.env` holds the shared API keys (model key + Discord bot token). Both agents get
a `runtime.json`; the community agent wants a fast, low-cost model (it answers in
channel), the worker can use a stronger model.

```bash
cat > "$WS/.env" <<'EOF'
OPENROUTER_API_KEY=sk-or-v1-xxxx
DISCORD_BOT_TOKEN=xxxx
EOF
```

## 3. Wire the community agent's tools

The community agent needs to run the bridge process (via wackypub) and to dispatch
async work (via wackyproc). Its `tools/` below stays a fixed, hand-picked set of
four commands (no arbitrary scripting beyond the binaries it is given) — a narrow
capability surface compared to the worker:

```bash
mkdir -p "$WS/community/tools" "$WS/worker/tools"
ln -s "$(command -v wackypub)"    "$WS/community/tools/wackypub"
ln -s "$(command -v wackyproc)"   "$WS/community/tools/wackyproc"
ln -s "$(command -v bash)"        "$WS/community/tools/bash"
ln -s "$(command -v files-rw)"    "$WS/community/tools/files-rw"
# worker gets the full coding-agent toolset; see coding-agent.md
ln -s "$(command -v bash)"        "$WS/worker/tools/bash"
ln -s "$(command -v files-rw)"    "$WS/worker/tools/files-rw"
ln -s "$(command -v wackyproc)"   "$WS/worker/tools/wackyproc"
ln -s "$(command -v wackypub)"    "$WS/worker/tools/wackypub"
```

## 4. Grant file access

```bash
cat > "$WS/community/FILES_RW_ACCESS" <<'EOF'
w: ./
EOF
cat > "$WS/worker/FILES_RW_ACCESS" <<'EOF'
w: ../
EOF
```

files-rw rules are `r: <path>` / `w: <path>` with literal paths (no globs), resolved
against the agent's own directory. The community agent gets `w: ./` - only its own
folder - keeping its capability surface narrow; the worker gets `w: ../` (the
workspace root) so it can do real work across the workspace. `w:` grants read +
write together; `r:` alone is read-only.

## 5. Allow the community agent to call the worker

A2A is gated by the caller's `WACKYPUB_ALLOWED_AGENTS` file:

```bash
cat > "$WS/community/WACKYPUB_ALLOWED_AGENTS" <<'EOF'
worker
EOF
cat > "$WS/worker/WACKYPUB_ALLOWED_AGENTS" <<'EOF'
community
EOF
```

## 6. Install the skills

```bash
mkdir -p "$WS/community/skills" "$WS/worker/skills"
ln -s "$REPO/skills/wackypub-ws"            "$WS/community/skills/wackypub-ws"
ln -s "$REPO/skills/wackypub-a2a"           "$WS/community/skills/wackypub-a2a"
ln -s "$REPO/skills/scratchpad-efficiency"  "$WS/community/skills/scratchpad-efficiency"
ln -s "$WACKYPROC_REPO/skills/wackyproc"    "$WS/community/skills/wackyproc"
ln -s "$FILESRW_REPO/skills/files-rw"       "$WS/community/skills/files-rw"
ln -s "$REPO/skills/wackypub-ws"            "$WS/worker/skills/wackypub-ws"
ln -s "$REPO/skills/scratchpad-efficiency"  "$WS/worker/skills/scratchpad-efficiency"
ln -s "$WACKYPROC_REPO/skills/wackyproc"    "$WS/worker/skills/wackyproc"
ln -s "$FILESRW_REPO/skills/files-rw"       "$WS/worker/skills/files-rw"
```

## 7. Verify both agents

```bash
cd "$WS" && wackypub workspace community
cd "$WS" && wackypub workspace worker
```

Both should report their `tools/`, `skills/`, `runtime.json` present as configured (the workspace-level `.env` stash is not printed by the inspect); `AGENTS.md`/`MEMORY.md`/`session.jsonl` are reported missing until the
first turn. (`WACKYPUB_ALLOWED_AGENTS` shows present with 1 allowed ID - the peer
in the other direction - and `FILES_RW_ACCESS` is exercised by the smoke tests, not
printed by workspace inspection.)

Smoke-test the A2A path:

```bash
cd "$WS" && wackypub agent community prompt \\
  "Ask the worker agent to report its model and tool list, using the wackypub-a2a skill."
```

## 8. Bind to Discord

With the Discord bridge configured for the workspace (per the bridge's own docs, or a
follow-up recipe), bind the community agent to the channel and start the bridge. The
agent then answers in-channel; long jobs it farms out to `worker` via async dispatch
(`wackyproc run wackypub agent worker prompt --async`), inbox-style, so the channel
stays responsive.

## What you have

- A long-lived community agent on Discord, persona-bound, with a fast model.
- A worker peer for heavy lifting, dispatched asynchronously without blocking the
  channel.
- A2A authorization between the two (allowlist-gated), containerized.

## Next steps

- More peers: extend `WACKYPUB_ALLOWED_AGENTS` lists, add agents per the
  [coding-agent](coding-agent.md) recipe.
- Keep history: run the workspace in git, rely on compaction for long sessions.
