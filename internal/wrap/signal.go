// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// SignalGate is the front-side WebSocket reverse proxy that sits in front of
// livekit-server's /rtc signaling endpoint. Its job:
//
//  1. Read the access token off the WebSocket-upgrade request (LiveKit's
//     clients pass it as the `access_token` query parameter; some send it
//     as the `Authorization: Bearer …` header — we accept both).
//  2. Run Validator.Validate to confirm the VULOS-MEET/1 profile:
//     well-formed JWT, correct API key, signature OK, room prefix matches
//     the tenant audience.
//  3. Only after a successful validate, forward the upgrade to livekit-
//     server. On failure, return 401/403 with NO token contents in the
//     response body.
//
// This is defense-in-depth on top of LiveKit's own signature check: it stops
// a token leaked from tenant A from ever reaching tenant B's room on this
// box. LiveKit verifies the JWT signature; we additionally verify the
// VULOS-MEET/1 binding (tenant in name claim == tenant prefix on room).
type SignalGate struct {
	validator *Validator
	upstream  *url.URL
	proxy     *httputil.ReverseProxy
}

// NewSignalGate constructs a gate forwarding to the given upstream signaling
// addr (e.g. ":7880" or "127.0.0.1:7880"). The reverse proxy uses HTTP/1.1
// and lets net/http's built-in upgrade machinery handle the WebSocket switch.
func NewSignalGate(v *Validator, upstreamAddr string) (*SignalGate, error) {
	if v == nil {
		return nil, errors.New("vulos-meet: signal gate requires a validator")
	}
	if upstreamAddr == "" {
		return nil, errors.New("vulos-meet: signal gate requires an upstream addr")
	}
	host := hostFromSignalingAddr(upstreamAddr)
	u, err := url.Parse("http://" + host)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	// Don't leak our gate's identity / internals into the upstream.
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = u.Host
		// Strip any header that might confuse the upstream.
		req.Header.Del("Authorization")
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		// Upstream-side errors come out as 502 (no internal detail leaks).
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	return &SignalGate{validator: v, upstream: u, proxy: rp}, nil
}

// Handler returns an http.Handler that mounts the signaling proxy AT THE
// ROOT and accepts a sibling handler for non-signaling routes (currently:
// the egress webhook receiver from MEET-RECORDING-DRIVER-05). The non-
// signaling handler is reached for any path other than /rtc.
//
// Layout:
//
//	/rtc(?token=…)     → token-validated, proxied to upstream LiveKit
//	/<anything-else>   → siblingHandler (egress webhook, /healthz, etc.)
func (g *SignalGate) Handler(siblingHandler http.Handler) http.Handler {
	if siblingHandler == nil {
		siblingHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rtc", g.handleRTC)
	mux.Handle("/", siblingHandler)
	return mux
}

// handleRTC is the WebSocket-upgrade entry point. It checks token validity
// BEFORE forwarding to livekit-server, so a malformed or cross-tenant token
// never reaches the SFU.
func (g *SignalGate) handleRTC(w http.ResponseWriter, r *http.Request) {
	raw := extractTokenFromRequest(r)
	if raw == "" {
		// No token at all — fail closed.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	vt, err := g.validator.Validate(raw)
	if err != nil {
		// Map sentinels to typed status codes. Everything that smells like
		// "token shape is wrong" returns 401 (caller needs a new token);
		// "tenant binding doesn't agree" returns 403 (caller has a token
		// but isn't allowed to use it here). The response body NEVER
		// includes any token contents — only an opaque label.
		switch {
		case errors.Is(err, ErrTokenMalformed),
			errors.Is(err, ErrTokenWrongAPIKey),
			errors.Is(err, ErrTokenSignatureBad),
			errors.Is(err, ErrTokenMissingGrants),
			errors.Is(err, ErrTokenMissingRoom),
			errors.Is(err, ErrTokenMissingTenant),
			errors.Is(err, ErrTokenRoomMalformed):
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case errors.Is(err, ErrTokenWrongTenant):
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}
	_ = vt // valid; the validated tenant + room are not needed by the proxy itself — livekit-server will re-read them from the same JWT.
	g.proxy.ServeHTTP(w, r)
}

// extractTokenFromRequest pulls the VULOS-MEET/1 JWT out of either the
// `access_token` query parameter (the LiveKit JS / Go SDK default) or the
// Authorization: Bearer header (used by some custom integrations). Both are
// in spec; either is accepted.
func extractTokenFromRequest(r *http.Request) string {
	if q := r.URL.Query().Get("access_token"); q != "" {
		return q
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}
