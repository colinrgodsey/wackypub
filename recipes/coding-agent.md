---
name: coding-agent
title: Full-Service Coding Agent
confinement: container-recommended
skills: [wackypub-ws, files-rw, wackyproc, scratchpad-efficiency]
tools: [bash, files-rw, wackyproc, wackypub]
---

# Full-Service Coding Agent Workspace

The workhorse setup: one agent with bash, scoped file access, and background-process
supervision so it can edit, build, run, and test code. This is the shape this swarm
itself uses for its engineering agents.

> **Confinement: container-recommended.** The agent gets bash and programming tools,
> so run this recipe in a container (the repo's Docker image is the primary venue).
> A director agent operating inside the container can set it up for you; outside the
> container, prefer a bespoke container for the recipe.

> Prerequisite: `wackypub`, `files-rw`, and `wackyproc` on your PATH (see each repo's
> README Install section — `go install github.com/colinrgodsey/<tool>@latest`).

## 1. Create the workspace + agent

Run this from your wackypub checkout (or anywhere you want the workspace to live).
It defines the absolute paths every later block uses, so each block works no matter
which directory you are in:

```bash
export REPO="$PWD"                # your wackypub checkout (skills/ lives here)
export WS="$PWD/ws"               # absolute workspace dir
# Point these at your files-rw and wackyproc CHECKOUTS so the skill links resolve
# (the binaries on PATH are not enough - the skills/ folder ships in each repo):
export FILESRW_REPO="$HOME/files-rw"      # e.g. /home/you/src/files-rw
export WACKYPROC_REPO="$HOME/wackyproc"  # e.g. /home/you/src/wackyproc
mkdir -p "$WS/coder" && touch "$WS/WACKYPUB_ROOT"
```

Adjust `REPO`, `FILESRW_REPO`, `WACKYPROC_REPO` if your checkouts live elsewhere.

## 2. Runtime + API key

Same as the [single-agent-chat](single-agent-chat.md) recipe: write
`$WS/coder/runtime.json` pointing at your provider/model with a `${VAR}`-expanded API
key, and stash the key in `$WS/.env`:

```bash
cat > "$WS/.env" <<'EOF'
OPENROUTER_API_KEY=sk-or-v1-xxxx
EOF
```

## 3. Gate the tools

Symlink the four capability binaries into `$WS/coder/tools/`:

```bash
mkdir -p "$WS/coder/tools"
ln -s "$(command -v bash)"      "$WS/coder/tools/bash"
ln -s "$(command -v files-rw)"  "$WS/coder/tools/files-rw"
ln -s "$(command -v wackyproc)" "$WS/coder/tools/wackyproc"
ln -s "$(command -v wackypub)"  "$WS/coder/tools/wackypub"
```

These four are the recommended minimum. `bash` gives the agent the universal
fallback; `files-rw` is the safe, allowlist-gated file tool; `wackyproc` gives it
background process supervision; `wackypub` lets it dispatch to other agents later.

## 4. Grant files-rw access

`files-rw` refuses to do anything until it sees a `FILES_RW_ACCESS` grant file in the
agent directory. The grant is a simple allowlist — see the files-rw skill for the full
syntax. Grant for editing inside the workspace:

```bash
cat > "$WS/coder/FILES_RW_ACCESS" <<'EOF'
w: ../
EOF
```

files-rw rules are `r: <path>` / `w: <path>` with literal paths (no globs). Paths
resolve against the agent's working directory, which is its own folder
(`$WS/coder`), so `../` is the workspace root and `w:` grants read + write together.
A `w:` rule is the whole-workspace grant; use `r:` alone for read-only, or a more
specific prefix to tighten.

## 5. Install the skills

The coding agent needs the workspace, file, process, and scratchpad skills:

```bash
mkdir -p "$WS/coder/skills"
ln -s "$REPO/skills/wackypub-ws"            "$WS/coder/skills/wackypub-ws"
ln -s "$REPO/skills/scratchpad-efficiency"  "$WS/coder/skills/scratchpad-efficiency"
ln -s "$FILESRW_REPO/skills/files-rw"       "$WS/coder/skills/files-rw"
ln -s "$WACKYPROC_REPO/skills/wackyproc"    "$WS/coder/skills/wackyproc"
```

## 6. Verify

```bash
cd "$WS" && wackypub workspace coder
```

Expect `tools/` to report 4 discovered tools, `skills/` to report 4 discovered
skills, and `runtime.json` to be present + valid. Notes: the inspect does not print
`FILES_RW_ACCESS` (it is exercised by the smoke test below, not by workspace
inspection), nor does it report the workspace-level `.env` stash your key lives in
(it only shows an agent-directory `.env`); and `AGENTS.md`/`MEMORY.md`/
`session.jsonl` are reported missing until you run a turn - that is expected at
this point.

Then smoke-test the capabilities:

```bash
cd "$WS" && wackypub agent coder prompt \\
  "Run a quick sanity check: create $WS/coder-smoke.txt with files-rw, list it back, and tell me what you did."
```

## 7. Optional: background builds with wackyproc

Builds and test suites are long-running; tell the agent it may use `wackyproc run`
for them and poll with `wackyproc wait <id>` (explicit-ID form: `wait [seconds]
<proc_id> [proc_id...]`, returns the first listed process to finish) then
`wackyproc get <id>`. The wackyproc skill covers the full lifecycle (run, list,
wait, get, stop, prune).

## What you have

- A coding agent that can read/write files (allowlist-gated), run arbitrary bash,
  background long jobs, and manage its own scratchpad.
- The bundled skills teach it the workspace model, file-tool discipline, process
  management, and zero-token scratchpad patterns.
- A `wackypub` link in `tools/` so it can grow into A2A later.

## Next steps

- Add peer agents: `WACKYPUB_ALLOWED_AGENTS` lists + the `wackypub-a2a` skill.
- Make it long-lived behind Discord: the [discord-community-agent](discord-community-agent.md)
  recipe, using this agent as the bound persona.
- Add git-versioned history: run the agent workspace in a git repo, or read the
  wackypub-ws skill's git/manifest sections.
