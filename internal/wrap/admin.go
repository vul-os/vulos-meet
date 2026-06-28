// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vul-os/vulos-meet/internal/apikey"
)

// AdminTokenEnv is the environment variable that carries the bearer token
// guarding /admin/*. Empty value at startup is a hard error: there is no
// "open admin" mode.
const AdminTokenEnv = "MEET_ADMIN_TOKEN"

// RoomService is the narrow interface admin handlers use to reach LiveKit's
// RoomService. In production this is backed by livekit/protocol's
// RoomServiceClient; in tests we substitute an in-memory fake. Keeping the
// surface tiny is the point — admin should not learn enough about LiveKit to
// poke at it sideways.
type RoomService interface {
	ListRoomIDs(ctx context.Context) ([]string, error)
	DeleteRoom(ctx context.Context, roomID string) error
}

// AdminLiveness is the minimal, UNAUTHENTICATED response from GET
// /admin/health. It is deliberately just a 200 + {"status":"ok"} so the public
// liveness probe discloses nothing about build/version/region/topology to
// unauthenticated callers. Anything an attacker could fingerprint the box with
// (version, region, protocol, tenant separator) moved to GET /admin/info,
// behind the admin token.
type AdminLiveness struct {
	Status string `json:"status"` // "ok" while we are accepting traffic
}

// AdminHealth is the JSON shape returned by GET /admin/info. That endpoint is
// admin-token gated. The vulos-cloud georoute layer (CLOUD-REGION-01) polls it
// (with the admin bearer) to discover which regions a tenant can be steered to.
type AdminHealth struct {
	Status    string `json:"status"`    // "ok" while we are accepting traffic
	Version   string `json:"version"`   // build version of this binary
	Region    string `json:"region"`    // e.g. "za-jhb", "eu-fra"
	Protocol  string `json:"protocol"`  // VULOS-MEET/<n>
	Separator string `json:"separator"` // tenant separator byte (": " is "}":")
}

// AdminServer is the HTTP surface for vulos-meet admin operations. It is
// deliberately tiny: health, list rooms in a tenant, delete a room in a
// tenant. Every non-health endpoint goes through both the admin-token gate
// AND the tenant gate before reaching LiveKit.
type AdminServer struct {
	tenant      *Tenant
	rooms       RoomService
	geo         *GeoRouter
	adminToken  string
	version     string
	metrics     *Metrics            // optional — nil disables metrics emission
	usage       UsageStatter        // optional — nil disables the usage stats read
	rateLimiter *RateLimiter        // optional — nil disables per-IP rate limiting
	intro       apikey.Introspector // optional — nil disables vk_ key path (VULOS_CP_BASE_URL unset)
}

// UsageStatter is the read-only seam the admin surface uses to expose live
// meet-usage stats (participant-minutes per room). It is satisfied by
// *UsageReceiver. Optional: when nil, GET /admin/tenants/{t}/usage 404s.
type UsageStatter interface {
	Snapshot(tenant string) []RoomUsageSnapshot
}

// NewAdminServer constructs the admin surface. adminToken MUST be non-empty
// or NewAdminServer returns an error — there is no anonymous admin mode.
func NewAdminServer(tenant *Tenant, rooms RoomService, geo *GeoRouter, adminToken, version string) (*AdminServer, error) {
	if tenant == nil {
		return nil, errors.New("vulos-meet: admin server requires a tenant gate")
	}
	if rooms == nil {
		return nil, errors.New("vulos-meet: admin server requires a room service")
	}
	if geo == nil {
		return nil, errors.New("vulos-meet: admin server requires a geo router")
	}
	if adminToken == "" {
		return nil, errors.New("vulos-meet: admin token is empty (set " + AdminTokenEnv + ")")
	}
	if version == "" {
		version = "0.0.0-unknown"
	}
	return &AdminServer{
		tenant:     tenant,
		rooms:      rooms,
		geo:        geo,
		adminToken: adminToken,
		version:    version,
	}, nil
}

// SetMetrics attaches a metrics registry. Calling with nil clears the
// attachment. Metrics are optional — every admin code path tolerates a nil
// registry. We keep the wiring opt-in so unit tests don't have to set up a
// metrics scrape target for every assertion.
func (s *AdminServer) SetMetrics(m *Metrics) {
	s.metrics = m
}

// SetUsageStatter attaches the meet-usage stats source so the admin surface can
// serve GET /admin/tenants/{tenant}/usage. Optional; nil clears it (the route
// then returns 404). Wired in main.go from the usage webhook receiver.
func (s *AdminServer) SetUsageStatter(u UsageStatter) {
	s.usage = u
}

// SetRateLimiter wires a per-IP token-bucket rate limiter onto ALL admin
// endpoints (including the unauthenticated health probe). Rate limiting is
// applied before admin-token verification so a high-rate brute-force attempt
// does not exhaust CPU on constant-time compare. Passing nil clears the limiter.
func (s *AdminServer) SetRateLimiter(r *RateLimiter) {
	s.rateLimiter = r
}

// SetIntrospector wires the vk_ API-key introspection seam. When non-nil,
// an `Authorization: Bearer vk_…` credential is validated via the Vulos
// control-plane introspection endpoint (POST {VULOS_CP_BASE_URL}/api/keys/introspect)
// on every guarded admin route. The key must be valid and carry the "meet"
// product scope. When nil (VULOS_CP_BASE_URL unset) the vk_ path is disabled
// and all guarded routes require the static MEET_ADMIN_TOKEN — self-host is
// unchanged. Passing nil clears a previously wired introspector.
func (s *AdminServer) SetIntrospector(intro apikey.Introspector) {
	s.intro = intro
}

// Handler returns the http.Handler registering all /admin routes. The router
// is kept in this method (not as a free function) so callers can mount the
// admin surface under a sub-path if they want.
//
// If a metrics registry has been attached via SetMetrics, every response is
// counted under vulos_meet_admin_requests_total{status="..."}.
func (s *AdminServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/health", s.handleHealth)
	mux.HandleFunc("GET /admin/info", s.guarded(s.handleInfo))
	mux.HandleFunc("GET /admin/tenants/{tenant}/rooms", s.guardedTenant(s.handleListRooms))
	mux.HandleFunc("DELETE /admin/tenants/{tenant}/rooms/{room}", s.guardedTenant(s.handleDeleteRoom))
	mux.HandleFunc("GET /admin/tenants/{tenant}/usage", s.guardedTenant(s.handleUsage))
	h := instrumentAdmin(s.metrics, mux)
	if s.rateLimiter != nil {
		h = s.rateLimiter.Middleware(h)
	}
	return h
}

// tenantHandler is the inner callback type for tenant-scoped admin routes.
type tenantHandler func(w http.ResponseWriter, r *http.Request, tenant string)

// guarded wraps a handler with admin auth only (no tenant-path validation).
// Used for admin endpoints not scoped to a single tenant, such as GET /admin/info.
// Auth accepts EITHER a vk_ API key (when VULOS_CP_BASE_URL is set) OR the
// static MEET_ADMIN_TOKEN bearer — vk_ prefix selects the scheme.
func (s *AdminServer) guarded(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(w, r) {
			return
		}
		inner(w, r)
	}
}

// guardedTenant wraps a handler with admin auth + tenant-path validation.
// Auth accepts EITHER a vk_ API key (when VULOS_CP_BASE_URL is set) OR the
// static MEET_ADMIN_TOKEN bearer — vk_ prefix selects the scheme.
func (s *AdminServer) guardedTenant(inner tenantHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(w, r) {
			return
		}
		tenant := r.PathValue("tenant")
		if err := s.tenant.validateTenant(tenant); err != nil {
			http.Error(w, "bad tenant", http.StatusBadRequest)
			return
		}
		inner(w, r, tenant)
	}
}

// checkAuth authenticates an admin request, writing the error response itself
// and returning false when auth fails. It accepts two schemes:
//
//  1. vk_ API key — `Authorization: Bearer vk_…` — validated via the CP
//     introspection seam (apikey.Introspector). The key must be valid and carry
//     the "meet" product scope. Only attempted when an introspector is wired
//     (VULOS_CP_BASE_URL is set). CP unavailable → 503 (fail-closed).
//
//  2. Static admin bearer — `Authorization: Bearer <MEET_ADMIN_TOKEN>` — the
//     existing constant-time compare path. Used when the token does not start
//     with the vk_ prefix, or when no introspector is wired.
//
// The vk_ prefix is the exclusive selector: a vk_ token is never tried as a
// static admin token, and a static admin token is never tried as a vk_ key.
func (s *AdminServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	raw := adminBearerRaw(r)

	// ── vk_ API-key path ─────────────────────────────────────────────────────
	// Only when an introspector is configured AND the credential looks like a
	// Vulos API key. A vk_ token is never tried as a static admin token (and
	// vice versa), so the two schemes can't be confused.
	if s.intro != nil && strings.HasPrefix(raw, apikey.KeyPrefix) {
		res, err := s.intro.Introspect(r.Context(), raw)
		if err != nil {
			// CP unreachable: fail closed rather than guess.
			http.Error(w, "API key validation unavailable", http.StatusServiceUnavailable)
			return false
		}
		if !res.Valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		if !res.HasProduct(apikey.ProductMeet) {
			http.Error(w, "API key not authorized for the meet product", http.StatusForbidden)
			return false
		}
		return true
	}

	// ── Static admin-token path ───────────────────────────────────────────────
	if !constantTimeEqualString(raw, s.adminToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// adminBearerRaw returns the raw token from an `Authorization: Bearer <token>`
// header (no scheme prefix, trimmed), or "" when absent or wrong scheme.
func adminBearerRaw(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// checkAdminToken pulls the bearer from Authorization and constant-time
// compares it to the configured admin token. Retained for callers (tests) that
// use it directly. New code should prefer checkAuth which also handles vk_ keys.
func (s *AdminServer) checkAdminToken(r *http.Request) bool {
	return constantTimeEqualString(adminBearerRaw(r), s.adminToken)
}

// constantTimeEqualString is a length-safe wrapper around
// crypto/subtle.ConstantTimeCompare. We compare lengths in *also* constant
// time by feeding both into subtle (length difference still leaks, but it
// leaks via timing of len() not via byte-by-byte).
func constantTimeEqualString(a, b string) bool {
	// Equal-length compare via subtle.
	if subtle.ConstantTimeEq(int32(len(a)), int32(len(b))) != 1 {
		// Run the compare anyway to keep timing flat-ish (still bounded by
		// the longer of the two lengths). Result is forced to 0.
		_ = subtle.ConstantTimeCompare([]byte(a), padTo([]byte(b), len(a)))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func padTo(b []byte, n int) []byte {
	if len(b) >= n {
		return b[:n]
	}
	out := make([]byte, n)
	copy(out, b)
	return out
}

// handleHealth is the UNAUTHENTICATED liveness probe. It returns a bare 200 +
// {"status":"ok"} and nothing else: build/version/region/protocol/separator are
// deliberately withheld from anonymous callers (see handleInfo).
func (s *AdminServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, AdminLiveness{Status: "ok"})
}

// handleInfo is the admin-token-gated build/region detail endpoint. The
// vulos-cloud georoute layer polls this (with the admin bearer) to learn the
// box region/protocol; it carries the fields that used to leak from the public
// health probe.
func (s *AdminServer) handleInfo(w http.ResponseWriter, r *http.Request) {
	resp := AdminHealth{
		Status:    "ok",
		Version:   s.version,
		Region:    s.geo.Region(),
		Protocol:  SubprotocolVersion,
		Separator: s.tenant.Separator(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// listRoomsResponse is the JSON shape returned by GET /admin/tenants/{t}/rooms.
type listRoomsResponse struct {
	Tenant string   `json:"tenant"`
	Rooms  []string `json:"rooms"` // per-tenant short names (sep-stripped)
}

func (s *AdminServer) handleListRooms(w http.ResponseWriter, r *http.Request, tenant string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	all, err := s.rooms.ListRoomIDs(ctx)
	if err != nil {
		http.Error(w, "list failed", http.StatusBadGateway)
		return
	}
	mine := s.tenant.FilterRooms(tenant, all)
	short := make([]string, 0, len(mine))
	sep := s.tenant.Separator()
	prefix := tenant + sep
	for _, r := range mine {
		short = append(short, strings.TrimPrefix(r, prefix))
	}
	// Feed the per-tenant active-rooms gauge AND the box-wide total. This is the
	// natural cardinality moment: we already have an authenticated tenant in
	// hand, a fresh per-tenant count, and the full box-wide room list (`all`)
	// for the per-box ceiling — all with no extra LiveKit RPCs.
	s.metrics.SetActiveRooms(tenant, len(short))
	s.metrics.SetTotalRooms(len(all))
	// Per-room participant gauge: only if the room service can report counts
	// (LiveKit client does; the in-memory fake does). Refresh this tenant's
	// rooms each list so closed rooms drop out of the gauge.
	s.refreshParticipantGauges(ctx, tenant)
	writeJSON(w, http.StatusOK, listRoomsResponse{Tenant: tenant, Rooms: short})
}

// refreshParticipantGauges repopulates vulos_meet_participants_in_room for one
// tenant if the room service supports participant counts. Best-effort: a
// listing error leaves the previous gauge values in place rather than failing
// the admin response (the room list already succeeded). Bounded to the calling
// tenant's rooms so the metrics layer never sees cross-tenant room names.
func (s *AdminServer) refreshParticipantGauges(ctx context.Context, tenant string) {
	if s.metrics == nil {
		return
	}
	lister, ok := s.rooms.(RoomParticipantLister)
	if !ok {
		return
	}
	rps, err := lister.ListRoomParticipants(ctx)
	if err != nil {
		return
	}
	s.metrics.ResetParticipantsForTenant(tenant)
	sep := s.tenant.Separator()
	prefix := tenant + sep
	for _, rp := range rps {
		if !strings.HasPrefix(rp.Name, prefix) {
			continue // not this tenant's room
		}
		short := strings.TrimPrefix(rp.Name, prefix)
		s.metrics.SetParticipantsInRoom(tenant, short, rp.NumParticipants)
	}
}

// usageResponse is the JSON shape returned by GET /admin/tenants/{t}/usage. It
// is the same data vulos-meet meters to cp, exposed read-only so an operator can
// reconcile the live participant-minute accrual without scraping cp.
type usageResponse struct {
	Tenant string              `json:"tenant"`
	Rooms  []RoomUsageSnapshot `json:"rooms"`
}

func (s *AdminServer) handleUsage(w http.ResponseWriter, r *http.Request, tenant string) {
	if s.usage == nil {
		// Usage tracking not wired (no usage receiver attached). Distinct from
		// "no active rooms" — the feature is simply not present.
		http.Error(w, "usage stats not available", http.StatusNotFound)
		return
	}
	rooms := s.usage.Snapshot(tenant)
	writeJSON(w, http.StatusOK, usageResponse{Tenant: tenant, Rooms: rooms})
}

func (s *AdminServer) handleDeleteRoom(w http.ResponseWriter, r *http.Request, tenant string) {
	rest := r.PathValue("room")
	full, err := s.tenant.QualifyRoom(tenant, rest)
	if err != nil {
		http.Error(w, "bad room", http.StatusBadRequest)
		return
	}
	// Belt and braces: independently re-verify ownership before deleting.
	// A bug in QualifyRoom would otherwise let one tenant delete another's
	// room. This second check is cheap and the consequence of getting it
	// wrong is severe.
	if err := s.tenant.EnforceRoom(tenant, full); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.rooms.DeleteRoom(ctx, full); err != nil {
		http.Error(w, "delete failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
