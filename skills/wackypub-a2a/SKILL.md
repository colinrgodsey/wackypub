---
name: wackypub-a2a
description: Guide for using the wackypub CLI for agent-to-agent (A2A) communications, command discovery, and inter-agent calling.
always_load: true
---
# WackyPub AI A2A Communication & CLI Guide

`wackypub` is a Go CLI and SDK for managing folder-based AI agents built on Google Agent Development Kit (ADK) v2. Each agent's runtime configuration, system prompt, long-term memory, turn history, persistent scratchpad, tools, and skills live in plain files under a workspace directory (`<ws_dir>/<agent_id>/`).

## Command Discovery via `--help`

The CLI is a thin wrapper over `AgentSDK`. Every subcommand is self-documenting — use `--help` to inspect exact command signatures, argument definitions, flag options, and preconditions without making assumptions about syntax.

### 1. Workspace Self-Inspection
Before running operations on an agent, use `workspace` to inspect the workspace layout or diagnose an individual agent's state:
```bash
# List all discovered agent directories and top-level diagnostic summary
wackypub workspace

# Report detailed on-disk state, missing files, and issues for <agent_id>
wackypub workspace <agent_id>
```

### 2. Auto-Discovering Commands & Options
Use `--help` at any level of the command hierarchy to explore usage:
```bash
# Global flags (--ws <dir>, --config <file>, -m/--model <model>, --api-key <key>)
wackypub --help

# List all agent management subcommands
wackypub agent --help

# Detailed usage for specific agent operations
wackypub agent prompt --help
wackypub agent generate --help
wackypub agent add --help
wackypub agent read-session --help
wackypub agent read-memory --help
wackypub agent render-prompt --help
wackypub agent compact --help
wackypub agent strip-signatures --help
```

### 3. Syntax & Execution Conventions
- **Positional Agent Dispatch**: Both `wackypub agent <cmd> <agent_id> [args...]` and `wackypub agent <agent_id> <cmd> [args...]` (agent ID first) are supported.
- **Flag Ordering Caveat**: Flags do *not* work with the agent-id-first ordering above — not just `--help`, *any* subcommand-specific flag (`--message`, `--skip-lines`, `--regex`, etc.). `wackypub agent prompt --help` and `wackypub agent <agent_id> add --message "..."` work; `wackypub agent <agent_id> prompt --help` and `wackypub agent <agent_id> add --message "..."` (agent ID before the subcommand) fail with `unknown flag`, because the outer `agent` command's own flag parsing runs before the agent-id-first form ever dispatches to the subcommand's `RunE`. If a command takes a flag, put the subcommand name directly after `agent`, agent ID after that: `wackypub agent <cmd> <agent_id> [flags]`. Positional-only calls (no flags) work fine in either order.
- **Workspace Discovery (`--ws`)**: Specify `--ws <path>` to target a workspace directory containing `WACKYPUB_ROOT`. If omitted, `wackypub` automatically walks up from CWD to find the nearest workspace root.
- **Cross-Agent Access is Gated**: An agent can only target other agents listed in its own `WACKYPUB_ALLOWED_AGENTS` file (default is deny-all if that file doesn't exist). Attempting to reach an unauthorized agent — including yourself, unless explicitly listed — fails with a clear authorization error. To check who you're allowed to talk to, run `wackypub workspace` (no arguments) from your own directory rather than reading the file directly.

### 4. Inter-Agent Communication (A2A)
- Simple request->response flows between agents should use `agent prompt`:
  ```bash
  wackypub agent prompt <target_agent_id> "Message content"
  ```
- `AGENT2AGENT` metadata (caller ID, call chain, trace ID, and sender git commit revision) is automatically propagated across environment variables down the execution chain.

### 5. Response Routing Semantics (who sees your output)

How your reply reaches the other agent depends entirely on WHICH command they used to reach you - this is not obvious, and getting it wrong wastes turns:

- **They used `agent prompt <you>`** (synchronous): their CLI is LIVE and BLOCKED, waiting for your turn to finish. Your final response text of this turn IS the reply they receive - it is returned to them automatically as the tool result. Do NOT try to `agent prompt` them back: the cycle detector will reject it (their call is in your chain), and the rejection costs a turn. Just answer normally and end your turn. If the topic is done, your turn ending IS the message delivered.
- **They used `agent add <you>`** (asynchronous, fire-and-forget): nobody is waiting. Your turn output goes nowhere they can see. If a reply is needed, YOU must initiate it - `agent prompt <sender>` from your own next turn (or now; their turn is not holding a chain open, so no cycle).
- **How to tell which happened**: you cannot reliably tell from the message text alone. The AGENT2AGENT env metadata (caller_id, call_chain, trace_id) tells you WHO sent it, but not whether they are blocking on your response. Heuristic until tooling improves: if the message is a question or task handed to you mid-conversation by a known collaborator, assume synchronous and answer in-turn; if it is a notification, FYI, or batch instruction, assume async.
- **Cycle-blocked replies are recoverable**: if your `agent prompt` reply is rejected with "already in call chain", the sender is still waiting - your in-progress turn's final response will be delivered to them when you finish. Do not keep retrying the prompt; finish your turn with the answer as your final text.

### 6. Async Supervised Dispatch (`--async` via wackyproc)

When an agent needs to start and sustain a real turn in a target agent without blocking the caller, use wackyproc-supervised async dispatch:

```bash
wackyproc run wackypub agent prompt --async <target_agent_id> "<message>\n\nYour response is not needed. Respond with only NO_RESPONSE."
```

#### Key Semantics:
- **Supervision Gate**: `--async` requires `WACKYPROC_SUPERVISED` in the environment. Bare dispatches without wackyproc supervision are refused immediately (`--async requires WACKYPROC_SUPERVISED in environment: the dispatch must be wackyproc-supervised (dangerous flag; output must be captured)`).
- **Anti-Bleed Protection**: `wackypub`'s tool environment builder (`childEnv`) strips `WACKYPROC_SUPERVISED` from all tool children. Even if the calling agent runtime was spawned via wackyproc, its tool children cannot execute bare `--async` calls; dispatches must explicitly route through `wackyproc run` so that output capture and supervision are guaranteed.
- **Cycle-Detection Relaxation**: `--async` skips call-chain cycle detection for that single dispatch only. The child process inherits the intact call chain (with the target appended), ensuring all downstream calls made by the target remain fully cycle-protected.
- **NO_RESPONSE Convention**: Async dispatches MUST append the hard suffix:
  `"Your response is not needed. Respond with only NO_RESPONSE."`
  This convention removes the reply temptation that cycle detection normally polices. If a model fails to follow the instruction, its output is safely captured in wackyproc stdout rather than lost.
- **Output Retrieval (No Callback)**: Callers check wackyproc stdout on completion (`wait`/`get`); there is no callback.
- **Dispatch Receipts**: The dispatch receipt is wackyproc's own: `wackyproc run` prints the proc ID and `list` shows target/state; the output arrives via `wait`/`get`. No separate wackyproc-side delivery.
- **Deadline Rule (no timeout machinery)**: There is deliberately no deadline/watchdog. A hung supervised call holds the same locks and has the same risk profile as a hung normal call, so the remedy is the existing kill signals: `wackyproc stop <id>` (SIGTERM, then SIGKILL after the grace period) or `wackyproc kill <id>` (immediate SIGKILL of the whole process group).
- **Open Architectural Note**: The `.git.lock` shared-repo commit serialization question remains OPEN in the task card (`tasks/wackypub/a2a-async-reply-pattern`). Per-target `session.lock` serializes turns per agent, but cross-agent concurrent commits to a single shared repo remain unresolved.

**Hook recommendation**: if your workspace does a2a, install a receiver-side announce hook (see `examples/hooks/on-user-message/10-announce-check` in the wackypub repo) so inbound agent turns are mechanically annotated with `[Message from agent: <id>]` - generally wanted; a minority of use cases do not call for it. Verify what hooks you have via `git-kb show knowledge/gitkb-swarm-process` and the hooks inspection command (pending).

