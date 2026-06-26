<div align="center">

<img src="logo.png" width="96" alt="Vulos Meet" />

# Vulos Meet

**Self-hostable video meetings — a Go wrapper around the LiveKit SFU that adds Vulos auth, multi-tenancy, and metering.**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![SFU: LiveKit](https://img.shields.io/badge/SFU-LiveKit-FF6352.svg)](https://github.com/livekit/livekit-server)

<sub><img src="logo.png" height="14" alt="VulOS"> Part of <strong><a href="https://vulos.org">VulOS</a></strong> — the open, self-hostable web OS &amp; app suite. Runs standalone, or combined under one login by <a href="https://vulos.org">Vulos Workspace</a>.</sub>

*Vulos — rooted in **vula**, the Zulu and Xhosa word for **open**.*

</div>

---

## What is Vulos Meet?

Vulos Meet is the open-source (MIT, Go) video-meetings server for the VulOS
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

## Part of VulOS

VulOS is an open, self-hostable web OS + app suite — independent products, each
self-hostable alone and combined under one login by **Vulos Workspace**:

- **Vulos Mail** — mail + calendar + contacts
- **Vulos Talk** — team chat + channels/Spaces + huddles
- **Vulos Meet** — video meetings (LiveKit SFU) — *this repo*
- **Vulos Office** — documents: docs, sheets, slides, PDF
- **Vulos Relay** — sovereign connectivity fabric
- **Vulos Workspace** — the open suite shell (one login, app switcher, admin)
- **Vulos OS** — the web-native desktop

Workspace *links/embeds* products; products never import each other (clean
seams). **Vulos Meet runs standalone and is combined by Vulos Workspace.**

**Meet's role — the canonical real-time video/SFU product.** Every VulOS product
that needs live audio/video routes to a Meet room rather than shipping its own
SFU:

- **Vulos Workspace** surfaces Meet as the in-suite meetings UI.
- **Vulos Talk** hands off huddles and 1:1/group calls to a Meet room.
- **Vulos Mail / Calendar** attach meeting links that resolve to a Meet room
  (the Mail/Calendar ⇄ Meet handoff is *seam-C*).

The handoff is **purely token + room join** — there is no Meet-specific RPC for
callers to learn. A control plane (`vulos-cloud`) mints a `VULOS-MEET/1` token
bound to `<tenant><sep><room>` (see [`spec/TOKEN.md`](spec/TOKEN.md)) and the
client joins that room through the public **signal gate** using a standard
LiveKit client SDK. Meet validates the token and the tenant↔room binding at the
gate; it does **not** need to know which product originated the meeting. This is
the same admission path the standalone deployment uses, so the suite seam adds
no new trust surface. Central usage metering rides the separate, optional
control-plane seam (`CP_URL`, see [Configuration](#configuration)) and is off in
standalone deployments.

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

> Vulos Meet is a pure-backend SFU wrapper — **there is no browser UI to
> screenshot.** See [Architecture](#architecture) for the data-plane diagram;
> clients use the standard LiveKit SDKs.

---

## Quick start (standalone)

Vulos Meet runs entirely on its own — **no Vulos cloud service required**. You
need a `livekit-server` binary and a token minter (any LiveKit-compatible
minter; the same `(api_key, api_secret)` pair on both sides).

### Docker

The image ([`Dockerfile`](Dockerfile)) bundles a pinned `livekit-server`, so the
SFU comes for free; the entrypoint renders the LiveKit config from `MEET_*`
variables (see [`docker-entrypoint.sh`](docker-entrypoint.sh)).

[`docker-compose.yml`](docker-compose.yml) builds the wrapper and brings up a
password-protected `redis` plus a `meet` instance wired to it (so you can
exercise the cascading SFU locally). It needs three secrets in the environment:

```sh
export MEET_LIVEKIT_API_KEY=APIxxxxxxxx
export MEET_LIVEKIT_API_SECRET=supersecretsigningvalueof32bytes
export MEET_CLUSTER_REDIS_PASSWORD=change-me
docker compose up         # builds meet, starts redis, then meet once redis answers PING
```

Or build and run the image standalone (no Redis; single box):

```sh
docker build -t vulos-meet .
docker run --rm \
  -e MEET_LIVEKIT_API_KEY=APIxxxxxxxx \
  -e MEET_LIVEKIT_API_SECRET=supersecretsigningvalueof32bytes \
  -e MEET_REGION=eu-fra \
  -e MEET_ADMIN_TOKEN=change-me \
  -p 7883:7883 \
  vulos-meet
```

### Binary

```sh
# 1. build (pure Go, static)
CGO_ENABLED=0 go build -o vulos-meet ./cmd/vulos-meet

# 2. a livekit-server binary must be on PATH (or set VULOS_MEET_LIVEKIT_BIN).
#    grab a release: https://github.com/livekit/livekit-server/releases

# 3. minimal config.yaml
cat > config.yaml <<'YAML'
region: "eu-fra"            # required — the region this box advertises
livekit:
  api_key: "APIxxxxxxxx"    # MUST match what your token minter uses to MINT
  api_secret: "secret..."   # the shared secret used to verify JWTs
admin:
  token: "change-me"        # guards /admin/* (or set MEET_ADMIN_TOKEN; env wins)
YAML

# 4. run
./vulos-meet --config config.yaml
```

Everything else has sane defaults (codec, simulcast layers, audio mix,
active-speaker, cascading SFU, room caps, listen addresses). The full schema —
including `media`, `room`, `recording`, `cluster`, and `signal` blocks — lives
in [`internal/wrap/config.go`](internal/wrap/config.go).

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

**Fly.io (UDP media).** A per-region [`fly.toml`](fly.toml) and a dedicated
Redis [`fly-redis.toml`](fly-redis.toml) template are included. WebRTC media is
raw UDP, so a **dedicated IPv4** is required; use either a narrow UDP port range
or the single-port UDP mux (`livekit.rtc_udp_port`) — the latter sustains the
hundreds-of-participants tier. The deploy image must contain **both** the
`vulos-meet` and `livekit-server` binaries.

For the architecture and maintaining the supervised-LiveKit relationship, see
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) and
[`CONTRIBUTING-FORK.md`](CONTRIBUTING-FORK.md).

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

The full component breakdown, request lifecycles, and suite seams are in
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Configuration

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
| `VULOS_STORAGE_BROKER_SECRET` | no | **Optional unified-storage seam.** Gates the gateway-injected `X-Vulos-Storage-*` egress destination (matched against `X-Vulos-Storage-Broker-Auth`); unset = seam off, egress storage forwarded verbatim. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#unified-storage-seam). |

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

## Documentation

| Doc | What's in it |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Component breakdown, request lifecycles, suite seams, the supervise-vs-embed rationale. |
| [`spec/TOKEN.md`](spec/TOKEN.md) | The `VULOS-MEET/1` token shape — claims, validation rules, forbidden patterns. |
| [`spec/VERSIONS.md`](spec/VERSIONS.md) | The token sub-protocol version registry and bump policy. |
| [`SECURITY-TESTING.md`](SECURITY-TESTING.md) | The adversarial (pentest-style) test methodology and attack-class matrix. |
| [`CONTRIBUTING-FORK.md`](CONTRIBUTING-FORK.md) | Maintaining the supervised-LiveKit relationship (tracking upstream, deploy-image two-binary requirement). |
| [`CHANGELOG.md`](CHANGELOG.md) | Notable changes (Keep a Changelog). |

---

## Development

```sh
# Build, vet, and run the full suite — pure Go, no network / Redis / livekit
# binary needed (the LiveKit upstream is a local httptest stand-in).
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...

# Coverage
CGO_ENABLED=0 go test ./... -cover

# Format check
gofmt -l .
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

## Contributing

Contributions are welcome. Because Vulos Meet supervises an upstream
`livekit-server` rather than vendoring it, the most important contributor
guidance is in [`CONTRIBUTING-FORK.md`](CONTRIBUTING-FORK.md): how to track
upstream LiveKit releases, the deploy-image two-binary requirement, and the
egress-base-URL invariant. Keep `go build`, `go vet`, `go test`, and `gofmt -l .`
green, and add an adversarial test for any new admission-path behavior.

---

## License

MIT — see [LICENSE](LICENSE).

`livekit-server` is Apache 2.0 and is invoked as a subprocess (not vendored);
`github.com/livekit/protocol` is Apache 2.0 and imported as a Go module (the
Apache 2.0 grant is compatible with this MIT distribution).
</content>
