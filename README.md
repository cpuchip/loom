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
loom run   --agent claude --events "count the .go files"         # + stream tool calls/thinking to stderr
loom run   --agent local  --model gemma-4-12b "..."              # a free local model on llama-chip's :8090
loom chat  --agent claude --dir /path/to/repo                    # multi-turn: one msg per stdin line
loom panel  --agents local,claude "is this function correct?"    # cloud+local council: fan + compare
loom review --agents claude,local [--dir R] [--diff HEAD] [files...]   # review a git diff or files
loom run    --agent claude --isolate --dir /path/to/repo "..."   # claude in a docker sandbox (host walled off)
loom run    --agent claude --remote cpuchip@box --dir /repo "..." # claude on another machine over ssh
loom run    --agent claude --remote cpuchip@box --isolate --dir /repo "..." # sandboxed claude ON the remote box
loom run    --agent claude --resume <session-id> "..."           # reattach to a prior session (survives process/pipe death)
loom agents                                                      # list backends
```

`loom review` loads a **git diff** (default: the working-tree diff vs HEAD; `--diff HEAD`/`main...HEAD`)
or named **files**, and fans a reviewer prompt across the agent(s) — a one-shot code review, or a
cloud+local council. loom found and fixed real bugs in its *own* code this way (history-poisoning, a data
race, and an incomplete `<think>`-stripper — the orphan `</think>` case a self-review caught).

`--events` makes loom **observable** — the agent's tool calls (`→ Glob`), tool results, and thinking
stream to stderr as they happen, while the final answer comes back on stdout. Backends emit what they
can: `claude` the full stream, `local`/`agy` a coarse one.

## Sessions — carry & resume

Two different guarantees, both verified on the real path:

- **Carry context across turns** (one live process): the claude backend is a single persistent
  `claude -p --input-format stream-json … --verbose` process; each turn writes to the *same* stdin and reads
  to that turn's `result`, so claude holds full context. That's `loom chat`. ✅ (`LOOM_SMOKE` oracle: turn 1
  "remember 42" → turn 2 "42", one process.)
- **Resume across a process restart / dropped pipe** (`--resume <id>`): the session persists to disk on
  whichever box runs claude. `loom run`/`loom chat` print the `session_id`; reopen it later — even from a
  *fresh process on another day* — with `--resume <id>`, and the context is restored. ✅ verified 2026-06-30
  by a two-process oracle (process A remembers 73 and exits → a brand-new process B `--resume` recalls 73)
  and the CLI end-to-end. This is what makes a **remote** session durable: a broken ssh pipe doesn't lose the
  session — loom just reattaches by id.

```sh
loom run --agent claude "remember the number 88, reply OK"  # prints: [session <id> — resume: loom run --resume <id> ...]
loom run --agent claude --resume <id> "what number?"         # → 88, from a brand-new process
```

Under the hood: `claude --resume <id>` (the real CLI also has `--session-id <uuid>` to *pre-assign* an id,
`--fork-session` to branch, `-c` for most-recent — natural follow-ons).

- **Interrupt a turn in flight & steer** (`Interrupt()`): stop the agent *while it's working* and redirect it,
  without losing the session. loom writes a stream-json `control_request`/`subtype:interrupt` to stdin (the
  real wire format, probe-verified 2026-06-30 — claude acks with a `control_response` success, then ends the
  turn with a `result` `subtype:error_during_execution`); the subprocess stays alive, so a following `Send`
  steers with full context. ✅ verified by a live oracle: interrupt a running turn (~0s to stop) → `Send`
  "reply ALIVE" → `ALIVE`, context intact. Race-checked (`go test -race`) on the concurrent read-vs-interrupt
  path. In the CLI, **the first Ctrl-C during a turn interrupts the agent** (not loom) — type your next line to
  steer; a second Ctrl-C at the prompt exits. Programmatically it's the optional `loom.Interruptible`
  interface (`if it, ok := sess.(loom.Interruptible); ok { it.Interrupt() }`).

The session-lifecycle triad is complete on the real path: **carry** (across turns) · **resume** (across
process death) · **interrupt+steer** (mid-turn). The only remaining nuance is *concurrent mid-turn injection
without* interrupting (queue-while-working) — undocumented upstream and not needed: interrupt-then-instruct
covers the steering case cleanly.

## Isolation — the wall (`--isolate`)

A Claude Code session runs in a **real directory with full host access** (it has `Bash`, `Read`, `Write`).
That's the *asset* — loom can hand the substrate reach into a repo or corpus. It's also the *risk* — a
full-filesystem agent commanded by a backend could touch the host. `--isolate` is the wall: it runs claude
**inside a docker container** (`docker/Dockerfile.claude` → `loom-claude`) that sees **only**:

- `/work` — the repo (`--dir`), the one host path the agent can read/write, and
- `~/.claude/.credentials.json` — the subscription auth, mounted **read-only** (claude writes its own
  session state into an ephemeral in-container `~/.claude`, gone on `--rm` — pass `--claude-home` to persist
  it and to inject skills/instructions; see *Configuring the agent* below).

```sh
docker build -t loom-claude -f docker/Dockerfile.claude .
loom run --agent claude --isolate --dir /path/to/repo "review this repo"
```

Verified: the container's `/` is a stock Linux fs + `/work` — **no host `C:\Users`, no host system**. The
agent can't reach anything you didn't hand it. (Honest scope: the container still holds the OAuth token and
has network, so it isn't *zero-trust* — a tighter version would use a scoped/short-lived token or egress
limits. But the **host filesystem is walled**.) `agy --isolate` is not yet wired (its Antigravity auth is
gnarlier). This is the presiding covenant made literal — delegation needs a lawful wall (D&C 121).

## Remote (`--remote`)

The third point on the trust axis: run the agent on **another machine** over ssh — the same
transport-wrapping pattern as `--isolate`. stream-json flows over the ssh pipe unchanged. This is how a
backend (pg-ai-stewards) commands a Claude Code session on a remote box — the substrate's reach, at distance.

```sh
loom run --agent claude --remote cpuchip@workchip --dir /home/cpuchip/repo "review this repo"
```

**Verified 2026-06-30:** a Windows `loom.exe` → `ssh cpuchip@<box>` → a Claude Code agent listing/summarizing a
repo on the remote Ubuntu machine, its `→ Bash` tool-events streaming back live (~$0.12/turn). The pipe worked
first try; the far-side PATH (below) was the only catch.

The whole trust axis is one transport tree in the claude backend (`claudeCmd`):

```
direct                claude …                                                     (full host access)
--isolate             docker run -i … loom-claude claude …                          (host walled)
--remote H            ssh -T H bash -lc 'cd <dir> && claude …'                       (another machine)
--remote H --isolate  ssh -T H bash -lc 'docker run -i … loom-claude claude …'       (sandboxed ON the remote)
```

**`--remote --isolate`** composes the two: a sandboxed claude *on* the remote box — the exact shape "manage
remote claude sessions *safely*" wants (reach + wall). The docker command runs over ssh, so its volume paths
resolve **on the remote** (`$HOME` expanded there, `--dir` is a remote path). Pass `--dir` to scope the
sandbox; without it, it falls back to the remote `$HOME`.

Requirements: the remote box has **`claude` installed + authed** (it uses its *own* `~/.claude`), and your
ssh key reaches it. loom runs the remote command in a **login shell** (`bash -lc`) so the box's full PATH
loads — a plain non-interactive `ssh host "claude …"` uses a shell that misses nvm / npm-global / `~/.local/bin`
installs and dies with `claude: command not found` *even when claude works fine in your interactive ssh session
there* (a real gotcha, hit + fixed 2026-06-30). For `--remote --isolate`, the remote also needs **docker + the
`loom-claude` image** (build it there: `docker build -t loom-claude -f docker/Dockerfile.claude .`). Verify from
your own agent-loaded shell — a passphrase-locked key with no agent can't authenticate from an automated context.

`--model` overrides the model (e.g. `--model haiku`); `--dir` sets the agent's cwd.

## Configuring the agent — the substrate hinge

For a backend (pg-ai-stewards) to drive claude as a *worker* — not just ask it a question — loom forwards
claude's configuration flags, and under `--isolate` controls the container's `~/.claude`. **Two walls, set
independently:**

- **Filesystem wall** — `--isolate` / `--remote`: *where* the agent can touch (container / remote / host).
- **Capability wall** — `--mcp-config` + `--allowed-tools`: *what* it can call.

| loom flag | claude flag | for |
|---|---|---|
| `--mcp-config <file>` | `--mcp-config` | **the hinge** — wire in an MCP server (e.g. pg-ai-stewards) so the agent reads/writes the substrate back |
| `--allowed-tools <list>` | `--allowed-tools` | scope which tools (incl. MCP) it may call — the capability wall |
| `--permission-mode <mode>` | `--permission-mode` | e.g. `acceptEdits`, `plan` |
| `--skip-permissions` | `--dangerously-skip-permissions` | headless autonomy — **safe only inside `--isolate`** (the container is the wall) |
| `--system-prompt-file <f>` | `--append-system-prompt-file` | inject instructions |
| `--claude-home <dir>` | *(mount)* | `--isolate` only — the injection point, below |

**`--claude-home <dir>` is the injection point for everything under `--isolate`.** It mounts a host directory
as the container's *writable* `~/.claude`, so it carries **skills** (`<dir>/skills/`), **instructions**
(`<dir>/CLAUDE.md`), settings, MCP config — and it **persists claude's session state across containers**. That
last part is what makes **resume + isolate** work: every `docker run` is a fresh container, but the session
lives in the mounted home, so a later `--resume` reattaches. Verified 2026-06-30 (remember 55 in one container
→ recall 55 in a brand-new one, `projects/`/`sessions/` written to the home). *Without* `--claude-home`, an
isolated session's state dies with the container (`--rm`), so `--resume --isolate` silently starts fresh.

Config-file paths (`--mcp-config`, `--system-prompt-file`) are interpreted **on the target** — local host,
remote box, or (under `--isolate`) inside the container. So for isolate, put them in `--claude-home` and pass
the container path (e.g. `--mcp-config /root/.claude/mcp.json`).

**The substrate pattern:** keep a per-work-item `--claude-home` seeded with the substrate's skills +
instructions + the pg-ai-stewards MCP config; mount the repo as `--dir`; run `--isolate --skip-permissions`;
store loom's `session_id` on the work-item so a later dispatch resumes by re-mounting the same home. That is
loom as the substrate's hands: reach (dir), voice back (MCP hinge), wall (isolate), memory (resume).

### Test

```sh
go test ./...                 # pure unit tests (parsing, registry) — no money
LOOM_SMOKE=1 go test ./...    # + the live claude multi-turn oracle (spends a little)
```

## Status / roadmap (v0.1)

- ✅ Core `Backend`/`Session` interface · claude backend (persistent stream-json) ·
  **local backend (OpenAI-HTTP → `:8090`; verified single + multi-turn against the live rig; cloud+local `panel` proven)** ·
  **structured event streaming (`SendStream` + `--events`; verified — claude's tool calls/thinking observable, proven on a real tool-using task)** ·
  agy backend (experimental) · `panel` (concurrent council) · **`loom review` (diff/files → fan a review across agents)** · CLI · smoke oracle.
- ✅ **Dogfooded:** loom reviewed its own code and found+fixed real bugs (history-poisoning, a `SessionID` data race, the orphan-`</think>` CoT-strip gap).
- ✅ **Isolation (`--isolate`):** claude in a docker sandbox (`loom-claude`), host walled to `/work` + read-only creds — verified.
- **North star:** loom = the substrate's *agent fabric* — a uniform, **walled** way to summon intelligence; its soul is running agentic harnesses (Claude Code, agy) the substrate can't run itself, safely. Axes: agency (raw model ↔ agent) × trust (local ↔ sandboxed ↔ remote).
- ✅ **Remote (`--remote`):** ssh transport **live-verified end-to-end** (2026-06-30) — a Windows `loom.exe` drove a Claude Code agent on a remote Ubuntu box, its `→ Bash` tool-events streaming back, ~$0.12/turn. The **trust axis is complete** on the real path (direct / `--isolate` / `--remote`).
- ✅ **Resume (`--resume <id>`):** durable sessions — reattach to a prior session across a process restart / dropped pipe (context restored from claude's on-disk session store). Verified 2026-06-30 by a two-process oracle + CLI e2e. The piece that makes a **remote** session survive a broken pipe.
- ✅ **Interrupt + steer (`Interrupt()` / Ctrl-C):** stop a turn in flight and redirect on the live session (stream-json `control_request` interrupt; probe-verified wire format). Live oracle + `-race` on the concurrent path. **Completes the session-lifecycle triad** (carry / resume / interrupt+steer).
- ✅ **`remote + isolate`:** sandboxed claude *on* the remote box (ssh → docker-on-remote, volume paths resolved there via `$HOME`). Built + unit-tested (the composed argv); live-verify pending the `loom-claude` image built on the remote. Reach + wall composed — "manage remote sessions *safely*."
- ✅ **The substrate hinge (config surface):** `--mcp-config` (wire the substrate MCP into the agent — reads/writes back), `--allowed-tools` (capability wall), `--skip-permissions` (headless, safe in `--isolate`), `--system-prompt-file` (instructions), and `--claude-home` (the container's `~/.claude`: skills/instructions/settings + **persisted sessions → resume+isolate now works**, live-verified). loom can now drive claude as a *configured* substrate worker, not just a chat.
- **★ Next:** the first real `pg-ai-stewards → loom run --isolate --mcp-config …` dispatch (the viability test); tighter sandbox (scoped/short-lived token, egress limits — toward zero-trust); `agy --isolate`; panel role-routing (doer→critic).
- **Backlog:** `--session-id`/`--fork-session` (pre-assign / branch) surfaced in the CLI; agy `--conversation` resume in the CLI; a condenser for very long sessions (pattern from OpenHands' `LLMSummarizingCondenser`); routing/role assignment across the panel.

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
