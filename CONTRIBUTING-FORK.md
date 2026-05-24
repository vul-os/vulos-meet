# Tracking upstream LiveKit in `vulos-meet`

This document is for `vulos-meet` maintainers. End-users do not need to read
it. It explains how we track upstream
[`github.com/livekit/livekit-server`](https://github.com/livekit/livekit-server)
releases — what "tracking upstream" actually means for a project that
**supervises** the upstream binary instead of vendoring it.

It also documents the `livekit/protocol` Go-module bump path (a different
release cadence and a different risk surface — token validation lives there)
and the test matrix we ship.

---

## TL;DR

| Thing we depend on | How we track it | Where it lives |
|---|---|---|
| **`livekit-server` binary** | Bump a pinned version constant, smoke + token-validation tests | Operator deploy + `spec/UPSTREAM.md`-style pin |
| **`livekit/protocol` Go module** | `go get` + `go mod tidy`, run `go test ./...` | `go.mod` |
| **Vulos wrapper layer** | Lives in our tree; no merge from upstream | `internal/wrap/`, `cmd/vulos-meet` |

We do NOT do a `git merge upstream/main` (the way `vulos-mail` merges from
`mox`). The reason: we are not a fork of `livekit-server`. We are a small Go
program that *runs* `livekit-server` as a child process and adds the Vulos
auth + multi-tenancy seams in front of it. So "tracking upstream" is:

1. Bump the pinned **binary** version we test against, run our token-validation
   tests against it, then ship.
2. Bump the **`livekit/protocol`** Go-module pin when its release notes touch
   the auth/grants surface or have a CVE.

These two paths are independent.

---

## 1. Binary-bump workflow (`livekit-server` releases)

LiveKit ships standalone binaries at
<https://github.com/livekit/livekit-server/releases>. We supervise that
binary; we do not import it.

When a new `livekit-server` release lands:

1. **Read the release notes.** Look specifically for:
   - Auth / JWT / claim-shape changes (would break `internal/wrap/auth.go`)
   - Room-service gRPC API changes (would break
     `internal/wrap/rooms_livekit.go`)
   - Egress webhook payload changes (would break
     `internal/wrap/recording.go`)
   - Redis cluster-discovery key changes (would break
     `internal/wrap/cluster.go`)
   - CVEs (always pull these in promptly; see §3 below)
2. **Download the new binary** on a test box:
   ```
   curl -fsSL https://github.com/livekit/livekit-server/releases/download/vX.Y.Z/livekit_X.Y.Z_linux_amd64.tar.gz \
     | tar -xz -C /usr/local/bin livekit-server
   ```
3. **Run the wrap suite against it.** From the repo root:
   ```
   VULOS_MEET_LIVEKIT_BIN=/usr/local/bin/livekit-server \
     go test ./...
   ```
   The integration tests that need a real binary (the ones guarded by
   `livekitIntegration` — see §4) will execute. Tests that do NOT see a
   binary on `$PATH` skip cleanly.
4. **Run our token-validation suite explicitly** — this is the surface most
   sensitive to upstream auth changes:
   ```
   go test ./internal/wrap -run 'TestValidator' -v
   ```
   If any of these fail under the new binary, do NOT ship the bump. File an
   upstream issue, hold the rollout, and investigate.
5. **Update the pinned binary version.** The pin lives in:
   - operator-facing docs (README "Running standalone")
   - the `spec/`-side compatibility table (when present)
   - any CI/CD that fetches the binary
   - the deploy manifest (systemd unit, container, etc.) — Vulos uses
     `VULOS_MEET_LIVEKIT_BIN` to swap it without code changes
6. **Smoke-test the egress webhook path** if egress is on the change list.
   Use `internal/wrap/recording_test.go`'s sample payloads as the contract.
7. **Commit the pin bump** as a separate commit titled
   `chore: track livekit-server vX.Y.Z`. Do NOT mix it with wrap-layer changes.

---

## 2. `livekit/protocol` Go-module bump

`github.com/livekit/protocol` is the small Apache-2.0 module that carries the
JWT/claim types. **Token validation lives here**, not in `livekit-server`.
A change to the claim shape is a change to our admission seam, so this bump
needs the most attention.

When `livekit/protocol` releases:

1. **Read its release notes.** Anything mentioning `auth`, `ClaimGrants`,
   `VideoGrant`, `ParseAPIToken`, `AccessToken`, or `Verify` is in scope.
2. **Bump and tidy:**
   ```
   go get github.com/livekit/protocol@vA.B.C
   go mod tidy
   ```
3. **Re-run the validator tests:**
   ```
   go test ./internal/wrap -run 'TestValidator' -v
   ```
   Our tests deliberately exercise every sentinel error path
   (`ErrTokenMalformed`, `ErrTokenSignatureBad`, `ErrTokenWrongAPIKey`,
   `ErrTokenMissingGrants`, `ErrTokenMissingRoom`, `ErrTokenWrongTenant`,
   `ErrTokenMissingTenant`, `ErrTokenRoomMalformed`) so a change to the
   parse/verify path will fail loudly.
4. **Re-render the spec doc** if any field name we cite in
   `spec/TOKEN.md` moved.
5. **Bump notes:** if the protocol upstream renames `ClaimGrants.Name` (our
   tenant-audience carrier — see `extractTenantAudience` in `auth.go`), that
   is a **`VULOS-MEET/1` → `VULOS-MEET/2`** event. See `spec/VERSIONS.md`
   "When to bump the major version".

---

## 3. CVE / security-release fast path

Both surfaces have a fast path:

- **livekit-server CVE.** Bump the binary pin same-day. Run §1 steps 3 + 4.
  If our wrap-layer tests pass, ship. If they fail, hold the binary at the
  previous version and patch the wrapper in parallel.
- **livekit/protocol CVE.** Bump the Go module same-day. Run §2 step 3. Cut
  a release.

Never skip the validator-test step on a CVE-driven bump. The whole point of
keeping the validator code path byte-identical to LiveKit's is so we don't
develop a second-implementation drift bug — and a CVE bump is exactly when
that drift would become an outage.

---

## 4. Test matrix

We support **one LiveKit minor version we currently ship + one prior**.

Concretely, if we ship against `livekit-server v1.7.x`, our test matrix is:

| Track | LiveKit binary | `livekit/protocol` | Notes |
|---|---|---|---|
| Current | latest patch of `v1.7.x` | matching minor | Default for CI + release |
| Prior   | latest patch of `v1.6.x` | matching minor | Tested on bump, allowed to lag |

Integration tests that exercise the real binary are guarded by a build tag and
an env hint:

- **Build tag:** `//go:build livekitintegration`
- **Env hint:** `VULOS_MEET_LIVEKIT_BIN` — if unset and `livekit-server` is
  not on `$PATH`, the integration test calls `t.Skip(...)`.

Unit tests (everything in `internal/wrap` without the build tag) run in CI
unconditionally and do NOT depend on the binary being present.

CI variants we run on every PR:

1. `go build ./...` (no tags)
2. `CGO_ENABLED=0 go build ./...` (pure-Go invariant — see top of
   `tasks.md`)
3. `go vet ./...`
4. `go test ./...` (no tags) — runs the wrap-layer unit tests
5. `gofmt -l .` (must be empty)

CI variants we run on bump PRs (binary OR protocol):

6. `go test -tags livekitintegration ./...` (against the candidate binary)

---

## 5. vulos-meet is the sole LiveKit-talking surface

**Invariant (FROZEN):** `vulos-meet` is the only Vulos process that talks
directly to the supervised `livekit-server` child. The cloud control plane
(`vulos-cloud`) MUST NOT bypass it.

Concretely, that means cloud-side env vars target the vulos-meet signal-gate
listener (`SignalGateAddr`), NOT `livekit-server` directly:

- `MEET_EGRESS_BASE_URL` — cloud's `internal/meetalloc/recording.go`
  HTTPEgressClient sends `POST {base}/twirp/livekit.Egress/<Method>` here.
  MUST resolve to `vulos-meet`'s signal-gate (e.g. `https://meet.vulos.org`),
  NOT `livekit-server:7880`.
- Meeting-join WebSocket URLs handed to clients also target vulos-meet
  (`wss://meet.vulos.org/rtc`), again NOT LiveKit directly.

vulos-meet's signal-gate proxies two paths through the tenant gate:

| Path                                | What it does                                  | Token grant required |
|-------------------------------------|-----------------------------------------------|----------------------|
| `/rtc` (WebSocket upgrade)          | `Validator.Validate` → forward to LiveKit /rtc | `RoomJoin: true`     |
| `/twirp/livekit.Egress/*` (POST)    | `Validator.Validate` + `RoomRecord: true` invariant → forward verbatim to LiveKit Twirp | `RoomRecord: true`   |

Other Twirp namespaces (e.g. `/twirp/livekit.RoomService/*`) are NOT proxied
today — the cloud uses its own gRPC RoomService client for those calls, and
vulos-meet's own admin surface (`/admin/tenants/{tenant}/rooms/*`) is the
documented external seam. If a future task moves RoomService through
vulos-meet too, extend `internal/wrap/egress_proxy.go` with a sibling
auth-checked handler (the egress proxy is the template).

**Why this matters.** If the cloud points `MEET_EGRESS_BASE_URL` directly
at `livekit-server`, the tenant gate, the metric counters, and the
lifecycle hook all silently no-op for egress calls — LiveKit's own JWT
verification still accepts the token, but the VULOS-MEET/1 tenant-binding
rule is not enforced. That is exactly the configuration this section
exists to prevent.

The relevant proxy code is `internal/wrap/egress_proxy.go`; tests in
`internal/wrap/egress_proxy_test.go` lock the behaviour.

---

## 6. What this file is NOT

- It is **not** a fork-merge runbook. We don't fork `livekit-server`.
- It is **not** a place to record vendoring policy. We don't vendor.
- It is **not** a Vulos-wide upstream-tracking guide. Sibling repos (`vulos`,
  `vulos-mail`, `vulos-relay`, `vulos-office`) have their own upstream
  postures; cross-repo conventions live in those repos.

If we ever stop supervising and start embedding `livekit-server` as a Go
module, this file gets rewritten — the bump path would then look much more
like `vulos-mail`'s "merge from upstream Mox" runbook.

---

## See also

- [`README.md`](README.md) §Architecture: vendor or supervise?
- [`spec/VERSIONS.md`](spec/VERSIONS.md) — when to bump `VULOS-MEET/N`
- [`spec/TOKEN.md`](spec/TOKEN.md) — the wire-level token shape we validate
