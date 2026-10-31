---
name: assistant-agent
title: Long-Lived Personal Assistant
confinement: safe-unconfined
skills: [wackypub-ws, assistant-capabilities, memory-system, security-safety, execution-discipline, files-rw, wackypub-a2a, bashc, wackyproc]
tools: [files-rw, wackypub, bash, wackyproc]
---

# Long-Lived Personal Assistant

A persistent assistant with a real identity, a tiered memory system, and explicit
safety/execution discipline - the shape for an agent a person talks to across many
sessions and expects to remember them, not a one-shot task runner. The base steps build
it with no execution capability at all: file writes (identity updates, memory, daily
logs) go through `files-rw`, scoped to the agent's own directory. Later steps add
capabilities on top of that base, and choosing among them is a security decision, not a
setup detail.

> **The containment tier is a security decision, and it belongs to the human.**
>
> Steps [2](#2-choose-the-containment-tier-human-decision), 13 and 14 decide how much
> of the machine this agent can touch. That choice must be made by the person the
> agent is being built for, not by whoever is following this recipe. Three
> prohibitions: do not choose a tier on their behalf; do not default upward because a
> higher tier is the more impressive demo; do not read silence, or "whatever you
> think", as consent.
>
> Present the decision with its concrete blast radius - what each tier can do, what it
> still cannot do, and what continues to hold when the agent is completely
> prompt-injected, because an assistant like this one ingests content it did not
> write: a fetched page, an inbound email, a peer's message, a line of its own memory
> that some earlier run recorded badly. Answer their questions, including the fine
> grained ones, by reading this repository's source - `pkg/agent/` is where the
> boundaries actually live and it is the authority on what a tool can and cannot do.
> Then get an explicit answer before wiring anything from step 13 or 14, and write the
> chosen tier and who chose it into the agent's `AGENTS.md`, so the next person to
> touch this agent does not quietly re-decide it.
>
> If the person who owns the risk is not available to decide, build Tier 1 and stop.
> Hand the decision back rather than adding capabilities later without asking.

> **Confinement: `safe-unconfined` at Tier 1.** The base recipe ships no bash and no
> execution tool of any kind, so it is safe to run unconfined on the host. Tier 2 adds
> dispatch but no execution, and does not change that verdict - its blast radius is the
> roster, not the machine. Tier 3 does change it: 3a puts execution inside a container
> (what the [coding-agent](coding-agent.md) recipe calls `container-recommended`) and 3b
> is host-trusted by definition. If you add bash outside these steps, reclassify and
> containerize the same way.
>
> `wackyproc` (step 15) is not a shell and does not by itself trigger a
> reclassification: it backgrounds only binaries discovered in `tools/`, with no `PATH`
> fallback.

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

## 2. Choose the containment tier (human decision)

**This step is not yours to make.** Per the disclaimer at the top of this recipe, the
tier is chosen by the person this agent is for, in conversation with you, and
confirmed explicitly before you wire anything from step 13 or 14. Your job is to
explain, to answer capability questions from the source rather than from guesswork,
and to record the answer. If they are not present, build the base and leave the tier
steps untouched.

The tiers are strictly additive: the base steps build Tier 1, and every later step only
adds wiring on top of it. Nothing wired later has to be unwound to move up, and
nothing in the base depends on a higher tier existing.

| Tier | Wiring added | What it gains over the tier below | What still holds if the agent is fully prompt-injected |
| --- | --- | --- | --- |
| 1 | `files-rw` only (the base steps) | - | It cannot execute anything, because there is no executable in `tools/` to run. Reads and writes stay inside its own directory, gated by `FILES_RW_ACCESS`. The worst case is its own notes being damaged, or quoted back to whoever it is allowed to talk to |
| 2 | `wackypub` binary + `WACKYPUB_ALLOWED_AGENTS` roster (step 13) | Dispatch to named peers (`agent prompt`, `agent add`), scratchpad verbs, self-compaction, `agent watch` | It still cannot execute a single program. It can only address ids present in its own roster file, which is deny-all when the file is absent, and that check runs inside `wackypub` rather than on the arguments it was handed. The real exposure is transitive: whatever a listed peer can do becomes reachable through that peer |
| 3a | `bashc` instead of `bash` (step 14a) | Runs commands inside a Docker container | Arbitrary execution, bounded by mounts: the container exposes three host directories at identical paths (project worktrees, project source, its own `container-ws`) and no push credentials, and host paths outside those mounts do not exist inside it. Sibling worktrees inside a mount are another agent's territory by convention, not by enforcement |
| 3b | `bash` (step 14b) | Runs commands as your user, across the whole host | Nothing. An injected Tier 3b agent executes arbitrary commands with your permissions, and `FILES_RW_ACCESS` stops mattering, because bash does not consult it. This is coordinator-grade trust and should be granted as such |

How to read the table: the last column is the one that survives an adversary, so it is
the column to decide on rather than the second. Two things make Tier 2 look smaller
than it is, and both are worth saying out loud. A roster entry is a capability grant,
not a contact list - listing an agent that has bash gives this agent indirect bash.
And dispatch is a *write* into another agent's session, so a hijacked Tier 2 agent can
push text at the peers it is allowed to name, and those peers will read it as coming
from a colleague.

And the reason Tier 1 is a reasonable default rather than a crippled one: an
assistant's usual jobs - remember, reflect, write, summarise, talk - need files, not
execution. Raise the tier for a specific capability the person named, one tier at a
time, and treat each raise as its own decision.
## 3. Runtime + API key

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

## 4. Grant files-rw access

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

## 5. Gate the tool (Tier 1 wiring)

```bash
ln -s "$(command -v files-rw)" "$WS/assistant/tools/files-rw"
```

No `bash`, no `wackypub`, no `wackyproc` - this agent's only
external capability is scoped file access, and that IS Tier 1. Steps 13 and 14 add
capabilities on top, each one gated behind the human decision in step 2, and nothing
before them needs them.

## 6. Install the skills

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

## 7. Write the identity files

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

## 8. Write the AGENTS.md linkage file

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

## 9. Scaffold MEMORY.md and WORKBOOK.md

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

## 10. Install the date hook

`memory-system` dates its daily archive files (`memory/YYYY-MM-DD.md`) and expects
the agent to know today's date reliably - the repo ships an example hook for
exactly this:

```bash
cp "$REPO/examples/hooks/on-user-message/00-date" "$WS/assistant/hooks/on-user-message/00-date"
chmod +x "$WS/assistant/hooks/on-user-message/00-date"
```

This prepends `[Current datetime: ...]` to every inbound message - see
`docs/agents.md` §6 (Hooks) for how hook discovery works if you want to adjust it.

## 11. Verify

```bash
cd "$WS" && wackypub workspace assistant
```

Expect `tools/` to report 1 discovered tool, `skills/` to report 6 discovered
skills, `runtime.json` present + valid, `AGENTS.md` present, and `MEMORY.md`
present. `FILES_RW_ACCESS` is exercised by the smoke test below, not by the
inspect, and `session.jsonl` is reported missing until the first turn - expected.
Wiring Tier 2 or 3 later adds tools and skills, so a re-run then reports more,
which is the point of the tier being visible.

## 12. Smoke test: memory write-back

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

## 13. Tier 2 wiring: peer dispatch, still zero execution (optional)

**Only wire this on the human's explicit Tier 2 choice from step 2.** Wiring a tier is
the same security decision as choosing it, so it needs the same explicit answer rather
than your judgement that it would be handy.

```bash
ln -s "$(command -v wackypub)" "$WS/assistant/tools/wackypub"
ln -s "$REPO/skills/wackypub-a2a" "$WS/assistant/skills/wackypub-a2a"

cat > "$WS/assistant/WACKYPUB_ALLOWED_AGENTS" <<'EOF'
coder
EOF
```

The roster is one agent id per line, and its absence means deny-all: no cross-agent
verb works until you list someone. The check runs inside `wackypub`
(`AuthorizeAgentTarget` in `pkg/agent/workspace.go`), which resolves the roster from
the working directory of the process making the call, and for a tool call that
directory is the agent's own folder. That ordering is the whole point: the model picks
the target and the arguments, but it does not get to pick whose roster applies, so a
hijacked prompt cannot widen the grant by rephrasing the command or pointing `--ws`
somewhere else. From the agent's own directory, `wackypub workspace` prints who it can
reach, which is worth showing the person so the grant stays visible rather than
implicit.

What Tier 2 buys, concretely: hand work to a peer (`agent prompt`, `agent add`), read
and write scratchpad entries, compact its own memory (`agent compact`), and watch a
session. What it still cannot do, however a fetched page instructs it, is run a
program - there is still nothing in `tools/` that executes code. The residual risk is
transitive rather than local: every id in that file is a set of capabilities this agent
can borrow, so add peers the way you would add someone to a permissions group, and say
so when you ask for the tier.

Also link the `scratchpad-efficiency` skill from this checkout
if you want it using the scratchpad verbs, and add `assistant` itself to the roster only when you wire step 15,
because the completion wake is the agent messaging itself.

## 14. Tier 3 wiring: code execution (optional)

**Only wire this on the human's explicit Tier 3 choice from step 2, and say plainly
what Tier 3 means before they answer:** a sufficiently persuaded agent can do anything
the account running it can do. If they want execution but not that, 3a is the flavor
that keeps a wall standing.

### 14a. `bashc` - execution contained in a container

```bash
ln -s "$(command -v bashc)" "$WS/assistant/tools/bashc"
ln -s "$REPO/skills/bashc" "$WS/assistant/skills/bashc"
```

`bashc` is a drop-in `run_command` shell that executes every command inside an isolated
container. Three host directories are mounted at identical paths - the project
worktrees, the project source, and the agent's own `container-ws` scratch space - the
rest of the host simply does not exist in there, and the container holds no push
credentials. The agent-facing view (what is shared, what is ephemeral, which folder is
its own) is the bundled `bashc` skill linked above; the operator-side setup for the
container itself is `scripts/sandbox/README.md` in this checkout.

A containment claim needs its exact boundary, so: the mounts are the guarantee, and
inside the worktrees mount, other agents' worktrees are off-limits by convention rather
than by kernel enforcement. If the person needs a hard wall between agents at Tier 3,
that is a per-agent mount decision made in the sandbox setup, and this recipe cannot
promise it for them.

### 14b. `bash` - execution on the host

```bash
ln -s "$(command -v bash)" "$WS/assistant/tools/bash"
```

This is full execution as the user running `wackypub`, across every path that user can
reach, with no mount boundary and no credential filter. Two consequences belong in
front of the person explicitly: the `FILES_RW_ACCESS` grant from step 3 no longer
constrains anything, because bash does not consult it, and prompt injection converts
directly into arbitrary command execution instead of into a bad note. Grant it the way
you would grant ssh access, and offer 3a first when what they are describing is a need
to run builds rather than a need to own the machine.

## 15. Cross-tier enhancement: defer long work and get woken (optional)

The base recipe has no `wackyproc` on purpose. Add it when this agent starts issuing
commands that outlast a turn, and it becomes fire-and-forget: the command goes to a
background job, the turn ends, and a hook wakes the agent when the job finishes so it
reads the result on its own. Two example scripts from the wackyproc repo do the whole
thing; nothing about the base recipe changes.

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

The scope changes with the tier, and the difference is enforced rather than
documented: the shipped auto-defer script refuses to defer `wackyproc` and `wackypub`
calls, because a backgrounded dispatch must not quietly change A2A semantics. So at
Tier 3 the hook above defers the agent's *commands*, marked or not per your policy.
At Tier 2 the long thing worth deferring is a *peer dispatch*, and you get it
explicitly instead:

```bash
cd "$WS/assistant" && wackyproc run wackypub agent prompt --async coder "your message"
```

`--async` refuses to run unless `WACKYPROC_SUPERVISED` is in the environment, which is
precisely what `wackyproc run` sets, so the supervised form is the only supported one
and it still respects the Tier 2 roster. Everything below this paragraph - the binary,
the marker, the policy, the wake requirements - applies to both shapes.

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

- **Tier 1, the base:** a long-lived assistant with a real identity (`IDENTITY.md`/
  `SOUL.md`), a tiered memory system (`BRAIN.md`/`WORKBOOK.md`/`memory/`/`MEMORY.md`),
  and explicit security and execution-discipline boundaries - all always-loaded, not
  optional reading. Its only external capability is allowlist-gated file access to its
  own directory, and it cannot execute anything at all.
- **Tier 2, if chosen:** the same agent, able to hand work to rostered peers, use
  scratchpads, compact itself and watch a session - with a hard authorization boundary
  in `wackypub` and still zero code execution.
- **Tier 3a, if chosen:** execution inside a container, limited to the three mounted
  host directories and holding no push credentials.
- **Tier 3b, if chosen:** execution as your user across the host, with no boundary
  left to describe.
- A reliable notion of "today" via the date hook, which the memory system's daily
  archive convention depends on.
## Next steps

- Raise the tier deliberately, or not at all: step 2 is a standing decision rather
  than a one-time gate, so revisit it with the same conversation when the agent's job
  changes. Do not raise it because a capability exists.
- Put it behind a chat surface: the
  [discord-community-agent](discord-community-agent.md) recipe uses this same
  identity/memory shape as its base.
- Set up proactive heartbeats: `assistant-capabilities` covers the pattern (a cron
  job prompting the agent to read a `HEARTBEAT.md`), not wired by default here.
- If it grows peers and real dispatch traffic, the `wackypub-a2a` skill (step 13) and
  the hooks reference in `docs/agents.md` section 6 are the pair to read next.
