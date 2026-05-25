# vulos-meet — Security Testing

This document describes the adversarial (pentest-style) security test suite for
`vulos-meet`, the Vulos video-meeting SFU that wraps LiveKit Server.

`vulos-meet` is the **sole public-facing admission seam** in front of
livekit-server. It does **not** mint tokens (the cloud control plane,
MEET-CP-01, is the only issuer); it only **validates** them and enforces the
per-tenant room-namespace binding before any request reaches LiveKit. The tests
here play the attacker against every one of those boundaries.

## Philosophy

Each test in `internal/wrap/pentest_security_test.go` **attempts a concrete
attack** and **asserts that it is blocked**. A test failure prefixed
`LIVE VULN:` means a real, exploitable hole was found — not a flaky assertion.
The attacker path is byte-identical to production: the tests drive the real
`Validator`, `SignalGate`, `EgressProxy`, `AdminServer`, `Tenant` gate, and the
real LiveKit config renderer, reusing the existing wrap test helpers
(`mintToken`, `mintEgressToken`, `newGateForTest`, `newTestAdminServer`, the
`fakeLiveKitSignal` / `fakeLiveKitTwirp` upstream stand-ins).

## How to run

```sh
cd vulos-meet

# the full gate (build + vet + every test)
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...

# just the adversarial suite, verbose
CGO_ENABLED=0 go test ./internal/wrap/ -run 'TestPentest' -v
```

All tests are pure Go (CGO_ENABLED=0) and need no network, no Redis, and no
livekit-server binary — the LiveKit upstream is a local `httptest` stand-in.

## Coverage matrix

29 adversarial tests across 6 attack classes:

### A. Token validation (forging, signature games, time)
The validator delegates JWT signature/exp/nbf verification to
`livekit/protocol` (go-jose under the hood) so the verification path is
byte-identical to LiveKit's own — there is no second implementation to drift.
These tests confirm that path actually rejects forged tokens.

| Test | Attack attempted | Asserted block |
|---|---|---|
| `AlgNoneForgedTokenRejected` | `alg:none` downgrade — unsigned token, attacker-chosen claims (any tenant/room, RoomRecord) in 2-seg, empty-sig, and garbage-sig shapes | rejected (no unsigned acceptance) |
| `SignatureStrippedTokenRejected` | take a legit token, strip / truncate / bit-flip / drop the signature | rejected |
| `TamperedPayloadKeepsOldSignatureRejected` | rewrite payload (swap tenant, add RoomRecord) keeping the original MAC | rejected (MAC covers payload) |
| `AlgorithmConfusionRS256Rejected` | RS256 header to trick HMAC verifier into key-type confusion | rejected |
| `WrongSecretForgedTokenRejected` | well-formed HS256 token signed with attacker's own secret | `ErrTokenSignatureBad` |
| `ExpiredTokenRejected` | `exp` 1h in the past (beyond go-jose's 1-min leeway) | `ErrTokenSignatureBad` |
| `NotYetValidTokenRejected` | `nbf` 1h in the future (beyond leeway) | `ErrTokenSignatureBad` |
| `MissingTokenRejected` | empty / whitespace / `..` / `a.b` garbage | rejected |
| `VulosMeetNeverMintsTokens` | architectural invariant: validator only verifies, never issues | no accept-on-presentation |

### B. Tenant isolation (cross-tenant, prefix confusion, charset)
Every room id is `<tenant><sep><rest>`; a token's `aud`/Name claim must
**byte-equal** the room-id tenant prefix.

| Test | Attack attempted | Asserted block |
|---|---|---|
| `CrossTenantRoomReplayRejected` | token minted for `evil` pointed at `victim:boardroom` | `ErrTokenWrongTenant` |
| `PrefixConfusionRejected` | `aud=acme` vs room `acmecorp:secret` (shared string prefix) | `ErrTokenWrongTenant` (whole-segment compare, no `HasPrefix` sloppiness) |
| `EmptyTenantPrefixRejected` | room `:standup` (empty tenant segment) | `ErrTokenRoomMalformed` |
| `TenantCharsetIsStrict` | separator byte, `../`, NUL, whitespace, dot/DNS, `*`, shell metachars, non-ASCII | all rejected; valid ids still pass |
| `FilterRoomsNeverLeaksAcrossTenants` | list-rooms filter with lookalike prefixes, bare prefix, suffix-not-prefix, other tenant | only the caller's own rooms survive |

### C. Egress / recording auth
The egress proxy gates `POST /twirp/livekit.Egress/*` and additionally requires
the per-call `RoomRecord` video grant.

| Test | Attack attempted | Asserted block |
|---|---|---|
| `EgressJoinTokenCannotTriggerRecording` | a regular meeting-JOIN token (no RoomRecord), own tenant, every egress method | 403; upstream untouched |
| `EgressCrossTenantRecordingRejected` | RoomRecord token for `evil` recording `victim`'s room | 403; upstream untouched |
| `EgressRejectsNonPostMethods` | GET/PUT/DELETE/PATCH/HEAD/OPTIONS on the egress path (even with a valid RoomRecord token) | 405; upstream untouched |
| `NonEgressTwirpNotProxied` | tunnel RoomService/Ingress Twirp RPCs and the bare egress prefix through the egress gate | routed to sibling / 307-redirected; never forwarded to LiveKit |
| `EgressForgedTokenRejected` | `alg:none` forged token claiming RoomRecord | 401; upstream untouched; body does not leak token |

### D. Admin auth
`/admin/*` (except the unauthenticated georoute health probe) requires a bearer
admin token compared in constant time; admin + metrics are not on the public
signal-gate.

| Test | Attack attempted | Asserted block |
|---|---|---|
| `AdminRequiresToken` | list/delete admin routes with no token | 401; room store unmutated |
| `AdminWrongTokenRejected` | same-length wrong, shorter, longer, Basic scheme, raw token, empty bearer, last-char near-miss | not 200 |
| `AdminTokenCompareIsConstantTime` | exercises `constantTimeEqualString` across equal/first-byte-diff/last-byte-diff/length-diff/empty | correct boolean for all (no early-exit leak) |
| `AdminAndMetricsNotOnSignalGate` | request `/admin/*` and `/metrics` on the public signal-gate listener | falls through to the generic sibling (404), never a privileged 200 |
| `MetricsDefaultAddressIsLoopback` | deploy posture: metrics default bind | `127.0.0.1` (loopback), not the wildcard |

### E. Signal gate (block before WebSocket upgrade)
The gate validates the token **before** forwarding the `/rtc` upgrade to
livekit-server.

| Test | Attack attempted | Asserted block |
|---|---|---|
| `SignalGateBlocksForgedBeforeUpstream` | `alg:none` forged token at `/rtc` | 401; upstream `/rtc` never reached; body does not leak token |
| `SignalGateBlocksCrossTenantBeforeUpstream` | valid-signature cross-tenant token at `/rtc` | 403; upstream never reached (catches what LiveKit's own sig check would not) |
| `SignalGateExpiredTokenBlockedBeforeUpstream` | expired token at `/rtc` | 401; upstream never reached |

### F. Participant limits (config-level)
LiveKit treats `room.max_participants: 0` as **unlimited** — an unbounded room
is a DoS / quota-evasion vector, so the renderer must always emit a finite cap.

| Test | Attack attempted | Asserted block |
|---|---|---|
| `ParticipantCapIsFiniteAndServerEnforced` | default render, operator-set `0` (would mean unlimited), and configured `250` | finite cap always; `0` floored to the default; configured value honoured |
| `NegativeParticipantCapRejectedAtConfig` | negative cap in YAML | config validation error |

## Findings

**No live vulnerabilities found.** Every attack above is blocked by the
existing code; no fixes were required. Two boundary behaviors were investigated
and confirmed safe rather than vulnerable:

1. **`alg:none` downgrade** — go-jose's `jwt.ParseSigned` will *parse* a
   3-segment `alg:none` token, but the subsequent
   `verifier.Verify` step (`token.Claims([]byte secret, ...)`) fails the
   cryptographic-primitive check because an HMAC key cannot verify an unsecured
   JWS. The full validator therefore returns `ErrTokenSignatureBad`. Locked in
   by `AlgNoneForgedTokenRejected` and the egress/signal-gate variants so a
   future go-jose bump cannot silently regress it.

2. **Egress prefix without trailing slash** (`/twirp/livekit.Egress`) — Go's
   `http.ServeMux` issues a **307 redirect** to the canonical
   `/twirp/livekit.Egress/` subtree; the un-canonicalised request never reaches
   the LiveKit upstream. Confirmed by `NonEgressTwirpNotProxied`.

## Maintenance notes

- The 1-minute go-jose validation **leeway** (`jwt.DefaultLeeway`) means
  exp/nbf tests must place the boundary well outside ±1 minute. The suite uses
  ±1–2 hours.
- `signRawHS256` (suite-local helper) signs JWTs directly with the test HS256
  secret, bypassing `livekit/protocol`'s `SetValidFor` floor so a test can set
  arbitrary `exp`/`nbf`. It mirrors the technique already used by
  `TestValidator_ExpiredTokenRejected` in `auth_test.go`.
- The metrics-default and admin-token constants in the tests
  (`127.0.0.1:7882`, `MEET_ADMIN_TOKEN`) are kept in sync with
  `cmd/vulos-meet/main.go` and `internal/wrap/admin.go`; if those change, the
  corresponding pentest assertions must change with them.
