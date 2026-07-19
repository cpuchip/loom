# loom federation — harnesses meshed the way llama-chip meshes models

**Status:** PROPOSED 2026-07-18, from Michael's vision (verbatim intent): "does loom
mesh like llama-chip? so that all instances can talk and access sessions? … any
computer I have with loom on the mesh could be usable by any other loom session or
agent through loom? so like my android app could see loom sessions on say NOCIX
server or workchip too? like llama-chip federates llms?" — and his own gate, same
breath: "we may need to get tokens/mtls working because this is powerful and can be
destructive."

**Hard precondition (his ruling): the mTLS floor ships first.** No federation work
begins until pinned-mTLS transport is live (audit/build in flight). A llama-chip
peer serves inference; a loom peer serves HANDS — sessions that read, write, and
spend on that box. The security floor is not a refinement here; it is the
foundation.

## The shape (steal llama-chip's proven design wholesale)

llama-chip solved this exact topology for models, and every piece transfers:

- **Local-first peer mesh, no head/worker hierarchy.** Every node runs loom serve,
  fully functional alone; a peer's sessions appear only while the mesh reaches it
  and evict when it drops (roaming laptop reality).
- **Hub-managed roster** — llama.cpuchip.net's hub pattern (or a sibling
  `loom.cpuchip.net`, or even the SAME hub grown a second roster kind): nodes
  heartbeat their presence + session inventory; the roster is control-plane only,
  never carries session traffic. Sessions route peer-to-peer over the mesh.
- **Per-peer credentials** — the `peer_tokens` lesson generalized: with mTLS live,
  the peer credential IS the client cert pin; enrollment via `loom pair`.
- **The surface federates naturally:** `sessions_overview()` aggregates
  local + remote (owned_by peer:workchip, the /v1/models pattern);
  `session_open(node=…)` commissions on a named peer; `session_close` reaches any
  node's sessions (the e-stop stays universal — an agent on fermion can stop a
  runaway on NOCIX). The Android app sees one roster: every session, every box.

## What it enables (the good)

- The app as the fleet's window: sessions on fermion, NOCIX, any future box — one
  list, one stop surface.
- Placement: a commission that needs the 4090s runs on fermion; one that needs
  always-on cheap CPU runs on NOCIX; Spin (or I) pick the node the way llama-chip
  routes a model.
- The shipwright/A2A picture completes: agents handing work to agents across
  machines with loom as the hallway — coordination without the human as courier.

## The council questions (Michael's to rule, before any build)

1. **★ THE WORKCHIP BOUNDARY — RULED 2026-07-18 (Michael):** "fermion to/from
   workchip should purely be coordination over OSS repos that both use." So:
   workchip is **EXCLUDED from the loom mesh entirely** — no roster presence, no
   session visibility, no commissioning, either direction. The personal↔work
   channel is asynchronous OSS-repo collaboration (PRs/issues/A2A over shared
   public repos; lodestar named as a future candidate). This SUPERSEDES the
   proposal's option analysis below, kept for the record.
   *(original question:)* **THE WORKCHIP BOUNDARY (load-bearing).** The standing ruling is emphatic:
   work content OFF this box; NO personal↔work substrate federation EVER; the
   MAXIMUM channel is "loom bridge — coordination only." A loom mesh that includes
   workchip must therefore be scoped hard: visibility/coordination of workchip
   sessions MAY be within the ceiling; COMMISSIONING work on workchip from
   personal agents (or vice versa) almost certainly is not — it would make
   personal agents actors inside the work estate. Options: (a) exclude workchip
   from the loom mesh entirely; (b) a read-only/coordination-only peer class
   (see-and-signal, never open/send); (c) full peer. Recommendation: (b) at most,
   designed explicitly; default (a) until ruled. This one is not mine to lean on
   further — it touches employer trust.
2. **Spend authority — RULED 2026-07-18:** cap lives on the node that runs the
   session; opener identity travels. PLUS Michael's auth model: if the remote box
   has its OWN claude auth, sessions there use ITS account (buckets remain
   theirs); if the remote is unauthed, the ORIGINATING box's creds travel with
   the commission (session-home auth follows the session). Both modes supported.
   *(original)* **Spend authority across nodes.** A commission opened on NOCIX by an agent on
   fermion spends whose budget, under whose cap? Recommendation: the CAP lives on
   the node that runs the session (each serve enforces its own commission cap +
   spend posture), and cross-node opens carry the originating agent's identity in
   the session record.
3. **Tap gates — RULED 2026-07-18:** the app's LOOM TAB carries the bells/cards
   for commissioning gates (all nodes card to the one phone).
   *(original)* **Tap gates across nodes.** Writable commissions on ANY node should still land
   on Michael's ONE phone as a card. The tool_confirm rows live in the fermion
   substrate — remote nodes need a path to enqueue there (the roster hub could
   relay, or remote serves call fermion's gate endpoint). Design choice.
4. **NOCIX — RULED 2026-07-18:** MESH-ENROLL. No loom on a public IP; NOCIX
   joins NetBird. (Note for later alignment: llama-chip's NOCIX node currently
   uses the public+bearer pattern because NOCIX wasn't a mesh peer — once
   enrolled, that exposure can be closed to mesh-only too.)
   *(original)* **NOCIX exposure.** NOCIX is not a mesh peer today (public box, token-gated
   services). A loom peer there means either NetBird enrollment for NOCIX or a
   public wss endpoint with cert-pinning as the only wall. With mTLS, (b) is
   defensible; (a) is cleaner. His infrastructure call.
5. **Blast radius — RULED 2026-07-18:** remote-commissioning OFF by default,
   per-node explicit enable. *(original)* **Blast-radius defaults.** Which nodes accept REMOTE commissions at all
   (vs. visibility-only)? Recommendation: every serve ships remote-commissioning
   OFF by default; enabling it is a per-node, explicit act.

## Phasing (after mTLS lands and council rules)

- **P0:** council on the five questions above.
- **P1:** roster (hub or sibling) + `sessions_overview` federation — visibility
  only, all nodes. The app's one-list view. No remote opens.
- **P2:** remote `session_open`/`send`/`close` between PERSONAL nodes
  (fermion↔NOCIX), tap-gated writes relayed to the one phone, per-node caps.
- **P3:** whatever the workchip ruling permits — likely nothing beyond P1
  visibility, possibly not even that.

## Non-goals

Not a scheduler (placement stays explicit — a named node, not an auto-picker, in
P1/P2). Not a substrate federation (that proposal exists separately with its own
walls; loom federation moves SESSION control, never doc/engram content). Not a
replacement for A2A (durable tracked work still goes through the engine; this is
the live-hands hallway).
