# vulos-meet — Task Backlog

**Status: 1 / 1 tasks done (100%).**
Wave B foundation (MEET-CORE-01) is shipped: repo bootstrap, LiveKit-supervise
wrapper, VULOS-MEET/1 token validation, per-tenant room-namespace gate, admin
HTTP surface with `MEET_ADMIN_TOKEN` constant-time compare, georoute hook for
vulos-cloud CLOUD-REGION-01, YAML config, and the spec docs
(`spec/VERSIONS.md`, `spec/TOKEN.md`).

Module: `github.com/vul-os/vulos-meet` — the 5th OSS sibling repo. Wraps
[LiveKit Server](https://github.com/livekit/livekit-server) (Apache 2.0,
invoked as a supervised child process — see README "Architecture: vendor or
supervise?" for the decision).

`vulos-meet` is the open-source (MIT, Go) Vulos video-meetings **server**.
Token minting is OUT of scope — the cloud control plane (`vulos-cloud`
`MEET-CP-01`) is the sole token issuer; `vulos-meet` only validates. Self-
hostable as a standalone SFU when paired with any LiveKit-compatible minter.

> **Invariants (FROZEN):** License **MIT** (LiveKit Server is Apache 2.0,
> invoked as a subprocess — not vendored). Pure-Go where possible, **never CGO**.
> Module path `github.com/vul-os/vulos-meet`. **vulos-meet does NOT mint
> tokens** — minting is a vulos-cloud responsibility (`MEET-CP-01`); this repo
> only validates. **Per-tenant room namespace is mandatory** — every room ID
> is `<tenant><sep><rest>` and the admin surface enforces it before any
> read/write reaches LiveKit. Track LiveKit upstream binary releases for
> security/correctness fixes (`git fetch upstream && merge` posture as
> vulos-mail).

---

## How to read a task

Each task is a self-contained chunk of work. Format:

```
### [ID] short title
`todo` · P0|P1|P2|P3 · S|M|L · dep: <IDs or none> · parallel: yes|no — owned file path(s)
Scope: one paragraph; enough for an autonomous agent.
AC: [ ] verifiable outcome 1 [ ] outcome 2 [ ] go build ./... && go test ./...
```

**Status token** — line immediately after `### [ID]` carries `` `todo` `` or `` `done` ``.
**Priority** — `P0` highest → `P3` lowest.
**Effort** — `S` / `M` / `L` rough size.
**`parallel: no`** — touches a hot shared file or LiveKit-touching code; rebase on main before opening PR.
**Picking a task** — any `todo` whose `dep:` entries are all `done` is fair game.

---

## Area: Core wrapper

_Prefix: `MEET-*`_

### [MEET-CORE-01] vulos-meet repo: LiveKit Server wrap with Vulos auth + multi-tenancy
`done` · P1 · L · dep: none · parallel: yes — repo root, cmd/vulos-meet, internal/wrap, spec/
Scope: Bootstrap the new MIT Go repo at `github.com/vul-os/vulos-meet`. Supervise LiveKit Server as a child process (chosen over Go-module embedding — see README). Build the Vulos wrapping layer: token validator (`VULOS-MEET/1`), per-tenant room-namespace gate (every room is `<tenant><sep><rest>`; cross-tenant admin ops rejected), admin HTTP surface (`/admin/health`, `/admin/tenants/{tenant}/rooms`, `/admin/tenants/{tenant}/rooms/{room}`) guarded by `MEET_ADMIN_TOKEN` (constant-time), georoute hook exposing the box region on health, and YAML config with the production-relevant defaults (VP9 simulcast 3 layers, top-N audio mix = 3, active-speaker on, cascading SFU on, recording-egress hook). Spec docs `spec/VERSIONS.md` + `spec/TOKEN.md` capture the wire-level token shape.
AC: [x] repo created at /Users/pc/code/exo/vulos-meet with MIT LICENSE + go.mod [x] cmd/vulos-meet supervises livekit-server [x] VULOS-MEET/1 token validation (livekit/protocol/auth path) [x] per-tenant namespace + cross-tenant admin rejection [x] admin endpoint with constant-time token compare [x] georoute hook on /admin/health [x] config parse + defaults (VP9, top-N=3, cascading SFU, active speaker) [x] spec/VERSIONS.md + spec/TOKEN.md [x] go build ./... && go vet ./... && go test ./...

---

## Planned follow-ups (not yet started)

### [MEET-ROOMSVC-02] Replace MemoryRoomService with the real LiveKit RoomServiceClient
`todo` · P1 · M · dep: MEET-CORE-01 · parallel: no — internal/wrap/rooms_livekit.go (new), cmd/vulos-meet/main.go
Scope: The admin surface today is wired to an in-memory `MemoryRoomService` stand-in. Swap it for a thin wrapper around `livekit/protocol`'s `RoomServiceClient` (gRPC to the supervised livekit-server). Keep the `RoomService` interface unchanged so the admin handlers and tenant gate stay untouched. Add a circuit-breaker so an admin call cannot hang on a stalled child process.
AC: [ ] new struct implements `RoomService` against `RoomServiceClient` [ ] cmd/vulos-meet uses it instead of MemoryRoomService [ ] integration test against a real livekit-server binary (skipped when binary absent) [ ] go build ./... && go test ./...

### [MEET-SIGNAL-GATE-03] Signaling reverse proxy enforces VULOS-MEET/1 at the edge
`todo` · P1 · M · dep: MEET-CORE-01 · parallel: no — internal/wrap/signal.go (new), cmd/vulos-meet/main.go
Scope: Today the token validator exists but is only constructed (fail-fast). Wire it into a reverse proxy that sits in front of livekit-server's `/rtc` signaling endpoint, validates the token's Vulos profile (tenant binding, room prefix), and only then forwards the upgrade. Rejection MUST return 401/403 with no token contents in the response body. This is the practical seam that stops a token leaked from tenant A from being replayed against tenant B's room — defense-in-depth on top of LiveKit's own signature check.
AC: [ ] reverse proxy validates incoming tokens with `Validator.Validate` before forwarding [ ] rejection paths return typed status without leaking token contents [ ] e2e test: valid token reaches livekit, invalid tenant-binding does not [ ] go build ./... && go test ./...

### [MEET-CASCADE-CFG-04] Cascading-SFU cluster config + Redis discovery
`todo` · P2 · M · dep: MEET-CORE-01, MEET-ROOMSVC-02 · parallel: yes — internal/wrap/cluster.go (new), internal/wrap/supervise.go
Scope: LiveKit's cascading SFU uses Redis for node discovery. The default config in MEET-CORE-01 advertises `cascading_sfu: true` but does not render the Redis config block into the generated LiveKit YAML. Add the YAML rendering, the config fields (`cluster.redis.addr`, `cluster.region`, `cluster.node_id`), and a startup self-check that pings Redis before exec'ing livekit-server. Region MUST come from the existing `cfg.Region` so the cluster view stays consistent with the georoute view.
AC: [ ] Redis discovery YAML rendered when cascading SFU is enabled [ ] startup self-check fails fast on unreachable Redis [ ] cluster region == cfg.Region [ ] go build ./... && go test ./...

### [MEET-RECORDING-DRIVER-05] Egress-driver client for vulos-cloud MEET-RECORDING-01
`todo` · P2 · M · dep: MEET-CORE-01 · parallel: yes — internal/wrap/recording.go (new)
Scope: `cfg.Recording.EgressEndpoint` today is rendered into the LiveKit config as a webhook URL. Add a thin Go-side client that receives egress events from LiveKit (the webhook arrives at vulos-meet, not the cloud), verifies the LiveKit webhook signature, scopes the event to the right tenant (room ID prefix), and forwards a Vulos-shaped envelope to the cloud's egress driver. This keeps the cloud out of LiveKit's webhook secret rotation loop.
AC: [ ] webhook receiver verifies LiveKit signature [ ] tenant extraction from room ID prefix [ ] envelope forwarded to cloud egress endpoint with retries [ ] go build ./... && go test ./...

### [MEET-METRICS-06] Prometheus metrics for admin gate + token-validation outcomes
`todo` · P2 · S · dep: MEET-CORE-01 · parallel: yes — internal/wrap/metrics.go (new)
Scope: Emit Prometheus counters for admin requests (per status), token-validation outcomes (per sentinel error), and per-tenant active-room gauge. Wire `/metrics` next to `/admin/health` (but on a separate listener so it can be scoped to the internal network without exposing admin).
AC: [ ] /metrics surface exists [ ] counters cover admin auth fail, tenant mismatch, token-malformed, expired, wrong-tenant, missing-grant [ ] per-tenant room gauge updated on list [ ] go build ./... && go test ./...

### [MEET-UPSTREAM-MERGE-07] CONTRIBUTING-FORK.md: how to track upstream LiveKit releases
`todo` · P3 · S · dep: MEET-CORE-01 · parallel: yes — CONTRIBUTING-FORK.md (new)
Scope: Document the workflow for tracking upstream `github.com/livekit/livekit-server` releases. Because we supervise (not vendor) livekit-server, "tracking upstream" means bumping the pinned binary version and re-running our token-validation tests against it, NOT a Go-module merge. Capture the test matrix (one LiveKit minor we ship, one prior). Also document the `livekit/protocol` Go-module bump path (where token validation actually lives).
AC: [ ] CONTRIBUTING-FORK.md lists the binary-bump workflow [ ] documents `livekit/protocol` Go-module bump [ ] no kernel/wrap code edited by this task

---
