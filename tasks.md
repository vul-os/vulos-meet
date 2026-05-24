# vulos-meet — Task Backlog

**Status: 8 / 8 tasks done (100%).**
Wave B foundation (MEET-CORE-01) is shipped: repo bootstrap, LiveKit-supervise
wrapper, VULOS-MEET/1 token validation, per-tenant room-namespace gate, admin
HTTP surface with `MEET_ADMIN_TOKEN` constant-time compare, georoute hook for
vulos-cloud CLOUD-REGION-01, YAML config, and the spec docs
(`spec/VERSIONS.md`, `spec/TOKEN.md`). Follow-ups MEET-ROOMSVC-02, MEET-SIGNAL-
GATE-03, MEET-CASCADE-CFG-04, MEET-RECORDING-DRIVER-05, MEET-METRICS-06, and
MEET-UPSTREAM-MERGE-07 are shipped. Audit-2 fix wave: FIX-MEET-EGRESS-PROXY-01
(Twirp egress reverse-proxy on the signal-gate).

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
`done` · P1 · M · dep: MEET-CORE-01 · parallel: no — internal/wrap/rooms_livekit.go (new), cmd/vulos-meet/main.go
Scope: The admin surface today is wired to an in-memory `MemoryRoomService` stand-in. Swap it for a thin wrapper around `livekit/protocol`'s `RoomServiceClient` (gRPC to the supervised livekit-server). Keep the `RoomService` interface unchanged so the admin handlers and tenant gate stay untouched. Add a circuit-breaker so an admin call cannot hang on a stalled child process.
AC: [x] new struct implements `RoomService` against `RoomServiceClient` [x] cmd/vulos-meet uses it instead of MemoryRoomService [x] integration test against a real livekit-server binary (skipped when binary absent) [x] go build ./... && go test ./...

### [MEET-SIGNAL-GATE-03] Signaling reverse proxy enforces VULOS-MEET/1 at the edge
`done` · P1 · M · dep: MEET-CORE-01 · parallel: no — internal/wrap/signal.go (new), cmd/vulos-meet/main.go
Scope: Today the token validator exists but is only constructed (fail-fast). Wire it into a reverse proxy that sits in front of livekit-server's `/rtc` signaling endpoint, validates the token's Vulos profile (tenant binding, room prefix), and only then forwards the upgrade. Rejection MUST return 401/403 with no token contents in the response body. This is the practical seam that stops a token leaked from tenant A from being replayed against tenant B's room — defense-in-depth on top of LiveKit's own signature check.
AC: [x] reverse proxy validates incoming tokens with `Validator.Validate` before forwarding [x] rejection paths return typed status without leaking token contents [x] e2e test: valid token reaches livekit, invalid tenant-binding does not [x] go build ./... && go test ./...

### [MEET-CASCADE-CFG-04] Cascading-SFU cluster config + Redis discovery
`done` · P2 · M · dep: MEET-CORE-01, MEET-ROOMSVC-02 · parallel: yes — internal/wrap/cluster.go (new), internal/wrap/supervise.go
Scope: LiveKit's cascading SFU uses Redis for node discovery. The default config in MEET-CORE-01 advertises `cascading_sfu: true` but does not render the Redis config block into the generated LiveKit YAML. Add the YAML rendering, the config fields (`cluster.redis.addr`, `cluster.region`, `cluster.node_id`), and a startup self-check that pings Redis before exec'ing livekit-server. Region MUST come from the existing `cfg.Region` so the cluster view stays consistent with the georoute view.
AC: [x] Redis discovery YAML rendered when cascading SFU is enabled [x] startup self-check fails fast on unreachable Redis [x] cluster region == cfg.Region [x] go build ./... && go test ./...

### [MEET-RECORDING-DRIVER-05] Egress-driver client for vulos-cloud MEET-RECORDING-01
`done` · P2 · M · dep: MEET-CORE-01 · parallel: yes — internal/wrap/recording.go (new)
Scope: `cfg.Recording.EgressEndpoint` today is rendered into the LiveKit config as a webhook URL. Add a thin Go-side client that receives egress events from LiveKit (the webhook arrives at vulos-meet, not the cloud), verifies the LiveKit webhook signature, scopes the event to the right tenant (room ID prefix), and forwards a Vulos-shaped envelope to the cloud's egress driver. This keeps the cloud out of LiveKit's webhook secret rotation loop.
AC: [x] webhook receiver verifies LiveKit signature [x] tenant extraction from room ID prefix [x] envelope forwarded to cloud egress endpoint with retries [x] go build ./... && go test ./...

### [MEET-METRICS-06] Prometheus metrics for admin gate + token-validation outcomes
`done` · P2 · S · dep: MEET-CORE-01 · parallel: yes — internal/wrap/metrics.go (new)
Scope: Emit Prometheus counters for admin requests (per status), token-validation outcomes (per sentinel error), and per-tenant active-room gauge. Wire `/metrics` next to `/admin/health` (but on a separate listener so it can be scoped to the internal network without exposing admin).
AC: [x] /metrics surface exists [x] counters cover admin auth fail, tenant mismatch, token-malformed, expired, wrong-tenant, missing-grant [x] per-tenant room gauge updated on list [x] go build ./... && go test ./...

### [MEET-UPSTREAM-MERGE-07] CONTRIBUTING-FORK.md: how to track upstream LiveKit releases
`done` · P3 · S · dep: MEET-CORE-01 · parallel: yes — CONTRIBUTING-FORK.md (new)
Scope: Document the workflow for tracking upstream `github.com/livekit/livekit-server` releases. Because we supervise (not vendor) livekit-server, "tracking upstream" means bumping the pinned binary version and re-running our token-validation tests against it, NOT a Go-module merge. Capture the test matrix (one LiveKit minor we ship, one prior). Also document the `livekit/protocol` Go-module bump path (where token validation actually lives).
AC: [x] CONTRIBUTING-FORK.md lists the binary-bump workflow [x] documents `livekit/protocol` Go-module bump [x] no kernel/wrap code edited by this task

---

## Area: Audit-2 fix wave

_Findings from the second audit pass (2026-05). Each P0 task closes a gap where the cloud doc claims a behaviour that vulos-meet didn't actually implement; the cloud's only working escape was to bypass vulos-meet, breaking the "sole LiveKit-talking surface" invariant._

### [FIX-MEET-EGRESS-PROXY-01] Twirp egress reverse-proxy on the signal-gate
`done` · P0 · M · dep: MEET-SIGNAL-GATE-03, MEET-RECORDING-DRIVER-05 · parallel: no — internal/wrap/egress_proxy.go (new), internal/wrap/signal.go, cmd/vulos-meet/main.go, CONTRIBUTING-FORK.md, README.md
Scope: Before this task, `SignalGate.Handler` only proxied `/rtc`; Twirp egress paths weren't handled, so the cloud's `internal/meetalloc/recording.go` had to point `MEET_EGRESS_BASE_URL` at `livekit-server` directly, bypassing the tenant gate, metrics, and lifecycle hook. Add a `/twirp/livekit.Egress/*` reverse proxy as a sibling to the `/rtc` proxy: validate the inbound JWT (VULOS-MEET/1 with `RoomRecord: true` invariant), reject cross-tenant tokens with 403 + opaque body BEFORE upstream is contacted, and forward the request body byte-for-byte (Twirp is protobuf — opaque pass-through, `io.Copy`). Config knob `livekit.egress_upstream_addr` (default = `livekit.signaling_addr` since LiveKit serves Twirp on the same port). Document in CONTRIBUTING-FORK.md §5 + README.md that vulos-meet is the sole LiveKit-talking surface and `MEET_EGRESS_BASE_URL` MUST target the signal-gate.
AC: [x] `/twirp/livekit.Egress/*` proxy added with auth check [x] cross-tenant token returns 403 before upstream is contacted [x] missing/malformed token returns 401 [x] token without `RoomRecord` returns 403 (defense vs. meeting-join-token replay) [x] non-egress Twirp paths fall through to sibling (policy documented) [x] body forwarded verbatim (regression test asserts byte-equal) [x] `/rtc` proxy still works (regression-tested) [x] go build ./... && go vet ./... && go test ./... [x] gofmt -l . empty

---
