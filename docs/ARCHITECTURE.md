# Architecture — Vulos Meet

Vulos Meet is the **canonical real-time video/SFU product of the VulOS suite**.
It is a small Go wrapper that supervises an upstream
[`livekit-server`](https://github.com/livekit/livekit-server) (Apache-2.0
Selective Forwarding Unit) and fronts it with the admission, multi-tenancy,
and metering pieces a Vulos deployment needs. It runs standalone — no Vulos
cloud service is required — and is surfaced inside the suite by Vulos Workspace.

---

## Overview

```
                 LiveKit client SDK (browser / mobile / native)
                        │  token (minted upstream — VULOS-MEET/1)
                        ▼
        ┌──────────────────────────────────────────────┐
        │                 vulos-meet                     │
        │                                                │
        │  signal-gate  ──/rtc (WS)──────────┐           │   admin  :7881   (token-guarded)
        │  (public)     ──/twirp/Egress/*──┐ │           │   metrics:7882   (loopback)
        │   • validate VULOS-MEET/1 token  │ │           │
        │   • tenant binding + room cap    │ │           │
        │   • egress RoomRecord authz      │ │           │
        │   • webhook receiver (metering)  │ │           │
        └──────────────────────────────────┼─┼───────────┘
                                            │ │  supervises (child process)
                                            ▼ ▼
                              ┌──────────────────────────┐
                              │   livekit-server (SFU)   │  ◀── UDP media (clients)
                              │   bound to loopback      │
                              └──────────────────────────┘
                                            │
                              Redis (cascading-SFU discovery, optional)
```

The SFU child is bound to **loopback only**; it is reachable exclusively through
the signal gate, never directly on the public interface. The wrapper binary is
the sole LiveKit-talking surface.

---

## Listeners

| Listener | Default | Scope | Purpose |
|---|---|---|---|
| signal-gate | `127.0.0.1:7883` | **public** | `/rtc` WS + `/twirp/livekit.Egress/*` + LiveKit webhooks |
| admin | `:7881` | private | `/admin/*` (bearer-token guarded, tenant-scoped) |
| metrics | `127.0.0.1:7882` | loopback | `/metrics` (Prometheus text) |
| livekit-server | `127.0.0.1:7880` | loopback only | the supervised SFU (signaling + Twirp) |

---

## Components

The wrapper lives in two packages:

- **`internal/wrap`** — the core, self-contained product. Never imports the
  control-plane seam.
  - `config.go` — YAML schema + LiveKit config rendering; env overrides.
  - `supervise.go` — exec/supervise `livekit-server` as a child, signal
    propagation, clean shutdown.
  - `auth.go` — the `VULOS-MEET/1` token `Validator` (JWT verification delegated
    to `github.com/livekit/protocol/auth` / go-jose).
  - `tenant.go` — room-ID qualification and the tenant↔room-prefix binding.
  - `signal.go` — the public signal gate: validates before the WS upgrade and
    enforces the per-box concurrent-room ceiling.
  - `egress_proxy.go` — the egress Twirp proxy with `RoomRecord` authorization.
  - `rooms.go` / `rooms_livekit.go` — tenant-scoped room list/delete against the
    LiveKit room service.
  - `recording.go` / `recording_cloud.go` / `retention.go` — the recording
    lifecycle ledger and retention sweep.
  - `usage.go` — the LiveKit webhook receiver and participant-minute accounting.
  - `cluster.go` — cascading-SFU Redis wiring and boot-time reachability check.
  - `georoute.go` — region advertisement on the health endpoint.
  - `admin.go` — the admin HTTP surface.
  - `metrics.go` — the dependency-free Prometheus exposer.
- **`internal/cp`** — the **optional** control-plane metering seam. Built (and
  imported) only when `CP_URL` is set. Durable, fire-and-forget delivery with
  idempotency keys and a drop counter.

`cmd/vulos-meet/main.go` wires these together and owns the process lifecycle.

---

## Request lifecycles

**Join.** Client presents a `VULOS-MEET/1` JWT to the signal gate (`/rtc`). The
gate runs the validator (signature, time, grants, room well-formedness, tenant
binding) and checks the per-box room cap **before** proxying the WebSocket
upgrade to the loopback SFU. A rejected token never reaches LiveKit.

**Record.** An egress request (`/twirp/livekit.Egress/*`) must carry a token
with the per-call `RoomRecord` grant for the *same* tenant as the target room.
A plain join token cannot trigger a recording; a recording token for tenant A
cannot record tenant B's room.

**Meter.** LiveKit posts room/participant lifecycle webhooks back to the gate.
The receiver accrues participant-minutes per room (visible on `/admin` and
`/metrics`); if `CP_URL` is set, the `internal/cp` seam reports them centrally
without blocking the webhook hot path.

---

## Why supervise instead of embed

`github.com/livekit/livekit-server` pulls a very large dependency graph (the
full Pion WebRTC stack, Redis, NATS, OTel, …) and upstream's documented
deployment mode is the standalone binary. Running it as a supervised child:

- keeps the `vulos-meet` binary tiny;
- tracks upstream security releases via simple binary swaps rather than dep-bump
  merges;
- keeps the wrapper's job (validate token, enforce tenant namespace,
  proxy/admin) out of process.

Only the small `github.com/livekit/protocol/auth` module is imported, so token
verification is byte-identical to the SFU's own. See
[`CONTRIBUTING-FORK.md`](../CONTRIBUTING-FORK.md) for maintaining this
relationship (upstream tracking, the deploy-image two-binary requirement, the
egress-base-URL invariant).

---

## Suite seams

Vulos Meet is the destination other VulOS products hand off to; it never imports
them, and they never import it.

- **Workspace** surfaces Meet as the in-suite meetings UI.
- **Talk** hands off huddles and 1:1/group calls to a Meet room.
- **Mail / Calendar** attach meeting links that resolve to a Meet room — the
  Mail/Calendar ⇄ Meet handoff is *seam-C*.

Every handoff is **purely token + room join**: a control plane (`vulos-cloud`)
mints a `VULOS-MEET/1` token bound to `<tenant><sep><room>` (see
[`spec/TOKEN.md`](../spec/TOKEN.md)) and the client joins through the public
signal gate with a standard LiveKit SDK. There is no Meet-specific RPC for
callers to learn, and the suite path is the same admission path standalone uses —
so it adds no new trust surface. Central metering rides the separate, optional
`CP_URL` seam and is off in standalone deployments.

---

## Unified storage seam

In a full VulOS deployment the OS gateway gives every product a shared,
per-user object-storage bucket and brokers short-lived credentials for it. When
the gateway proxies an egress (`/twirp/livekit.Egress/*`) request to Meet, it
injects the destination as a family of request headers; Meet honors them by
repointing the recording/egress S3 output at that bucket instead of whatever
bucket the cloud named in the body. Absent the headers, Meet falls back to its
existing egress storage config untouched — that fallback is the standalone /
self-host contract.

**Injected headers** (consumed by `internal/wrap/storage_seam.go`):

| Header | Meaning |
|---|---|
| `X-Vulos-Storage-Endpoint` | S3 endpoint URL; **empty/absent ⇒ no seam, fall back** |
| `X-Vulos-Storage-Bucket` | shared per-user bucket |
| `X-Vulos-Storage-Prefix` | per-user key prefix |
| `X-Vulos-Storage-Region` | S3 region |
| `X-Vulos-Storage-Access-Key` / `-Secret-Key` / `-Session-Token` | short-lived credentials |
| `X-Vulos-Storage-Broker-Auth` | shared broker secret proving the gateway injected the seam |

**Broker-auth gate.** The injected headers are honored **only** when the request
also presents `X-Vulos-Storage-Broker-Auth` matching the configured
`VULOS_STORAGE_BROKER_SECRET` (constant-time compare). If the secret is unset
(standalone) or the header is missing/wrong, the gate is **closed**: the seam is
ignored and egress is forwarded verbatim, exactly as before the seam existed.
This stops an on-box caller from steering recording output at an attacker-chosen
bucket by spoofing the headers.

**Key-space.** All Meet artifacts are filed under `<userID>/<appID>/meet/`
within the shared bucket (the per-user prefix joined with the reserved `meet/`
space), so products never collide in the shared bucket.

**Fail-closed.** Under an authorized seam, Meet refuses to ship the short-lived
credentials to a plaintext/public endpoint (plain `http://` is tolerated only for
loopback / private hosts), and a body decode/encode failure during the S3
rewrite **fails the egress (400)** rather than silently forwarding to the
cloud-named bucket — storing user media in an unintended place is worse than
refusing the recording.

**Defense-in-depth.** The `X-Vulos-Storage-*` headers (and the legacy
`X-Vulos-Broker-Auth` name) are stripped before the egress proxy forwards to the
loopback `livekit-server` child — the broker secret and storage credentials are
consumed entirely in the wrapper and never reach the SFU subprocess.

---

## Security model

`vulos-meet` is the sole public-facing admission seam in front of
`livekit-server`. It never mints tokens; it validates them and enforces the
per-tenant room-namespace binding before any request reaches the SFU. The
adversarial (pentest-style) test suite
(`internal/wrap/pentest_security_test.go`) attempts concrete attacks — token
forgery, cross-tenant replay, egress authorization bypass, admin auth, signal-
gate bypass, participant-cap DoS — and asserts each is blocked. See
[`SECURITY-TESTING.md`](../SECURITY-TESTING.md) for the full matrix.
</content>
