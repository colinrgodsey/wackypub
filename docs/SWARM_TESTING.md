# 🐝 Swarm-Based Security Testing

The process used to satisfy the pen/escape-testing bar in
[`.agents/SECURITY_TESTING.md`](../.agents/SECURITY_TESTING.md): a
`wackypub`-orchestrated swarm of agents attacking a real, live build of the
tool under test, coordinated through an idea-propose / dedupe / execute /
report loop, run inside a disposable Docker container. This dogfoods
`wackypub`'s own multi-agent tool-calling to test `wackypub`'s own tools.

## Why swarm, not one scripted pass or one solo agent

A scripted probe list only tests the vectors its author already thought of.
A single agent - even a capable one - explores one train of thought at a
time. A swarm of independent workers proposing ideas in parallel, cross-
pollinated and refined by a coordinator across several rounds, covers more
of the real attack surface than either alone, for the same reason a human
red team is more than one person.

## Environment: always inside the Docker container

The entire process - coordinator, every worker, the tool under test, and
every fixture the swarm creates - runs inside the disposable container
defined by the repository's master `Dockerfile` and `docker-compose.yml` (see
`.agents/LOCAL_TESTING.md`). This is not optional: a genuine escape found by
the swarm should be contained to a throwaway environment, not the host
running the test. Destroy and recreate the container/workspace between test runs
so a prior round's fixtures (symlinks, planted files, whatever the swarm built)
never leak into the next one.

### Access Bands: Privilege Level as an Explicit Test Parameter (D67)

Rather than treating in-container privilege as a fixed environmental constant,
privilege level is an explicit parameter of the test matrix. Swarm testing
should be conducted (and documented) across distinct **access bands**:

1. **Standard Band (Default)**: Runs as the non-root user (`USER wackypub`,
   UID/GID 1000) with scoped `sudo` for package management (`apt-get`/`apt`).
   This mirrors real-world production deployments: workers can install tools
   needed to construct attack fixtures, but file operations in the host bind
   mount (`/ws`) remain bound by standard Unix permissions. Host-side cleanup
   (`rm -rf .swarm-ws`) works cleanly without `Permission denied` or `sudo`.
2. **Root Band (`--user root`)**: Runs with full root privileges inside the
   container (`docker compose run --user root wackypub` or `docker run --user root ...`).
   Used to test whether root-in-container capabilities (device nodes, raw socket
   operations, filesystem override flags) can breach a tool's internal
   enforcement boundary.
   *(Note: files written to the host bind mount during a root-band run will be
   root-owned on the host. Clean them up via `docker run --rm -v <host-path>:/cleanup alpine rm -rf /cleanup/*`.)*
3. **Restricted Band**: Runs as non-root with dropped Linux capabilities
   (`--cap-drop=ALL`) or without `sudo` access, testing least-privilege defense.

Every swarm run's report must state which access band was tested. Running the
same swarm methodology across different bands provides high-value contrast: a
finding present only under the root band indicates an environment-escalation
dependency, whereas a finding present under the standard or restricted band
indicates a pure application-level logic vulnerability.

## Setup: the local trusted agent's job, not a fixed script

**Prerequisite: load `skills/wackypub-a2a/SKILL.md` and `skills/wackypub-ws/SKILL.md` first.** Standing up a
swarm test means driving the `wackypub` CLI yourself - through `docker compose run`
or `docker exec`, to configure the coordinator and every worker, and to sanity-check
the sandbox before handing it to the swarm - not just describing it. That
skill is the dense, load-bearing reference for exactly how `wackypub`
works (workspace/agent layout, `tools/`/`skills/` discovery,
`WACKYPUB_ALLOWED_AGENTS` format, the CLI's own `--help` conventions - see
its own "Setting Up an Agent & Swarm From Scratch" section). Without it,
you're reconstructing that knowledge from scratch mid-setup instead of
just having it.

There is deliberately no automation script for standing up a swarm test.
Whoever runs one - in practice, a local trusted CLI agent (e.g. Claude Code)
already operating in this repo with `.agents/` loaded - is expected to
build the containerized workspace itself: setting `WORKSPACE_DIR=./.swarm-ws`
in `.env` or `docker-compose.yml`, scaffolding one coordinator and N workers
for this specific target and boundary using the standard `wackypub-ws` conventions.
A swarm test is already an expensive, infrequent thing to run, and every run's specifics
(which tool, what grant, which access band, how many workers, white-box or not) differ enough
that a rigid script would mostly get in the way - judgment from an agent
that's actually read this doc and `SECURITY_TESTING.md` is more reliable
here than a fixed recipe.

**Persona templates to start from**: `agents/pen-escape-testing/COORDINATOR.md`
and `agents/pen-escape-testing/WORKER.md` - copy these in as the
coordinator's and each worker's `AGENTS.md`, adapting as needed for the specific test.
These are meant to be iterated on across test runs, not treated as frozen -
if a run reveals the coordinator or worker persona needs to behave
differently, update the template, not just that one run's copy.

Judgment calls the setting-up agent needs to make per run, informed by this
doc and `SECURITY_TESTING.md`: how many workers; what access band to test under;
what sandbox/grant to hand the target tool (e.g. a `files-rw` test's `FILES_RW_ACCESS`,
with some planted content that should be unreachable); which model to point
`runtime.json` at (a genuinely capable one - see `SECURITY_TESTING.md`'s
"who's qualified" bar, not the lightweight default used for routine dev
work); and `iterations` for this run. All of these end up recorded in the
resulting `docs/<tool>-security-test.md` report, not just decided silently.

**White-box by default, not a judgment call.** This is an open-source
project - the source is available either way, so black-box testing would
only be simulating a constraint that doesn't actually exist here. Give
workers read access to the target tool's own source alongside the binary
unless there's a specific reason not to for a given run (worth noting in
that run's report if so).

## Roles

### Coordinator

Does not attack the tool directly. Runs the process:

1. **Propose.** Prompt every worker for exactly 3 new candidate attack
   ideas against the tool's stated boundary (what should be impossible;
   what the grant does and does not cover), telling each worker what's
   already been proposed so far so its 3 slots go toward new angles, not
   repeats. A worker reporting nothing new is a legitimate answer.
2. **Collect.** Gather all worker-submitted ideas for the round (up to 3
   per worker), noting which worker proposed each one.
3. **Assess & deduplicate.** Merge near-duplicate ideas, discard ones that
   don't actually apply to this tool/boundary, and keep the rest as this
   round's candidate list. Record discarded/merged ideas too (see report
   requirement below) - a future run shouldn't waste a round rediscovering
   an idea already ruled out.
4. **Assign & execute.** Dispatch every idea that survived dedup this
   round, preferring to send each idea back to the worker who originally
   proposed it (most context on their own idea) - reassign only if that
   worker's unavailable. Each assigned worker actually runs its idea(s)
   against a live instance of the tool (not reason about hypothetically)
   and reports back: exact commands, exact output, whether the goal was
   achieved.
5. **Compile.** Gather worker execution reports into the round's findings.
6. **Iterate.** Repeat the whole propose -> dedupe -> execute -> report cycle
   for `iterations` rounds. `iterations` is not fixed by this process doc -
   it's chosen per test run and declared in that run's report (see below).
   Later rounds should build on earlier findings (a partial weakness found
   in round 2 is exactly what round 3's ideas should try to push further).
   If every worker runs out of new ideas before `iterations` is reached,
   that's expected, not a failure - stop there rather than running empty
   rounds, and report the actual round count reached alongside the
   requested one.
7. **Write the report.** Produce `docs/<tool-name>-security-test.md` (exact
   contents below) and update `.agents/SECURITY_TESTING.md`'s state for
   that tool.

### Workers (N of them, N is a per-run choice)

Each worker gets the tool under test wired up exactly as a real deployment
would (symlinked into their own `tools/`, invoked through `run_command` like
any other agent tool - not a mocked interface), plus source read access -
white-box by default (see Setup above). Each round, a worker either:
proposes ideas when asked, or executes a specific assigned idea against a
live instance and reports back precisely what happened - including ideas
that didn't pan out; a clean "tried X, blocked as expected, here's the
error" is still a useful record, not just successes.

## Operational notes (learned running real tests)

- **Name the host-side workspace directory with a leading dot** (e.g.
  `.swarm-ws`, not `swarm-ws`) when white-boxing a target tool that's part
  of this same Go module - not just gitignored. A white-box run copies the
  target's Go source into each worker's directory (see "white-box by
  default" above), and `go build ./...`/`go test ./...` walk the whole repo
  tree by default - a non-dot-prefixed workspace directory means those
  copied source files (and their `_test.go` files) get picked up and
  compiled/run as if they were real packages, silently duplicating test
  output on every local `go test ./...`. Go's own tooling already skips any
  directory starting with `.` or `_` (plus a directory literally named
  `testdata`) for exactly this purpose - no separate ignore-file mechanism
  needed, just use the naming convention from the start.
- **`docker exec` needs `-i` to attach stdin.** Piping content into a
  command inside the container from the host (feeding a worker's stdin
  manually, verifying a finding yourself) silently does nothing without it
  - the command still "succeeds," it just runs against empty input. This
  produced one real false-negative moment during the first run's manual
  verification pass; always double-check with `-i` before trusting a
  stdin-dependent result run this way.
- **Cancelling a stuck coordinator or worker.** Killing the host-side
  `docker exec` wrapper process (e.g. the PID from a backgrounded `docker
  exec ... &`) does **not** kill the process running inside the container -
  `docker exec` exiting on the host doesn't propagate. Find and kill the
  real process instead: `docker exec <container> ps aux | grep wackypub`,
  then `docker exec <container> kill -9 <pid>`. The session lock releases
  automatically when the process dies (a kernel-level `flock` tied to its
  file descriptors, not something needing manual cleanup) - confirmed live,
  a fresh `agent add` succeeded immediately after killing a stuck run. (See
  `.agents/TODOS.md`'s "no way to cancel an in-flight agent task" entry -
  this is the working-but-manual procedure until that exists properly.)
- **Expect some workers to just time out**, especially less-mainstream
  backend models at high reasoning effort - this is a real backend
  reliability issue (see `.agents/TODOS.md`'s HTTP timeout entry), not a
  swarm-process bug. The coordinator persona already handles it fine on its
  own without extra kickoff guidance: notices a worker's calls keep
  failing, gives up on that worker after a retry or two, and proceeds with
  whichever workers did respond.
- **`patch` is preinstalled in the base image** (see `Dockerfile`)
  specifically so a `files-rw`-style patch-subcommand attack doesn't go
  untested for lack of the binary, the way it did on the first run (had to
  `apt-get install` it live mid-test, then re-verify that one idea by hand,
  to close the gap).
- **Kickoff framing**: giving the coordinator a rough total idea-count
  target ("~10 ideas") instead of a strict round count worked fine in
  practice - it translated that into however many rounds it judged
  necessary (2, on the first run) and reported the actual round count used
  in its final report, exactly as required. Either framing is fine;
  whichever is clearer for the specific test.
- **Always set a memory limit on the container** (`docker run --memory
  8g ...`, or `docker update --memory=8g --memory-swap=8g <container>` on
  one already running). Skipped this on the second run, and a worker's
  resource-exhaustion idea (`base64 /dev/urandom | head -c 1G`) hit an
  apparent bug in the container's `base64` (`uutils-coreutils`, not GNU -
  didn't respect the downstream `head -c` limit) and grew to 43GB RSS on
  the *host*, with no cap to stop it. The container's disposability
  protects against data/filesystem damage, not against a worker exhausting
  real host resources - that needs an actual cgroup limit, not just "it's
  throwaway." Killing the runaway process alone wasn't enough either - the
  worker's agent turn retried the identical command immediately; had to
  kill the worker's whole in-flight `wackypub agent <worker> prompt`
  process (not just the spawned shell command) to stop the retry loop.
- **Interrupting a long run cleanly**: killing the coordinator's top-level
  `wackypub agent prompt coordinator` process mid-run (e.g. to cap cost on
  a 10-round request) leaves whatever rounds already completed intact in
  each agent's `session.jsonl` - a follow-up prompt telling it to stop and
  write the report now (instead of continuing) produces a report scoped to
  what was actually completed, with an honest note on how many rounds were
  requested vs. finished and why. No need to let a run finish all requested
  rounds to get a usable report out of it.

## Report requirement: `docs/<tool-name>-security-test.md`

Every checklist entry in `.agents/SECURITY_TESTING.md` marked `y` or `n`
(see states below) must have a matching report in `docs/`. Required
contents:

- Tool tested, and the exact commit/version it was tested against.
- Access band tested (e.g. Standard non-root + scoped sudo, Root override, or Restricted).
- `iterations` used for this run, and coordinator/worker counts.
- Every idea proposed each round - including discarded or deduplicated
  ones, and why they were dropped.
- For every idea actually executed: exact repro steps, exact observed
  result, and a pass/fail verdict against that idea's specific goal.
- Final overall verdict for the tool.
- If anything was found: a link to the fix (commit, or a
  `.agents/DECISIONS.md` entry) and to the follow-up report that re-tested
  the fix.

## Checklist states (`.agents/SECURITY_TESTING.md`)

Three states, not a plain checkbox:

- **`?`** untested / unknown - the default. No report exists, or a report
  existed but was deleted (see invalidation rule below).
- **`y`** tested, no exploitable finding - a report exists.
- **`n`** tested, a real finding came out of it - a report exists and is
  **kept even when the finding is bad news**. A subsequent fix and re-test
  produces a new, separate, dated report and updates the state to `y` (or
  back to `n`, if the fix didn't hold) - the original `n` report documenting
  the original finding is not deleted or overwritten. It's part of the
  tool's security history, not a mistake to erase.

## Mandatory invalidation rule

The moment a tested tool's enforcement logic changes - the same trigger
already defined in `.agents/SECURITY_TESTING.md` for resetting its
state - **every report in `docs/` describing the old behavior must be
deleted**, not just the checklist state reset to `?`. A report describing
code that no longer exists is actively misleading rather than merely
outdated, so it doesn't get to linger as history the way a genuine `n`
finding does. State reverts to `?` until a new swarm run against the
changed tool produces a new report.
