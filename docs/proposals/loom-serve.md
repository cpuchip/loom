# loom serve — loom as a service (websockets, loom-to-loom)

**Status:** proposal (2026-07-01, from the pg-ai-stewards lane — the first field consumer).
**Origin:** Michael, after the first live cross-machine bridge: "maybe loom as a service.
it runs, accepts websockets, and we can loom to loom."

## Why — the field evidence from the first real bridge (today)

The home session drove the work machine's `innovation-week` Claude session via
`loom run --remote --resume` (ssh transport). It **worked** — first cross-machine
Claude↔Claude contact, wall held, work queue exchanged — and every rough edge it hit
is exactly what a service transport dissolves:

1. **ssh auth archaeology.** The dispatch only auths from shells that see the Windows
   ssh-agent (PowerShell's `System32` ssh, not Git Bash's MinGW ssh). Cost an hour of
   probing. A `loom serve` client auths with a **token**, same from any shell on any OS.
2. **Cold restarts are expensive.** One `loom run --resume` turn into a 35MB session cost
   **$13.18** (cold prefix read × 3 internal turns). A server-side session process stays
   ALIVE between client messages — every follow-up turn is a prompt-cache read (~90%
   cheaper) with zero process-spawn latency.
3. **Quoting hell.** Driving remote commands through PowerShell→ssh→bash mangled every
   non-trivial pattern (CRLF here-strings, nested quotes; we resorted to base64-piping
   scripts). A WS protocol frames **JSON**; the shell disappears from the data path.
4. **Interrupt barely exists over ssh.** loom's interrupt is a stream-json
   `control_request` written to the live process's stdin — trivial for a server holding
   the process, fragile-to-impossible through a spawned ssh pipe. As a WS frame it's
   first-class, remotely.
5. **One client at a time.** A spawned pipe has one owner. A server can let the home
   session, Stewdio, and a phone client all attach to (or observe) the same session.

## What it is

A long-running `loom serve` on each participating box, exposing loom's existing
`Backend`/`Session` Go interface over websockets. The client side becomes a fourth
transport — `direct | docker (--isolate) | ssh (--remote) | ws (--connect)` — same flags,
same Reply shape.

```
loom serve --listen <mesh-ip>:7777 --token-file ~/.loom/tokens   # on each box
loom run  --connect ws://workchip:7777 --resume <id> --json "…"  # one-shot over WS
loom chat --connect ws://workchip:7777 --resume <id>             # live channel
```

### Protocol (frames, all JSON)

| op | direction | payload |
|---|---|---|
| `hello` | c→s | `{token, client}` → `{ok, server, backends}` |
| `open` | c→s | `SessionOpts` (agent, dir, model, resume, allowed-tools, permission-mode…) → `{session_id}` |
| `send` | c→s | `{session_id, text}` |
| `event` | s→c | streamed tool-calls/thinking (the `--events` stream, per session) |
| `reply` | s→c | the final `Reply{text, session_id, cost_usd, turns}` per turn |
| `interrupt` | c→s | `{session_id}` — stop the in-flight turn, session stays steerable |
| `attach` | c→s | `{session_id}` — reattach/observe a live server-held session |
| `close` | c→s | `{session_id, keep_alive: bool}` — drop the process or leave it resident |

Server holds a `session_id → live process` map. A dropped socket does NOT kill the
session (reattach on reconnect); an idle-timeout reaps resident processes (config),
and `--resume` recovers past the reap as today.

### loom-to-loom

Nothing asymmetric: each box runs a serve, each loom can be a client of the other.
Home drives work's `innovation-week`; work drives a home session to verify/implement.
This is the **live** counterpart of the substrate's A2A ledger — the ledger hands off
durable work items; the socket holds an interactive session. They compose: an A2A work
item can carry a `ws://` + `session_id` as its live handle.

## Security — this is a standing network capability (council-shaped)

A daemon that runs Claude with tool access on your box is a remote-execution service.
The walls, all existing house patterns:

- **Bind to the mesh only** (NetBird interface / `100.110.x` addr), never `0.0.0.0`.
- **Token auth** — steal `llama-hub`'s store verbatim (sha256 tokens, mint/revoke/list,
  admin key). Optionally per-token capability ceilings (max allowed-tools, allowed dirs).
- **Capability per session** stays exactly loom's existing wall: `allowed-tools`,
  `permission-mode`, `dir`, `--isolate` all still apply to what `open` may request —
  a token that only permits read-mostly opens can't open a write session.
- **No default `--skip-permissions`.** Same rule as today: headless+skip only inside
  isolation, or on an explicitly trusted session (owner's call in `open`).
- TLS optional at first (the mesh is already WireGuard-encrypted); required if ever
  bound off-mesh.

## Build shape (small — the hard parts already exist)

loom already has: persistent stream-json sessions, `Interruptible`, resume, the
`Backend/Session` interface, `--events` structured stream. `serve` is a thin WS shim
over that + the token store; `--connect` is a client transport implementing the same
`Session` interface over a socket. Suggested order:

1. `loom serve` + `hello/open/send/reply` (one client, no attach) + token store.
2. `--connect` transport for `run`/`chat` → **retire the ssh pain for daily use.**
3. `event` streaming + `interrupt` frames (the live-steer win).
4. `attach`/multi-client + idle-reap policy.
5. loom-to-loom dogfood: home⇄workchip both directions; then the A2A handle.

## Relationship to existing pieces

- **ssh transport stays** — zero-install fallback and the bootstrap (you need ssh once
  to start `loom serve` anyway).
- **llama-chip federation/hub** — same mesh, same token pattern; a future roster could
  advertise loom endpoints alongside model endpoints.
- **pg-ai-stewards** — the substrate's `loom_dispatch` (Phase 1 plan) can prefer
  `--connect` when a serve is reachable, falling back to subprocess/ssh.
