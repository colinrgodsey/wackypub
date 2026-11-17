---
name: agent-spawn-request
always_load: false
description: How to ask for a persistent helper agent instead of trying to create one - what to request, what a coordinator needs from you, and how the granted child behaves. Use when work is long-running, parallel, or needs standing memory you do not want in your own session.
---

# Asking for a helper agent

You do not create agents. Spawning multiplies capabilities, so the decision stays with the
operator or coordinator who runs `wackypub agent spawn`; your job is to ask well.

## When to ask

- The work is standing, not one-shot: a digest, a watcher, a queue you keep feeding.
- The work needs its own memory and session history that would pollute yours.
- Several independent streams run in parallel and each needs to accumulate state.

When the work is a single task with no future, use a background command instead
(`wackyproc`) - that is not a new identity.

## What to put in the request

One short message naming four things:

1. **Name** one purpose, e.g. `digestor`, `watcher-pr-ci`.
2. **Runtime** you need from the workspace presets, and why - cheaper model for bulk work,
   a stronger model for reasoning. Ask `wackypub agent runtimes` (or have the coordinator
   run it) to see what exists. You may not name an endpoint or a model directly.
3. **Lifetime**: standing or until a date, and who retires it.
4. **Containment tier** you and the human agreed. Say it out loud; in this version the tier
   is recorded in lineage and enforced by nothing, so agreement is the only control.

Example request:

> Requesting a persistent helper `ci-watcher` for the nightly `-race` failures. Preset
> `local-cheap` (bulk log triage). Standing, until the CI split lands; I will ask to retire
> it then. Tier 2, as agreed on 2026-11-16.

## What you get

- A workspace-level agent directory, listed in `wackypub workspace` with you in its PARENT
  column, and in `MANIFEST.md`.
- Its allowlist contains only you. It cannot talk to your peers unless somebody adds them.
- Depth 1 and no deeper: `spawn` refuses to let a helper spawn a helper.
- Turns are driven by you (`wackypub agent <child> prompt ...`). There is no heartbeat and
  no background wake unless someone sets one up deliberately.

If the pair needs a break, ask for `pause` (reachability removed, memory intact) rather than
`delete`. `retire` archives the subtree; `delete` only ever removes an already-archived copy.
