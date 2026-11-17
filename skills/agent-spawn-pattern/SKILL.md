---
name: agent-spawn-pattern
always_load: false
description: UNVALIDATED manual recipe for creating a persistent helper agent by hand, for environments where the coordinator has not shipped `wackypub agent spawn`. Prefer the command family; use this only when you must do it yourself and expect no validation.
---

# Manual helper-agent recipe (UNVALIDATED)

**This pattern is not validated by anything.** Nothing checks a hand-made scaffold: a
broken `@MACRO` in `AGENTS.md` renders as literal text instead of failing, and one did for
159 turns before anyone noticed. If `wackypub agent spawn` exists in your build, use it -
it refuses a dangling macro, an unparsable runtime, a free-text endpoint, a nested
workspace root, and a second generation of children. This page exists for the interim.

Ask the coordinator (or whoever owns the workspace) to do the workspace-level steps. You
cannot and should not do them yourself: creating an identity is not agent self-service.

## Minimum viable child

Four files make a working agent; everything else is payload.

1. `<ws>/<child>/runtime.json` - copy a preset the operator approved. Do not invent an
   endpoint; the preset folder exists so somebody decides what a child may spend against.
2. `<ws>/<child>/AGENTS.md` - the prompt. Every `@FILE` reference must be a file that
   actually exists in the child directory.
3. `<ws>/<child>/WACKYPUB_ALLOWED_AGENTS` - one line, the parent. A child starts closed.
   An empty file means deny-all.
4. `<ws>/<child>/MEMORY.md` - one line of prose; compaction fills it in later.

`WORKBOOK.md` (working state) and `IDENTITY.md` (who it is) are the convention this repo's
own scaffolds use; keep them. `session.jsonl` appears on the first turn - do not hand-write
it.

## Checks to run before calling it done

- `wackypub workspace <child>` reports zero issues, and the `AGENTS.md` line does not warn
  about a missing macro target.
- `wackypub agent render-prompt <child>` shows real text where the macros were, not
  `@MEMORY.md`.
- The child directory contains **no** `WACKYPUB_ROOT` marker. A child that owns a workspace
  root outranks every roster in the tree and is not a child, it is a bypass.
- Lineage is written down somewhere a human reads: `SPAWN_META` in the child directory with
  `child`, `parent`, `depth: 1`, `runtime`, `status: active`, `created_at`. Without it the
  child is invisible in `wackypub workspace`'s PARENT column and unaccountable later.
- Tools and skills are linked or referenced, never copied. A child with 80 copied skills is
  a cost, not a capability.

## Retiring

Move the directory out of the workspace root (for example `<ws>/archive/spawned/`), remove
the parent's roster entry, and mark the lineage `status: retired`. Never delete a child's
session or memory: it belongs to the workspace.
