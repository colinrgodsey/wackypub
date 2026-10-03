# WackyPub Recipes

Single-file setup guides that combine the bundled skills into concrete, purpose-built
workspace configurations. Each recipe is a runnable walkthrough: create the workspace,
wire capabilities, verify at checkpoints, and end with a working agent.

## How to use a recipe

1. **Install wackypub** (`go install github.com/colinrgodsey/wackypub@latest` or use the
   container image). All recipes assume `wackypub` is on your PATH.
2. **Pick a recipe** from the index below. Note its **confinement** declaration: recipes
   that hand agents bash or programming tools must run in a container; non-bash,
   read-only recipes can run unconfined on the host.
3. **Follow the steps in order.** Each step is copy-paste bash. Checkpoints call
   `wackypub workspace <agent_id>` which reports what is present and what is missing —
   if a checkpoint fails, fix the missing piece before moving on. (The inspect output
   covers tools/skills/runtime/env; file-access grants and a working turn are verified
   by each recipe's smoke test, not by the inspect.)

In the container image, recipes ship at `/opt/wackypub/template_ws/recipes/` and the
bundled skills live at `/opt/wackypub/template_ws/skillsets/`. The variable paths in
the steps (`$REPO`, `$FILESRW_REPO`, `$WACKYPROC_REPO`) are placeholders for host
checkouts - inside the container replace them with the template paths (`skillsets/`
is where the bundled skills already are, and `toolsets/` holds the tool symlinks).
4. **Talk to your agent** with `wackypub agent <id> prompt`.

## Recipe index

| Recipe | Kind | Confinement | Skills it installs |
|---|---|---|---|
| [single-agent-chat](single-agent-chat.md) | One agent, minimal toolset | safe-unconfined | wackypub-ws |
| [coding-agent](coding-agent.md) | Full-service coding agent with bash + file + process access | container-recommended | wackypub-ws, files-rw, wackyproc, scratchpad-efficiency |
| [research-reader](research-reader.md) | Read-only research agent over a local document corpus | safe-unconfined | wackypub-ws, files-rw, scratchpad-efficiency |
| [discord-community-agent](discord-community-agent.md) | Long-lived Discord community agent | container-required | wackypub-ws, wackypub-a2a, wackyproc, scratchpad-efficiency |

## Recipe format

Every recipe starts with YAML frontmatter that a setup flow (or a director agent
operating inside the container) can read mechanically:

```yaml
---
name: <recipe-name>
title: <human title>
confinement: container-required | container-recommended | safe-unconfined
skills: [list of skill names the recipe wires]
tools: [list of tool binaries the recipe symlinks]
---
```

- **confinement** is the machine-readable container posture: `container-required`
  (bash/programming tools — must run in a container), `container-recommended`
  (give it a container when one is available), `safe-unconfined` (no bash, no
  dangerous tools — may run on the host).
- **skills** name the bundled skill folders the recipe expects to exist in
  `<agent>/skills/`. A docs-build check in the repo verifies every referenced name
  resolves to a bundled skill.
- **tools** name the executables the recipe symlinks into `<agent>/tools/`.

## What a recipe does NOT do

Recipes are setup guides, not sandboxes: they reference the bundled skills and teach
proven workspace shapes, but every decision (runtime endpoint, API keys, ACL grants,
agent identity) stays yours in the step where it belongs. If you want a turnkey,
self-bootstrapping experience instead, run the container and let the Director agent
scaffold a workspace for you.
