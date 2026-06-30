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
| **local** | stateless `POST /v1/chat/completions` (OpenAI-compat: llama-chip `:8090`, LM Studio, vLLM) | loom keeps the message history | **the simplest backend** — no process/stdio; **verified** (single + multi-turn) against the live rig; free. Makes `panel` a cloud+local council. |
| **agy** | one-shot `agy -p` per turn | `--conversation <id>` resume (fresh process/turn) | works (single-turn **verified** in a live `panel`, 2026-06-29) — two headless bugs worked around: stdin-EOF hang (feed empty stdin) + stdout-drop (recover the answer from the transcript file) |

**Why agy is the awkward one:** agy has **no working stdio/stream-json mode** — it's an open, Google-acknowledged gap (antigravity-cli issues [#76](https://github.com/google-antigravity/antigravity-cli/issues/76) stdout-drop, [#119](https://github.com/google-antigravity/antigravity-cli/issues/119) stream-json parity, [#31](https://github.com/google-antigravity/antigravity-cli/issues/31) `--acp`); `--output-format json` is currently *rejected*. The two real workarounds are **transcript-scrape** (what loom does — the right path on Windows) or a **pseudo-TTY wrap** (`script -qec '…' /dev/null`, Unix-only). When agy ships stream-json, the agy backend swaps to the clean path and drops the scrape.

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
  **local backend (OpenAI-HTTP → `:8090`; verified single + multi-turn against the live rig; cloud+local `panel` proven)** ·
  agy backend (experimental) · `panel` (concurrent council) · CLI · smoke oracle.
- **★ Next (recommended, 2026-06-30):** **structured event streaming** (surface tool_call/tool_result, not just the final text — the Hinge-foundational one; turns loom from a black box into an observable harness) → **dogfood loom on a real code review** to surface the next real gap.
- **Backlog:** session resume (`--resume <session_id>` for claude, `--conversation` for agy)
  surfaced in the CLI; a condenser for very long sessions (pattern from OpenHands'
  `LLMSummarizingCondenser`); structured event streaming (not just the final text);
  routing/role assignment across the panel; a local backend (llama-chip) so the panel
  can include a fast local model.

## ACP — researched 2026-06-29, decision: skip for now

ACP (the [Agent Client Protocol](https://agentclientprotocol.com), JSON-RPC-2.0 over
stdio, now folding toward the Linux Foundation A2A standard) is an *optional future
backend*, not a near-term need:

- **Claude Code has no native ACP** (real-path-confirmed on v2.1.196); it'd require Zed's
  Node adapter (`@agentclientprotocol/claude-agent-acp`, renamed from `@zed-industries/…`
  — verify the exact name before installing). Our **direct stream-json claude backend is
  dependency-free, single-process, and faster** — no reason to route it through ACP.
- **Codex has no native ACP** either (community adapters only).
- **Gemini CLI (`gemini`, the standalone — NOT `agy`) DOES have native `--acp`.** That's
  the one real win: a small ACP-client backend driving `gemini --acp` would give a clean
  Gemini (streaming + resume + tool-approval) and replace the agy transcript-scrape — *if*
  we want Gemini badly enough to install `gemini`.

**Decision:** keep the direct CLI backends; add an *optional* ACP-client backend only when
we want `gemini --acp` as a first-class Gemini, or if Codex ships native ACP. ACP's
permission/approval surface is built for interactive IDEs, not headless orchestration, so
it buys little for our use. (ACP→A2A is worth watching — same lineage as pg-ai-stewards'
A2A engine.)

## Related

Built alongside `pg-ai-stewards` (a substrate that drives Claude Code for code review —
loom is the natural home for that long-lived-session logic) and `garrison` (a local-first
coding agent). Reference: OpenHands (`All-Hands-AI/OpenHands`) for heavier agent-loop
patterns.
