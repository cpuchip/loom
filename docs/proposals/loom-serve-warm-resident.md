# loom serve — warm-resident sessions + detach/await (seamless, cheap rounds)

**Status:** converged design (2026-07-01), from a two-agent coordination over the loom
link itself: the home-box Claude (implementer) + the workchip Claude (verify side, which
reviewed the merged `serve`/`connect`/`ws` code at `4b45ba3`). Michael's ask: "make the
loom link seamless + cheap for a round." I (home) own the build; general-workspace keeps
review.

## The problem (measured)
A round is home-implements → workchip-verifies over loom-serve. Today each drive is
`loom run --connect --resume <bigid>`, which **respawns claude and cold-reads the whole
session** — $13/$4/$18 across today's three drives on the 35 MB innovation-week session.
The serve holds a session resident only within one connection; `run` opens+sends+closes,
so residency is never leveraged. Two shapes make it worse: my harness drives from
**discrete, short-lived tool calls** (each a fresh client process — so `loom chat`'s
persistent stdin doesn't fit me), and the verify side's turns run **minutes** (build +
re-extract) — longer than a synchronous client turn wants to block.

## The fix (agreed)
Keep the claude process **resident and warm** across client disconnects; reattach to it
by a **stable name**; let long turns run **detached**. First open cold-reads once; every
later drive is a cache-warm reattach — instant + cheap.

### Core
1. **Name-keyed resident sessions.** `open{SessionName:"verify-loop", ...}` → if a resident
   session with that name exists, **reattach** to the live warm process; else cold-open
   (with `Resume` as the fallback anchor for a first open). **Do NOT key on claude's
   session id** — it forks on every cold resume (the id for next resume rides back in the
   last reply), so an id-keyed map goes stale after one resume.
2. **Two-writer fence.** When a resident matches, **reattach-or-refuse** — never spawn a
   second process against the same lineage (two processes appending one history = silent
   context divergence).
3. **Per-turn reply ring.** Buffer the last N replies by turn-id on the resident session.
   A client whose socket dropped mid-turn (harness timeout) reconnects and fetches the
   result by turn-id — no lost verdict, no duplicate work. (Without this, warm-reattach
   still loses turns.)
4. **`send --detach` → turn-id; `await <turn-id>` / `--last-reply`.** Start a long turn,
   return a turn-id immediately; poll/await for the reply. Lets a minutes-long verify run
   without a synchronous client blocking on it — the real ergonomics unlock for both a
   short-tool-call driver and a long verify.
5. **`loom sessions`** (over `--connect`): list residents — name, backend, opts summary
   (model/dir/allowed-tools/permission-mode), idle time, last turn-id. Needed for reattach
   UX, the janitor, and knowing what you're attached to.
6. **Idle TTL** (generous — hours): a resident past TTL is **downgraded to cold-resumable**
   (closed, its lineage id remembered), not lost. TTL costs one cold-read, never data.
   (keep_alive residents are otherwise immortal until daemon restart.)

### Notes / traps
- Opts (permission-mode / allowed-tools / mcp-config) **freeze at open** — a mis-provisioned
  resident must be cold-killed (and the cold-read returns), so surface opts in `sessions`.
- Two concurrent clients on one resident: `turnMu` already serializes turns (stdio safe),
  but events/replies route only to the sending socket and either client can `interrupt`
  the other's turn — document, don't redesign.
- `--report-json`: don't invent a schema — codify the verify-runbook's existing report line
  (`{"verdict":"pass|fail|mixed","ref":"<branch@sha>","deltas":{...}}`) so the driver parses
  the verdict cleanly instead of scraping prose.

## Build order
Core 1–4 first (that's the seamless-cheap round). Then 5 (`sessions`) + 6 (TTL). Then
`--report-json`. ssh + one-shot `run` stay as-is (fallback + first-open bootstrap).
