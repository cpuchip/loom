# loom flow — deterministic orchestration with cached resume

**Status:** DESIGN 2026-07-21 → v1 implemented in the same PR. Parity proposal #5
(`.spec/proposals/native-harness-parity.md`) — the native Workflow tool's shape,
adopted as a loom command.

## Why

The walk foreman pattern — briefs, waves, oracles, integration — is a DAG of
agent runs executed by hand. Two things make the native Workflow tool a machine
where the hand-run foreman is an art:

1. **Deterministic edges.** Steps declare what they need; the scheduler runs
   what's ready, in parallel where dependencies allow, and a failure stops its
   dependents without stopping independent branches.
2. **Resume-from-journal.** Every step's result is journaled with its oracle
   verdict. Re-running the flow SKIPS steps that already went green — a foreman
   crash (or a mid-flow model outage) costs nothing but the re-issue of the
   command.

The oracle is the load-bearing piece: a step is "done" when its *deterministic
check* passes, not when the model says so. Green = cached is only sound because
green is a shell exit code, not a vibe.

## The flow file (JSON — loom is zero-dependency, so no YAML)

```json
{
  "flow": "svc-refactor",
  "concurrency": 3,
  "steps": [
    {"id": "brief",  "agent": "claude", "model": "sonnet",
     "prompt": "Write refactor-brief.md covering …",
     "oracle": "test -s refactor-brief.md"},
    {"id": "build",  "agent": "codex", "dir": "svc",
     "prompt_file": "prompts/build.md", "needs": ["brief"],
     "oracle": "go build ./... && go test ./..."},
    {"id": "docs",   "agent": "claude", "model": "haiku",
     "prompt": "Update README for the refactor", "needs": ["brief"]},
    {"id": "review", "agent": "claude", "needs": ["build", "docs"],
     "prompt": "Review the combined change; write REVIEW.md",
     "oracle": "test -s REVIEW.md"}
  ]
}
```

Per step:

| field         | meaning                                                                                 |
|---------------|-----------------------------------------------------------------------------------------|
| `id`          | required, unique — the journal key                                                       |
| `agent`       | backend name (default `claude`)                                                          |
| `model`       | model override ("" = backend default)                                                    |
| `dir`         | the step's working dir; relative paths resolve against the FLOW FILE's directory (flows are portable) |
| `prompt` / `prompt_file` | exactly one; `prompt_file` resolves against the flow file's directory        |
| `needs`       | step ids that must be green first                                                        |
| `oracle`      | shell command run in the step's dir after the agent turn; exit 0 = green. Omitted = the turn itself succeeding is green (weaker — prefer an oracle) |

Flow-level: `flow` (the id; default = file basename minus `.json`),
`concurrency` (max steps in flight; default 3).

Trust and plumbing are FLOW-WIDE, from the CLI — steps that edit files need
them, and per-step trust would invite a nobody-audits-it matrix:
`--isolate --skip-permissions --mcp-config --skills --budget` apply to every
step's session exactly as they do on `loom run`. Flags come BEFORE the
positional (`loom flow run --skip-permissions f.json`), matching every other
loom command.

**Trust is per-invocation, NOT saved in the flow copy** — `resume` must be
given the same trust flags as the original `run`. This is deliberate (a saved
file must never silently escalate a later invocation), and it has a sharp
edge found in the first live smoke: a resume WITHOUT `--skip-permissions` ran
claude with edits failing closed, and the model **reported the write as done
anyway** ("created two.txt with the exact text…") while the file never
changed. The oracle caught it — three times — which is the whole argument for
oracles. If a resume's steps go `oracle_failed` while their replies claim
success, check the trust flags first.

## The journal

`$LOOM_HOME/flows/<flow-id>/` holds:

- `flow.json` — a verbatim copy of the flow file, saved at `flow run`. Resume
  reads THIS copy, so a resume is deterministic even if the original moved.
- `journal.jsonl` — one JSON line per step attempt, appended across runs:

```json
{"step":"build","status":"green","started_at":"…","finished_at":"…",
 "reply":{…the full Reply, usage included…},
 "oracle_rc":0,"oracle_tail":"ok\n","cached":false}
```

`status` ∈ `green` | `agent_failed` | `oracle_failed` | `dependency_failed` |
`budget_refused`. The READER takes the LAST record per step — a later green
supersedes an earlier failure. `cached:true` marks a resume that skipped the
step because it was already green (recorded so a journal reads as a complete
account of every run).

## Execution semantics

- Validation before anything runs: duplicate ids, unknown `needs`, dependency
  cycles, `prompt` XOR `prompt_file`, no steps — all hard errors at parse.
- Scheduler: a bounded pool (`concurrency`) dispatches any step whose needs are
  all green. Independent branches run in parallel; there is no other ordering
  guarantee.
- Each step is ONE fresh loom session: `Open` (workdir = step dir) → `Send`
  (the prompt) → `Close`. The Reply — usage included — lands in the journal.
- The oracle runs in the step's dir after a successful turn (`cmd /C` on
  Windows, `sh -c` elsewhere — the `loom race` oracle contract). Exit 0 =
  green; anything else = `oracle_failed` with the exit code and output tail
  journaled.
- A failed step marks every transitive dependent `dependency_failed` (recorded,
  not silently dropped); independent steps keep running.
- `--budget` (usage.go): checked before DISPATCHING each step — once either
  meter crosses the ceiling, not-yet-started steps record `budget_refused` and
  their dependents `dependency_failed`. Steps in flight complete.
- Exit code: 0 only when EVERY step is green.

## Resume

`loom flow resume <flow-id>` reloads `flow.json`, reads the journal, and:

- a step whose latest record is **green** is SKIPPED — its result is served
  from the journal (a `cached:true` line is appended). Its oracle is NOT
  re-run: green-is-cached is the killer feature, and re-checking would turn
  resume into rerun.
- everything else (`agent_failed`, `oracle_failed`, `dependency_failed`,
  `budget_refused`, never-ran) runs again, with cached greens satisfying
  their `needs`.

`flow run` on a flow-id that already has a journal REFUSES (the journal is
evidence; clobbering it silently would destroy the resume story) unless
`--fresh` is passed, which archives the old dir to `<flow-id>.<timestamp>/`
and starts clean.

## Non-goals (v1)

- Nested flows / cross-flow dependencies.
- Templating of prompts (no variable substitution — a prompt_file is the
  escape hatch; generate flows with a script if you need generation).
- Per-step trust flags, per-step schemas, per-step remotes.
- Editing a flow between resume runs (resume reads the saved copy; changing
  the flow means a new flow-id or `--fresh`).
- Watch/await integration (#1) — a flow is its own supervisor while it runs;
  evented completion plugs in later.

## Relation to the rest of loom

`loom race` = N contenders, ONE oracle, first green wins (competition).
`loom duo` = one workdir, worker + critic (opposition).
`loom flow` = many steps, each with its OWN oracle, dependency-ordered
(composition). The three share the session/trust surface and the oracle
contract; flow adds only the DAG and the journal.
