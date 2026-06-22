<div align="center">

<img src="logo.png" width="96" alt="Vulos Meet" />

# Vulos Meet

**Self-hostable video meetings — a Go wrapper around the LiveKit SFU that adds Vulos auth, multi-tenancy, and metering.**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![SFU: LiveKit](https://img.shields.io/badge/SFU-LiveKit-FF6352.svg)](https://github.com/livekit/livekit-server)

*Vulos — rooted in **vula**, the Zulu and Xhosa word for **open**.*

</div>

---

## What is Vulos Meet?

Vulos Meet is the open-source (MIT, Go) video-meetings server for the Vulos
suite. It is **not a fork of LiveKit** and it does not reimplement an SFU —
it is a small Go wrapper that **supervises an upstream
[`livekit-server`](https://github.com/livekit/livekit-server)** (the
industry-standard, Apache-2.0 Selective Forwarding Unit) as a child process and
wraps it with the pieces a multi-tenant Vulos deployment needs: token
validation, per-tenant room isolation, egress/recording authorization, usage
metering, and an optional control-plane seam.

Because the media plane is plain LiveKit, **clients connect with the standard
[LiveKit client SDKs](https://docs.livekit.io/reference/)** (JS, Swift, Kotlin,
Flutter, React, Unity, …) — no custom client. Vulos Meet sits in front of the
SFU as the sole public-facing admission gate; a token that verifies at the gate
verifies in the SFU, because both use the exact same `livekit/protocol`
verification code path.

It targets Google-Meet-class meetings (hundreds of participants per room,
cascading to more) and is **self-hostable standalone**: bring your own
LiveKit-compatible token minter and you have a Vulos-flavoured Meet deployment,
with no dependency on any Vulos cloud service.

---

## Features

- **Token validation, never minting.** Meeting tokens are minted upstream (by a
  control plane or your own minter); `vulos-meet` only *validates* them against
  the `VULOS-MEET/1` profile — LiveKit JWT shape, signature, `exp`/`nbf`,
  room-prefix and tenant-audience binding. A compromised SFU box **cannot issue
  tokens for itself**. JWT signature/time checks are delegated to
  `livekit/protocol` (go-jose) so there is no second implementation to drift.
- **Per-tenant room isolation.** Every room ID is `<tenant><sep><rest>`. The
  token's tenant audience must byte-equal the room-ID tenant prefix (whole
  segment, no `HasPrefix` sloppiness), so a token for tenant A cannot be
  replayed against tenant B's room. The admin surface lists/deletes rooms only
  within the caller-specified tenant.
- **Signal gate.** A reverse proxy in front of LiveKit's `/rtc` WebSocket: it
  validates the token **before** the upgrade ever reaches the SFU, and enforces
  a per-box concurrent-room ceiling (even though LiveKit's config has
  `auto_create: true`).
- **Egress / recording authorization.** The egress Twirp proxy gates
  `POST /twirp/livekit.Egress/*` and additionally requires the per-call
  `RoomRecord` grant — a plain meeting-join token cannot trigger a recording,
  and a recording token for one tenant cannot record another's room. `vulos-meet`
  is the **sole** LiveKit-talking surface, so this check cannot be bypassed.
- **Usage metering.** A LiveKit webhook receiver computes participant-minutes
  per room and exposes them on the admin/metrics surfaces; an **optional**
  control-plane seam reports them centrally (off unless `CP_URL` is set).
- **Recording retention.** A local lifecycle ledger
  (`recording → available → expired → deleted`, plus `failed`) with a
  configurable retention sweep (by TTL and/or per-room / per-tenant count caps).
  The recording **blob** is owned by your sink — `vulos-meet` never holds the
  bytes.
- **Cascading SFU.** Default-on multi-node cascade via a shared Redis for peer
  discovery, so a room can grow past a single box. Redis reachability is
  self-checked at boot (fail-fast).
- **Multi-region geo-routing hook.** Each box advertises its region on the
  admin health endpoint so an upstream geo-router can steer a tenant to the
  nearest box.
- **Prometheus metrics.** A dependency-free `/metrics` exposer on its own
  internal-only listener (token outcomes, room/participant gauges, egress and
  recording lifecycle, retention, metered minutes).
- **Production media tuning by default.** VP9 simulcast (180p/360p/720p),
  top-N (3) server-side audio mixing, active-speaker detection.

---

## Architecture

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

**Why supervise instead of embed?** `github.com/livekit/livekit-server` pulls a
very large dependency graph (the full Pion WebRTC stack, Redis, NATS, OTel, …)
and upstream's documented deployment mode is the standalone binary. Running it
as a supervised child keeps the `vulos-meet` binary tiny, tracks upstream
security releases via simple binary swaps rather than dep-bump merges, and keeps
the wrapper's job (validate token, enforce tenant namespace, proxy/admin) where
it belongs — out of process. We import only the small
`github.com/livekit/protocol/auth` module so token verification is byte-identical
to the SFU's own. The SFU child is bound to loopback; it is reachable **only**
through the signal gate, never directly on the public interface.

---

## Quickstart

### Prerequisites

- Go **1.26+**
- A `livekit-server` binary on `PATH` (or pointed to by `VULOS_MEET_LIVEKIT_BIN`).
  Grab a release from
  [livekit/livekit-server](https://github.com/livekit/livekit-server/releases).
- (Optional) Redis, only if you enable the cascading SFU.

### Build

```sh
CGO_ENABLED=0 go build -o vulos-meet ./cmd/vulos-meet
```

### Configure

`vulos-meet` takes a single YAML config. A minimal one:

```yaml
region: "eu-fra"            # required — the region this box advertises
livekit:
  api_key: "APIxxxxxxxx"    # MUST match what your token minter uses to MINT
  api_secret: "secret..."   # the shared secret used to verify JWTs
admin:
  token: "change-me"        # guards /admin/* (or set MEET_ADMIN_TOKEN; env wins)
```

Everything else has sane defaults (codec, simulcast layers, audio mix,
active-speaker, cascading SFU, room caps, listen addresses). The full schema —
including `media`, `room`, `recording`, `cluster`, and `signal` blocks — lives
in [`internal/wrap/config.go`](internal/wrap/config.go).

### Run

```sh
./vulos-meet --config config.yaml
```

`vulos-meet` renders a LiveKit config from your YAML, exec's `livekit-server`
as a child, supervises it, and propagates `SIGTERM`/`SIGINT` cleanly to the
child on shutdown. It opens four listeners:

| Listener | Default | Scope | Purpose |
|---|---|---|---|
| signal-gate | `127.0.0.1:7883` | **public** | `/rtc` WS + `/twirp/livekit.Egress/*` + webhooks |
| admin | `:7881` | private | `/admin/*` (bearer-token guarded) |
| metrics | `127.0.0.1:7882` | loopback | `/metrics` (Prometheus text) |
| livekit-server | `127.0.0.1:7880` | loopback only | the supervised SFU (signaling + Twirp) |

**Flags:**

```
--config string         path to YAML config file (required)
--addr string           admin HTTP listen address (overrides config.admin.addr)
--metrics-addr string   metrics HTTP listen address (default "127.0.0.1:7882")
--version               print version and exit
```

### Admin surface

```
GET    /admin/health                              # status, version, region, protocol (unauthenticated — georoute probe)
GET    /admin/tenants/{tenant}/rooms              # list a tenant's rooms
DELETE /admin/tenants/{tenant}/rooms/{room}       # delete a room within a tenant
GET    /admin/tenants/{tenant}/usage              # live participant-minute accrual
```

All but `/admin/health` require `Authorization: Bearer <admin-token>`
(constant-time compared) **and** pass through the tenant gate before reaching
LiveKit.

---

## Configuration & environment variables

Most tuning lives in the YAML config; secrets and a few deploy knobs come from
the environment (env overrides the config so secrets never have to ship on
disk).

| Variable | Required | Purpose |
|---|---|---|
| `MEET_ADMIN_TOKEN` | recommended | Bearer token guarding `/admin/*`. Overrides `config.admin.token`. |
| `VULOS_MEET_LIVEKIT_BIN` | no | Path to the `livekit-server` binary (default: `livekit-server` on `PATH`). |
| `MEET_CLUSTER_REDIS_PASSWORD` | no | Password for the cascading-SFU discovery Redis (env-only, never in YAML). |
| `MEET_RECORDING_CLOUD_TOKEN` | no | Bearer token for the recording-blob delete + egress-forward legs. |
| `CP_URL` | no | **Optional metering seam.** When set, meet usage is reported to a control plane; unset = standalone. |
| `CP_SHARED_SECRET` | no | Sent as `X-Relay-Auth` on the usage POST when the `CP_URL` seam is on. |

The Docker entrypoint renders the YAML from a fuller set of `MEET_*` variables
(`MEET_LIVEKIT_API_KEY`, `MEET_LIVEKIT_API_SECRET`, `MEET_REGION`,
`MEET_SIGNAL_ADDR`, `MEET_RTC_PORT_START`/`END`, `MEET_EGRESS_ENDPOINT`,
`MEET_CLUSTER_REDIS_ADDR`, …) — see
[`docker-entrypoint.sh`](docker-entrypoint.sh).

### The optional control-plane seam

`vulos-meet` is the standalone product by default. The metering seam
(`internal/cp`) is built **only** when `CP_URL` is set; the core (`internal/wrap`)
never imports it. When `CP_URL` is unset the seam is off and `vulos-meet`
behaves exactly as it always has — no central dependency. Delivery is
fire-and-forget with bounded retries and idempotency keys, so the LiveKit
webhook hot path is never blocked and a retried event never double-counts
minutes.

---

## Self-hosting

`vulos-meet` is self-hostable as a standalone, cloud-free SFU. The two paths:

- **Docker / Compose.** [`Dockerfile`](Dockerfile) builds the wrapper and bundles
  a pinned `livekit-server`. [`docker-compose.yml`](docker-compose.yml) brings up
  a password-protected `redis` plus a `meet` instance wired to it (so you can
  exercise the cascading SFU locally); `meet` only boots once Redis answers an
  authenticated `PING`.
- **Fly.io (UDP media).** A per-region [`fly.toml`](fly.toml) and a dedicated
  Redis [`fly-redis.toml`](fly-redis.toml) template are included. WebRTC media is
  raw UDP, so a **dedicated IPv4** is required; LiveKit advertises the machine's
  public IP in ICE candidates. Use either a narrow UDP port range or the
  single-port UDP mux (`livekit.rtc_udp_port`) — the latter is what sustains the
  hundreds-of-participants tier. The deploy image must contain **both** the
  `vulos-meet` and `livekit-server` binaries.

For maintaining the supervised-LiveKit relationship (tracking upstream releases,
the deploy-image two-binary requirement, the egress-base-URL invariant), see
[`CONTRIBUTING-FORK.md`](CONTRIBUTING-FORK.md).

---

## Development & testing

```sh
# Build, vet, and run the full suite — pure Go, no network / Redis / livekit
# binary needed (the LiveKit upstream is a local httptest stand-in).
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...

# Coverage
CGO_ENABLED=0 go test ./... -cover
```

The codebase ships an **adversarial (pentest-style) security suite** in
[`internal/wrap/pentest_security_test.go`](internal/wrap/pentest_security_test.go):
each test *attempts a concrete attack* against the real `Validator`,
`SignalGate`, `EgressProxy`, `AdminServer`, and tenant gate, and asserts it is
blocked. A failure prefixed `LIVE VULN:` means a real, exploitable hole. It
covers token forgery (`alg:none` downgrade, signature stripping, payload
tampering, algorithm confusion, expiry/nbf), cross-tenant replay and prefix
confusion, egress/recording authorization, admin auth (constant-time compare,
listener scoping), signal-gate blocking before upstream, and participant-cap
DoS vectors.

```sh
# Just the adversarial suite, verbose
CGO_ENABLED=0 go test ./internal/wrap/ -run 'TestPentest' -v
```

See [`SECURITY-TESTING.md`](SECURITY-TESTING.md) for the full attack-class
matrix and findings.

---

## Security

The security model and adversarial test methodology are documented in
[`SECURITY-TESTING.md`](SECURITY-TESTING.md). In short: `vulos-meet` is the sole
public-facing admission seam in front of `livekit-server`; it never mints
tokens, it validates them and enforces the per-tenant room-namespace binding
before any request reaches the SFU. If you believe you have found a
vulnerability, please report it privately to the Vulos maintainers rather than
opening a public issue.

---

## License

MIT — see [LICENSE](LICENSE).

`livekit-server` is Apache 2.0 and is invoked as a subprocess (not vendored);
`github.com/livekit/protocol` is Apache 2.0 and imported as a Go module (the
Apache 2.0 grant is compatible with this MIT distribution).
