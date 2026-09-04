# SECURITY_TESTING.md

Tracks tools that enforce a security boundary (filesystem access, command
execution, cross-agent authorization, or anything else an agent - or a
prompt-injected one - could try to escape) and whether that boundary has
actually been pen/escape-tested: adversarial probes run against a live
build, not just unit tests for the happy path. Unit tests prove the logic
does what the code says; escape testing proves an attacker-shaped input
can't get past it anyway. Both matter, only one is tracked here.

**The actual testing process is [`docs/SWARM_TESTING.md`](../docs/SWARM_TESTING.md)** -
a `wackypub`-orchestrated swarm (coordinator + N workers) attacking a live
build inside a disposable Docker container, over some number of propose/
execute/report rounds. Read it before running a test or writing a report.

## States

Three states, not a plain checkbox:

- **`?`** untested / unknown - the default, and the state a tool reverts to
  the moment its enforcement logic changes (see invalidation rule below).
- **`y`** tested, no exploitable finding.
- **`n`** tested, a real finding came out of it. Stays `n` - and its report
  stays on disk - even after a fix, until a fresh test run against the fix
  produces a new report and (hopefully) a new `y`. Bad news doesn't get
  erased, it gets superseded by a dated follow-up.

**Every `y` or `n` entry must link a report in `docs/`** written per
`docs/SWARM_TESTING.md`'s report requirements - exact commit tested,
`iterations` used, every idea proposed (including discarded ones), every
idea actually executed with its exact result, and a final verdict.

**Directive: if a tool's enforcement logic changes, reset it to `?` and
delete its report(s) in the same commit.** A `y`/`n` state is a claim about
the code as it stood when it was last tested, not a permanent property of
the tool - and a report describing code that no longer exists is actively
misleading, not just stale, so it doesn't get to linger the way a real `n`
finding does. This applies even if the change looks unrelated or is "just a
refactor." Adding a new such tool means adding a row here at `?`.

**Who's qualified to write a `y`/`n`.** A scripted list of probes written by
whoever implemented the tool only tests the escape vectors that person
already thought of - that's regression coverage, not a pen test. A `y`/`n`
verdict means the boundary was actually attacked by the swarm process in
`docs/SWARM_TESTING.md`, run against a capable, motivated model - not the
same lightweight pass used for routine implementation work. Implementer-
written adversarial probes and live but non-adversarial usage (an agent
using the tool normally and incidentally hitting a bug) are both useful and
worth keeping as a record, but neither alone earns `y` or `n`.

## Checklist

- **`?` `files-rw`** (`tools/files-rw/` submodule, [colinrgodsey/files-rw](https://github.com/colinrgodsey/files-rw)) - filesystem read/write/edit/patch/
  copy/move/delete/symlink/list gated by a per-directory `FILES_RW_ACCESS`
  allowlist (see DECISIONS.md D22-D26, D86). The `symlink` command (D86)
  is in scope: creation-time ACL confinement (write@source, read@target
  coverage) closes path-existence probing, chains/cycles cap at 8 hops,
  dangling links error on write/patch without auto-vivification, and delete
  unlinks only the symlink itself. The `n` report from the second
  swarm run is deleted per the invalidation rule - D24's findings drove a
  fix, revised once more per D26 (`go-gitdiff` for `patch`, a flat
  `Nlink > 1` check for hardlinks). Verified directly since (not a swarm
  pass, doesn't earn `y`/`n`): hardlink read, hardlink+copy, and
  cross-agent hardlink read are all now denied cleanly; legitimate
  read/edit/copy still work. The TOCTOU race from the second run is still
  reproducible and, on reflection, isn't a `files-rw`-fixable bug at all -
  see the "no-bash swarm re-test" entry in `TODOS.md`. Pending a fresh
  swarm run against the current state.

- **`?` `wackyproc`** (`tools/wackyproc/` submodule, [colinrgodsey/wackyproc](https://github.com/colinrgodsey/wackyproc)) - zero-daemon background
  process manager (see DECISIONS.md D51): spawns/tracks detached
  processes under `.proc/<id>/`, resolving target tools strictly against
  `<cwd>/tools/<tool>` (no `$PATH` fallback). No swarm pass yet. Two review
  gaps found and fixed before landing (independently re-verified, not just
  taken on report): an `os.Stat` + `os.MkdirAll` TOCTOU on candidate process
  IDs, resolved via atomic `ClaimUniqueProcessDir` (`os.Mkdir` returning
  `os.ErrExist` on collision with retry, matching scratchpad `O_CREATE|O_EXCL`
  semantics); and a narrower race where a child that had just died but whose
  supervisor hadn't yet finished writing `exit_code` could get misreported as
  `CRASHED`, fixed by checking the recorded supervisor PID is still alive
  before concluding a crash. Worth a swarm run once this settles, plus
  attention to whether the two safeguards the original design doc called
  for and this pass didn't ship - `flock` on concurrent state read/write, and
  log rotation/capping for long-running processes - are worth adding before then.

## Not yet on this list

Anything that shells out to an external command with agent-influenced
arguments, or resolves an agent-supplied path against a boundary, belongs
here once it exists - `run_command`'s own executable-discovery and
`WACKYPUB_ALLOWED_AGENTS` cross-agent gating are both candidates that
predate this file and haven't been backfilled with a dedicated escape-test
pass yet.
