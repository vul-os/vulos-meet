# Vulos Meet — Token Shape (`VULOS-MEET/1`)

This document specifies the Vulos Meet token: the JWT carried by a client to
the SFU when joining a meeting. The current sub-protocol version is
`VULOS-MEET/1`; see [`VERSIONS.md`](VERSIONS.md) for the version registry.

---

## 1. Background

Vulos Meet is a Vulos-flavoured layer on top of LiveKit Server. Tokens are
LiveKit-compatible JWTs — LiveKit Server validates them with its own
verifier on the signaling path. `vulos-meet`'s wrapper validates the **same
token** at its admission seam to enforce additional Vulos invariants
(tenant binding, room-prefix discipline). Both validators consume identical
bytes; if one accepts and the other rejects, that is a bug.

Tokens are **minted** by `vulos-cloud`'s control plane (`MEET-CP-01`) and
**only** by `vulos-cloud`. The `vulos-meet` server NEVER mints tokens — a
breach of an SFU box therefore cannot escalate to issuing meeting access.

---

## 2. Signing

- **Algorithm:** HS256.
- **Secret:** the shared `(api_key, api_secret)` pair. The same pair is
  configured on both the minter (`vulos-cloud`) and the verifier
  (`vulos-meet`). Rotation is out-of-band; both sides MUST be updated together.
- **Carrier:** the JWT goes in the LiveKit signaling request as a query
  parameter (`access_token`) or `Authorization: Bearer …` header, per LiveKit
  Server's conventions.

---

## 3. Claims

| Claim         | JWT slot                | Required | Description |
|---------------|-------------------------|----------|-------------|
| `iss`         | `iss`                   | yes      | Equals the LiveKit `api_key`. |
| `sub`         | `sub`                   | yes      | Vulos user ID. Surfaces as `identity` in LiveKit. |
| `exp`         | `exp`                   | yes      | Expiration (Unix seconds). MUST be ≤ 6 h after `iat`. |
| `nbf`         | `nbf`                   | yes      | Not-before (Unix seconds). MUST be ≤ now. |
| `name`        | `name` (top-level grant)| yes      | **Tenant audience.** The Vulos tenant ID this token authorizes. |
| `video.room`  | `video.room`            | yes      | Full room ID `<tenant><sep><rest>` where `<sep>` is the configured tenant separator (default `:`). |
| `video.roomJoin` | `video.roomJoin`     | yes      | MUST be `true`. |

The remaining LiveKit `video.*` grants (`canPublish`, `canSubscribe`,
`canPublishData`, `hidden`, `recorder`) are passed through verbatim. The
minter sets them per the per-user role; the validator does not constrain them
beyond what LiveKit itself enforces.

### 3.1 The tenant-binding invariant

The validator MUST reject the token unless **both** of these hold:

1. `video.room` parses as `<tenant><sep><rest>` with non-empty `tenant` and
   non-empty `rest` (per [`internal/wrap/tenant.go`](../internal/wrap/tenant.go)).
2. The `tenant` parsed from `video.room` is byte-equal to the `name` claim.

If either fails, the validator returns `ErrTokenRoomMalformed` or
`ErrTokenWrongTenant` and the SFU MUST NOT serve the join request.

This invariant is the only thing standing between "two Vulos tenants share an
SFU box" and "tenant A reads tenant B's traffic". It is mandatory.

---

## 4. Validation rules (normative)

A `VULOS-MEET/1` validator MUST perform, in order:

1. **JWT parse.** Reject on malformed JWT (`ErrTokenMalformed`).
2. **API-key match.** Reject if `iss` ≠ the configured `api_key`
   (`ErrTokenWrongAPIKey`).
3. **Signature + time.** Verify HS256 against the configured `api_secret`,
   and check `exp` ≥ now and `nbf` ≤ now. Reject on failure
   (`ErrTokenSignatureBad`). This step also rejects any token signed with a
   secret the validator does not know.
4. **Grants present.** Reject if `video` grant is missing or has no `room`
   (`ErrTokenMissingGrants` / `ErrTokenMissingRoom`).
5. **Room well-formed.** Parse `video.room` against the tenant gate; reject
   if the prefix is missing or the tenant ID is invalid
   (`ErrTokenRoomMalformed`).
6. **Tenant audience present and matches.** Reject if `name` is empty
   (`ErrTokenMissingTenant`) or if `name` ≠ the tenant parsed from the room
   (`ErrTokenWrongTenant`).

On any rejection the validator MUST return only the typed sentinel error —
the token contents (signed or unsigned) MUST NOT appear in the response body.

---

## 5. Example token (decoded)

```json
{
  "iss": "APIabcdef0123456789",
  "sub": "u_2YgKp9xVz3qN",
  "name": "acme",
  "nbf": 1748000000,
  "exp": 1748021600,
  "video": {
    "room": "acme:standup-2026-05-24",
    "roomJoin": true,
    "canPublish": true,
    "canSubscribe": true
  }
}
```

Here the tenant is `acme`, the room is `standup-2026-05-24`, the separator is
the default `:`, and the binding holds (`name` == `acme` == prefix of
`video.room`).

---

## 6. Forbidden patterns

- **A token without `name`.** The validator has no audience to bind against
  and MUST reject.
- **A token whose `name` is not the room prefix.** A leaked token from tenant
  A replayed against a room minted for tenant B trips this check.
- **A token whose `video.room` does not contain the separator byte.** The
  prefix invariant cannot be checked; reject.
- **A `name` containing the separator byte.** The split would be ambiguous;
  reject at validation AND at mint.
- **A long-lived token.** Validators MAY (and `vulos-cloud`'s minter SHOULD)
  cap `exp - iat` at 6 hours so a leaked token's blast radius is small.
