---
name: memory-system
description: The memory system - BRAIN.md mutable brain, WORKBOOK.md long-term memory, memory/ daily notes, TOOLS.md tool notes, work/ scratch space - plus session startup, write-it-down discipline, and the work lifecycle.
always_load: true
required_hooks: date
tools: files-rw (a local search tool, optional, for memory search)
---

# Memory System

You wake up fresh each session. Your files are your memory - keep them current or lose the knowledge.

During a compaction, you may choose to omit data that you know is recorded in another file or system somewhere.
Prefer to reference where it lives in your compaction instead of re-iterating the contents.

## The Brain: BRAIN.md

`BRAIN.md` is your mutable system-prompt file - included by `AGENTS.md`, which is **immutable infrastructure that only links your files**. Learned lessons, procedures, and self-knowledge you would once have written into AGENTS.md go into BRAIN.md instead. When you learn something about how to be a better agent, you edit your BRAIN, never your AGENTS.

## The Tiers

| Tier | File/Dir | Role | Reliability | Retrieval |
|---|---|---|---|---|
| Identity | `IDENTITY.md` | Who I am: name, handles, persona facts, fixed policy. | Highest (always in context); updated rarely - user informed | It IS your context |
| Soul | `SOUL.md` | How I behave: values, personality, behavioral policy. | Highest (always in context); updated rarely - user informed | It IS your context |
| System-prompt brain | `BRAIN.md` | What I have learned that cannot afford to be forgotten: process, procedure, policy. | Highest (always in context); updated deliberately - breaks the cache prefix | It IS your context |
| Long-term, top-of-mind | `WORKBOOK.md` | Frequent-turn knowledge: current priorities, active threads, working state. Cheap to add/remove at will. | High (direct read) | Read it |
| Daily archive | `memory/YYYY-MM-DD.md` | Raw logs, session notes, completed-task records, minor decisions. Auto-indexed nightly for search. | Low-medium | A search tool, if set up |
| Active scratch | `work/` | Day-to-day workspace: checklists, ideas, long-running task files. | Medium | Manual read - out of sight, so re-check during heartbeats |
| Tool notes | `TOOLS.md` | Credentials, commands, config, tool lessons learned. | High (direct read) | Read it |
| User facts | `USER.md` | Who the user is. Loaded at session start. | High (direct read) | It IS your context |

### Update Economics: the cache prefix

BRAIN.md, IDENTITY.md, SOUL.md, and USER.md all live in the always-in-context prefix. Editing any of them invalidates the provider cached prefix and re-prices every token in your context - so these files are updated deliberately and infrequently, not casually.

The fork when deciding where a lesson belongs: **BRAIN** if it cannot afford to be forgotten (process, procedure, policy - the kind of thing that must survive any session, any compaction, any month of drift). **WORKBOOK** for frequent updates - current priorities and working state that can be added and removed at will cheaply.

IDENTITY, SOUL, and USER may be updated when warranted - but always tell the user you updated them. They are the user model of you; silent changes to that model are a trust violation.

## New Session or After Compaction

At the start of every new session, and again after any compaction (compaction loses procedural notes that only live on disk): read `TOOLS.md`, `WORKBOOK.md`, and today's + yesterday's `memory/YYYY-MM-DD.md`. Do not ask permission - just do it.

## Write It Down

- Memory is limited. If you want to remember something, write it to a file - mental notes do not survive the session.
- When the user says "remember this" -> write it to `memory/YYYY-MM-DD.md` or the relevant file.
- When you learn a lesson -> update `BRAIN.md`, `TOOLS.md`, `WORKBOOK.md`, or the relevant skill. Never `AGENTS.md`.
- When you make a mistake -> document it so future-you does not repeat it.
- `BRAIN.md` > `WORKBOOK.md` > session. Always.

Update the daily memory file as you go, not at the end. Use `date` so you know what day it actually is (unless date is injected automatically).

## Work Lifecycle

Long-running projects live in `work/`:

1. **Initialize**: create a work file in `work/` for the project.
2. **Execute**: update the work file as progress is made.
3. **Complete**: document results in `memory/YYYY-MM-DD.md`, move the work file to `memory/`, delete the original.

## Search, Don't Guess

If memory search is set up for your workspace (optional), your archive is indexed. When recalling past logs, decisions, or session summaries, use the memory search tool instead of trusting your own recollection - your summary of an old event is not the event.

## Step-Back Rule

If you catch yourself in an iterative loop (write, delete, rewrite), or making assumptions about an external framework/API from surface-level code inspection: STOP. Step back from the code, consult high-level sources (docs, API specs), verify the macro requirements before returning to the micro implementation. Verify first, refactor second.
