# loom enrollment + the plain→mTLS migration

**Status: IMPLEMENTED 2026-07-18** (companion to `docs/proposals/loom-mesh-and-mtls.md`,
RATIFIED 2026-07-02). This doc covers the two pieces that complete the pinned-mTLS floor
so the federation work can begin: **code-driven enrollment** (so a phone/agent can join
without the two-screen PIN compare) and **coexistence serve** (so live clients migrate
from token to pin one at a time).

The transport itself — pinned-SPKI mTLS, `loom serve --tls`, `loom pair` — was already
live before this. See the proposal for the trust model (RFC 7250 raw-key pinning; no CA;
revoke = delete the pin).

---

## Two ways to establish trust

| | `loom pair` (SAS) | `loom enroll` (code) |
|---|---|---|
| Humans | **two**, one at each screen | **one** operator reads a code aloud |
| Client must be | a full loom binary with a TCP listener | anything that can HTTP POST (a phone app, a script) |
| Trust anchor | mutual PIN compare (ZRTP SAS) | one-time high-entropy code (MAC-bound) |
| Use for | box↔box on the mesh | **the phone**, unattended machines, agents, spin_bot |

Both end in the **same** state: each side pins the other's SPKI fingerprint, and every later
connection is pinned mTLS. Enrollment is the ratified "bootstrap-token → client-cert"
pattern (kubeadm join / Tailscale auth keys), specialized to loom's keys-never-leave model.

---

## `loom enroll` — the CLI

**On the box (server side)** — open a one-shot window and read out the code:

```
loom enroll --serve --listen 100.x.y.z:7779 --name phone
  Enrollment code (read it to the device being enrolled):

      GV56-OSAX

waiting for one enrollment on 100.x.y.z:7779 …
```

**On the enrolling machine/agent (client side)** — submit the code:

```
loom enroll --connect 100.x.y.z:7779 --code GV56-OSAX --name mybox
pinned "mybox" = f9df040a…d3b30
trusted. drive it with:
  loom run --connect wss://100.x.y.z:7777 --peer mybox ...
```

The window accepts exactly **one** valid enrollment, then closes (the code is single-use).
A bad code is refused (401) and does **not** consume the window. `--timeout` (default 5m)
bounds how long it stays open.

---

## The phone flow (for the brain-app team — the app implements the CLIENT side)

The Android app does the client side of the exact exchange `loom enroll --connect` runs.
No new server code is needed — the app POSTs to a `loom enroll --serve` window (or, later,
to the fold-in endpoint on the main serve — see "Follow-ups").

### One-time device setup

1. Generate a persistent **ECDSA P-256** keypair; keep the private key in the Android
   Keystore — **it never leaves the phone**.
2. Wrap the public key in a **self-signed X.509 certificate** (fields don't matter — loom
   ignores CN/validity; a 100-year NotAfter avoids an expiry ceremony). This cert is the
   phone's *identity envelope*.
3. The phone's stable identity is its **SPKI fingerprint**:
   `hex( sha256( cert.RawSubjectPublicKeyInfo ) )` — lowercase, 64 chars, no separators.
   This is exactly what loom pins. (It is a fingerprint of the SubjectPublicKeyInfo, **not**
   the whole cert — so the phone may re-issue the envelope around the same key and stay the
   same peer.)

### Enrollment (once per box the phone should reach)

The operator runs `loom enroll --serve --name <phone-name>` on the box and reads the code
to the user, who types it into the app. Then the app:

```
POST http://<box-host>:<enroll-port>/enroll        # PLAIN http — see "Why plain is safe"
Content-Type: application/json

{
  "label": "michael-pixel",                 # optional; the box may use it as the pin name
  "cert":  "<base64(client cert DER)>",
  "mac":   "<base64(client MAC)>"
}
```

- **`mac`** = `HMAC-SHA256(key, "loom-enroll-client-v1\0" || clientCertDER)`
  where `key = normalize(code)` = the code uppercased with spaces/hyphens stripped
  (so `gv56-osax`, `GV56 OSAX`, `GV56OSAX` all key the same).
- Base64 is standard (Go's `encoding/json` default for `[]byte`).

On success the box replies **200**:

```json
{
  "cert": "<base64(server cert DER)>",
  "mac":  "<base64(server MAC)>",
  "name": "phone"
}
```

- **`mac`** = `HMAC-SHA256(key, "loom-enroll-server-v1\0" || serverCertDER)`.
- The app **must verify** this MAC (constant-time) before trusting the cert — it proves the
  box actually holds the code (i.e. this is the box whose operator read it out, not an
  impostor). If it fails: pin **nothing**, show an error.
- Then compute the server's SPKI fingerprint the same way and **store it** as the pin for
  this box (under the app's own local name, e.g. "home loom").

On a bad/expired code the box replies **401** `{"error":"enrollment code invalid or expired"}`.

### Connecting after enrollment

Open a **wss://** connection to the box's mTLS listener and:
- **present** the phone's client cert in the TLS handshake,
- **disable** the platform's CA + hostname verification (there is no CA), and instead
- **verify** the server's leaf cert SPKI fingerprint **equals the pinned one** — reject
  otherwise. (TLS 1.3 only.)

Over that pinned channel the app speaks either loom's native websocket protocol (the
`sessions`/`open`/`send` frames) **or** the OpenAI-compatible shim at
`POST /v1/chat/completions` — both are served on the same TLS listener.

### Why a plain-HTTP enrollment is safe

The code authenticates **both** certs via the MACs, so a man-in-the-middle who does not know
the code cannot substitute either key — it can only relay the true certs, which is harmless
(the later mTLS is end-to-end between the real endpoints). The code is 40 bits and
single-use, so it also resists offline grinding within its short life. No private key ever
crosses the wire. Still: run enrollment on the trusted mesh (NetBird 100.x) when you can, and
keep the window brief.

---

## Coexistence serve — migrate one client at a time

`loom serve --tls` replaces the plain listener. To move a fleet of live clients without a
hard cutover, run **both at once** against one shared session set:

```
loom serve --listen 100.x.y.z:7777 --tls-listen 100.x.y.z:7778 --token-file ~/.loom/tokens
loom serve (coexistence) — plain 100.x.y.z:7777 (token) + pinned mTLS 100.x.y.z:7778 (pin+token) …
```

- The **plain** listener keeps working exactly as before (token-gated).
- The **mTLS** listener requires a pinned client cert **and** a token — during coexistence
  the token wall stays up on *both* listeners (the pin is added on top, never in place of
  the token). A wss client with no token is refused.
- Both listeners drive **one** server, so a named warm resident opened over plain can be
  reattached over wss with no re-spawn — a client keeps its warm session across the move.

### Migration steps

1. Switch the box to coexistence (`--tls-listen …`), token unchanged. Nothing breaks.
2. Enroll each client (phone via the app; machines via `loom pair` or `loom enroll`;
   spin_bot via `loom enroll`). Enrollment does not disturb live connections.
3. Point each client at `wss://<box>:7778 --peer <name>` (keep passing its token during
   coexistence). Move them **one at a time**, verifying each.
4. Once every client is on wss, cut over to pure `loom serve --tls --listen 100.x.y.z:7778`
   (drop the plain listener; the token becomes optional — the pin is the wall).

---

## Migration map — every client of the live serve (`ws://127.0.0.1:7791`)

| Client | How it connects today | What its migration to wss needs |
|---|---|---|
| **User CLI** (`loom run/send/sessions/await --connect`) | `ws://…` + `--token`, OR already `wss://… --peer` (supported) | Nothing new — `loom pair`/`loom enroll` the box, then `--connect wss://… --peer <name>`. |
| **loom-mcp** (`cmd/loom-mcp`) | `ConnectBackend{URL:"ws://127.0.0.1:7791", Token}` — loopback only, no cert | It talks to a **local** serve on loopback, where the token already gates it and nothing else can reach 127.0.0.1 — it can stay plain+token safely. If the serve ever goes tls-only, loom-mcp must set `URL=wss://`, load identity+pins, pin the serve's cert, and pass a peer name. *(Owned by the sessions-visibility arc — not changed here.)* |
| **spin_bot** (voice HMI) | plain **http** POST to the OpenAI shim `/v1/chat/completions` | Enroll it (`loom enroll --connect` → its own client cert pinned by the serve), then present that client cert on an **https** POST to the shim on the tls listener, verifying the server's pinned fingerprint. The shim already works over TLS (verified). |
| **brain-app (Android phone)** | *(not connected to 7791 yet — the whole reason enrollment exists)* | The phone flow above: generate identity → `loom enroll` → wss with client cert + pinned server. |

**Do NOT switch the live 7791 serve to tls in this step.** 7791 is loopback + token today;
flipping it to tls is a separate, deliberate act **after** the app and spin_bot gain wss +
client-cert support. Coexistence exists precisely so that flip can be gradual.

---

## Follow-ups (surfaced, not built)

- **Fold enrollment into the main serve** as a native op/endpoint, so a phone enrolls over
  the *existing* token'd channel instead of a separate one-shot port. Deferred here to avoid
  colliding with the in-flight sessions-visibility work on `serve.go`; the standalone
  `loom enroll --serve` window is the coordination-safe first cut and mirrors `loom pair`.
- **QR-coded server fingerprint** as an alternative anchor: the app scans the box's
  fingerprint (a "discovery hash", kubeadm-style) so the phone pins the server out-of-band
  and only needs a bearer enroll token — an option for a QR-capable UX. The MAC-bound code
  flow above needs no camera and works from a spoken code, so it ships first.
