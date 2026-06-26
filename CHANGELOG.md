# Changelog

All notable changes to **Vulos Meet** are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)  
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

### Added

- **Unified storage seam (OS gateway → egress).** When the Vulos OS gateway
  injects per-request `X-Vulos-Storage-*` headers, the egress proxy rewrites
  `Start*Egress` requests so recording/egress artifacts land in the shared
  per-user bucket under the `meet/` key-space (file, segment, image, and direct
  track outputs are repointed at the injected S3 bucket/creds; stream outputs
  are left alone). Absent the seam (no endpoint header), requests are forwarded
  verbatim and the existing egress storage config is used. A decode/encode
  failure fails the request rather than silently storing to the wrong bucket.
- **Token validation (`VULOS-MEET/1`).** Admission-gate validator for
  LiveKit-compatible JWTs — JWT shape, signature, `exp`/`nbf`, and the
  tenant-audience ↔ room-prefix binding. `vulos-meet` validates, never mints.
- **Per-tenant room isolation.** Room IDs are `<tenant><sep><rest>`; the token's
  tenant audience must byte-equal the room-ID prefix (whole segment).
- **Signal gate.** Reverse proxy in front of LiveKit's `/rtc` WebSocket that
  validates the token before the upgrade reaches the SFU and enforces a per-box
  concurrent-room ceiling.
- **Egress / recording authorization.** Twirp proxy gating
  `POST /twirp/livekit.Egress/*` with a per-call `RoomRecord` grant requirement
  and tenant binding.
- **Usage metering.** LiveKit webhook receiver computing participant-minutes,
  surfaced on the admin/metrics endpoints.
- **Optional control-plane metering seam (`internal/cp`).** Built only when
  `CP_URL` is set; durable, fire-and-forget reporting with idempotency keys and
  a drop counter. The core (`internal/wrap`) never imports it.
- **Recording retention.** Local lifecycle ledger
  (`recording → available → expired → deleted`, plus `failed`) with a
  configurable retention sweep (TTL and/or per-room / per-tenant count caps).
- **Cascading SFU.** Default-on multi-node cascade via a shared Redis for peer
  discovery, with boot-time Redis reachability self-check (fail-fast).
- **Multi-region geo-routing hook.** Each box advertises its region on the admin
  health endpoint for an upstream geo-router.
- **Prometheus metrics.** Dependency-free `/metrics` exposer on a loopback
  listener (token outcomes, room/participant gauges, egress/recording lifecycle,
  retention, metered minutes).
- **Production media defaults.** VP9 simulcast (180p/360p/720p), top-N (3)
  server-side audio mixing, active-speaker detection.
- **Adversarial security suite** (`internal/wrap/pentest_security_test.go`) and
  [`SECURITY-TESTING.md`] documenting the attack-class matrix.
- **Docs:** [`docs/ARCHITECTURE.md`], [`spec/TOKEN.md`], [`spec/VERSIONS.md`],
  [`CONTRIBUTING-FORK.md`]; Docker/Compose and Fly.io self-hosting templates.

### Notes

- Versioned `0.0.1-dev`; the wire token sub-protocol is `VULOS-MEET/1` (see
  [`spec/VERSIONS.md`]).
- Conformed to the VulOS product-repo standard: README structure, the
  "Part of VulOS" banner + product map, and a documented standalone quick start.

[`SECURITY-TESTING.md`]: SECURITY-TESTING.md
[`docs/ARCHITECTURE.md`]: docs/ARCHITECTURE.md
[`spec/TOKEN.md`]: spec/TOKEN.md
[`spec/VERSIONS.md`]: spec/VERSIONS.md
[`CONTRIBUTING-FORK.md`]: CONTRIBUTING-FORK.md
</content>
