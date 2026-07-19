# loom ↔ native-harness parity — what to adopt from Claude Code's agent system

**Status:** PROPOSED 2026-07-20 (Michael: "what features do you have with your
sub agents / background agents / workflow that we could adopt into loom to make
it more like your native tools"). Ranked by pain already felt in the walk build.

## 1. Completion notification (the gap that bit us) — HIGHEST
Native background tasks re-invoke their parent with a task-notification when
they finish; a foreman never has to poll. loom runs end silently — a supervisor
waits on the process return, and a wrapper death mutes the signal forever (the
2b stall). **Adopt:** the run-manifest/sentinel package (building now) + a
`loom await <run-id>` / watch API so any supervisor gets woken, not just the
process parent. Serve-side: session-end events pushed to subscribers (loom-mcp
already polls; make it evented).

## 2. Streamed output files for every run — shipped with the durability package
Native background tasks always write an output file readable mid-run. loom run
buffered until completion (lost on death). Same fix package.

## 3. Uniform usage + budget accounting — HIGH
Native agents report token usage per subagent; Workflow tracks a shared budget
with hard ceilings (`budget.spent()/remaining()`). loom surfaces USD only on
the opencode backend — the walk foreman flew blind on codex/agy cost.
**Adopt:** per-run usage in the manifest (each backend reports what it can:
tokens, USD, turns) + an optional `--budget` that refuses further turns past a
ceiling. Fleet-level: `loom runs` sums the day.

## 4. Structured output (schema-forced results) — HIGH, cheap
Workflow's `agent(prompt, {schema})` forces the worker's final answer through a
validated JSON schema — no parsing, retry on mismatch. **Adopt:**
`loom run --output-schema file.json`: loom validates the worker's final message
against the schema, re-prompts once on failure, exit code reflects validity.
Foremen stop regex-mining worker prose.

## 5. `loom flow` — deterministic orchestration with cached resume — THE BIG ONE
The native Workflow tool: a JS script orchestrates agents with `pipeline()`/
`parallel()`, phases, and — the killer feature — **resume-from-journal**: rerun
the script and completed agent() calls return cached results instantly; only
new/changed steps run live. The walk foreman is exactly this pattern done by
hand (briefs, waves, oracles, integration). **Adopt as loom flow:** a script
(Go-embedded JS or a simple YAML DAG — decide in design) where steps = loom
runs with lane, brief, oracle command, and dependency edges; a journal under
LOOM_HOME/flows/<id>/ records each step's result; `loom flow resume` skips
green steps. A foreman crash costs nothing — the flow resumes where it died.
This + #1 turns the foreman pattern from an art into a machine.

## 6. Typed agent presets — MEDIUM (role homes are 80% of it)
Native subagent_type = named agent with own prompt/tools/model. loom's role
homes (companion/critic/wargame…) already carry persona+settings; presets would
add default backend/model/flags per role: `loom run --role critic` instead of
five flags. Formalize what exists.

## 7. Watches/monitors — MEDIUM
Native Monitors tail logs/conditions and wake the owner per event. With
manifests on disk, a tiny `loom watch --stale 120s` that alerts (or execs a
hook) when any run's heartbeat goes stale gives fleets a watchdog for free.

## Explicitly NOT adopting
- Nested permission prompts / interactive plan modes — loom workers run
  headless by design; gates live in the substrate tap system.
- A full hook system — oracles-run-by-the-foreman already cover it with less
  machinery.

## Sequencing
Durability package (in flight) → #1 await/events + #3 usage → #4 schema →
#5 flow (design doc first — it deserves its own spec) → #6/#7 as quick wins.
