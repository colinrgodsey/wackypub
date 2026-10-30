---
name: bashc
always_load: true
description: Sandbox execution environment - explains bashc (containerized bash), what folders are shared, and how to use your container-ws scratch space. Load this before using bash when your tools expose bashc instead of bash.
---

# bashc - your sandboxed shell

You do not have a host `bash`. Your `run_command` bash binary is `bashc`, which executes every command **inside an isolated Docker container**. Everything still works like bash - same syntax, same exit codes - but the filesystem you see is deliberately limited.

(Operator setup reference, not something you need to act on: `scripts/sandbox/README.md` in the wackypub repo.)

## What the container can see

Exactly three host folders are bind-mounted, each at its **identical absolute path** inside the container (no path translation - a path that works on the host works verbatim in-container):

| Mount | Path (same inside and out) | Rules |
|---|---|---|
| Project worktrees | `<workspace>/worktrees` | Work here - but **only your assigned worktree**. Sibling worktrees belong to other agents; never modify, commit, or build in them. |
| Project source | `<workspace>/projects` | Read freely; **generally do not write here**. This is the canonical clone that coordination happens through. |
| Your scratch space | `<workspace>/<your_agent_id>/container-ws` | Yours alone. General-purpose files, test output, downloads, temp scripts. |

Everything in the bashc container outside of these folders is ephemeral.

Everything else on the host - other agents' workspaces, config, and state - does not exist in the bashc container.

## Working directory

bashc commands run with the working directory already set to `<workspace>/<your_agent_id>/container-ws`. So:

```bash
pwd          # -> <workspace>/<your_agent_id>/container-ws
cp foo.txt . # lands in your container-ws
```

You can `cd` anywhere within the three mounts, but avoid relying on cwd when working outside container-ws - be explicit with full paths (e.g. path-scoped git commands rather than bare ones).

## What changes in practice

- **File identity is preserved.** The container's `agent` user has the same UID as the host workspace owner, so files you create in any mount appear with correct ownership on the host and to other agents. `whoami` inside the container reports `agent` - that's expected.
- **You cannot push.** You CAN make local commits in your assigned worktree (the same-path mount preserves the worktree's `.git` link to the canonical repo). Credentials live on the host; the coordinator handles pushes.
- **No host state leaks in.** Host environment variables and processes are not visible. Do not expect host-set env vars to be present in bashc commands.
- **The container is persistent.** It survives across your turns and host reboots. Files you leave in container-ws stay put - use it as your durable scratch, same as scratchpad but for files.

## Etiquette

1. **Your worktree only.** Absolute rule. If a task needs a different worktree, tell the coordinator.
2. **`projects/` is read-mostly.** Reading source is fine and expected; creating branches/worktrees there happens host-side via the coordinator, not in your sandbox.
3. **container-ws is your yard.** Keep scratch tidy-ish, but you never need to ask permission to use it.
