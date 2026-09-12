# Integration test pass — 2026-09-11 (testws, real OpenRouter gemma)

Result: **7/7 checks passed** in `testws` with `openrouter-gemma.json` (google/gemma-4-26b-a4b-it). Full checklist recorded on the task card `tasks/wackypub/integration-test-pass-in-testws-with-real-provider`.

| Check | Result |
|---|---|
| `agent add` | PASS — user turn appended |
| `agent generate` | PASS — real model output, exit 0 |
| `agent context` | PASS — 41,562 est. tokens / 200k window, 174 turns |
| D93 usage sidecar (`.last_usage.json`) | PASS — prompt 1321 / candidates 88 / total 1409 |
| Hooks fire (on-user-message) | PASS — `[Message from agent: dranbo]` annotation applied |
| Scratchpad expand/capture | PASS — create/list, macro expand, output auto-capture |
| Compaction | PASS — exit 0, MEMORY.md updated |

Notes: keys stayed out of tracked files (env-var runtime symlink, testws gitignored). Bob's heavy 174-turn session was impractically slow for a live generate (>18 min, killed); a fresh `itp` agent with the same runtime completed in seconds — no integration-path bug.
