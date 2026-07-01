# Integrating loom into pg-ai-stewards

**Audience:** the pg-ai-stewards substrate (or any backend) that wants to drive Claude Code as a
*worker* — read a repo, build/modify files, run tests, and report back — rather than just ask a
model a question.

**Status:** loom's session lifecycle (carry / resume / interrupt+steer), trust axis (direct /
isolate / remote), and the configuration surface (MCP / tools / permissions / skills / instructions)
are all built and verified on the real path (2026-06-30). The one thing not yet done is *this* — the
first real substrate→loom dispatch. This guide is the contract.

This file lives in `cpuchip/loom` (public, MIT). Pull it into the substrate repo, or reference it.

---

## 1. The mental model

You already invoke `claude -p` (the Hinge reviewer) and you have a coder/code-pr pipeline. **loom is
the upgrade to that plumbing** — now durable (resume), steerable (interrupt), and walled
(isolate/remote). It is the concrete "claude-p in a transient workspace" piece of the Workspace-Host
vision.

- **Call loom as a subprocess** (`loom run …`), one per dispatch. This fits the A2A / ledger model —
  no long-lived loom service needed yet.
- **You don't stream code to claude — you give it a working directory.** Text streamed in the prompt
  is the *task*. The corpus/repo is a filesystem claude reaches with its own tools (Bash/Read/Write/
  Edit). That is claude's whole advantage over a raw model.
- **loom-claude is a tier, not a replacement.** Keep the local coder loop for cheap/bulk work; route
  the hard, multi-file, full-harness tasks to loom-claude.

## 2. Two walls, set independently

| wall | flags | governs |
|---|---|---|
| **filesystem** | `--isolate` (docker), `--remote H` (ssh), or neither (direct) | *where* the agent can touch |
| **capability** | `--mcp-config` + `--allowed-tools` (+ `--strict-mcp-config`) | *what* it can call, incl. which substrate tools |

This is the presiding covenant made operational: you hand the agent exactly the reach you intend.
For autonomous dispatch, **always `--isolate`** — the container is the wall that makes
`--skip-permissions` (headless, no prompts) safe.

## 3. Giving claude the code — and getting work back out

```
--dir <path>      # the agent's cwd = the repo/corpus it works, via its own tools
```

Three placements, matching the trust axis:

- `--dir /local/clone` — claude works a clone on the substrate host (digest a corpus *into* the
  substrate from outside).
- `--isolate --dir /clone` — same, but host-walled (the default for untrusted/autonomous work).
- `--remote H --isolate --dir /remote/clone` — sandboxed, on another box.

The substrate provisions the clone (its existing clone step), then points loom at it. (A `--clone
<git-url>` convenience could move that into loom later; not needed to start.)

**Getting work back out — the wall runs both ways.** Work leaves the container *only* through a channel you
opened. Four exist:

1. **Bind-mounted `/work`** — a bind mount is *bidirectional*, so anything claude writes under `/work` is on
   the host clone immediately (same inode). The main channel for code work: the diff / new files are just
   *there* on the host when the dispatch returns — no copy-out step.
2. **The MCP hinge** (§4) — claude calls `mcp__pg-ai-stewards__*` to write results into the substrate directly.
3. **git push** — claude commits in `/work` and pushes a branch (needs git creds + egress); the substrate pulls it.
4. **stdout** — the final answer text (the `Reply`; see §9, `--json`).

> **Anything written to an *unmounted* container path (`/tmp`, `/root/foo`) is destroyed by `--rm` and does
> not come out.** That's the wall, not a bug — but it means you must plan the exfil channel per dispatch: work
> that must survive has to land in `/work`, go through the MCP, or be pushed.

**Two models for getting data INTO the substrate:**

- **Push (claude → MCP hinge):** the agent writes into the substrate *as it works*. Real-time, agent-driven;
  best for **digestion** ("read this corpus → store engrams/docs"). Requires container **egress to the MCP**
  (the HTTP endpoint, §4).
- **Pull (substrate reads loom's output):** the substrate reads the mounted `/work` (the diff) + loom's stdout
  (`--json` `Reply`) + records the `session_id`, then *its own code* ingests. Requires **no container egress** —
  the more secure default, and it composes with a network-isolated sandbox.

Rule of thumb: **digestion → push (MCP); code-build → pull (bind-mount + `--json` stdout).** Pull is the
zero-egress default; add the hinge only when you want the agent to close the loop itself.

## 4. The hinge — wiring the pg-ai-stewards MCP

This is the linchpin: give the loom-driven claude the substrate's MCP, and it can **read/write the
substrate back** (`doc_import_corpus`, `a2a_submit`, `brain_create`, the graph) while it does the
file work.

```
--mcp-config <file>       # claude --mcp-config: a JSON file defining the MCP server(s)
--allowed-tools <list>    # scope exactly which tools (incl. MCP) it may call
--strict-mcp-config       # (passthrough) ignore any other MCP config, use only --mcp-config
```

MCP config file (**prefer an HTTP endpoint** so a *walled or remote* claude can still reach the
substrate — a stdio MCP can't cross the docker/ssh boundary):

```json
{ "mcpServers": { "pg-ai-stewards": { "type": "http", "url": "https://<substrate-mcp-endpoint>" } } }
```

Scope the capability wall to just the tools this dispatch should use:

```
--allowed-tools "mcp__pg-ai-stewards__doc_import_corpus,mcp__pg-ai-stewards__a2a_submit,Bash,Read,Write,Edit"
```

> **Prereq to confirm on your side:** expose the substrate MCP as an HTTP endpoint (you already do
> this for other remote MCPs). A read-only vs write toolset is your Hinge decision — the a2a_submit /
> write tools are what let claude *close the loop*, so gate them deliberately.

## 5. Skills, instructions, and durable sessions — `--claude-home`

Under `--isolate`, claude's `~/.claude` is inside an ephemeral container that `--rm` deletes. Mount a
host directory as that home to control all of it:

```
--claude-home <dir>       # mounted as the container's WRITABLE ~/.claude
```

One directory, four payoffs:

- **skills** — put them in `<dir>/skills/`
- **instructions** — `<dir>/CLAUDE.md` (or use `--system-prompt-file <f>` to append a system prompt)
- **settings / MCP** — `<dir>/settings.json`, or reference `<dir>` paths as container paths (e.g.
  `--mcp-config /root/.claude/mcp.json`)
- **persisted session state** — this is what makes **resume + isolate** work

**Path note:** config-file paths (`--mcp-config`, `--system-prompt-file`) are interpreted *on the
target*. Under `--isolate`, put the files in `--claude-home` and pass the **container** path
(`/root/.claude/…`), not the host path.

## 6. Session identity — resume as the durable handle

Every `docker run` is a fresh container. loom prints the `session_id`; **store it on the work-item.**
A later dispatch resumes by passing `--resume <id>` **and re-mounting the same `--claude-home`** (the
session state lives there). Without `--claude-home`, an isolated `--resume` silently starts fresh.

Verified: remember-55 in one container → recall-55 in a brand-new one, via the shared claude-home.

## 7. Control — interrupt & steer

A long-running dispatch can be stopped mid-turn. Programmatically (Go library) it's the
`loom.Interruptible` interface (`Interrupt()` writes a stream-json control_request; the session stays
alive to steer with the next `Send`). Via the CLI, the first Ctrl-C during a turn interrupts the
agent. This is the substrate's / Hinge's brake on a drifting agent.

## 8. The canonical dispatch

```sh
loom run --agent claude \
  --isolate \
  --dir /var/loom/wi-4213/clone \
  --claude-home /var/loom/wi-4213/home \        # seeded with skills/ + CLAUDE.md + mcp.json
  --mcp-config /root/.claude/mcp.json \          # container path (lives in --claude-home)
  --allowed-tools "mcp__pg-ai-stewards__a2a_submit,mcp__pg-ai-stewards__doc_get,Bash,Read,Write,Edit" \
  --skip-permissions \
  --json --events \
  "Implement the change described in a2a work item 4213; run the tests; submit the result via a2a_submit."
```

Then: parse the one-line JSON `Reply` from **stdout** (`--json`), store its `session_id` on the
work-item, and inspect the mounted clone/home for artifacts (diff, PR branch) the agent produced.
Streaming events (`--events`) went to stderr; the result JSON is clean on stdout.

## 9. Reading loom's output (subprocess)

Pass **`--json`** and loom emits the `Reply` as a single JSON line to **stdout** — the clean pull
channel for a subprocess caller:

```json
{"backend":"claude","text":"…final answer…","session_id":"<id>","cost_usd":0.06,"turns":1}
```

`error` is present only on failure (an interrupted turn reports `error_during_execution`). With
`--json --events`, streaming tool-call/thinking events still go to **stderr** while the final JSON lands
on stdout — so you log progress and parse the result cleanly. Store `session_id` on the work-item.

Without `--json`: stdout is the answer text; stderr carries `[<backend> $<cost>]` / `[session <id> …]`.
The Go library is also available directly (`loom.Backends()["claude"].Open(ctx, loom.SessionOpts{…})` →
`Session.Send`/`SendStream` return the `Reply{Text, SessionID, CostUSD, Turns, Err}`).

## 10. Prerequisites checklist

- [ ] `loom-claude` docker image built where claude will run (host: `docker build -t loom-claude -f
      docker/Dockerfile.claude .`; remote: build it on that box).
- [ ] The substrate MCP reachable as an **HTTP endpoint** (for walled/remote hinge).
- [ ] A claude subscription auth available where claude runs (`~/.claude/.credentials.json`; loom
      mounts it read-only into the container).
- [ ] A per-work-item `--claude-home` seeded with the substrate's skills + instructions + `mcp.json`.
- [ ] Decide the **tier policy**: which task classes route to loom-claude vs the local coder loop.

## 11. Council note

Dispatching a full Claude Code harness with write-back into the substrate is a **new standing
capability** (`dominion_in_council`). Wire it and prove one dispatch first; make it a *default* route
only after a council moment with Michael. Start narrow: read-mostly MCP scope, one work item, inspect
the result before widening.

---

*loom-side gaps this integration surfaces (ask general-workspace):* optionally a `--clone <url>`
convenience for the provision-the-clone step. Everything else in this guide — including `--json` — is
built and verified.
