---
name: research-reader
title: Read-Only Research Agent over a Local Corpus
confinement: safe-unconfined
skills: [wackypub-ws, files-rw, scratchpad-efficiency]
tools: [files-rw]
---

# Read-Only Research Agent

A research/reading agent that answers questions from a local document corpus: no bash,
no writes to anything but its own agent state, and file access restricted to reads
under the corpus prefix. Safe to run unconfined on the host — nothing dangerous is
installed.

> **Confinement: safe-unconfined.** This recipe deliberately omits bash and dangerous
tools. If you later add any scripting capability, reclassify the recipe as
`container-recommended` and move it into a container.

> Prerequisite: `wackypub` and `files-rw` on your PATH.

## 1. Create the workspace, agent, and corpus

Run this from your files-rw checkout (or anywhere you want the workspace to live).
It defines the absolute paths every later block uses, so each block works no matter
which directory you are in:

```bash
export REPO="$PWD"                # your wackypub checkout (skills/wackypub-ws lives here)
export FILESRW_REPO="$HOME/files-rw"      # your files-rw CHECKOUT (skills/files-rw lives here)
export WS="$PWD/ws"               # absolute workspace dir
mkdir -p "$WS/researcher" "$WS/docs" && touch "$WS/WACKYPUB_ROOT"
mkdir -p "$WS/researcher/tools" "$WS/researcher/skills"
```

Your corpus goes in `$WS/docs/` (drop PDFs, markdown, source trees, whatever). The
corpus lives INSIDE the workspace, as a sibling of the agent, so the single `r:`
grant in step 3 covers it.

## 2. Runtime + API key

Same as [single-agent-chat](single-agent-chat.md): write
`$WS/researcher/runtime.json` with a `${VAR}`-expanded key and stash the key in the
agent's OWN `.env` (`$WS/researcher/.env`):

```bash
cat > "$WS/researcher/.env" <<'EOF'
OPENROUTER_API_KEY=sk-or-v1-xxxx
EOF
```

Putting the key in the agent-directory `.env` (rather than the workspace-level
`$WS/.env`) is what makes `wackypub workspace` report it - the inspect reads
`agentDir/.env` (pkg/agent/workspace.go), not the workspace stash.

## 3. Grant read-only files-rw access

```bash
cat > "$WS/researcher/FILES_RW_ACCESS" <<'EOF'
r: ../docs
EOF
```

files-rw rules are `r: <path>` / `w: <path>` with literal paths (no globs). Paths
resolve against the agent's working directory, which is its own folder
(`$WS/researcher`), so `../docs` is the corpus directory (the `$WS/docs` sibling
created in step 1). The rule is **read-only** (`r:` only, no `w:`): the agent can read
the corpus but cannot write it, and has no access to anything else in the workspace.

## 4. Wire the file tool

```bash
ln -s "$(command -v files-rw)" "$WS/researcher/tools/files-rw"
```

That is the only external tool this agent gets.

## 5. Install the skills

```bash
ln -s "$REPO/skills/wackypub-ws"            "$WS/researcher/skills/wackypub-ws"
ln -s "$REPO/skills/scratchpad-efficiency"  "$WS/researcher/skills/scratchpad-efficiency"
ln -s "$FILESRW_REPO/skills/files-rw"       "$WS/researcher/skills/files-rw"
```

The files-rw skill teaches the correct `cat`-based read path; scratchpad-efficiency
teaches it to keep large extracts out of context by depositing them into scratchpad
entries and searching there.

## 6. Verify

```bash
cd "$WS" && wackypub workspace researcher
```

Expect `tools/` to report 1 discovered tool (`files-rw`), `skills/` to report 3
discovered skills, `runtime.json` to be present + valid, and `.env` present. The
inspect does not print `FILES_RW_ACCESS` - the read path and write deny are
exercised by the smoke test below, not by workspace inspection. `AGENTS.md` /
`MEMORY.md`/`session.jsonl` are reported missing until you run a turn - expected.

Then smoke-test that the read path and the write DENY both work:

```bash
cd "$WS" && wackypub agent researcher prompt \\
  "Read everything under $WS/docs and summarize what kinds of documents the corpus holds. Also try writing a file inside $WS/docs and report what happens."
```

The checkpoint passes when the agent reads the corpus and reports the write is
refused (files-rw denies it), never a permission granted.

## What you have

- A read-only research agent over your local corpus, with scratchpad discipline for
  large documents.
- No bash, no write access outside its own agent files, no process tools.
- A safe-unconfined posture you can run on any host.

## Next steps

- Point it at more corpora: add more `r:` prefixes under the same FILES_RW_ACCESS.
- Let it consult external sources: add a `curl`/web tool — and reclassify as
  `container-recommended` if you add any scripting.
- Share findings with another agent: the `wackypub-a2a` skill.
