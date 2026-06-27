# Vulos Meet — Sub-protocol Version Registry

This document is the authoritative declaration of the **current** Vulos Meet
sub-protocol version and the rules governing version changes. The token shape
itself is specified in [`TOKEN.md`](TOKEN.md).

---

## Current version

```
VULOS-MEET/1
```

- **Wire identifier:** the ASCII string `VULOS-MEET/1`. It is the value of the
  `protocol` field returned by `GET /admin/info` and the value `vulos-meet`
  expects when validating a token's Vulos profile.
- **Status:** STABLE. Implementations claiming Vulos Meet compatibility MUST
  implement `VULOS-MEET/1` exactly as specified.
- **Underlying carrier:** a LiveKit-compatible JWT (HS256, signed with the
  shared `(api_key, api_secret)` pair). The Vulos profile constrains how the
  standard LiveKit claims are populated and adds a tenant-binding invariant.

A token-issuer (`vulos-cloud` `MEET-CP-01`) advertises the sub-protocol
version(s) it mints. A validator (`vulos-meet`) MUST reject any token whose
Vulos profile it does not implement. There is no silent downgrade.

---

## Versioning model

The Vulos Meet protocol uses a single monotonic **major** integer carried in
the wire identifier (`VULOS-MEET/<N>`). There is no minor version: any change
two independent implementations could observe differently is a major bump.

Forward- and backward-compatibility is achieved by **negotiation**, not by
silently tolerating unknown fields:

- A validator MUST reject a token whose Vulos profile version it does not
  recognize.
- A validator MUST reject a token whose claim shape violates the version
  rules ("missing tenant audience", "tenant in `aud` does not match room
  prefix", etc.). See [`TOKEN.md`](TOKEN.md) §3.
- Adding a new optional claim is NOT permitted without a version bump:
  validators do not look at unknown claims, so the only way to introduce one
  safely is `VULOS-MEET/2`.

### When to bump the major version

Bump if any of the following change:

- The tenant audience carrier (currently `ClaimGrants.Name`).
- The room-prefix construction (currently `<tenant><sep><rest>` where `<sep>`
  is a configurable single byte, default `:`).
- The signing algorithm (currently HS256 via the shared API secret).
- The required claim set (currently `iss`, `sub`, `exp`, `nbf`, `video.room`,
  `video.roomJoin` + the tenant audience).

### What does NOT require a bump

- The LiveKit Server upstream version. Per-release LiveKit-side claim changes
  that LiveKit considers backward-compatible MAY be picked up via dep
  upgrade; we only bump if the change is observable across implementations
  on the Vulos profile surface.
- Admin HTTP shape (`/admin/health`, etc.) — that is API surface, not wire
  protocol.
- Media-tuning defaults (codec, simulcast layers, top-N audio mix).

---

## Registered sub-protocols

| Identifier      | Status | Spec doc                  | Notes                          |
|-----------------|--------|---------------------------|--------------------------------|
| `VULOS-MEET/1`  | STABLE | [`TOKEN.md`](TOKEN.md)    | Initial release. HS256 JWT.    |

---

## Companion sub-protocols across Vulos OSS

For context, here are the other Vulos OSS sub-protocols. They are versioned
independently — a `VULOS-MEET/1` token has no relationship to a `VULOS-PEER/1`
envelope beyond sharing the "single monotonic major" pattern.

| Repo            | Sub-protocol     | Document                                                             |
|-----------------|------------------|----------------------------------------------------------------------|
| `vulos-relay`   | `VULOS-PEER/1`   | [`vulos-relay/spec/VERSIONS.md`](https://github.com/vul-os/vulos-relay/blob/main/spec/VERSIONS.md) |
| `vulos-relay`   | `VULOS-SYNC/1`   | (payload sub-protocol over PEER)                                     |
| `vulos-relay`   | `VULOS-STREAM/1` | (payload sub-protocol over PEER)                                     |
| `vulos-meet`    | `VULOS-MEET/1`   | this document + [`TOKEN.md`](TOKEN.md)                               |
