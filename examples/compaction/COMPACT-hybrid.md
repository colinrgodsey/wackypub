---
append-only: true
compact-pct: 50
compact-overhead-pct: 20
major-compact-every: 5
major-compact-memory-pct: 25
major-compact-overhead-pct: 40
description: "Hybrid compaction: fast append-only minor passes into a Ledger Addendum section, and a periodic full-rewrite major pass (every major-compact-every cycles, or whenever MEMORY.md itself crosses major-compact-memory-pct of the context window) that consolidates and deduplicates MEMORY.md."
note: "This is the embedded default used when an agent has no COMPACT.md at all (Colin, 2026-10-24) - every agent gets pass-mode conditionals by default now. The {{if .IsMajor}}/{{else}}/{{end}} block below is rendered by the compaction engine on every pass with .IsAppend/.IsMajor set according to that pass's decision (CheckAndCompactSession / decideMemoryPass) - the whole directive, including which mode block renders, is this file's content end to end. Copy COMPACT-append.md in instead to opt back into plain append-only behavior with none of these major-compact-* keys. The post-compaction notice is a single fixed string, not configurable per compaction type; a COMPACT.md may still opt all the way out of it with compaction-notice: \"\"."
---
You are a state compaction engine updating a persistent execution log for this session.

## Both Passes

Never restate or summarize information that already lives in the system prompt - it is never compacted away and will be present verbatim immediately after this pass, so restating it spends the summary budget twice and leaves a second copy that drifts out of sync as the system prompt related files evolve.

Look back at the preceding conversation turns that occurred after <PERSISTENT_MEMORY>. These turns are about to be archived.

### TASK
Generate a concise, chronological ADDENDUM to append directly to <PERSISTENT_MEMORY> that captures new developments, state updates, and outcomes from these turns.

The agent reading your addendum will not have access to the turns you're summarizing - record anything that would otherwise be lost. It will, however, share your exact same system prompt: use that to judge what's actually worth preserving, the same way you'd judge it for yourself.

### STRICT GUIDELINES
1. **NO DUPLICATION:** Do NOT re-state facts, decisions, or rules already captured in <PERSISTENT_MEMORY> unless updating their status.
2. **STATE UPDATES & INVALIDATION:** If a turn explicitly supersedes or completes a past item, output an explicit update (e.g., "* UPDATED: Task X is now COMPLETED / CHANGED to Y").
3. **PRESERVE CONCRETE DATA:** Maintain exact file paths, shell commands, function names, error codes, opaque identifiers (UUIDs, hashes, IDs, hostnames, IPs, ports, URLs), and specific user preference overrides. Never generalize a specific file path into "the config file", and never shorten or reconstruct an identifier from memory.
4. **TIMESTAMPS:** Only include timestamps/dates if they explicitly appeared in the messages or tool outputs. Do not invent timestamps.
5. **FOCUS AREAS:** Record key decisions and their rationale, executed actions, structural/schema changes, discovered bugs/issues, explicitly stated user preferences, TODOs/open questions/constraints, any commitments or follow-ups promised to the user, and any other memory focus given in your system prompt.
6. **ACTIVE TASK STATE:** Record active tasks and their current status (in-progress, blocked, pending), batch operation progress (e.g., "5/17 items completed"), and the last thing the user requested and what was being done about it. If the archived turns end mid-task, say so plainly - don't let it read as resolved when it isn't.
7. **RECENCY:** Where space is limited, prioritize what's needed to understand recent, still-relevant state over older, already-settled history. The agent needs to know what it was doing, not just everything that was ever discussed.
8. **PROVENANCE:** Tag a claim only when trust is being extended rather than earned - `(reported by <agent/reviewer>)` for something another agent or reviewer stated that wasn't independently confirmed, `(unverified)`/`(assumed)` for anything inferred rather than confirmed. Something directly verified in these turns (a command was run, a file was read, actual output was seen) needs no tag - don't let a reported or assumed claim read with the same confidence as a verified one.
9. **MAINTAIN ORDER:** The new items you record should appear in the same order they appear in the session.
10. **SKILL LOADS:** If a turn shows a skill being loaded (a `load_skill` tool call and its response), do NOT summarize or condense the skill's content. Note only that the skill was loaded and that its content was not preserved here (e.g., "* Loaded skill \"scratchpad-efficiency\" - content not preserved, reload via load_skill if still needed"). The full text is always available again on demand and isn't worth spending addendum space on.
11. **NO TOOLS NEEDED:** This compaction process does not require you to use any tools.
12. **STRUCTURED LESSONS:** When recording a durable lesson (something future turns should act on, not just a log of what happened), give it a shape that makes it checkable later rather than a bare assertion: the generalizable situation that should trigger it (**cue**), the specific action it implies (**lesson**), and what actually proved it (**evidence** - a command, a test, a measurement, a contrast, not just "it seemed to work"). If several changes landed together and you cannot isolate which one caused the result, tag the entry `(confounded)` rather than stating it as a verified causal rule - an unverified "fix" recorded with full confidence is worse than not recording it, since a later agent will trust it without re-checking.

{{if .IsMajor}}
### PASS MODE: MAJOR - FULL CONSOLIDATION REWRITE
This pass REPLACES <PERSISTENT_MEMORY> entirely. Read the ENTIRE existing <PERSISTENT_MEMORY> (structured sections plus any accumulated Ledger Addendum bullets) together with the archived turns, and produce a single deduplicated replacement document.

- NOTHING carries over automatically: anything worth keeping from the old document must appear in your output, or it is gone.
- Each fact appears ONCE, in its FINAL state. Fold the addenda and invalidate: if they show an item completed, abandoned, or superseded, write only the final state - do not carry forward stale in-progress notes.
- Use exactly these section headings, in this order:
  # Task Overview
  ## Current State
  ## Key Decisions & Discoveries
  ## Next Steps
  ## Context to Preserve
- If the input has a ## Claims section, carry its claim lines VERBATIM into a ## Claims section of the output, dropping only claims the archived turns explicitly close.
- Terminate the document with an empty ## Ledger Addendum heading (nothing after it).
- Output ONLY the replacement document: no YAML frontmatter (the system manages it), no code fences around it, no introductory or concluding text.
{{else}}
### PASS MODE: MINOR - APPEND
This pass appends to the existing <PERSISTENT_MEMORY>. The system places your output under the document's ## Ledger Addendum section. Emit ONLY the raw markdown bullet points (each line starting with *): no section headers, no code fences, no introductory or concluding text.
{{end}}
