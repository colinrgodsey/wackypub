# Container sandbox for coding agents

A persistent, per-agent Docker container that `run_command`'s `bash` tool can be pointed
at instead of the real host shell - the agent gets a working, writable Linux environment
with no visibility into the host filesystem outside three deliberately bind-mounted
directories.

## The pieces

| File | Role |
|---|---|
| `Dockerfile` | Runtime image: Ubuntu + common build tooling (git, Go, Node, Python), plus a non-root `agent` user built with the **calling host user's** UID/GID so files created in a mount keep correct ownership on the host. |
| `build.sh` | Builds the image, capturing `id -u`/`id -g` as build args. Run as the real user - not under `sudo` - or the UID/GID baked into the image will be root's. |
| `agent-sandbox.sh <workspace-path>` | Creates (or replaces) one persistent, detached container for the agent folder at `<workspace-path>`, with `--restart unless-stopped` so it survives host reboots. |
| `bashc` | The `run_command` entry point: `docker exec`s into the already-running sandbox for the agent folder it's invoked from (CWD), running whatever args it's given as `/bin/bash "$@"`. |

## Setup, once per agent

```bash
# 1. Build the image once per host (not once per agent)
./build.sh

# 2. Start the sandbox for one agent's workspace directory
./agent-sandbox.sh /path/to/<ws_dir>/<agent_id>

# 3. Point the agent's tools/bash at the wrapper instead of a real shell
ln -s /path/to/scripts/sandbox/bashc /path/to/<ws_dir>/<agent_id>/tools/bash

# 4. Give the agent the skill that explains what it's now running inside
ln -s /path/to/skills/bashc /path/to/<ws_dir>/<agent_id>/skills/bashc
```

From here, every `run_command` call with `command: "bash"` for that agent executes inside
its container instead of on the host.

## Mount model (same-path, all three)

`agent-sandbox.sh` bind-mounts exactly three host directories into the container, each at
the **identical absolute path** on both sides - no path translation, so a path that
resolves on the host resolves identically in-container:

- `<ws_dir>/<agent_id>/container-ws/` - the agent's own scratch space. Created if absent.
- `<ws_dir>/../projects/` - canonical repo clones, if that directory exists next to the
  workspace. Mounted read-mostly in practice (nothing enforces this at the mount level;
  the [`bashc` skill](../../skills/bashc/SKILL.md) states it as etiquette, not a permission
  bit, since the coordinator needs normal write access to the same clones from the host).
- `<ws_dir>/../worktrees/` - git worktrees, if that directory exists next to the workspace.

Same-path mounting is what makes git work correctly from inside a worktree: a worktree's
`.git` file is a pointer back to the canonical repo's `.git/worktrees/<name>` by absolute
path. If the container saw that worktree at a different path than the host does, every git
command inside it would fail to resolve its own repository.

One container per agent workspace, not one container for the whole host - `agent-sandbox.sh`
derives the container's name from the workspace path itself (slashes replaced with
underscores), so multiple agents each get an isolated, independently restartable sandbox.

## Known gap - not solved here

A command `run_command` defers to a background job (wackyproc or any other out-of-process
dispatch mechanism) does not go through `bashc` at all if the dispatcher re-execs the
command directly rather than re-entering through the agent's `tools/bash` - it runs on the
bare host, outside the sandbox entirely. There is no enforcement point in this design that
catches that case; see the [`bashc` skill](../../skills/bashc/SKILL.md) and
[`docs/agents.md`](../../docs/agents.md) for how this is documented to agents and operators,
not fixed.
