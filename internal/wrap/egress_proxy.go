// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// EgressProxy is the front-side reverse proxy that sits in front of
// livekit-server's Twirp egress surface (POST /twirp/livekit.Egress/<Method>).
//
// Why it exists: cloud's `internal/meetalloc/recording.go` POSTs Twirp
// egress requests (`StartRoomCompositeEgress`, `StopEgress`, `UpdateLayout`,
// `ListEgress`) at `{MEET_EGRESS_BASE_URL}/twirp/livekit.Egress/<Method>`.
// The cloud doc declares vulos-meet to be the sole LiveKit-talking surface;
// the only way that holds is if vulos-meet proxies these calls on the same
// listener it already runs the /rtc reverse proxy on.
//
// What it does (per request):
//
//  1. Extract the bearer token from the Authorization header (LiveKit's
//     Twirp clients send it as `Authorization: Bearer <jwt>`; that is what
//     vulos-cloud's HTTPEgressClient also sends).
//  2. Run Validator.Validate to confirm VULOS-MEET/1: signature, API key,
//     tenant binding, and the per-call `RoomRecord` video-grant invariant
//     (cloud mints egress tokens with `RoomRecord: true`).
//  3. Forward the request body BYTE-FOR-BYTE to livekit-server's Twirp
//     surface (Twirp carries protobuf or proto-JSON depending on content-
//     type; we never parse — we are an opaque pass-through).
//
// What it does NOT do:
//
//   - It does NOT touch /twirp/livekit.RoomService/* or any non-egress Twirp
//     path. Today the cloud only routes egress RPCs through here; the
//     wrap's own `rooms_livekit.go` talks to LiveKit's RoomService over its
//     own gRPC client. A future task can extend this proxy with sibling
//     auth-checked branches when the cloud begins to route RoomService
//     calls through vulos-meet too.
//
// Defense-in-depth note: livekit-server itself also verifies the JWT
// signature, so a bad token would be rejected upstream. We do the validation
// in front so:
//
//   - Cross-tenant tokens (room prefix says tenant A, audience says tenant B)
//     are dropped here, before they touch LiveKit. LiveKit doesn't know about
//     the VULOS-MEET/1 tenant invariant.
//   - The token-validation metric counter sees egress-path token outcomes,
//     not just signaling outcomes.
//   - We do not have to read the request body to decide auth.
type EgressProxy struct {
	validator    *Validator
	upstream     *url.URL
	client       *http.Client
	metrics      *Metrics // optional — nil disables egress metrics emission
	brokerSecret string   // gates the storage seam; empty ⇒ seam disabled
}

// SetMetrics attaches a metrics registry so every egress request feeds the
// vulos_meet_egress_requests_total counter. Passing nil clears it. Optional
// (every path tolerates a nil registry) so unit tests need no metrics target.
func (p *EgressProxy) SetMetrics(m *Metrics) { p.metrics = m }

// EgressTwirpPathPrefix is the path prefix vulos-meet auth-checks and
// proxies on the signal-gate listener. The cloud's HTTPEgressClient targets
// `<base>/twirp/livekit.Egress/<Method>`; we match by this prefix.
const EgressTwirpPathPrefix = "/twirp/livekit.Egress/"

// NewEgressProxy builds a proxy forwarding /twirp/livekit.Egress/* requests
// to the supervised livekit-server's Twirp listener. The upstream addr
// follows the same shape rules as SignalingAddr (":7880" / "0.0.0.0:7880" /
// "127.0.0.1:7880" / explicit host:port).
//
// We use a hand-rolled forward (one http.Client.Do call) rather than
// httputil.ReverseProxy because:
//
//   - Twirp request/response shapes are tiny (a few hundred bytes of
//     proto-JSON), so the streaming benefits of ReverseProxy don't apply.
//   - We need to read the body INTO MEMORY once (for the upstream POST) and
//     do nothing else with it — io.Copy from the request body into a
//     bytes.Buffer is more obvious than NewSingleHostReverseProxy's
//     director/transport dance.
//   - We never need WebSocket upgrade handling on this path (that is /rtc).
func NewEgressProxy(v *Validator, upstreamAddr string) (*EgressProxy, error) {
	if v == nil {
		return nil, errors.New("vulos-meet: egress proxy requires a validator")
	}
	if upstreamAddr == "" {
		return nil, errors.New("vulos-meet: egress proxy requires an upstream addr")
	}
	host := hostFromSignalingAddr(upstreamAddr)
	u, err := url.Parse("http://" + host)
	if err != nil {
		return nil, err
	}
	return &EgressProxy{
		validator:    v,
		upstream:     u,
		client:       &http.Client{},
		brokerSecret: strings.TrimSpace(os.Getenv(StorageBrokerSecretEnv)),
	}, nil
}

// Handler returns an http.Handler that ServeHTTPs every request as an
// auth-checked forward to the upstream Twirp surface. Callers mount it
// behind a path match on EgressTwirpPathPrefix.
func (p *EgressProxy) Handler() http.Handler {
	return http.HandlerFunc(p.serve)
}

// serve does the auth-check + opaque forward. Behaviour:
//
//   - Method != POST → 405 (Twirp uses POST exclusively for unary RPCs).
//   - No bearer token / malformed token → 401 (caller needs a new token).
//   - Token's tenant binding does not agree with its room prefix → 403
//     (caller has a token but isn't allowed to use it here).
//   - Token lacks the `RoomRecord` video grant → 403 (egress operations
//     require the recording grant; a regular meeting-join token must NOT
//     be replayable here).
//   - Forward error (upstream unreachable, etc.) → 502.
//
// Response bodies are opaque labels; we never leak token contents.
func (p *EgressProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.metrics.ObserveEgress(EgressOutcomeRejected)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := extractTokenFromRequest(r)
	if raw == "" {
		p.metrics.ObserveEgress(EgressOutcomeUnauthorized)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	vt, err := p.validator.Validate(raw)
	if err != nil {
		switch {
		case errors.Is(err, ErrTokenWrongTenant):
			p.metrics.ObserveEgress(EgressOutcomeForbidden)
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			// Map every shape-of-token failure to 401: malformed JWT, wrong
			// API key, bad signature/exp/nbf, missing grants, missing room,
			// missing tenant, malformed room. The caller needs a new token.
			p.metrics.ObserveEgress(EgressOutcomeUnauthorized)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}
	// VULOS-MEET/1 egress path invariant: the token MUST carry RoomRecord.
	// Cloud mints egress tokens with RoomRecord=true; a regular meeting-
	// join token (no RoomRecord) MUST NOT be replayable here, even on the
	// caller's own tenant.
	if vt.Grants == nil || vt.Grants.Video == nil || !vt.Grants.Video.RoomRecord {
		p.metrics.ObserveEgress(EgressOutcomeForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Unified-storage seam: when the OS gateway has injected a per-user object-
	// storage destination, redirect egress recording artifacts to the shared
	// bucket (under meet/) instead of whatever bucket the cloud named in the
	// request. Absent the seam, forward verbatim (legacy/self-host config).
	//
	// Broker-auth gate: the injected X-Vulos-Storage-* headers are honored ONLY
	// when the request also proves it came from the OS gateway, by presenting
	// the shared broker secret (X-Vulos-Storage-Broker-Auth, constant-time
	// compared against VULOS_STORAGE_BROKER_SECRET). A request that merely sets
	// the storage headers — without the secret, or when no secret is configured
	// — is treated as if no seam were present and forwarded verbatim, exactly as
	// before this gate existed. This stops an on-box caller from steering
	// recording output at an attacker-chosen bucket by spoofing the headers.
	seam, ok := StorageSeamFromHeader(r.Header)
	if ok && !storageBrokerAuthorized(p.brokerSecret, r.Header) {
		seam, ok = StorageSeam{}, false // gate closed — ignore the injected seam
	}
	// Endpoint safety: under an authenticated seam, refuse to ship the short-
	// lived credentials to a plaintext/public endpoint. Fail closed (400)
	// rather than fall back to the cloud-named bucket — storing user media in
	// an unintended place is worse than refusing the egress.
	if ok && !seam.endpointAllowed() {
		p.metrics.ObserveEgress(EgressOutcomeRejected)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p.forward(w, r, seam, ok)
}

// forward reads the inbound body into memory and POSTs it verbatim to the
// upstream Twirp surface. We do NOT parse the body — Twirp's wire format is
// protobuf (or proto-JSON when content-type negotiates it), and parsing
// here would either add a Twirp dep to vulos-meet OR risk content-type
// drift breaking the proxy. The body is small (Twirp egress requests are
// kilobytes at most), so the buffer cost is bounded.
func (p *EgressProxy) forward(w http.ResponseWriter, r *http.Request, seam StorageSeam, seamPresent bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap; Twirp egress payloads are kilobytes
	if err != nil {
		p.metrics.ObserveEgress(EgressOutcomeRejected)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// When the gateway-injected storage seam is present, rewrite Start*Egress
	// bodies so recording/egress artifacts land in the shared per-user bucket.
	// A decode/encode failure FAILS the request (400) rather than silently
	// forwarding the original body to the wrong bucket — storing user media in
	// the unintended place is worse than refusing the egress. Methods with no
	// storage output (Stop/List/Update, stream-only Starts) pass through.
	if seamPresent {
		method := strings.TrimPrefix(r.URL.Path, EgressTwirpPathPrefix)
		rewritten, changed, rerr := rewriteEgressBodyForSeam(method, r.Header.Get("Content-Type"), body, seam)
		if rerr != nil {
			p.metrics.ObserveEgress(EgressOutcomeRejected)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if changed {
			body = rewritten
		}
	}

	upstreamURL := p.upstream.String() + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		p.metrics.ObserveEgress(EgressOutcomeBadGateway)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	// Copy upstream-relevant headers. We DO NOT pass the Authorization
	// header on to livekit-server unchanged — LiveKit Server expects its
	// OWN bearer (which it then verifies against the API key/secret pair).
	// Since vulos-meet shares the same key/secret pair with LiveKit Server
	// (cfg.LiveKit.APIKey/APISecret), the inbound JWT is *also* a valid
	// LiveKit JWT, so we forward it as-is: the cloud's HTTPEgressClient is
	// already setting `Authorization: Bearer <jwt>`, and that same header
	// is what LiveKit's Twirp surface wants too.
	copyForwardableHeaders(req.Header, r.Header)

	resp, err := p.client.Do(req)
	if err != nil {
		p.metrics.ObserveEgress(EgressOutcomeBadGateway)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	p.metrics.ObserveEgress(EgressOutcomeOK)

	// Copy response headers back to the caller, then stream the body.
	copyForwardableHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Opaque pass-through. Twirp responses are proto-JSON or protobuf; we
	// do not interpret. Limit to a generous cap (Twirp egress responses
	// are tiny — EgressInfo is bytes — but we allow 4 MiB to be safe).
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20))
}

// copyForwardableHeaders copies headers from src to dst, skipping hop-by-
// hop headers that should not survive a proxy hop. We keep the list small
// and explicit so we don't accidentally strip something Twirp depends on
// (Twirp's only required headers are Content-Type and the bearer).
func copyForwardableHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade", "host",
			// Content-Length is recomputed by the transport from the body we
			// actually send; copying a stale value is wrong once the seam
			// rewrite changes the body size.
			"content-length":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
