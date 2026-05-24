// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
)

// LiveKitRoomServiceConfig is the construction-time configuration for the
// real LiveKit RoomService client. It is a separate struct (not a borrow of
// Config) so this layer can be unit-tested without standing up a full Config.
type LiveKitRoomServiceConfig struct {
	// SignalingAddr is the LiveKit HTTP/signaling listen address, e.g.
	// ":7880" or "0.0.0.0:7880". RoomService is served by LiveKit on the
	// same port as the signaling WebSocket.
	SignalingAddr string

	// APIKey + APISecret are the shared (key, secret) pair used to mint
	// short-lived JWTs that the RoomService client sends as Authorization
	// Bearer headers. SAME pair as the validator — LiveKit accepts only one.
	APIKey    string
	APISecret string

	// CallTimeout caps how long any single RoomService RPC can wait before
	// the circuit-breaker opens. Defaults to 5s; admin endpoints already
	// have their own 5s context timeout, so this is the second-line guard.
	CallTimeout time.Duration

	// BreakerCooldown is how long the breaker stays open after a failure
	// before allowing a half-open probe through. Defaults to 30s.
	BreakerCooldown time.Duration

	// BreakerThreshold is the number of consecutive failures required to
	// open the breaker. Defaults to 3.
	BreakerThreshold int

	// HTTPScheme is "http" by default. Set "https" only if the upstream
	// livekit-server is behind a TLS-terminating sidecar — the default
	// supervise-the-child mode talks plain HTTP on loopback.
	HTTPScheme string
}

// LiveKitRoomService implements RoomService against a real livekit-server
// child via the Twirp HTTP/Protobuf RoomService client.
//
// Why this exists: until MEET-ROOMSVC-02 the admin surface was wired to an
// in-memory stand-in (MemoryRoomService). That was fine for the bootstrap
// of MEET-CORE-01 but doesn't actually delete rooms in LiveKit. This wrapper
// is the thin shim that does.
//
// The shim adds two operational properties LiveKit's own client does NOT
// give us:
//
//  1. A short-lived signed-token mint per call (RoomService requires
//     RoomList / RoomCreate / RoomAdmin grants in the JWT — we don't reuse
//     a long-lived admin token).
//  2. A circuit breaker so an admin call cannot hang on a stalled or dead
//     child process; once a small number of consecutive failures occur we
//     fail fast with ErrRoomServiceBreakerOpen until the cooldown elapses.
//     Without this, /admin would block forever the moment livekit-server
//     stops responding.
type LiveKitRoomService struct {
	cfg    LiveKitRoomServiceConfig
	client livekit.RoomService

	// Circuit-breaker state.
	mu            sync.Mutex
	consecFail    int
	openUntil     time.Time
	halfOpen      bool
	requestsTotal atomic.Uint64
}

// ErrRoomServiceBreakerOpen is returned when the circuit breaker is open.
// Admin handlers MUST map this to 503 Service Unavailable — it is the
// "child is stalled, don't pile on" signal.
var ErrRoomServiceBreakerOpen = errors.New("vulos-meet: room service breaker open")

// NewLiveKitRoomService constructs a RoomService backed by the real
// livekit-server child. The Twirp client itself is constructed once at
// startup; each call mints a fresh short-lived JWT with admin grants.
func NewLiveKitRoomService(cfg LiveKitRoomServiceConfig) (*LiveKitRoomService, error) {
	if cfg.APIKey == "" || cfg.APISecret == "" {
		return nil, errors.New("vulos-meet: room service requires api_key/api_secret")
	}
	if cfg.SignalingAddr == "" {
		return nil, errors.New("vulos-meet: room service requires signaling addr")
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 5 * time.Second
	}
	if cfg.BreakerCooldown == 0 {
		cfg.BreakerCooldown = 30 * time.Second
	}
	if cfg.BreakerThreshold == 0 {
		cfg.BreakerThreshold = 3
	}
	if cfg.HTTPScheme == "" {
		cfg.HTTPScheme = "http"
	}
	baseURL := cfg.HTTPScheme + "://" + hostFromSignalingAddr(cfg.SignalingAddr)
	httpClient := &http.Client{
		Timeout: cfg.CallTimeout + 2*time.Second, // hard ceiling above ctx
	}
	c := livekit.NewRoomServiceProtobufClient(baseURL, httpClient)
	return &LiveKitRoomService{cfg: cfg, client: c}, nil
}

// hostFromSignalingAddr converts ":7880" → "127.0.0.1:7880" (because RoomService
// is reached via TCP — bare ":7880" is fine for listening but not for
// dialling) and "0.0.0.0:7880" → "127.0.0.1:7880". A fully qualified host:port
// is left as-is.
func hostFromSignalingAddr(addr string) string {
	switch {
	case addr == "":
		return "127.0.0.1:7880"
	case addr[0] == ':':
		return "127.0.0.1" + addr
	}
	// Replace 0.0.0.0 with loopback so the dial doesn't hit a route-less
	// wildcard. Done by simple prefix match; anything else is left alone.
	const wildcard = "0.0.0.0"
	if len(addr) > len(wildcard) && addr[:len(wildcard)] == wildcard {
		return "127.0.0.1" + addr[len(wildcard):]
	}
	return addr
}

// ListRoomIDs returns every room ID known to LiveKit. The tenant gate at the
// admin layer filters this to a single tenant before the result reaches a
// caller — see admin.go handleListRooms.
func (l *LiveKitRoomService) ListRoomIDs(ctx context.Context) ([]string, error) {
	l.requestsTotal.Add(1)
	if err := l.preCall(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()
	authCtx, err := l.authCtx(ctx, &auth.VideoGrant{RoomList: true})
	if err != nil {
		l.postCall(err)
		return nil, err
	}
	resp, err := l.client.ListRooms(authCtx, &livekit.ListRoomsRequest{})
	l.postCall(err)
	if err != nil {
		return nil, fmt.Errorf("vulos-meet: livekit ListRooms: %w", err)
	}
	out := make([]string, 0, len(resp.Rooms))
	for _, r := range resp.Rooms {
		out = append(out, r.Name)
	}
	return out, nil
}

// DeleteRoom removes a room. Returns nil on success regardless of whether
// the room existed (LiveKit's RoomService semantics — idempotent delete).
func (l *LiveKitRoomService) DeleteRoom(ctx context.Context, roomID string) error {
	if roomID == "" {
		return errors.New("vulos-meet: empty room id")
	}
	l.requestsTotal.Add(1)
	if err := l.preCall(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, l.cfg.CallTimeout)
	defer cancel()
	authCtx, err := l.authCtx(ctx, &auth.VideoGrant{RoomAdmin: true, Room: roomID})
	if err != nil {
		l.postCall(err)
		return err
	}
	_, err = l.client.DeleteRoom(authCtx, &livekit.DeleteRoomRequest{Room: roomID})
	l.postCall(err)
	if err != nil {
		return fmt.Errorf("vulos-meet: livekit DeleteRoom: %w", err)
	}
	return nil
}

// Close is a hook for callers (cmd/vulos-meet) to release any pooled
// resources on shutdown. The Twirp client uses the supplied *http.Client and
// has no separate Close; this is currently a no-op but kept on the type so
// the lifecycle wiring in main.go is symmetric ("started -> closed").
func (l *LiveKitRoomService) Close() {}

// authCtx mints a short-lived JWT with the supplied admin grants and
// attaches it to the outgoing Twirp context as the Authorization header.
//
// The minting cost is low (HMAC over a few hundred bytes) and we don't
// cache: caching would expand the blast radius of a leaked admin token
// from "one call" to "TTL minutes of calls" for no real perf win.
func (l *LiveKitRoomService) authCtx(ctx context.Context, grant *auth.VideoGrant) (context.Context, error) {
	at := auth.NewAccessToken(l.cfg.APIKey, l.cfg.APISecret).
		AddGrant(grant).
		SetValidFor(2 * time.Minute)
	tok, err := at.ToJWT()
	if err != nil {
		return ctx, fmt.Errorf("vulos-meet: mint admin token: %w", err)
	}
	hdr := make(http.Header)
	hdr.Set("Authorization", "Bearer "+tok)
	out, err := twirp.WithHTTPRequestHeaders(ctx, hdr)
	if err != nil {
		return ctx, fmt.Errorf("vulos-meet: attach admin header: %w", err)
	}
	return out, nil
}

// preCall is the breaker's "is the gate open?" check. If the breaker is
// currently open we return ErrRoomServiceBreakerOpen unless the cooldown
// has elapsed, in which case we let one half-open probe through.
func (l *LiveKitRoomService) preCall() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.openUntil.IsZero() {
		return nil
	}
	if time.Now().Before(l.openUntil) {
		return ErrRoomServiceBreakerOpen
	}
	// Cooldown elapsed — allow ONE half-open probe.
	if l.halfOpen {
		// Already a probe in flight; reject the rest.
		return ErrRoomServiceBreakerOpen
	}
	l.halfOpen = true
	return nil
}

// postCall closes the breaker on success, opens it on Nth consecutive
// failure, and clears any half-open state.
func (l *LiveKitRoomService) postCall(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.halfOpen = false
	if err == nil {
		l.consecFail = 0
		l.openUntil = time.Time{}
		return
	}
	l.consecFail++
	if l.consecFail >= l.cfg.BreakerThreshold {
		l.openUntil = time.Now().Add(l.cfg.BreakerCooldown)
	}
}

// BreakerOpen reports whether the breaker is currently rejecting calls. Used
// by tests; also useful for admin /health if we ever surface it.
func (l *LiveKitRoomService) BreakerOpen() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.openUntil.IsZero() && time.Now().Before(l.openUntil)
}

// RequestsTotal returns the total number of RoomService calls attempted
// (including breaker-rejected ones). Exposed for tests + future metrics.
func (l *LiveKitRoomService) RequestsTotal() uint64 {
	return l.requestsTotal.Load()
}
