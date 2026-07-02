# loom mesh + mTLS — council plan

**Status:** council-pending. Michael's load-bearing shapes captured 2026-07-01
(decision-fatigued after a hard week+half — he gave the shapes he knew and asked
me to hold the rest). This doc is the durable capture + the resolved synthesis,
so nothing evaporates. **Ratify when rested; do not build off this until then.**

The vision, in Michael's words: *"fire this up on a whim and let any of my claude
sessions on any box work any other claude session on another box — or the same
box if needed."* loom becomes the fabric that lets any session drive any session,
across machines, over the secure mesh.

## What Michael decided (the shapes)

1. **Topology — peer-to-peer first, mesh as an option like llama-chip, both in
   the end.** P2P is the on-a-whim default; the llama-chip-style hub/roster is the
   scale option. Not either/or — sequence P2P → hub.

2. **Auth — mTLS everywhere is the goal; token stays as a simple primitive for
   adopters who want simple; phase off token as the *primary* over time.** His own
   insight resolves the "unless both makes sense": *"token could be useful to get
   mTLS set up between nodes."* Yes — that's the bootstrap pattern (see Synthesis).

3. **Orchestration (fan-out / stream-to-many) — DEFERRED, explicitly YAGNI.** The
   substrate (A2A ledger) is better poised to own coordination; a standalone
   fan-out tool is "pretty cool" but we **gather real use cases first** before
   building. Do not design multi-session orchestration speculatively.

4. **NOCIX — a loom node on the NOCIX box** so Michael can manage that server
   *agentically from here*, not just over ssh. It *could* be the hub (llama-chip
   shape). Not necessarily public-internet exposure — it's another mesh peer.

5. **Blast radius — full harness per peer.** A peer that's trusted drives with the
   full Claude Code harness. (This is exactly why auth must be strong — a
   full-harness peer needs cryptographic per-peer identity, not a shared secret.)

## The synthesized auth model (resolves #2)

Layered — each tool for its situation, which honors "both, but sequenced":

- **P2P default — SAS/PIN pairing → pinned mTLS (no pre-shared secret).** Each
  node self-generates a keypair/cert on first run. To pair two nodes, they do a
  key exchange and each derives a **short authentication string** (a PIN) from a
  hash of *both* identities. Both terminals display the PIN; the human confirms it
  matches on both ends and taps yes on each — exactly Michael's phone↔watch BLE
  pairing. A man-in-the-middle produces different transcripts → different PINs →
  mismatch → reject. After confirmation the nodes **pin each other's cert** and
  every later connection is mTLS. The human tap *is* the trust anchor; no CA, no
  shared token needed for P2P. Stays zero-dep (Go `crypto/tls` + `crypto/ecdsa` +
  a hash for the SAS — all stdlib).

- **Token — demoted to headless bootstrap.** For when you *can't* eyeball two
  screens (scripted enrollment, or standing up the remote NOCIX box), a token
  authorizes "you may enroll," then the cert exchange takes over and mTLS carries
  everything after. This is the well-trodden bootstrap-token→client-cert pattern
  (kubeadm, Tailscale auth keys). Token isn't removed — it's demoted from
  "every connection" to "optional pairing shortcut," which is the phase-off.

- **Hub (llama-chip shape) — the CA + roster, for scale.** When there are many
  nodes and pairwise-tapping is tedious, a loom-hub issues/revokes certs and holds
  the roster (mint a join token, see peers, revoke one). Reuses the exact machinery
  llama-hub already has, pointed at certs instead of bearer tokens. This is where
  the cert-lifecycle operational weight belongs.

Net: **SAS-pairing for whim-P2P, token for headless bootstrap, hub-CA for scale.**

## The pairing UX Michael wants (from the BLE analogy — make it exactly this)

```
$ loom pair --connect ws://100.x.y.z:7777          # on box A
  pairing with box B (100.x.y.z)…
  ┌─────────────────────────────┐
  │   Confirm this PIN matches   │
  │        the other screen:     │
  │           4 7 2 9 1 6        │
  └─────────────────────────────┘
  match on both screens? [y/N]

# box B shows the SAME 4 7 2 9 1 6 and the same prompt.
# y on both → certs pinned → trusted. n on either → abort (possible MITM).
```

The bar Michael set (via the Dave lens): **super easy to set up.** If the secure
path is harder than the insecure path, people route around it. The tap-to-confirm
is the whole UX.

## The Dave lens (see `.claude/skills/dave-in-the-room`)

Dave — Michael's security-minded friend, not in the room — drove the mTLS +
mutual-verification + device-pairing shape. Captured as a reusable council voice.
His known instincts already applied above: don't be safe *only* because you're on
the mesh (mTLS is self-carried); per-peer identity so one leak ≠ rotate-all;
human-confirmed SAS to defeat MITM; easy-path == safe-path.

## Deferred / to gather before the rested council

- **Orchestration use cases (YAGNI gate).** Collect concrete "I wish loom could
  fan one message to N sessions / stream one session to N watchers" moments before
  designing anything. Substrate A2A ledger stays the presumptive coordinator.
- **Hub: extend llama-hub, or loom-native?** Both plausible; decide when the hub is
  actually needed (i.e., when node count makes pairwise-tapping tedious).
- **Sequencing** of: mesh-IP bind (drop the ssh tunnel) → SAS pairing + mTLS →
  serve-on-both-sides (symmetric) → NOCIX node → hub.
- **Overlap with the Workspace-Host / A2A multi-tenancy council** (already
  council-pending; shares topology + blast-radius). Fold or keep separate — TBD.

## Where it stands today (for continuity)

- Warm-resident loom is live and proven (cold $12.73 → warm $0.62). Transport
  today is a websocket **inside an ssh tunnel**, daemon bound to `127.0.0.1:7777`.
- `loom serve --listen` already accepts a mesh IP (help text literally says "a mesh
  IP:port"); `Serve()` warns on wildcard binds and points at a NetBird 100.x
  address. Token auth (`--token-file`) is independent of bind address. So the
  mesh-IP bind + drop-the-tunnel step is a config change, not new code — it's
  gated only on the auth-model decision above (don't expose off-loopback until
  mTLS/SAS is the wall, per the Dave lens).
