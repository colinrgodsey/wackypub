---
name: assistant-agent
title: Long-Lived Personal Assistant
confinement: safe-unconfined
skills: [wackypub-ws, assistant-capabilities, memory-system, security-safety, execution-discipline, files-rw, wackyproc]
tools: [files-rw, wackyproc]
---

# Long-Lived Personal Assistant

A persistent assistant with a real identity, a tiered memory system, and explicit
safety/execution discipline - the shape for an agent a person talks to across many
sessions and expects to remember them, not a one-shot task runner. No bash: file
writes (identity updates, memory, daily logs) go through `files-rw`, scoped to the
agent's own directory. Safe to run unconfined on the host.

> **Confinement: safe-unconfined.** This recipe deliberately omits bash. If you
> later add bash or any other scripting capability, reclassify as
> `container-recommended` and move it into a container - see the
> [coding-agent](coding-agent.md) recipe.
>
> Step 12's `wackyproc` is not a shell and does not trigger that reclassification: it
> backgrounds only binaries discovered in `tools/`, with no `PATH` fallback.

> Prerequisite: `wackypub` and `files-rw` on your PATH (each repo's README Install
> section — `go install github.com/colinrgodsey/<tool>@latest`).

## 1. Create the workspace + agent

Run this from your wackypub checkout (or anywhere you want the workspace to live).
It defines the absolute paths every later block uses, so each block works no matter
which directory you are in:

```bash
export REPO="$PWD"                   # your wackypub checkout (skills/ lives here)
export FILESRW_REPO="$HOME/files-rw" # your files-rw CHECKOUT (skills/files-rw lives here)
export WS="$PWD/ws"                  # absolute workspace dir
mkdir -p "$WS/assistant/memory" "$WS/assistant/work" && touch "$WS/WACKYPUB_ROOT"
mkdir -p "$WS/assistant/tools" "$WS/assistant/skills" "$WS/assistant/hooks/on-user-message"
```

Adjust `REPO`/`FILESRW_REPO` if your checkouts live elsewhere.

## 2. Runtime + API key

Same pattern as [single-agent-chat](single-agent-chat.md): write
`$WS/assistant/runtime.json` pointing at your provider/model with a
`${VAR}`-expanded API key, and stash the key in the agent's OWN `.env`
(`$WS/assistant/.env` - this is what makes `wackypub workspace` report it, per
`pkg/agent/workspace.go`):

```json
{
  "provider": "openai",
  "endpoint": "https://openrouter.ai/api/v1",
  "model": "anthropic/claude-3.5-sonnet",
  "apiKey": "${OPENROUTER_API_KEY}"
}
```

```bash
cat > "$WS/assistant/.env" <<'EOF'
OPENROUTER_API_KEY=sk-or-v1-xxxx
EOF
```

## 3. Grant files-rw access

The assistant only ever needs to read and write its own directory - identity files,
`MEMORY.md`, `WORKBOOK.md`, and the `memory/`/`work/` subdirectories all live there.
Grant exactly that, nothing wider:

```bash
cat > "$WS/assistant/FILES_RW_ACCESS" <<'EOF'
w: .
EOF
```

A `w:` rule grants read + write together, and (per the `files-rw` skill) covers the
whole subtree under the path, not just that single entry - so this one line covers
`memory/` and `work/` too. Paths resolve against the agent's own working directory
(`$WS/assistant`), so `.` is "my own folder and everything under it" - deliberately
tighter than the workspace-wide `w: ../` grant the [coding-agent](coding-agent.md)
recipe uses, since this agent has no reason to touch anything outside itself.

## 4. Gate the tool

```bash
ln -s "$(command -v files-rw)" "$WS/assistant/tools/files-rw"
```

No `bash`, no `wackyproc`, no `wackypub` - this agent's only external capability is
scoped file access. [Step 12](#12-optional-background-long-commands-and-get-woken)
adds `wackyproc` later if you want background tasks, and nothing before it needs it.

## 5. Install the skills

```bash
ln -s "$REPO/skills/wackypub-ws"            "$WS/assistant/skills/wackypub-ws"
ln -s "$REPO/skills/assistant-capabilities" "$WS/assistant/skills/assistant-capabilities"
ln -s "$REPO/skills/memory-system"          "$WS/assistant/skills/memory-system"
ln -s "$REPO/skills/security-safety"        "$WS/assistant/skills/security-safety"
ln -s "$REPO/skills/execution-discipline"   "$WS/assistant/skills/execution-discipline"
ln -s "$FILESRW_REPO/skills/files-rw"       "$WS/assistant/skills/files-rw"
```

The four assistant skills are `always_load: true` - together they ARE this agent's
behavioral core (identity/memory discipline, safety boundaries, capabilities,
execution/communication discipline), the same way `wackypub-a2a` and
`scratchpad-efficiency` are always-loaded mechanics for other agents. `wackypub-ws`
stays `always_load: false` (on-demand reference) like it does everywhere else.

## 6. Write the identity files

These four are the agent's actual persona - fill in real values before talking to
it for real. What ships here is a template with placeholders; `memory-system`
explains the tier model (identity/soul/brain all live in the always-in-context
prefix - editing them later re-prices your whole context, so get the shape right
now and change the content deliberately afterward, not value by value):

```bash
cat > "$WS/assistant/IDENTITY.md" <<'EOF'
# Identity

- Name: <your name here>
- Handles/aliases: <none yet>
- Fixed policy: <anything that never changes about who this agent is>
EOF

cat > "$WS/assistant/SOUL.md" <<'EOF'
# Soul

- Values: <what this agent cares about>
- Personality: <tone, how it talks>
- Behavioral policy: <anything it should always/never do>
EOF

cat > "$WS/assistant/USER.md" <<'EOF'
# User

- Name: <the user's name here>
- What the assistant should know about them: <context, preferences, nothing sensitive you would not want in every prompt>
EOF

cat > "$WS/assistant/BRAIN.md" <<'EOF'
# BRAIN.md - What I Have Learned

Mutable learned layer: process, procedure, and policy that cannot afford to be
forgotten. See the memory-system skill for the full tier model and when to write
here versus WORKBOOK.md or a dated memory/ entry.

(Empty at setup - the agent fills this in as it learns.)
EOF
```

## 7. Write the AGENTS.md linkage file

`AGENTS.md` is infrastructure, not content - it only links the four files above.
`memory-system` tells the agent never to edit it; lessons go in `BRAIN.md` instead:

```bash
cat > "$WS/assistant/AGENTS.md" <<'EOF'
# AGENTS.md - Your Workspace

This folder is home. Treat it that way. Never update AGENTS.md.

@IDENTITY.md

@SOUL.md

@USER.md

@BRAIN.md
EOF
```

## 8. Scaffold MEMORY.md and WORKBOOK.md

`MEMORY.md` is the session-compaction target (see `docs/agents.md` §7); start it
empty so the first real compaction has a clean document to write into. `WORKBOOK.md`
is the cheap-to-edit top-of-mind file `memory-system` describes:

```bash
cat > "$WS/assistant/MEMORY.md" <<'EOF'
# Memory

(Nothing recorded yet.)
EOF

cat > "$WS/assistant/WORKBOOK.md" <<'EOF'
# Workbook

Current priorities and active threads go here. Cheap to add and remove.
EOF
```

## 9. Install the date hook

`memory-system` dates its daily archive files (`memory/YYYY-MM-DD.md`) and expects
the agent to know today's date reliably - the repo ships an example hook for
exactly this:

```bash
cp "$REPO/examples/hooks/on-user-message/00-date" "$WS/assistant/hooks/on-user-message/00-date"
chmod +x "$WS/assistant/hooks/on-user-message/00-date"
```

This prepends `[Current datetime: ...]` to every inbound message - see
`docs/agents.md` §6 (Hooks) for how hook discovery works if you want to adjust it.

## 10. Verify

```bash
cd "$WS" && wackypub workspace assistant
```

Expect `tools/` to report 1 discovered tool, `skills/` to report 6 discovered
skills, `runtime.json` present + valid, `AGENTS.md` present, and `MEMORY.md`
present. `FILES_RW_ACCESS` is exercised by the smoke test below, not by the
inspect, and `session.jsonl` is reported missing until the first turn - expected.

## 11. Smoke test: memory write-back

Prove the write path actually works end to end - the agent uses `files-rw` to
write to its own `MEMORY.md`, then a second turn proves it can read the result
back without being told what it wrote:

```bash
cd "$WS" && wackypub agent assistant prompt \
  "Using files-rw, append a line to the end of MEMORY.md that says: smoke test recorded at $(date -Iseconds). Then tell me you did it."

cd "$WS" && wackypub agent assistant prompt \
  "What does MEMORY.md currently say? Read it with files-rw and quote the line back to me."
```

The second turn's answer should quote the exact line the first turn wrote - if it
does, the write path (tool -> grant -> file) and the read path both work.

## 12. Optional: background long commands and get woken

The core recipe has no `wackyproc` on purpose. Add it when this agent starts issuing
commands that outlast a turn, and it becomes fire-and-forget: the command goes to a
background job, the turn ends, and a hook wakes the agent when the job finishes so it
reads the result on its own. Two example scripts from the wackyproc repo do the whole
thing; nothing about the core recipe changes.

```bash
export WACKYPROC_REPO="$HOME/wackyproc"   # your wackyproc checkout
ln -s "$(command -v wackyproc)" "$WS/assistant/tools/wackyproc"
ln -s "$WACKYPROC_REPO/skills/wackyproc" "$WS/assistant/skills/wackyproc"

mkdir -p "$WS/assistant/hooks/pre-run-command" "$WS/assistant/hooks/on-proc-complete"
cp "$WACKYPROC_REPO/examples/hooks/pre-run-command/10-auto-defer" \
   "$WS/assistant/hooks/pre-run-command/10-auto-defer"
cp "$WACKYPROC_REPO/examples/hooks/on-proc-complete/10-wake-agent" \
   "$WS/assistant/hooks/on-proc-complete/10-wake-agent"
chmod +x "$WS/assistant/hooks/pre-run-command/10-auto-defer" \
   "$WS/assistant/hooks/on-proc-complete/10-wake-agent"
```

The wake talks to the agent from the outside, so it needs the two `wackypub`
requirements for an agent messaging itself:

```bash
echo assistant >> "$WS/assistant/WACKYPUB_ALLOWED_AGENTS"
```

Self-targeting is denied unless your own id is in that list, and the script strips
both chain carriers (`AGENT2AGENT`, `WACKYPROC_CALL_CHAIN`) before dispatching - if
either survives, the chain of the turn that started the job is still live, the wake
reads as a cycle, and the notification is lost. Leaving `wackypub` off `tools/`
keeps it out of the agent's own tool list: the wake calls it from `PATH`, host-side.

**The marker, not a heuristic.** The default policy defers only calls the agent
explicitly marks, so nothing about ordinary tool calls changes:

```bash
echo 'WACKYPROC_AUTODEFER_POLICY=marker' >> "$WS/assistant/.env"
```

`marker` is what an unset value means already; writing it down just makes the choice
visible. Accepted values are `off` (kill switch), `marker`, `heuristic` (a stated
list of build/test commands - `go test`, `cargo build`, `npm install`, and friends),
and `always`. The marker itself is per call, in `run_command`'s `env`:

```json
{"WACKYPROC_BG": "1"}
```

`1`, `true`, `yes` and `on` all count. A deferred call never runs inline as well: the
hook short-circuits it and the agent sees the job id plus the retrieval verbs
(`wackyproc get`, `wait`, `peek`), which is why the recipe does not need the agent to
have read a manual for this to be usable. Anything the hook cannot do - no python3,
no `wackyproc` binary, spawn failure - exits silently and the command runs inline as
if the hook were absent. `wackyproc` and `wackypub` calls are never deferred, so A2A
semantics are untouched.

**Verify both halves.** `wackypub agent hooks` lists only the files discovery would
actually execute, so it catches the two usual mistakes (missing exec bit, wrong
directory) rather than printing what you meant to install:

```bash
cd "$WS" && wackypub agent hooks assistant
```

Expect both scripts. Then prove the wake end to end from the host, where the job
runs as this user:

```bash
cd "$WS/assistant" && wackyproc run files-rw access
cd "$WS" && wackypub agent read-session assistant
```

The session should gain a turn whose opening message is the completion notice,
followed by the agent's reply about the finished job. Nothing appearing means the
self-dispatch grant above is missing; that is the failure this step is easy to get
wrong.

**What opt-in costs you, honestly.** Hooks are executed by `wackypub` on the host,
not inside the agent's tool sandbox, so paths in them are host paths and a container
added later does not confine them. `wackyproc` jobs are host processes too: they see
the filesystem as the agent's user, not through the mounts the agent's own shell has,
and they keep running after the turn ends. The wake fires only when the agent's
session lock is free - a busy agent is skipped, not queued, and the job stays
un-notified until a later `wackyproc get`/`wait` read or the
`end-of-turn` piggyback example (`$WACKYPROC_REPO/examples/hooks/end-of-turn/`)
surfaces it. See `docs/agents.md` section 6 for how hook discovery, the action
contract, and the `pre-run-command` payload work; the [coding-agent](coding-agent.md)
recipe shows the same `wackyproc` install without the wake.

## What you have

- A long-lived assistant with a real identity (`IDENTITY.md`/`SOUL.md`), a tiered
  memory system (`BRAIN.md`/`WORKBOOK.md`/`memory/`/`MEMORY.md`), and explicit
  security and execution-discipline boundaries - all always-loaded, not optional
  reading.
- Scoped, allowlist-gated write access to its own directory only - no bash, no
  access to anything else on the host.
- A reliable notion of "today" via the date hook, which the memory system's daily
  archive convention depends on.

## Next steps

- Give it real capabilities: follow the [coding-agent](coding-agent.md) recipe's
  tool-gating steps if it needs bash/process access later (reclassify confinement
  to `container-recommended` first).
- Put it behind a chat surface: the
  [discord-community-agent](discord-community-agent.md) recipe uses this same
  identity/memory shape as its base.
- Set up proactive heartbeats: `assistant-capabilities` covers the pattern (a cron
  job prompting the agent to read a `HEARTBEAT.md`), not wired by default here.
