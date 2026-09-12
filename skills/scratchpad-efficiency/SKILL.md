---
name: scratchpad-efficiency
description: Advanced scratchpad patterns, zero-token inter-agent data hand-offs, macro templating, line-level search/pagination, and diffing two entries to verify an edit
always_load: true
---
# Scratchpad Efficiency & Swarm Communication Patterns

The persistent scratchpad is WackyPub's out-of-band memory buffer and inter-agent data pipe. Storing large payloads in scratchpads protects context windows, eliminates token waste, and enables high-throughput data flows between agents and tooling. *Each agent has their own scratchpad with its own IDs*. If you want to share one of your own scratchpad entries with another agent, use the `Zero-Token Inter-Agent Data Hand-Off` protocol below.

---

## Authorization Prerequisite & Gotcha (`WACKYPUB_ALLOWED_AGENTS`)

> [!IMPORTANT]
> **Cross-Agent & Self-Targeting Authorization Requirement:**
> Every cross-agent CLI/SDK call (e.g. `wackypub agent <target_id> scratchpad create/read/list/search` or `wackypub agent <target_id> prompt`) validates the target agent against `<ws_dir>/<calling_agent>/WACKYPUB_ALLOWED_AGENTS`.
> 
> **The Gotcha:** Self-targeting is denied by default! If an agent invokes `wackypub` CLI commands via `run_command` targeting **itself** (e.g., `wackypub agent agentB scratchpad create`), `agentB` **must be explicitly listed in its own `WACKYPUB_ALLOWED_AGENTS` file**, otherwise the call will fail with an authorization error.
>
> You don't need to go read that file directly to check - run `wackypub workspace` (no arguments) from your own directory and it tells you directly who you can talk to.

---

## Core Capabilities Quick Reference

> [!TIP]
> **Automatic `run_command` Output Capture:**
> Any stdout or stderr output produced by a tool executed via `run_command` that exceeds some threshold is **automatically captured into your scratchpad as a new entry**.
> Instead of dumping thousands of tokens into your context window, `run_command` returns placeholder tags containing the entry ID and exact payload size in bytes:
> `<STDOUT><SCRATCHPAD_DATA id="v8n2" size="15420" /></STDOUT>`

| Action | In-Agent ADK Tool | CLI Command (`run_command` tool) | Lock Behavior |
|---|---|---|---|
| **Create** | `create_scratchpad(text)` | `run_command(command="wackypub", args=["agent", "<id>", "scratchpad", "create", "[msg]"])` | Session Lock |
| **Read** | `get_scratchpad(id, skip_lines, num_lines)` | `run_command(command="wackypub", args=["agent", "<id>", "scratchpad", "read", "<id>"])` | No Lock (Atomic Read) |
| **List** | `list_scratchpads()` | `run_command(command="wackypub", args=["agent", "<id>", "scratchpad", "list"])` | No Lock (Atomic Read) |
| **Search** | `search_scratchpad(id, query, case_sensitive, regex, max_results)` | `run_command(command="wackypub", args=["agent", "<id>", "scratchpad", "search", "<id>", "<query>"])` | No Lock (Atomic Read) |

---

## 1. Zero-Token Inter-Agent Data Hand-Off

### The Problem
When **Agent A** produces a large dataset, file dump, or log payload (e.g., 100KB) that **Agent B** needs to process, passing that payload via standard prompt turns (`agent prompt`) forces 100KB of text through LLM output generation and prompt ingestion tokens.

### The Solution: Direct Cross-Deposit
Agent A uses `run_command` to execute `wackypub` and write the payload *directly* into Agent B's scratchpad out-of-band:

```json
{
  "command": "wackypub",
  "args": ["agent", "agentB", "scratchpad", "create", "<SCRATCHPAD_DATA id=\"a_payload\" />"]
}
```
*Output:* `Created scratchpad entry "k3p1" (102400 bytes) for agent "agentB".`

*(Prerequisite: Agent A must be allowed to talk to agentB - check with `wackypub workspace` from your own directory if you're not sure.)*

Agent A then prompts Agent B with a minimal 1-line turn:
```json
{
  "command": "wackypub",
  "args": ["agent", "agentB", "prompt", "I deposited the audit log directly into your scratchpad entry 'k3p1'. Please search for error codes using search_scratchpad."]
}
```

**Token Savings:** 100% of the payload text is kept out of turn generation history. Zero tokens spent streaming the 100KB payload!

---

## 2. Scratchpad Mirroring & Cross-Agent Pull (Auto-Capture Pattern)

When **Agent B** wants to copy/mirror a large scratchpad entry (e.g., `"k3p1"`) held by **Agent A** into Agent B's own local scratchpad, Agent B executes `run_command` to read Agent A's entry:

```json
{
  "command": "wackypub",
  "args": ["agent", "agentA", "scratchpad", "read", "k3p1"]
}
```

### How Auto-Capture Mirrors the Payload
- Since `wackypub agent agentA scratchpad read k3p1` prints the payload to stdout, if the content exceeds some threshold, WackyPub **automatically captures stdout into a fresh local scratchpad entry for Agent B** (`CreatedBy: "run_command"`) and returns:
  ```xml
  <STDOUT><SCRATCHPAD_DATA id="m9x2" size="102400" /></STDOUT>
  ```
- **Result**: Agent B instantly gets its own local scratchpad entry ID (`"m9x2"`) containing the mirrored content.
- **Advantages over shell pipes**:
  - Requires no `sh` binary (which agents typically do not have in `tools/`).
  - Avoids self-targeting session lock conflicts.
  - Zero tokens spent streaming payload bytes into conversation turns!

*(Prerequisite: `agentB` must be allowed to communicate with`agentA`).*

---

## 3. ID-Only Asynchronous Delegation

### Pattern
When a **Coordinator Agent** delegates a task to a **Worker Agent**, instruct the worker to return only a scratchpad entry ID rather than full text responses:

1. **Coordinator prompt to Worker**:
   > *"Run the security scan on /app. Do NOT return the full log in text. Deposit the output into a scratchpad entry and reply with ONLY: `Done: <scratchpad_id>`."*
2. **Worker response**:
   > *"Done: `r7q4`"*
3. **Coordinator action**:
   The Coordinator can either inspect specific lines (`get_scratchpad`), search for errors (`search_scratchpad`), or pass the entry handle to another tool/agent without ever reading the full log into context.

---

## 4. Server-Side Macro Templating (`<SCRATCHPAD_DATA ... />`)

Positional arguments (`args`) and standard input (`stdin`) in `run_command` support inline `<SCRATCHPAD_DATA>` macros. Macro expansion happens **server-side inside `run_command`** immediately before the command binary is executed.

### Syntax
```xml
<SCRATCHPAD_DATA id="ENTRY_ID" skip_lines="N" num_lines="M" />
```
*(Attributes `skip_lines` and `num_lines` are optional).*

### Multi-Scratchpad Assembly Example
Pass pre-staged prompts, templates, or raw inputs directly to a command tool without reading them into LLM context:

```json
{
  "command": "data_processor",
  "args": [
    "--header", "<SCRATCHPAD_DATA id=\"hdr1\" />",
    "--input", "<SCRATCHPAD_DATA id=\"dat2\" skip_lines=\"10\" num_lines=\"50\" />"
  ],
  "stdin": "<SCRATCHPAD_DATA id=\"tmpl\" />"
}
```

- WackyPub server expands the `<SCRATCHPAD_DATA>` macro tags in `argv` and `stdin` immediately before process execution.
- Argument size safety cap: expanded CLI positional arguments exceeding **500,000 bytes** fail fast to prevent OS exec argument limits (`E2BIG`).

---

## 5. Iterative Search & Slice Navigation

When command tool output exceeds **4,000 bytes**, WackyPub automatically captures stdout/stderr into a fresh scratchpad entry and returns tag placeholders containing the entry `id` and payload `size` in bytes:
```xml
<STDOUT><SCRATCHPAD_DATA id="v8n2" size="45000" /></STDOUT>
```

### Using the `size` Attribute to Decide Your Strategy
- **Moderate Size (e.g. 4,000 – 20,000 bytes / ~5–20KB)**: You can slice exact line windows with `get_scratchpad(id, skip_lines, num_lines)`.
- **Large Size (e.g. 20,000+ bytes / 50KB+)**: Avoid reading the whole payload! Use `search_scratchpad(id, query)` first to pinpoint lines, or pass `<SCRATCHPAD_DATA id="v8n2" />` directly as `stdin`/`args` to another `run_command` tool out-of-band.

### Optimal 2-Step Inspection Workflow

#### Step 1: Search for Line References
Call `search_scratchpad` to find exact lines without loading surrounding text:
```json
{
  "id": "v8n2",
  "query": "FATAL",
  "case_sensitive": false,
  "max_results": 10
}
```
*Result:*
```json
{
  "total_matches": 1,
  "matches": [
    { "line": 412, "skip_lines": 411, "text": "2026-08-08 12:00:00 FATAL connection refused to db:5432" }
  ]
}
```

#### Step 2: Slice Exact Line Window
Use the precomputed `skip_lines` value (`411`) directly in `get_scratchpad` to pull only the surrounding 20 lines:
```json
{
  "id": "v8n2",
  "skip_lines": 405,
  "num_lines": 20
}
```

**Token Savings:** Reduces a 10,000-line log read down to 20 lines of context window footprint.

### Searching Inside a File's Content

`files-rw` has no built-in search/grep command - don't look for one. Instead, use this exact same auto-capture pattern: read the file via `files-rw`, let the output land in a scratchpad entry, then `search_scratchpad` that entry, same as above.

```json
{
  "command": "files-rw",
  "args": ["read", "app.log"]
}
```
If the file's content exceeds 4,000 bytes, the output auto-captures into a fresh scratchpad entry (`<STDOUT><SCRATCHPAD_DATA id="..." /></STDOUT>`) exactly like any other large command output - then follow the same 2-step search + slice workflow above.

**Known limit**: this only works up to `files-rw read`'s own 200KB cap - a file larger than that can't be pulled into a scratchpad this way at all, `read` refuses outright with a pagination-suggestion error instead of producing output to capture. For a file that large, fall back to `files-rw read --start N --end M` in smaller ranges directly (no scratchpad search across the whole file in one shot).

---

## 6. Zero-Token Out-of-Band Scratchpad Combination

`CreateScratchpad` (`create_scratchpad` ADK tool, CLI `scratchpad create`, and Go SDK `sdk.CreateScratchpad`) automatically resolves `<SCRATCHPAD_DATA id="X" ... />` macros server-side before storing the new entry.

### Pattern: Combining Data Out-of-Band
An agent can concatenate, prefix, or stitch together multiple scratchpad entries (or slices of scratchpads) into a single new scratchpad entry in one tool call, without ever reading or outputting their text content:

```json
{
  "text": "--- Combined Report ---\nHeader:\n<SCRATCHPAD_DATA id=\"hdr1\" />\nBody:\n<SCRATCHPAD_DATA id=\"dat2\" skip_lines=\"10\" num_lines=\"50\" />\nFooter:\n<SCRATCHPAD_DATA id=\"ftr3\" />"
}
```

- Returns a single fresh 4-character ID (e.g. `"c9m3"`) holding the fully merged document.
- **Token Efficiency**: Combines arbitrary-sized datasets with **zero LLM generation tokens** spent on the payload contents!

---

## 7. Additional Advanced Swarm & Pipeline Patterns

### A. Scatter-Gather / MapReduce Swarm Pipeline
*(Note: Macro expansion occurs server-side inside `run_command`).*

1. **Scatter Phase**: Coordinator agent has a 500KB codebase payload in scratchpad entry `s1`. Instead of reading 500KB into context or prompting workers with raw text, Coordinator calls `run_command` to slice and deposit chunks into workers' scratchpads:

   **Worker 1 Scatter Call (`run_command`):**
   ```json
   {
     "command": "wackypub",
     "args": [
       "agent", "worker1", "scratchpad", "create",
       "<SCRATCHPAD_DATA id=\"s1\" skip_lines=\"0\" num_lines=\"1000\" />"
     ]
   }
   ```
   *Output:* `Created scratchpad entry "w1e1" for agent "worker1".`

   **Worker 2 Scatter Call (`run_command`):**
   ```json
   {
     "command": "wackypub",
     "args": [
       "agent", "worker2", "scratchpad", "create",
       "<SCRATCHPAD_DATA id=\"s1\" skip_lines=\"1000\" num_lines=\"1000\" />"
     ]
   }
   ```
   *Output:* `Created scratchpad entry "w2e1" for agent "worker2".`

2. **Map Phase**: Coordinator prompts Worker 1 and Worker 2 to analyze their assigned scratchpads (`w1e1`, `w2e1`) and deposit their partial summaries back into Coordinator's scratchpad.

3. **Gather & Reduce Phase**: Coordinator merges the partial summaries out-of-band using `create_scratchpad`:
   ```json
   {
     "text": "--- Master Summary ---\n<SCRATCHPAD_DATA id=\"sum1\" />\n<SCRATCHPAD_DATA id=\"sum2\" />"
   }
   ```

### B. Out-of-Band Unix Tool Chaining (Zero-Token Pipelines)
Chain tools sequentially without intermediate outputs ever entering LLM context:
- Tool 1 runs -> stdout > 4KB auto-captures into `sp1`.
- Tool 2 runs via `run_command` with `stdin: "<SCRATCHPAD_DATA id=\"sp1\" />"` -> stdout > 4KB auto-captures into `sp2`.
- Tool 3 runs via `run_command` with `stdin: "<SCRATCHPAD_DATA id=\"sp2\" />"` -> returns final summary (<4KB) to conversation turn.

### C. Scratchpad Execution Ledger
Maintain an ongoing, append-only audit trail across a 20-turn complex task:
- At turn $N$, the agent appends its current status to the ledger out-of-band:
  `create_scratchpad(text="<SCRATCHPAD_DATA id=\"ledger_vN-1\" />\n[Turn N] Result: OK")` -> `ledger_vN`.
- Preserves complete execution history without bloating `session.jsonl` or forcing premature session compaction.

---

## 8. Downstream Display Expansion Sentinel (`<SCRATCHPAD_EXPAND id="X" />`)

*(Optional pattern supported by downstream presentation layers such as `wackydiscord` - not expanded by `wackypub` core).*

### The Problem
In long-form generative agents (such as narrator or storytelling personas), an agent might generate large prose text, store it in a persistent scratchpad via `create_scratchpad(text=...)`, and then need human-facing chat frontends (like Discord) to display that prose.
Re-emitting the full prose a second time in the final assistant response text wastes generation tokens and clutters `session.jsonl`.

### The Solution: Output Display Sentinel
An agent generates its large output directly into a scratchpad via `create_scratchpad`, and outputs the sentinel `<SCRATCHPAD_EXPAND id="X" />` in its final message text:

1. **Agent Tool Call:**
   `create_scratchpad(text="The rain poured heavily over the neon-lit rooftops of Neo-Kyoto...")` -> returns ID `"n1a7"`
2. **Agent Final Response:**
   `<SCRATCHPAD_EXPAND id="n1a7" />`

### Downstream Behavior
- **`wackypub` Core (CLI / SDK / session.jsonl):** Leaves `<SCRATCHPAD_EXPAND id="n1a7" />` untouched as literal plain text. No core rewriting occurs.
- **Downstream Consumers (e.g. `wackydiscord`):** Before delivering the assistant message to human users, `wackydiscord` resolves the sentinel against the agent's scratchpad (`get_scratchpad("n1a7")`) and substitutes the full prose inline.
- **Fallback:** If the scratchpad entry cannot be found or was evicted, downstream consumers gracefully display the raw `<SCRATCHPAD_EXPAND id="X" />` tag.


---

## 9. Diff Two Entries To Verify An Edit (Zero-Token Patch Preview)

### The Pattern

Snapshot the before-state into an entry, make the edit, snapshot the after-state, then hand the two entry IDs to `diff_scratchpad`. Neither version comes back into context. Only the patch does, and only once, with no need to re-read or regenerate either snapshot.

### The Tool

Inside a turn, call the `diff_scratchpad` tool, whose two arguments are the entry IDs:

```text
diff_scratchpad(before_id: "ab12", after_id: "cd34")
```

Both IDs are required, and there is no mode where an absent side is treated as empty: comparing against nothing would turn a mistyped ID into a patch that adds or deletes the whole entry.

It answers the way the other scratchpad tools do, and the contract is deliberately blunt:

- entries differ: `diff` carries the unified patch, `identical` is false, and the `---` and `+++` headers name the two entry IDs so a patch stays traceable
- entries identical: `diff` is empty and `identical` is true, so "did anything change" is a field lookup rather than a test against an empty string that might just be an empty entry
- an entry ID that does not exist, or is malformed: an error naming the ID, never an empty diff, so a typo cannot masquerade as "nothing changed"
- binary entries: refused, the way reading them is refused

External callers (a shell, a script, another agent's CLI) use the equivalent command, which behaves identically: `wackypub agent <agent_id> scratchpad diff <before_id> <after_id>`. Both orderings of that command work, matching the other scratchpad verbs. The SDK counterparts are `agent.DiffScratchpadEntries(wsDir, agentID, beforeID, afterID)` and `(s *AgentSDK) DiffScratchpadEntries(agentID, beforeID, afterID)`, the latter being the authorized path.

### Getting The Two Entries

- **An existing file**: `files-rw read <path>`. Above the capture threshold the read result *is* the snapshot entry, at no token cost. Reads are gated by the `r:` rules in `FILES_RW_ACCESS`.
- **Command output**: run the command. Large stdout becomes an entry.
- **A slice of an entry**: `create_scratchpad` with `skip_lines` and `num_lines` macro attributes.
- **Another agent's version**: it deposits straight into your scratchpad (section 1), then you diff against yours. Neither version touches anyone's context.
- **Explicitly**: `wackypub agent <id> scratchpad create "text"`, or redirect a file into it with no message argument, in which case stdin is read. Passing a lone dash stores a literal dash instead of the file.

### Worked Example: Preview A Multi-File Refactor

1. Before touching anything, snapshot every file the refactor rewrites and keep the entry IDs.
2. Run the refactor.
3. Snapshot the same files again.
4. Diff one pair per file. A pair whose result comes back `identical` is an unchanged file, verified rather than assumed.
5. Search the largest patch for a symbol that should have disappeared entirely. Every hit being a removal line is the check that catches a rename which missed a call site.
6. Everything here is a preview. Nothing is written, and git restore still undoes the edit.

### Large Diffs

Through the CLI nothing to remember: a patch past the capture threshold arrives as the scratchpad entry reference tag instead of as text, which you then narrow with `search_scratchpad` or paginate with `skip_lines`. The tool has no such wrapper, it returns the patch straight into context, so when a diff is likely to be huge (two long entries that differ almost everywhere) diff slices of them, or run the CLI form through a command and let the capture do its job. To keep a patch on purpose, pipe the CLI output into `wackypub agent <id> scratchpad create` with the message argument omitted.

### Limits

- Text entries only. A binary entry is refused rather than diffed as garbage.
- Both sides obey the single-read size cap, so two entries too large to read are also too large to diff whole. Diff slices of them instead, or diff the files with git.
- It previews; it does not apply. Applying means writing a real file and running `patch` or `git apply`.
- For files git already tracks, plain `git diff` is the better tool, for staging awareness and rename detection. This section earns its keep on untracked or generated content, and on reviewing another agent's candidate version without loading it into context.

