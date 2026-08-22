# Example `runtime.json` configs

Safe templates for the model backends this project has actually been tested
against - preconfigured with environment variable placeholders (such as
`"${OPENROUTER_API_KEY}"`, `"${GEMINI_API_KEY}"`, `"${ANTHROPIC_API_KEY}"`).
WackyPub automatically expands environment variables in `runtime.json` from the
workspace `.env` or process environment. Copy one, configure your `.env` or
environment variables, and either point `<agent_dir>/runtime.json` directly at it
or symlink it in (see `.agents/LOCAL_TESTING.md` for the symlink-per-backend
pattern used for switching backends without duplicating an agent's config).

- **`openrouter-auto.json`** - OpenRouter's `"auto"` model routing, which
  picks a backend per-request. `supportsReasoningDetails` is deliberately
  `false` here: OpenRouter's structured `reasoning_details` blocks can be
  tied to the specific backend that generated them, and replaying one to a
  *different* backend than "auto" happens to route to next time can fail
  outright (see `.agents/DECISIONS.md` D6).
- **`openrouter-haiku.json`** - OpenRouter, pinned to a specific model
  (`anthropic/claude-haiku-4.5`) rather than `"auto"` - safe to enable
  `supportsReasoningDetails` since every request goes to the same backend.
- **`llamacpp.json`** - A local `llama.cpp` server (default port `8080`).
  Update `endpoint`/`model` to match your actual server. No reasoning
  fields set - add `reasoningField`/`supportsReasoningDetails` if your
  server's chat template actually emits structured reasoning.
- **`gemini-flash.json`** - Native Gemini (`"provider": "gemini"`), no
  `endpoint` needed. Uses `geminiThinkingLevel` (mutually exclusive with
  `geminiThinkingBudget` - Gemini's API rejects a request setting both,
  see `.agents/DECISIONS.md` D21) plus `geminiIncludeThoughts` to get
  readable reasoning text back instead of just a thought signature.
- **`anthropic-sonnet.json`** - Native Anthropic (`"provider": "anthropic"`),
  no `endpoint` needed. Uses `anthropicThinkingMode: "adaptive"` (the
  effort-based reasoning API) paired with `anthropicThinkingEffort` - the
  `"enabled"` classic mode instead takes `anthropicThinkingBudgetTokens`,
  see D21.

Both OpenRouter examples set `extraBody.reasoning.effort: "high"` - an
OpenRouter-specific passthrough field, not something WackyPub interprets
itself. Swap the model string for any other OpenRouter-hosted model and
this still applies.

All five configs here have been verified against their real backend (see
`.agents/DECISIONS.md` D21 and `.agents/LOCAL_TESTING.md`). If you switch an
existing agent between providers, run `wackypub agent <id> strip-signatures`
first - reasoning signatures are provider-specific and get rejected outright
if replayed to a different provider than the one that issued them.
