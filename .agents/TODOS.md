# TODOS.md

Deferred work and known gaps. Not a backlog of feature ideas - only things
that are already known to be incomplete, fragile, or blocked on something
external.

## Handle OpenRouter / OpenAI Rate Limiting & Empty Choices Error Recovery

When calling OpenAI-compatible APIs (specifically OpenRouter), rate-limiting (429), capacity limits, upstream timeouts, or moderation flags can result in HTTP 200 OK responses returning an empty choices array (`"choices": []`). 

The official `openai-go/v3` SDK (used by `adk-utils-go`'s `genai/openai` adapter) parses `resp.Choices` as a 0-length slice, causing `convertResponse` to return a bare `ErrNoChoicesInResponse` ("no choices in OpenAI response") without surfacing the underlying root cause.

**Key Triggers & Gaps**:
1. **Rate Limiting & 429s**: When OpenRouter or an upstream provider rate-limits a request, OpenRouter sometimes wraps rate-limit errors inside an HTTP 200 JSON envelope carrying `"choices": []` alongside an embedded `"error"` object (e.g. `{"choices": [], "error": {"code": 429, "message": "Rate limit reached"}}`).
2. **Upstream Provider Errors**: Upstream nodes (Novita, Together, Fireworks, DeepInfra) hitting gateway timeouts or capacity limits return `"choices": []` with embedded error payloads.
3. **Masked Error Strings**: Because `openai-go` only inspects `resp.Choices`, embedded `"error"` objects on HTTP 200 responses are discarded by `convertResponse`, masking actionable error messages.

**Required Work**:
- In `adk-utils-go`'s `convertResponse`, inspect `resp.RawJSON()` for top-level `"error"` fields when `len(resp.Choices) == 0` and surface the embedded error message (e.g. `fmt.Errorf("no choices in OpenAI response: %s", rawErrMessage)`).
- Implement retry / backoff logic in the caller or model adapter for transient rate-limits (429) and upstream provider timeouts when `choices: []` is received.

## Consider a timeout on session lock acquisition

`AcquireSessionLock` (`pkg/agent/lock.go`) blocks forever on `syscall.Flock`
with no timeout. The read-only SDK methods that used to call it
unnecessarily were fixed to not lock at all (see the self-deadlock fix
found live via clerk), but the genuinely mutating methods (`AddUserTurn`,
`GenerateTurn`, `AddAndGenerateTurn`, `StripSignatures`,
`CompactSession`) still correctly need to hold it, and still block
indefinitely if something else is holding it - a real deadlock we haven't
found yet, or just a second legitimate caller genuinely waiting its turn.

A bounded wait (fail with a clear "timed out waiting for session lock -
another process appears to be using this agent" error instead of hanging)
would be a reasonable safety net. The real constraint: this session
directly observed legitimate `GenerateTurn` calls taking several minutes
(high reasoning effort + several chained tool turns), so the timeout needs
to be generous enough not to false-positive-fail a slow-but-working
generation - probably needs to be configurable rather than a small fixed
constant. `syscall.Flock` has no native blocking-with-timeout mode, so
this would mean polling `LOCK_NB` in a loop until success or timeout
elapses.

## Modular compaction strategy

`CheckAndCompactSession`/`CompactionDirectivePrompt` (`pkg/agent/compaction.go`)
is a single hardcoded approach: an LLM directive prompt that summarizes
archived turns into a `<PERSISTENT_MEMORY>` addendum. There's no seam for
a different strategy (e.g. plain truncation with no summarization,
externally pluggable compaction, per-agent-configurable directives beyond
the AGENTS.md `## Memory Focus` override already supported). Worth
factoring compaction behind some kind of strategy interface/config knob
instead of the one fixed implementation, once a second real use case for
a different strategy actually shows up.

## No total budget on cross-agent call depth, only cycle prevention

`WACKYPUB_CALL_CHAIN` (D16) stops an agent from being re-entered mid-chain
(A -> B -> A), but nothing caps how long or expensive a legitimate, acyclic
chain can get. Live testing showed an agent can chain multiple cross-agent
calls to a peer agent entirely on its own within a single generation turn
(see D16/D17); nothing currently stops that same pattern from fanning out
across several agents on one bad turn. Worth
adding a max-depth counter alongside the existing cycle check - likely
threaded through `WACKYPUB_CALL_CHAIN` the same way `--max-tool-turns`
caps a single agent's own tool loop.

## Dedicated `wackypub init` command

Bootstrapping a new workspace's `WACKYPUB_ROOT` file currently requires creating `WACKYPUB_ROOT` by hand (`touch WACKYPUB_ROOT`). A dedicated `wackypub init` command (to create `WACKYPUB_ROOT` and scaffold an agent directory) may be worth adding later.


## Open question: does `WACKYPUB_ALLOWED_AGENTS` restrict CWD-based invocations in general, or only actual tool-call context?

Flagged during the D16 design discussion, deliberately deferred. If
`WACKYPUB_ALLOWED_AGENTS` is checked purely based on "does CWD fall under
this agent's folder," then a human who `cd`s into agent A's folder and
manually runs `wackypub agent B ...` to debug something also gets
restricted by A's allowlist - even though there's no actual deadlock or
authorization risk in that case, since a human isn't "agent A calling B."
The alternative is scoping the check to only apply when the invocation is
actually happening as a tool call spawned from A's own live generation
(e.g. via an explicit signal set only during a real tool-use loop, such as
`WACKYPUB_CALL_CHAIN` already being non-empty), which would leave manual/
debugging use from inside an agent's folder unrestricted. Needs a decision
if this behavior should be refined; currently implemented with the simpler CWD-only check (see DECISIONS.md D16).

## `FolderAgent.RunWithRunner` is unused (Narrowed: D45)

`BuildADKAgent`/`llmagent.New` (`pkg/agent/adk_agent.go`) is not actually
dead - confirmed it's exercised in production via `BuildADKAgentWithConfig`,
which `LoadFolderAgent` calls for every real agent, and D45's compaction fix
now builds a second, disposable `agent.Agent`-driven session on top of the
same pipeline. Only `FolderAgent.RunWithRunner` itself
(`pkg/agent/agent_folder.go`) remains genuinely unused - D45 needed the same
"disposable in-memory session + one real runner call" shape but had to
hand-roll it rather than call `RunWithRunner` directly, since `RunWithRunner`
only supports seeding a single fresh prompt against an empty session, not
compaction's need to seed a specific historical turn slice (the memory turn
plus the archived turns) before the new message. Either find `RunWithRunner`
a real caller with that single-fresh-prompt shape (a plain one-off
generation outside the session.jsonl-backed flow) or remove it.

## ~~`session.jsonl` has no defense against the missing-trailing-newline corruption mode~~ (closed D75)

Fixed in `pkg/agent/session_store.go`. `AppendSessionContent` now checks the
last byte of the file before appending and writes a healing `'\n'` if it's
missing. The Gotchas entry in `AGENTS.md` can be removed or softened now that
the fix is in place; the mitigation note ("don't hand-edit without checking")
is no longer the only protection.

## No way to cancel an in-flight agent task

Every other agent harness worth comparing against gives you some way to
interrupt a run that's taking too long or has gone off the rails (Ctrl-C
mid-generation, a cancel button, a kill command) - `wackypub` has nothing
like this. Once `wackypub agent prompt`/`generate` is running, the only way
to stop it is killing the process outright, which (depending on exactly
where it lands) can leave the session lock held or a turn half-appended.
Separate from the timeout issue above (a good cancellation story doesn't
remove the need for a sane default timeout, and vice versa), but related:
once agents are routinely being orchestrated into swarms/multi-agent
chains, a stuck or runaway one needs to be stoppable without taking down
the whole process tree around it. No design started yet.

## Positioning idea: "it's like bash, for agents"

Not code work - a framing/messaging angle for README/philosophy sections
that isn't written up anywhere yet. The parallel is closer than it first
sounds, point for point:

- **Process per command, no daemon.** Every `bash` command is its own
  process; state survives via the filesystem, not a long-lived
  interpreter holding memory. Every `wackypub agent <cmd>` invocation is
  the same - a fresh process per call, state persisted to
  `session.jsonl`/`MEMORY.md` between calls, nothing held in memory
  between invocations.
- **Respects env vars.** `bash` passes environment down a process tree
  (`PATH`, `HOME`, whatever's exported). `wackypub` does the same with
  `WACKYPUB_CALL_CHAIN` (D16) - it's just inherited by every subprocess a
  cross-agent call spawns, exactly like any other env var, and that's the
  entire mechanism the cross-agent deadlock-cycle guard relies on.
- **Special files in a home folder.** `bash` reads `~/.bashrc`,
  `~/.bash_profile`, etc. Every agent has its own directory functioning
  as its "home": `AGENTS.md`, `MEMORY.md`, `runtime.json`,
  `WACKYPUB_ALLOWED_AGENTS`, `WACKYPUB_ROOT` as the workspace-level marker
  - same idea, same reason (config as plain files a human can read/edit
  directly, not a database or hidden state).
- **Looks up available executables.** `bash` resolves a command name
  against `$PATH`. An agent's `tools/` directory (D14) is the same lookup
  - what's callable is whatever's discoverable there, nothing more.
- **Piping between commands.** `bash` pipes let one command's output feed
  another without going through a human. The scratchpad + `<SCRATCHPAD_DATA
  id="X" />` stdin macro (D18) is the rough equivalent for agents - moving
  data between tool calls without forcing it through the model's own
  context/output tokens.

Worth writing up properly in the README's philosophy section once there's
a natural moment for it - it's a genuinely accurate analogy, not just a
catchy line, and might be a better hook than the current "every capability
is a file" framing alone.

## `files-rw`: fd-based access checking & hardlink safety (first pass done, revision in progress per D26)

First pass implemented in `pkg/filesrw/access.go` and `pkg/filesrw/ops.go` to resolve the two gaps documented in D24 - `Access.OpenFile` (opens the target fd during access validation, does I/O on the open handle, closing the check-then-open TOCTOU window) and `checkHardlinkSafety` (rejects a target whose `Nlink > 1` unless every link resolves within the allowed roots, via a `countInodesInRoots` directory walk).

Reviewed before spending a swarm run on it and found two problems the first pass didn't cover - see D26 for the revised plan, now in progress:
1. `PatchFile` opens+checks+closes the fd, then does the real work via the `patch` subprocess against the path string afterward - reopens the exact TOCTOU window for `patch` specifically. Fix: replace the subprocess with `github.com/bluekeyes/go-gitdiff`, applied against the already-open fd like `EditFile` already does.
2. `countInodesInRoots` does a full recursive `filepath.Walk` over every allowed root on *every* access check - a new performance/DoS surface that didn't exist before this fix. Fix: drop the walk, reject on bare `Nlink > 1` instead (O(1), no walk) - closes every attack the second swarm run actually demonstrated, at the cost of also refusing a legitimate file that happens to have multiple hardlinks for unrelated reasons (expected rare for agent-workspace files). A real, non-blunt hardlink defense remains an open question for later - noted in D26.

D26 landed and was verified directly (not yet a swarm pass, doesn't earn `y`/`n`): hardlink read, hardlink+copy, and cross-agent hardlink read are all now denied cleanly, confirmed live including against a real second agent directory; legitimate read/edit/copy still work. The TOCTOU race, however, is still 100/100 reproducible against both `read` and `copy` even after the fix - and on reflection isn't a `files-rw` bug in the fixable sense at all (see the "no-bash swarm re-test" TODO below for why). `SECURITY_TESTING.md` stays at `?` until a fresh swarm pen-test pass runs against the current state.

## Re-run the next `files-rw` swarm test without giving workers `bash`

The second swarm run's "TOCTOU race" finding (`cp target/secret.txt scratch/race_target &` racing `files-rw read`) doesn't actually demonstrate a `files-rw`-specific leak on reflection: `cp` overwrites the destination inode's content in place, not via a rename/symlink-swap `Access.OpenFile`'s fd-based check could ever catch, and for the race to have anything to win with, `cp` first has to read `target/secret.txt` *directly* - meaning the "secret" is already sitting in the agent-writable, agent-readable `./scratch` the instant `cp` finishes, independent of whether `files-rw read` is ever called afterward. Same underlying issue as the hardlink case's original "if an agent has bash access it's game over" framing (D22/D24): the test setup gives workers real `bash` (needed so far for building fixtures - symlinks, hardlinks, race loops), which means every worker already has full OS-level read access to anything the OS user can read, making some findings a property of the test harness rather than of `files-rw` itself.

Next full swarm run against `files-rw` should try giving workers *only* `files-rw` - no `bash`, no `ln`, no fixture-building tools at all. This is closer to `files-rw`'s actual intended deployment (D22: the tool is meant to be the *only* file-touching capability an agent has) and would cleanly separate "vulnerabilities reachable through `files-rw`'s own command surface alone" from "things possible because the test setup handed out a shell." Loses the ability for workers to construct symlink/hardlink attack fixtures themselves (there's no `files-rw` command that creates a symlink or hardlink), which is a real trade-off, not a pure improvement - probably worth running both configurations rather than only ever switching to bash-less, since each answers a different question about the deployment surface.

## Scratchpad Diff & Patch Verification

Running `diff -u` over two scratchpad entries (`<SCRATCHPAD_DATA id="before" />` vs `<SCRATCHPAD_DATA id="after" />`) to auto-capture diffs and verify code edits out-of-band. Allows agents to preview, validate, and summarize patch changes out-of-band with zero token generation overhead.

## `MEMORY.md` grows forever - no consolidation, only ever appended to

`WriteMemoryFile`'s call site in `CheckAndCompactSession` (`pkg/agent/compaction.go`)
is an unconditional append every compaction cycle - `newMemory + "\n\n" +
addendum`, no cap, nothing that ever revisits or re-summarizes what's
already there. For a short-lived agent this is fine; for one that runs
long enough to compact repeatedly (weeks/months of real use, exactly the
case compaction exists for), `MEMORY.md` itself grows linearly with the
number of compaction cycles, with no bound. Eventually `MEMORY.md` becomes
the token-budget problem compaction was built to avoid in the first place
- it's sent in full on every single generation call (`FormatPersistentMemoryTurn`,
User Turn 1), so an ever-growing memory file directly inflates every
request's prefix forever, not just the turns that get archived.

Needs some kind of periodic re-consolidation: an occasional pass that
takes the accumulated `MEMORY.md` itself (not just newly-archived turns)
and re-summarizes/prunes it - collapsing superseded entries (some of this
already happens ad-hoc via the "STATE UPDATES & INVALIDATION" guideline in
`CompactionDirectivePrompt`, but that only ever adds an "UPDATED:" line
next to the old one, it doesn't remove the old one), dropping anything no
longer relevant, keeping it bounded. No design yet on the trigger (its own
size/token threshold, separate from `sessionCompactPct`? every Nth
compaction?) or how aggressive it should be about actually discarding
history versus just compressing it - worth designing before some
long-running agent actually hits this, rather than discovered live the
way most of this project's other gaps have been.

## `files-rw` has no way to search inside a file's content - probably doesn't need its own, for the single-file case

`files-rw` can read a whole file or a line range, but has no way to find
*where* something is inside a file's content - no equivalent of
`search_scratchpad` (D25) for actual files. Reviewed against real usage
(grep is one of the most-used tools across this whole project's own
sessions): line numbers and regex are used constantly, context lines
(`-A`/`-B`/`-C`) surprisingly often, recursive multi-file search
frequently for whole-codebase work.

**Single-file search probably doesn't need a new `files-rw` command at
all** - compose what already exists instead: `files-rw read <path>`, then
`search_scratchpad` on the result (large `read` output already
auto-captures into a scratchpad entry via the existing `run_command`
>4000-byte threshold, D18). Avoids maintaining two separate search
implementations (one for scratchpad entries, one for file content) for
the same underlying need, and matches how D29 resolved "replace the last
line" - composing existing primitives instead of adding a redundant one.

**Real, unresolved limit on that composition**: it's bounded by `read`'s
own 200KB cap (`MaxReadSizeBytes`) - a file larger than that can't be
pulled into a scratchpad this way at all, `read` refuses outright before
producing any output to auto-capture. That's exactly the case a search
capability would matter most for (a file too big to just read straight
through), and composition doesn't solve it. Worth deciding whether that's
an acceptable boundary (a 200KB single file is already a lot for an agent
workflow) or whether it needs its own answer later.

**Multi-file search is a genuinely harder, separate question** - not
resolved by the same composition. Concatenating several files into one
scratchpad entry to search loses per-file attribution (a match's line
number alone doesn't say which file it came from), so it'd need real
design (something closer to `list -R` crossed with search across
everything under an allowed root, returning matches tagged by file) if
it's ever wanted, not just "read a bunch of files into one entry."

Also: the scratchpad-efficiency skill (`skills/scratchpad-efficiency/`)
should document the "pull a file into scratchpad, then `search_scratchpad`
it" pattern explicitly as the recommended approach for searching inside a
file, so an agent doesn't go looking for a `files-rw search` command that
doesn't exist.

## `always_load` skills drop their `description` when auto-injected

`RenderAutoloadedSkills` (`pkg/agent/skill.go`) renders each always-loaded
skill as `<SKILL name="X">\n{body}\n</SKILL>` - name and body only. Compare
the on-demand skill picker (`BuildFolderAgentTools`, `agent_folder.go`),
which lists every loadable skill as `- {name}: {description}` so the model
has a one-line pitch to decide whether to `load_skill` it. A skill's
`description` frontmatter field is only ever rendered while the skill is
*not* loaded - the instant it's `always_load: true` and its full body gets
injected, the description that was meant to frame it just disappears.

Probably harmless for a short skill (the body says everything), but for a
long one - `scratchpad-efficiency` is ~190 lines - a one-line thesis
before the body would likely help the model orient before reading the
whole thing. Cheap fix if wanted: prepend the description into the
`<SKILL>` block, e.g. `<SKILL name="X" description="Y">\n{body}\n</SKILL>`.
Not done yet, no decision on whether it's worth it.

## `files-rw` has no `mkdir` - directory creation is real but undiscoverable

There's no explicit directory-creation command. `write`/`append` both create
any missing parent directories as a side effect (`os.MkdirAll` in
`pkg/filesrw/ops.go`, confirmed live - `write allowed/sub/dir/nested.txt`
creates `sub/` and `dir/` along with the file) - so the capability exists,
composed the same way D29 resolved "replace the last line" without a new
primitive. But it's only documented in `write`/`append`'s own per-command
`--help` text ("creating any missing parent directories"), buried in a
sentence an agent has no reason to read unless it already suspected
`write` was the answer. The root `files-rw --help` (`rootCmd.Long` in
`cmd/files-rw/main.go`) doesn't mention directory creation at all, and
nothing states outright "there is no mkdir - creating a file inside a
new folder creates the folder."

Seen live: an agent that wanted to create a folder looked for something
mkdir-shaped, didn't find one, and got stuck - it never tried writing a
file into the not-yet-existing folder, which would have worked. A
capability that only works if you already know to try it isn't really
discoverable. Cheap fix, no new command needed: state the behavior
explicitly in `rootCmd.Long` (something like "no separate directory-
creation command - `write`/`append` create any missing parent
directories automatically") so it's visible before an agent goes
looking for `mkdir` and comes up empty.

## `wackypub trace` shows misleading content at a compaction step ("fog of war")

`extractTurnDiff` (`pkg/agent/trace.go`) isolates what's new at a commit by
comparing `session.jsonl` turn *count* against the parent commit - works
for the normal append-only case, but a compaction commit makes
`session.jsonl` *shrink* (old turns archived into `MEMORY.md`, replaced by
fewer). Verified live: 5 turns/5 commits, then a compaction collapsing
down to 1 turn - the `compact` step in the trace shows the same content as
the very next step, not anything representing what compaction actually
did. No crash, no effect on hop logic or `MaxSteps`/cycle detection, just
misleading content specifically for `compact` steps.

Left as-is deliberately for now - `trace` is meant to stay a simple tool,
and this is a real "fog of war" spot, not a bug in the core mechanism. A
more capable version could reconstruct what actually happened at a
compaction step properly: the pre-compaction `session.jsonl` is still
fully recoverable from the parent commit (git doesn't lose it, only the
*live* file shrinks), and the `compact` commit itself has the new
`MEMORY.md` addendum - a real diff between those two would show exactly
which turns got archived and what they were summarized into, not just a
count-based guess. Worth doing once someone actually needs to trace
through a compaction, not before.

## `Access.Resolve` and `Access.OpenFile` disagree on whether reading `FILES_RW_ACCESS` itself is always allowed

Found while building D42 (`files-rw access`), confirmed live. `Resolve`
(`pkg/filesrw/access.go`) has an early bypass - if the target path equals
`FILES_RW_ACCESS`'s own canonical path, reads are always allowed
regardless of root membership, only writes are denied. `OpenFile` doesn't
have that bypass in the same place - it checks root membership *first*,
and only reaches its own denyFileInfo special-case (which permits reads)
if that check already passed. Since every actual read-path operation in
`ops.go` (`ReadFile`, `TailFile`, `SearchScratchpad`, etc.) goes through
`OpenFile`, not `Resolve`, the "reading `FILES_RW_ACCESS` is always
allowed" promise `Resolve`'s own doc comment implies doesn't actually
hold for `files-rw read FILES_RW_ACCESS` in practice - confirmed with a
live repro (`w:repo` as the only rule, `FILES_RW_ACCESS` sitting in the
CWD - `files-rw read FILES_RW_ACCESS` fails with "not covered by any r:
rule"). Not blocking D42 (that command reads the already-parsed `Access`
struct in memory instead of going through this path at all), but worth
deciding whether `OpenFile` should get the same early bypass `Resolve`
has, or whether `Resolve`'s bypass is the one that's actually wrong.

## `wackyproc` has no `prune`/cleanup command - `.proc/` grows unbounded

The original `wackyproc` design doc proposed `prune --max-age`, but the
final implementation (D51) shipped a deliberately minimal `run`/`list`/
`wait`/`get` surface (plus `stop`) without it. Unlike wackypub's own
scratchpad system (a hard 300-entry cap with mtime-based eviction),
nothing ever removes a `.proc/<id>/` directory once created - a
long-running workspace that calls `wackyproc run` repeatedly accumulates
state (meta.json, stdin/stdout/stderr captures, etc.) forever. Not
urgent - each entry is small and this doesn't affect correctness - but
worth a `prune` command (or an eviction policy on `run`, mirroring
`EvictOldestScratchpad`) before `wackyproc` sees heavy real-world use.

## No automatic follow-up turn after a deferred scratchpad image is queued

Confirmed live (via `wackydiscord`, D70's investigation): when `get_scratchpad`
defers a binary/image entry (D49), the canned response says "It will be
available in your next turn. Send another message to continue" - but nothing
actually triggers that next turn automatically. A human has to notice the hint
and manually send another message before the agent ever reacts to the image;
otherwise it just sits queued in `session.jsonl` indefinitely.

Discussed doing this transparently inside `AgentSDK.AddAndGenerateTurnStream`'s
own iterator - detect that a turn just deferred an image (the
`deferredScratchpadIDs` tracking already exists internally in
`FolderAgent.GenerateTurnStream`, would just need to be exposed as a checkable
signal, similar to how `UsageTracker` is already a side-channel alongside the
stream) and automatically call `GenerateTurnStream` again with no new user
message (the session already ends on the image-bearing turn once queued),
yielding that turn's chunks too as part of the same overall stream - fully
transparent to every consumer (CLI, `wackydiscord`), no changes needed
downstream of D69's already-reviewed stream consumers.

Deliberately not decided yet: whether this belongs at the SDK level (every
consumer gets it "for free," but a single `AddAndGenerateTurn` call could then
silently trigger two full LLM generations instead of one - real cost/latency
implications even for one-shot CLI/scripted usage that never asked for
auto-continuation) or scoped specifically to `wackydiscord` (which already has
more context about driving a live back-and-forth, and is where "a human has to
notice a subtle text hint" is most acutely a problem). Either way, needs a
bounded retry cap (a small max auto-continuation count, same spirit as
`maxToolTurns` bounding a tool loop) to avoid a pathological repeat-deferral
loop if the agent's reaction to one deferred image immediately defers another.

## No way to escape `<SCRATCHPAD_DATA ... />` so an agent can write a literal example of it

Hit live: an agent writing a skill file documenting how to use the
scratchpad-macro pattern for loading images needed to include a literal
example of `<SCRATCHPAD_DATA id="X" />` in the skill's own body text - and
couldn't, because `ExpandScratchpadMacros` (`pkg/agent/scratchpad.go`) has no
escape mechanism at all. It's a bare substring check (`strings.Contains(text,
"<SCRATCHPAD_DATA")`) followed by an unconditional regex replace - any
well-formed-looking occurrence gets treated as a real macro reference,
whether the agent meant it as one or was just writing documentation. Confirmed
via code reading: this hits all three call sites the function has
(`run_command`'s `args`, `run_command`'s `stdin`, `create_scratchpad`'s
`text`) - so this isn't just a `create_scratchpad`-specific gap, it also
breaks writing such an example via `files-rw write`/`bash` through
`run_command`'s `stdin`, which is almost certainly the actual path this was
hit through.

No design decided yet, but the shape most templating/macro systems use for
exactly this problem is a leading-backslash escape (`\<SCRATCHPAD_DATA ... />`
- the expander recognizes the backslash, skips expansion for that specific
occurrence, and strips the backslash from the output so the agent gets back
the literal tag text it intended to write). Worth checking whether the same
gap/fix applies to the newer `<SCRATCHPAD_EXPAND id="X" />` sentinel (D61) too,
since it's a sibling convention living in `wackydiscord`, not core - a skill
documenting *that* pattern would hit the identical problem, just with no
core-side expansion to escape from (the escaping problem there, if any, would
be `wackydiscord`-side instead).

## `wackydiscord` silently drops non-image attachments

`HandleMessageCreate`'s attachment loop (`tools/wackydiscord/bot/handlers.go`)
only matches `image/*` content types or `.png`/`.jpg`/`.jpeg` filenames before
calling `downloadAttachment`/`SDK.AddMedia`. Anything else - a `.txt`, `.pdf`,
`.zip`, a code file - just falls through the `if` unhandled: not downloaded,
not surfaced to the agent, not mentioned to the user. The message's text (if
any) still goes through normally, so the attachment just silently vanishes.

Confirmed image attachments go through the real `NormalizeAndResizeImage`
pipeline via `AgentSDK.AddMedia` (D47) - same path as scratchpad-deferred
images. Generic files obviously shouldn't be dumped straight into model
context the way an image turn is, but there's currently no alternative path
at all - no staging to a temp/scratch location the agent could then reach via
`files-rw`, no scratchpad ingestion, nothing. Needs real design (where would
a downloaded file even land - the agent's own directory? a dedicated scratch
subdir? - and how would the agent be told it's there) before building
anything. No design started yet.

## Rename `wackyproc` and create an embedded skill if we don't have one

`wackyproc` is the background-process manager that wraps long-running commands. We link it into agent `tools/` directories (e.g. `dranbo/tools/wackyproc` symlinks to the binary) but unlike `files-rw` and `wackypub` itself, it doesn't have a discoverable `wackyproc skill` subcommand and isn't really "first-class" in agents' eyes.

Open question: do we even need a separate `wackyproc` binary, or is it just `wackypub` with a different command surface? The original rationale was that `wackyproc` is a standalone companion tool, not wackypub-specific. But if we're making `files-rw skill` work the same way, wackyproc should probably follow suit for consistency.

Two pieces of work:
1. **Rename:** `wackyproc` -> some better name? (or leave it) — discuss with Colin
2. **Embedded skill:** if no skill exists, draft one in the style of `files-rw`'s (~19 lines, always_load, the essentials only). Cover: when to use wackyproc (anything that might take a while), the run/list/wait/get/stop lifecycle, the cleanup gap (no auto-eviction of `.proc/`).

Reference: `/wackypub/skills/wackypub-a2a/SKILL.md` for style, and the existing `wackyproc` skill content for substance.

## Fast-follow: Acquire `State.LockChannel` in `handleBindCommand`, `handleVerboseCommand`

`wackydiscord`'s `HandleMessageCreate`, `SyncAgentToChannels`, `handleFillCommand`, and `handleUnbindCommand` now serialize `ChannelBinding` access through `State.LockChannel` (D76). The remaining interaction handlers (`/bind`, `/verbose` in `tools/wackydiscord/bot/commands.go`) still mutate `ChannelBinding` without acquiring the channel lock. While `HandleMessageCreate` includes a defensive `AgentID` guard that prevents stamping old sync markers if `/bind` executes mid-generation, that guard is a band-aid; wrapping `handleBindCommand` in `State.LockChannel` is the real architectural fix to serialize binding creation against in-flight message processing and background sync.

## `CheckLiveness` collapses `CRASHED` into `FAILED` for every observer after the first

Found while reviewing D78 (pre-existing; `liveness.go` untouched by it). `CheckLiveness`
(`liveness.go:60-69`) reads the on-disk `exit_code` file *before* any liveness or crash logic
and reports any non-zero value as `FAILED` - including exit code `137`
(`CrashedExitCode`, `types.go:18`), which is precisely the sentinel that same file's crash branch
persists at `liveness.go:103` and `:113`. `CRASHED` is therefore only visible during the single
`CheckLiveness` call that first detects and writes it; every later reader, whether a subsequent
`list` or a `wait` poll, sees the persisted `137` and reports `FAILED`.

Worse than a lost label: `137` is *also* the legitimate exit status of a process genuinely killed
by `SIGKILL`, which a surviving supervisor records as an honest `FAILED`. Once written, "no exit
code was ever recorded, outcome unknown" and "this process was SIGKILLed and we watched it happen"
are byte-identical on disk. The sentinel is indistinguishable from one of the two states it exists
to separate, so fixing the read order alone is not sufficient - the crash marker needs its own
channel (a dedicated file, or a distinct sentinel outside the 0-255 signal range) rather than a
value in the same field. Verified live by `kill -9`-ing a supervisor and its child process group: `wait` still
returned the ID correctly because `isTerminal` treats both as terminal, but `list` showed
`FAILED`/137 where `CRASHED` was expected. Fix is to read crash state before falling back to the
generic non-zero path. This matters for D79. One of its stated reasons for keeping `consumed` a separate flag
rather than a fourth status is that a `CRASHED` process which gets read must stay identifiable as
crashed - and that is already false today, independent of anything D79 does. Either fix this first,
or amend D79 to rest solely on the other argument, which survives on its own: `status` is derived
fresh on every read by design and should not be merged with a persisted, externally-mutated bit.

## Negative `wackyproc wait` timeouts are swallowed by pflag with a misleading error

Pre-existing, not a D78 regression. `wackyproc wait -5` (and `wait --for <id> -5`) never reaches
`strconv.Atoi`: pflag consumes `-5` as shorthand flag parsing and fails with
`Error: unknown shorthand flag: '5' in -5`. The consequence is that `clampWaitSeconds`' negative
branch has been unreachable through the CLI for as long as it has existed - only reachable via
direct `proc.Wait` calls, which is exactly where the unit tests exercise it. The standard `wait -- -5`
escape works. Worth fixing so the message names the real problem, since anyone hitting it later is
likely to file it as a D78 regression.

## Bundled `wackyproc` skill still documents the pre-D78 `wait` surface

D78 added `wait --for <id>` and made already-terminal-at-entry processes invisible to any-mode
`wait`, and updated `tools/wackyproc/README.md`. It deliberately did NOT touch
`tools/wackyproc/skills/wackyproc/SKILL.md`, which still documents only the bare `wait <seconds>`
form. That file carries `always_load: true`, so it is injected into every agent's prompt: agents are
currently being taught the broken pattern, and nothing tells them the targeted form exists. Worth
covering the two modes and the "already-terminal processes are ignored" contract, since the natural
wrong assumption is that `wait N` means "give me anything that has finished."


## Workspace-root snapshot commit has been a silent no-op since D35, and the feature may deserve a redo

`CreateWorkspaceSnapshot` calls `CommitWorkspaceEvent(wsDir, "system", "snapshot")`. Because the agent
id is non-empty, `ResolveGitRepoDir` joins `<wsDir>/system` and requires that directory to be a git
repo; no such directory exists anywhere, so the root-repo snapshot commit never happens and never
failed. The line is byte-identical to the original D35 commit (0b0d3c4), the guard
`IsWorkspaceGitRepo(wsDir)` makes it look intentional, and it returns nil before any failure
reporting, so it stays invisible (found during the git-warning review, session git-warn-review).
Separately, no caller passes agentID == "" anywhere, so the `repoDir == wsDir` branch in
`ResolveGitRepoDir`/`CommitWorkspaceEvent` is effectively dead code.

Fixing the routing is one line but changes behavior (the root repo really starts committing, and is
ignored by D35's own workspace root gitignore which excludes everything except .gitignore,
WACKYPUB_ROOT, MANIFEST.md), so it needs sign-off rather than a silent fold-in. Colin said the
whole feature was "always wonky" and might be worth redoing: candidates are a dedicated snapshot
store, routing "system" to the root repo deliberately, dropping the snapshot commit entirely, and
the reviewer's broader recommendation, falling back to the `git` CLI when go-git fails, which is the
one change that addresses go-git's narrower config/validation rather than patching symptoms.


## Mid-turn short-circuit should compact instead of requiring a "continue" turn

When accumulated tool context crosses the compaction threshold mid-turn, the harness returns a
synthetic response telling the model to send another message to proceed (`adk_agent.go:187-215`,
tracked via `StoppedEarlyForCompaction`, D63/D68). The assumption is that the next top-level turn
triggers compaction, but if the next turn is another tool-heavy one, or the trigger is missed, the
bail wastes both a user message and budget. Colin's proposal: when we bail in this state, issue the
compaction directly rather than waiting. Compaction is safe at this boundary because the bail point
is between tool turns. Interplay to preserve: D77 skips forced compaction on the turn a mid-turn
short-circuit interrupted, so the auto-compact must be careful not to conflict.


**Shelved per Colin (2026-09-02).** Colin: this should be part of a broader "multi-turn" turn pattern, the same shape the deferred image queueing has, where a turn needs a follow-up to complete (compact-on-bail, image queueing). Do not re-open as a compaction-only fix; fold into the multi-turn pattern when that is designed.
## `wackyproc` "peek" command for stdout

`wackyproc get <id>` returns the full captured stdout/stderr of a process record and (per D79
consumption-order disposal) interacts with when the record is considered consumed. There is no way
to check a long-running process's latest output cheaply without a full dump. Add `wackyproc peek
<id> [lines]` (or a `--tail` flag on `get`) that shows the trailing N lines of stdout without
draining or consuming the record, for progress checks on still-running processes. wackyproc
subcommands today: run, list, wait, get, stop, supervise, skill.


**Decision: D82 (implemented, nested repo `b67427d`).**
## ~~Skills don't need to render `@` file pointers~~ (closed: verified non-issue — the only `ExpandMacros` caller is `RenderAgentSystemPrompt` on `AGENTS.md`; skill content is never macro-expanded on either the always-load or `load_skill` path)

**Colin's follow-up, tested and verdict stands.** He hypothesized that system-prompt assembly inflates `@` macros in autoloaded skills. Empirically disproved: built a scratch agent with an `always_load: true` skill containing `@other-file.md` and `@missing-file.md` (the first a real file in the agent dir), rendered via `RenderAgentSystemPrompt`, and both pointers survived verbatim inside the `<AUTOLOADED_SKILLS>` block. Code confirms: the block is appended AFTER `ExpandMacros(AGENTS.md)` and nothing re-expands the assembled prompt; the only `ExpandMacros` call site in the tree is `macro.go:39`.

`ExpandMacros` (`pkg/agent/macro.go:16`) expands `@path` references in content (`relPath :=
strings.TrimPrefix(match, "@")` at macro.go:61, exercised in macro_test.go). Skill content that
carries `@` pointers gets this rendering even though skill files are shipped material; the pointers
in a skill are references for the reader, not templates to expand. Scope where the skill load path
triggers expansion and skip it there, or only expand on explicit intent. The exact render site needs
confirming before implementing.

## `wackypub agent compact` should accept an alternate COMPACT.md file

The CLI compact (`cmd/agent.go:382` `agentCompactCmd`) always uses `<agentDir>/COMPACT.md` via
`LoadCompactConfig` (`compaction.go:111-116`). Add a flag (e.g. `--md-file <path>`) so a
CLI-triggered compact can use a different COMPACT.md, for one-off compaction recipes or a different
compact-pct/frontmatter without mutating the agent's real file.


**Decision: D83 (implemented, `fada78a`).**
## Append-only COMPACT.md needs truncation or rotation at some point

COMPACT.md is append-only by design (append-only/compact-pct frontmatter, embedded default from
`examples/compaction/COMPACT-append.md`, D45/D46) and its body grows unbounded across compactions;
no truncation exists anywhere in `compaction.go`. Decide a cap: summarize or prune old body lines,
keep the last N, or fold guidance into MEMORY.md. Careful: the body carries accumulated instructions
for future compactions, so naive truncation loses drift guidance. Related to the existing "Modular
compaction strategy" TODO.


**Left as a TODO per Colin (2026-09-02)** — may be a duplicate of the existing `MEMORY.md grows forever` TODO — COMPACT.md is read-only from the harness side (no write sites), and `append-only: true` governs MEMORY.md amends, not COMPACT.md growth. Revisit later; likely close as duplicate in favor of the MEMORY.md TODO.
## ~~Does files-rw honor absolute paths?~~ (closed: empirically verified — an absolute path lands exactly where given; the earlier finding was a relative-path mistake resolving against the agent workspace, which is the documented contract)

Empirical, from a real session: `files-rw write /wackypub/pkg/agent/git_warning_test.go` wrote to
`~/workspace/dranbo/pkg/agent/git_warning_test.go` instead, i.e. the path was treated as relative to
the agent's workspace and the absolute prefix was silently re-based. Either accept absolute paths
(with the same ACL/escape checks the rest of the harness applies) or reject them loudly so callers
learn the workspace-relative contract instead of losing files into their own agent dir. The tool
lives outside pkg/agent (tools/files-rw), so its argument handling needs its own check.

## wackydiscord `/stop` command

Slash commands today are bind, unbind, status, fill (`tools/wackydiscord/bot/commands.go:11`). No
way to cancel an in-flight agent turn from Discord; the existing "No way to cancel an in-flight
agent task" TODO (above) is the general form. Add a `/stop` interaction for the bound agent. Risk:
needs cancellation plumbing in the turn runner (generation stop / context cancel), which may not
exist yet, so may need harness work before the Discord surface.



**Decision: D85 (implemented, `01fb5a5` + nested `527e150`).** Colin: yes — a general, context-based cancellation pattern at the SDK level, applying to the CLI too, with `/stop` as one consumer.
## `wackypub agent compact` should accept a runtime override (large context -> smaller model)

Alongside a COMPACT.md override, compaction takes contextWindow and PreserveThinking from
runtime.json (`CheckAndCompactSession` at compaction.go:176/192/199/214-215, and the summary
model from the agent's configured runtime). Add a flag (e.g. `--runtime <path.json>`) so a
CLI-triggered compact can target a different runtime than the agent's live one. Colin's use case:
compact a large-context session down to a summary sized and priced for a smaller model, e.g. an
agent normally running a big-context model can have its session compacted against a
small-context/small-model runtime.json without changing what the agent runs on day to day.
Interplay: summary thresholds and overhead pct still come from the compaction config; the runtime
override only swaps contextWindow/PreserveThinking/model for this run.

**Decision: D84 (implemented, `9dd2a19`). Note the corrected shape: the override must build a disposable agent from the runtime, not just swap `runtimeCfg`, because the summarizer model comes from the agent, not the config parameter (see D84).**


## `@` macro expansion in the AGENTS.md include chain mangles email addresses and @-handles

`RenderAgentSystemPrompt` expands `@<FILE_PATH>` recursively over AGENTS.md and every file it
includes (`macroRegex = @([a-zA-Z0-9_\-./]+)`, `macro.go:12`). Any email address or @-handle in an
included file (IDENTITY.md, SOUL.md, USER.md, TOOLS.md, etc.) matches the regex and is treated as a
file path, producing `<-- Error reading macro file <token>: ... -->` markers (`macro.go:73`) that
destroy the original text. Live evidence in Dranbo's own rendered prompt: `dranbofieldston@agentmail.to`
became `dranbofieldston<!-- Error reading macro file agentmail.to: ... -->`, and `@DranboF` became a
marker too. Same class as the D80 residuals note about first-class false positives: expansion that is
happy to eat prose. Fix options are a design choice, pending Colin: an escape convention (`@@` or
backslash), only expand when the target file actually exists (typo'd macros then fail silently instead
of loudly), or a narrower regex (e.g. require a known extension or a path containing `/`). Note the
include chain itself must keep working.


## No way to ask how much context an agent session is using

There is no surface for "how big is this agent's session right now" outside the internal compaction
path. `EstimateTokens(turns, includeThinking)` exists (`session_store.go:149`) and the turn tracker
records `LastPromptTokens` from the last model response (`adk_agent.go:107/151`), both consumed
internally by `CheckAndCompactSession` (`compaction.go:200`) to decide whether to compact. But
nothing exposes the count to an operator or to the agent itself. `EstimateTokens` is rough
(heuristic per-content estimate), while `LastPromptTokens` is provider-reported and only reflects
the last request, not the whole session. TODO (no decision yet, Colin 2026-09-02): provide some way
to figure out the context count for a current agent session — questions to settle later: the
surface (CLI `wackypub agent <id> context`? SDK method? tool?), the source (estimate vs provider
usage vs a combination), and whether the agent should be able to ask itself.


## Git-style hook scripts as injection points (date, RAG, A2A headers)

**Decision: D87 (not yet implemented).**

There is no per-turn extension point where an operator can inject content without editing prompts. Proposal (Colin, 2026-09-03): git-hook-like scripts that run at defined lifecycle points. Concrete drivers:

- **Date injection**: agents have no notion of current date; they must shell out to `date` to write correctly-named daily memory files. The obvious first hook.
- **RAG injection**: retrieve relevant memory into context before a turn.
- **A2A headers**: hooks able to inject into or amend the A2A metadata on outbound peer messages.

Design surface: the event set (turn-start / pre-request-build / a2a-send / ...), hook placement (agent dir vs workspace root), the execution contract (what arrives on stdin/env, what a hook may modify, how results merge back), and failure semantics (does a failing hook fail the turn or warn?).

## Queued image loads should auto-trigger a follow-up turn

**Decision: D88 (in the works — punted 2026-09-03).**

The image pipeline is currently two turns: an image loaded into a scratchpad entry surfaces only on the NEXT turn, so an agent must deliberately end a turn just to see it (load, describe, wait). Proposal: when image loads are pending at end-of-turn, auto-trigger a follow-up turn carrying them instead of idling until the user pings. This is one more instance of the "turn that needs a follow-up to complete" pattern already gathering TODOs (compact-on-bail, deferred image queueing) - design it as part of that multi-turn pattern rather than ad-hoc.

## wackydiscord: verbose tool output lands after the turn; consider rendering from the session.jsonl watch

**Decision: D89 (in the works — questions pending).**

Verbose mode appends tool calls at the END, after the agent has finished its turn, instead of interleaving them in the order they happened. Question (Colin, 2026-09-03): does wackydiscord need a fundamental change - going back to rendering Discord output based ENTIRELY on the session.jsonl watch (the earlier design)? Under watch-based rendering the one hard case is deduplication of the user turn: the user's Discord message also appears in the watched session, so the watcher must suppress re-echoing it. Weigh push-based stream rendering (current) vs watch-based rendering (ordering falls out for free, single source of truth) before touching the display order.
