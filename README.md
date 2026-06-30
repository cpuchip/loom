# loom

**A harness around the harnesses.** loom drives multiple coding-agent CLIs — Claude
Code, Google Antigravity's `agy` (Gemini), and others — as long-lived workers behind
one Go interface, so a backend (or a human) can hand work to whichever agent fits,
keep a session alive across turns, or fan one prompt across several at once.

A weaving *harness* is literally a loom component, and a loom holds many harnesses at
once. That's the idea: each agent CLI is its own harness; loom weaves them.

MIT licensed.

## Why

Modern coding agents (Claude Code, agy, Codex, …) are themselves harnesses — they have
their own tools and loops. Driving them from a backend usually means one-shot
re-spawns that reload context every time. loom gives them a common `Session` interface
with **context that persists across turns**, plus a **panel** mode to run several
agents on the same task and compare.

The backends are deliberately heterogeneous, because the real CLIs are:

| backend | transport | multi-turn | notes |
|---|---|---|---|
| **claude** | persistent `--input-format stream-json` over stdin/stdout (NDJSON) | one process, many turns, holds context | the good path — **verified** (see below) |
| **agy** | one-shot `agy -p` per turn | `--conversation <id>` resume (fresh process/turn) | EXPERIMENTAL — two headless bugs worked around: stdin-EOF hang (feed empty stdin) + stdout-drop (recover the answer from the transcript file) |

## Verified (2026-06-29, Claude Code v2.1.196)

The claude backend's premise is proven on the real path, not assumed:

- **Context across turns in one process** — turn 1 "remember 42" → turn 2 recalled
  "42", same `session_id`. (The `LOOM_SMOKE=1` test reproduces this.)
- **Cost amortization is real, measured** — a cold one-shot pays ~27K cache-creation
  tokens *every* spawn ($0.055 for a trivial turn); in a live session, turn 2
  cache-READ ~24K (incl. turn 1's context) and created only ~7K. So `loom chat`'s
  persistent session is materially cheaper than re-spawning per turn.
- **Gotcha baked in:** `--print` + `--output-format stream-json` *requires*
  `--verbose`; the ~27K cold context loads the working dir's CLAUDE.md/MCP/memory, so
  run loom in the repo you want that context from.

## Use

```sh
go build -o loom.exe ./cmd/loom

loom run   --agent claude "summarize the files in this dir"      # one-shot
loom chat  --agent claude --dir /path/to/repo                    # multi-turn: one msg per stdin line
loom panel --agents claude,agy "is this function correct?"       # council: fan across agents, compare
loom agents                                                      # list backends
```

`--model` overrides the model (e.g. `--model haiku`); `--dir` sets the agent's cwd.

### Test

```sh
go test ./...                 # pure unit tests (parsing, registry) — no money
LOOM_SMOKE=1 go test ./...    # + the live claude multi-turn oracle (spends a little)
```

## Status / roadmap (v0.1)

- ✅ Core `Backend`/`Session` interface · claude backend (persistent stream-json) ·
  agy backend (experimental) · `panel` (concurrent council) · CLI · smoke oracle.
- **Next:** session resume (`--resume <session_id>` for claude, `--conversation` for agy)
  surfaced in the CLI; a condenser for very long sessions (pattern from OpenHands'
  `LLMSummarizingCondenser`); structured event streaming (not just the final text);
  routing/role assignment across the panel; a local backend (llama-chip) so the panel
  can include a fast local model. Verify the agy transcript-recovery on the real path
  before depending on it.

## Related

Built alongside `pg-ai-stewards` (a substrate that drives Claude Code for code review —
loom is the natural home for that long-lived-session logic) and `garrison` (a local-first
coding agent). Reference: OpenHands (`All-Hands-AI/OpenHands`) for heavier agent-loop
patterns.
