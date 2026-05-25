// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
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

	// Room-cap enforcement (optional). When roomCap.max > 0 and roomCap.lister
	// is non-nil, a join that would create a NEW room (a room id not currently
	// active) is REJECTED once the box-wide active-room count has reached the
	// ceiling. Joins to an already-active room are never affected, and the cap
	// is never consulted at all when max <= 0 (explicitly unbounded). See
	// SetRoomCap and enforceRoomCap.
	roomCap roomCapEnforcer
}

// RoomLister is the narrow read-only surface the signal-gate needs to count
// active rooms for the MaxRooms ceiling. The production *LiveKitRoomService
// (and the test MemoryRoomService) both satisfy it via the RoomService
// interface; we take only the list capability here so the gate can never be
// coaxed into mutating room state.
type RoomLister interface {
	ListRoomIDs(ctx context.Context) ([]string, error)
}

// roomCapEnforcer bundles the per-box concurrent-room ceiling, the lister used
// to observe the live count, an optional metrics sink, and the call timeout.
type roomCapEnforcer struct {
	max     int
	lister  RoomLister
	metrics *Metrics
	timeout time.Duration
}

// roomCapListTimeout caps how long the gate will wait on the RoomService list
// when deciding admission. Kept short: this sits on the connection-setup hot
// path, and a stalled list must not hang the join indefinitely. The fail-open
// vs fail-closed choice on timeout is documented in enforceRoomCap.
const roomCapListTimeout = 3 * time.Second

// SetRoomCap wires concurrent-room-ceiling enforcement into the gate. lister is
// the read-only room counter (the same RoomService the admin surface uses);
// max is the ceiling (a value <= 0 means "unbounded" — enforcement is a no-op);
// metrics is optional. Call once at startup, before serving. When configured,
// handleRTC rejects a join that would create a NEW room once the box already
// holds `max` active rooms; joins to an existing room are unaffected.
func (g *SignalGate) SetRoomCap(lister RoomLister, max int, metrics *Metrics) {
	g.roomCap = roomCapEnforcer{
		max:     max,
		lister:  lister,
		metrics: metrics,
		timeout: roomCapListTimeout,
	}
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
// signaling handler is reached for any path other than /rtc and the
// egress-Twirp prefix.
//
// Layout:
//
//	/rtc(?token=…)                      → token-validated, proxied to LiveKit /rtc
//	/twirp/livekit.Egress/<Method>      → token-validated (RoomRecord), proxied to LiveKit Twirp
//	/<anything-else>                    → siblingHandler (egress webhook, /healthz, etc.)
//
// The egress-Twirp branch is only mounted when egressProxy != nil. Callers
// that do not want the egress surface (early bring-up / test scaffolding)
// can pass nil and only /rtc + sibling are wired.
func (g *SignalGate) Handler(siblingHandler http.Handler, egressProxy *EgressProxy) http.Handler {
	if siblingHandler == nil {
		siblingHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rtc", g.handleRTC)
	if egressProxy != nil {
		// http.ServeMux pattern: trailing slash makes this a subtree match,
		// so /twirp/livekit.Egress/StartRoomCompositeEgress, /StopEgress,
		// /UpdateLayout, /ListEgress all dispatch into the egress proxy.
		mux.Handle(EgressTwirpPathPrefix, egressProxy.Handler())
	}
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
	// Concurrent-room-ceiling enforcement. The token is valid; now decide
	// admission against the box-wide MaxRooms cap. This is the reliable
	// enforcement point: LiveKit's config has auto_create:true, so a join to a
	// not-yet-existing room would otherwise create it on demand with no cap.
	// A join to an ALREADY-active room never trips the cap (it adds no rooms);
	// MaxParticipants still bounds that room server-side inside LiveKit.
	if !g.enforceRoomCap(r.Context(), vt.FullRoom) {
		http.Error(w, "room capacity reached", http.StatusServiceUnavailable)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// enforceRoomCap returns true if the join to fullRoom may proceed and false if
// it must be rejected because the box is at its concurrent-room ceiling and
// this join would create a NEW room.
//
// Rules:
//   - No cap configured (lister nil) or max <= 0 → always allow (unbounded).
//   - fullRoom is already in the active set → always allow (no new room).
//   - active count < max → allow.
//   - active count >= max AND fullRoom is new → REJECT.
//
// Failure mode: if the list call errors or times out we FAIL CLOSED only when
// we cannot prove the room already exists — i.e. we reject a would-be NEW room
// we cannot vet. We deliberately do NOT fail closed for a list error alone if
// it would also reject existing-room rejoins: we cannot tell new from existing
// without the list, so a hard list failure rejects the join. This keeps the
// DoS ceiling intact (the safe direction is to reject when uncertain) at the
// cost of refusing joins while the RoomService is unreachable; the admin
// breaker + metrics make that condition observable. See the honest-unknowns
// note in the change report re: the count-check↔create race.
func (g *SignalGate) enforceRoomCap(parent context.Context, fullRoom string) bool {
	rc := g.roomCap
	if rc.lister == nil || rc.max <= 0 {
		return true // unbounded / not configured
	}
	timeout := rc.timeout
	if timeout <= 0 {
		timeout = roomCapListTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	active, err := rc.lister.ListRoomIDs(ctx)
	if err != nil {
		// Cannot establish whether this is a new room; reject rather than let
		// an unbounded number of rooms through while the lister is down.
		rc.metrics.ObserveRoomAdmission(RoomAdmissionListError)
		return false
	}
	already := false
	for _, id := range active {
		if id == fullRoom {
			already = true
			break
		}
	}
	if already {
		rc.metrics.ObserveRoomAdmission(RoomAdmissionExisting)
		return true
	}
	if len(active) >= rc.max {
		rc.metrics.ObserveRoomAdmission(RoomAdmissionRejectedCapacity)
		return false
	}
	rc.metrics.ObserveRoomAdmission(RoomAdmissionNewRoom)
	return true
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
