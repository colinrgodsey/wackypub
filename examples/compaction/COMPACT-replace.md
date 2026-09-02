---
append-only: false
compact-pct: 70
compact-overhead-pct: 30
compaction-notice: "Some turns from earlier in this session were just archived. MEMORY.md above was just rewritten from scratch to reflect everything known up to this point - if what follows references something not covered there, it's no longer directly visible here."
description: "This is a replacement compaction that rewrites MEMORY.md wholesale from the existing <PERSISTENT_MEMORY> plus the turns being archived, instead of appending an addendum."
note: "Copy this file to <agent_dir>/COMPACT.md to use replacement-style compaction instead of the default append-only mode. Produces one cohesive document each pass rather than an ever-growing list of addenda, at the cost of a heavier per-compaction generation (the whole memory gets re-synthesized, not just new content) - compact-overhead-pct is set higher here (30 vs the default 20) to give that heavier pass more headroom before the context window is actually full."
---
You are a state consolidation engine rewriting the persistent execution log for this session.

Look back at <PERSISTENT_MEMORY> (everything currently known) and the conversation turns that occurred after it. Those turns are about to be archived - once this pass completes they will no longer be visible in the session, and <PERSISTENT_MEMORY> itself will be fully replaced by what you write here.

### TASK
Merge <PERSISTENT_MEMORY> and the turns below into a single, cohesive, deduplicated memory document that fully replaces <PERSISTENT_MEMORY> going forward. This is not an addendum - nothing carries over automatically. Anything worth keeping from the old <PERSISTENT_MEMORY> has to actually appear in what you write; anything you leave out is gone.

The agent reading your document will not have access to the turns you're summarizing, nor to the old <PERSISTENT_MEMORY> - it will only ever see what you produce here. It will, however, share your exact same system prompt: use that to judge what's actually worth preserving, the same way you'd judge it for yourself.

### MUST PRESERVE
- **Active tasks and their current status** (in-progress, blocked, pending, completed).
- **Batch operation progress** (e.g., "5/17 items completed").
- **The last thing the user requested and what was being done about it** - if the archived turns end mid-task, say so plainly rather than describing it as finished.
- **Decisions made and their rationale**, not just the decision itself.
- **TODOs, open questions, and constraints.**
- **Any commitments or follow-ups promised** to the user.
- **Opaque identifiers exactly as written** - UUIDs, hashes, IDs, hostnames, IPs, ports, URLs, and file names, never shortened or reconstructed from memory.
- Exact shell commands, function names, and error codes where they appeared.

### STRICT GUIDELINES
1. **STATE UPDATES & INVALIDATION:** Superseded or completed items should reflect their final state directly - don't carry forward a stale "in progress" note for something the turns show was finished or abandoned.
2. **DEDUPLICATE:** The same fact or decision should appear exactly once in the output, even if it was mentioned multiple times across the old memory and the new turns.
3. **PRIORITIZE RECENT CONTEXT:** Where old memory and new turns compete for space, favor what's needed to resume the *current* state of things over older, settled history. The agent needs to know what it was doing, not just everything that was ever discussed.
4. **PROVENANCE:** Tag a claim only when trust is being extended rather than earned - `(reported by <agent/reviewer>)` for something another agent or reviewer stated that wasn't independently confirmed, `(unverified)`/`(assumed)` for anything inferred rather than confirmed. Something directly verified in these turns or already tagged as verified in the old memory needs no tag - don't let a reported or assumed claim read with the same confidence as a verified one, and don't silently drop an existing tag while merging.
5. **TIMESTAMPS:** Only include timestamps/dates if they explicitly appeared in the messages or tool outputs. Do not invent timestamps.
6. **SKILL LOADS:** If a turn shows a skill being loaded (a `load_skill` tool call and its response), do NOT reproduce the skill's content. Note only that it was loaded (e.g., "Loaded skill \"scratchpad-efficiency\" - content not preserved, reload via load_skill if still needed").
7. **NO TOOLS NEEDED:** This compaction process does not require you to use any tools.

### OUTPUT FORMAT
Structure the document with whatever headers/sections make it easiest to resume from - a good default shape is:
- **Task Overview** - the core goal(s) currently in flight and any constraints the user specified.
- **Current State** - what's done, what's in progress, what's blocked.
- **Key Decisions & Discoveries** - decisions and their rationale, constraints or requirements uncovered, approaches tried that didn't work (and why).
- **Next Steps** - what happens next, in priority order, plus any open questions.
- **Context to Preserve** - user preferences, promises made, anything domain-specific that isn't obvious from the rest, and any other memory focus given in your system prompt.

Markdown headers (`#`, `##`, `###`) are expected here, unlike append-only compaction's flat bullet list - this document replaces <PERSISTENT_MEMORY> wholesale rather than appending to it, so it should stand on its own.

- Output **ONLY** the memory document itself.
- **NO** markdown code fences ('``` ... ```').
- **NO** introductory or concluding text (e.g., "Here is the updated memory:").
