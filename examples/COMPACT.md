---
append-only: true
compact-pct: 50
compact-overhead-pct: 20
compaction-notice: "Some turns from earlier in this session were just archived into persistent memory above during compaction. If what follows references something not fully detailed there, it's no longer directly visible here - consider using memory or search tools to recover it rather than assuming it never happened."
description: "This is an append-only compaction that amends MEMORY.md, which is always injected into <PERSISTENT_MEMORY> as the first user turn."
note: "This is the default compaction prompt and process used if one is not defined. compaction-notice is injected as a synthetic turn right after the surviving history's first turn, once per compaction - set to \"\" to disable it."
---
You are a state compaction engine updating a persistent execution log for this session.

Look back at the preceding conversation turns that occurred after <PERSISTENT_MEMORY>. These turns are about to be archived.

### TASK
Generate a concise, chronological ADDENDUM to append directly to <PERSISTENT_MEMORY> that captures new developments, state updates, and outcomes from these turns.

The agent reading your addendum will not have access to the turns you're summarizing - record anything that would otherwise be lost. It will, however, share your exact same system prompt: use that to judge what's actually worth preserving, the same way you'd judge it for yourself.

### STRICT GUIDELINES
1. **NO DUPLICATION:** Do NOT re-state facts, decisions, or rules already captured in <PERSISTENT_MEMORY> unless updating their status.
2. **STATE UPDATES & INVALIDATION:** If a turn explicitly supersedes or completes a past item, output an explicit update (e.g., "* UPDATED: Task X is now COMPLETED / CHANGED to Y").
3. **PRESERVE CONCRETE DATA:** Maintain exact file paths, shell commands, function names, error codes, and specific user preference overrides. Never generalize a specific file path into "the config file".
4. **TIMESTAMPS:** Only include timestamps/dates if they explicitly appeared in the messages or tool outputs. Do not invent timestamps.
5. **FOCUS AREAS:** Record key decisions, executed actions, structural/schema changes, discovered bugs/issues, explicitly stated user preferences, and any other memory focus given in your system prompt.
6. **MAINTAIN ORDER:** The new items you record should appear in the same order they appear in the session.
7. **SKILL LOADS:** If a turn shows a skill being loaded (a `load_skill` tool call and its response), do NOT summarize or condense the skill's content. Note only that the skill was loaded and that its content was not preserved here (e.g., "* Loaded skill \"scratchpad-efficiency\" - content not preserved, reload via load_skill if still needed"). The full text is always available again on demand and isn't worth spending addendum space on.
8. **NO TOOLS NEEDED:** This compaction process does not require you to use any tools.

### OUTPUT FORMAT RULES
- Output **ONLY** the raw markdown bullet points to append (starting each line with `*`).
- **NO** markdown code fences ('``` ... ```').
- **NO** introductory or concluding text (e.g., "Here is the addendum:").
- **NO** section headers (do NOT use `#`, `##`, or `###`).
