<div align="center">

# Vulos Meet

**Open-source video meetings for Vulos — LiveKit SFU with Vulos auth + multi-tenancy**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white)](https://golang.org)

*Vulos — rooted in **vula**, the Zulu and Xhosa word for **open**.*

</div>

---

## What is Vulos Meet?

Vulos Meet is the **video-meetings server** for Vulos. It is open source
(MIT, Go), targets Google-Meet-class meetings (up to ~500 participants per
room, cascading to more), and runs as a small Go wrapper around
[LiveKit Server](https://github.com/livekit/livekit-server) — the
industry-standard MIT-licensed Selective Forwarding Unit (SFU).

The wrapper adds the pieces LiveKit alone does not give us:

- **Vulos token auth.** Meeting tokens are minted by `vulos-cloud`'s control
  plane (`MEET-CP-01`). `vulos-meet` *validates* them (LiveKit JWT shape, plus
  the Vulos `VULOS-MEET/1` profile — tenant-bound `aud`, room-prefix check, exp/nbf)
  but **never mints**. A leaked SFU box cannot issue tokens for itself.
- **Per-tenant room namespace.** Every room ID is `<tenant>:<rest>`. The admin
  surface lists/gets/deletes rooms only within the caller-specified tenant; a
  cross-tenant request is rejected before it ever reaches LiveKit.
- **Multi-region geo-routing hook.** The box advertises its region on the admin
  health endpoint so `vulos-cloud`'s `internal/georoute` (CLOUD-REGION-01) can
  steer a tenant to the nearest Meet box.
- **Admin HTTP surface.** Tiny: `/admin/health`,
  `/admin/tenants/{tenant}/rooms`, `/admin/tenants/{tenant}/rooms/{room}`.
  Guarded by `MEET_ADMIN_TOKEN` (env), constant-time compared.
- **Sole LiveKit-talking surface.** vulos-meet is the only Vulos process
  that talks to the supervised `livekit-server` child. The signal-gate
  proxies BOTH `/rtc` (WebSocket signaling) AND
  `/twirp/livekit.Egress/*` (egress RPCs from
  `vulos-cloud`'s `meetalloc/recording.go`) through the tenant gate. The
  cloud's `MEET_EGRESS_BASE_URL` MUST target vulos-meet's signal-gate
  listener, not LiveKit-Server directly — bypassing vulos-meet would
  silently drop the VULOS-MEET/1 tenant-binding check and the metric
  counters. See [`CONTRIBUTING-FORK.md`](CONTRIBUTING-FORK.md) §5.

It is also **self-hostable as a standalone SFU.** Bring your own
LiveKit-compatible token-minter and you have a Vulos-flavoured Meet
deployment.

---

## How vulos-meet fits with the other Vulos OSS repos

`vulos-meet` is the **5th OSS sibling repo** in the Vulos open-source set.
They are independent Go modules tracked by independent upstream remotes:

| Repo | Job | Upstream |
|---|---|---|
| [`vulos`](https://github.com/vul-os/vulos) | OS shell + identity + sync + Spaces apps | — |
| [`vulos-mail`](https://github.com/vul-os/vulos-mail) | Mail server (SMTP submission, IMAP, JMAP, webmail) | [Mox](https://github.com/mjl-/mox) |
| [`vulos-relay`](https://github.com/vul-os/vulos-relay) | Outbound mail relay + Vulos-to-Vulos peering + WebRTC signaling | — |
| [`vulos-office`](https://github.com/vul-os/vulos-office) | Docs/sheets/slides + Spaces office apps | — |
| **`vulos-meet`** *(this repo)* | **Video meetings (LiveKit SFU + Vulos wrap)** | [livekit-server](https://github.com/livekit/livekit-server) |

The same `git fetch upstream && merge` posture as vulos-mail applies here:
LiveKit Server upstream releases are tracked for security and correctness fixes;
the Vulos wrapper layer (`internal/wrap`) is additive and lives in our tree.

---

## Architecture: vendor or supervise?

We **supervise** LiveKit Server as a child process (option **b**), and import
only `github.com/livekit/protocol/auth` (a small, well-defined module) inside
our binary for token validation and grant inspection.

Why not embed LiveKit Server as a Go module?

- `github.com/livekit/livekit-server` pulls a very large dependency graph
  (the full Pion WebRTC stack, Redis client, NATS, multiple zap variants,
  Twirp, OTel, ...). Embedding would couple our build to upstream's version
  selection for dozens of unrelated deps.
- LiveKit's documented deployment model is the standalone `livekit-server`
  binary. Staying out of the embedding business keeps us close to that golden
  path — security patches arrive as upstream binary releases, not as Go-module
  bumps.
- The wrapper job (validate token, enforce tenant namespace, expose admin) is
  naturally an out-of-process concern: we proxy/admin around LiveKit; we don't
  need to be it.
- vulos already supervises an external binary the same way (`gpuhost`
  supervises Sunshine), so the operational pattern is familiar.

Importing `livekit/protocol/auth` directly inside the wrapper lets us validate
tokens with the *exact same* code path LiveKit Server uses, so a token that
verifies in our gate will verify in the SFU too.

---

## Running standalone

```
vulos-meet --config config.yaml
```

See [`internal/wrap/config.go`](internal/wrap/config.go) for the YAML schema.
A minimal config sets `livekit.api_key`, `livekit.api_secret`, `region`, and
`admin.token`.

Flags:

```
--config string   path to YAML config (required)
--addr string     admin HTTP listen address (default ":7880" → set via config too)
--version         print version and exit
```

Subprocess lifecycle: `vulos-meet` execs `livekit-server` with a generated
LiveKit config file, supervises it, and exits when LiveKit exits (or when SIGTERM
is delivered, propagating cleanly to the child).

---

## Deploy on Fly (UDP media)

The default production target is **self-hosted LiveKit on Fly.io**. Fly is used
because the SFU carries WebRTC audio/video over **raw UDP**, which Fly supports
(dedicated IPv4 required — see below). A [`fly.toml`](fly.toml) is included as
the shared per-region template.

```bash
# One Fly app per region (vulos-meet-<region>); georoute steers tenants.
fly launch --no-deploy --name vulos-meet-iad --region iad --copy-config

# Dedicated IPv4 is REQUIRED to serve UDP media — Fly can't serve UDP from a
# shared IP. Allocate v4 (and v6 for parity).
fly ips allocate-v4 -a vulos-meet-iad
fly ips allocate-v6 -a vulos-meet-iad

# Secrets (never bake into fly.toml): livekit api_key/secret (MUST match
# vulos-cloud MEET-CP-01), admin token, recording-cloud token, redis password.
fly secrets set -a vulos-meet-iad MEET_ADMIN_TOKEN=... MEET_RECORDING_CLOUD_TOKEN=...

fly deploy -a vulos-meet-iad --config fly.toml
```

Fly UDP essentials (see the comment block in `fly.toml` for the full detail):

- **Dedicated IPv4** is mandatory for the UDP media port(s).
- LiveKit's generated config sets `rtc.use_external_ip: true` (see
  `internal/wrap/supervise.go`) so it advertises the machine's public IP in ICE
  candidates — required for clients to reach a Fly-hosted SFU.
- Fly only forwards UDP to a listener bound to `fly-global-services`.
- **Port range:** LiveKit defaults to a 50000–60000 UDP range. Enumerating 10k
  UDP ports on Fly is impractical, so the `fly.toml` narrows it (default
  50000–50200) — and the LiveKit `rtc.port_range_*` in your `config.yaml` MUST
  match. Alternatively use a single `rtc.udp_port` UDP-mux port. Confirm Fly's
  current per-app UDP port-range limits before production.
- The deploy image must contain BOTH the `vulos-meet` binary AND a pinned
  `livekit-server` binary on `PATH` (or `VULOS_MEET_LIVEKIT_BIN`); add a
  `Dockerfile` that installs both — see [`CONTRIBUTING-FORK.md`](CONTRIBUTING-FORK.md) §1.

**Managed alternative.** Self-hosting on Fly is the default, but
[LiveKit Cloud](https://livekit.io/cloud) remains a possible managed option
later: point vulos-cloud's `MEET_*` env at the managed endpoints and skip this
deploy. The Vulos wrap layer (token gate, tenant namespace, admin) would still
front it. LiveKit stays Go/MIT-compatible either way.

---

## Default tuning (production-relevant)

- **Codec:** VP9 SVC simulcast, **3 spatial layers (180p / 360p / 720p).**
- **Audio mix:** top-N = **3** active speakers mixed server-side (long-tail
  attendees are silenced server-side, not sent over the wire).
- **Active-speaker detection:** enabled.
- **Cascading SFU:** enabled. >300 participants per room are split across
  cascading SFU nodes.
- **Recording-egress hook:** points at the `vulos-cloud` egress driver
  (`MEET-RECORDING-01`). The driver itself does NOT live in this repo.

---

## License

MIT — see [LICENSE](LICENSE). LiveKit Server is Apache 2.0 (it is invoked as
a subprocess, not vendored), and `github.com/livekit/protocol` is Apache 2.0
(imported as a Go module — the Apache 2.0 grant is compatible with our MIT
distribution).
