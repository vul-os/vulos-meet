// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metrics is the small, dependency-free Prometheus exposer used by
// vulos-meet. It lives on a SEPARATE listener from /admin/* so an operator
// can scope it to an internal network (Prometheus scraper VPC, sidecar
// localhost, etc.) without granting that network access to the
// admin-token-guarded surface.
//
// We hand-roll the text-format exposition (it is trivial) so we don't pull
// the prometheus client library + collector machinery. The counters /
// gauges this layer needs are few and named explicitly. If we ever need
// histograms or summaries, swap to github.com/prometheus/client_golang.
type Metrics struct {
	mu sync.Mutex

	// admin_requests_total{status="200"} — every admin response.
	adminRequests map[int]uint64

	// token_validation_total{outcome="ok"|sentinelName} — every Validator
	// outcome. Sentinel names are the lower_snake_case of the wrap error
	// (e.g. "malformed", "expired", "wrong_tenant", "missing_grant",
	// "wrong_api_key", "missing_tenant", "room_malformed").
	tokenValidation map[string]uint64

	// active_rooms{tenant="<id>"} — set by the admin list-rooms handler on
	// every successful list. A list call is the right cardinality moment
	// (we already have an authenticated tenant in hand and a fresh count).
	activeRooms map[string]int

	// egress_requests_total{outcome="ok"|"unauthorized"|"forbidden"|"bad_gateway"|"rejected"}
	// — one per egress-proxy request, recorded by the EgressProxy.
	egressRequests map[string]uint64

	// Static limit gauges, set once at startup via SetRoomLimits. They make the
	// configured per-room participant cap and per-box room ceiling visible on
	// the metrics surface so a scrape can correlate active_rooms against the cap.
	maxParticipants int
	maxRooms        int

	// rooms_at_capacity — 1 when the latest observed total room count across all
	// tenants reached/exceeded maxRooms, else 0. Refreshed on each admin list.
	totalRooms      int
	roomsAtCapacity int
}

// TokenOutcome is the (small) set of outcome labels we expose on the
// token_validation_total counter. We use these constants rather than the
// sentinel error strings so the metric names are stable across error
// message tweaks.
type TokenOutcome string

const (
	TokenOutcomeOK            TokenOutcome = "ok"
	TokenOutcomeMalformed     TokenOutcome = "malformed"
	TokenOutcomeWrongAPIKey   TokenOutcome = "wrong_api_key"
	TokenOutcomeSignatureBad  TokenOutcome = "signature_bad" // covers expired/nbf-violated too — go-jose folds these into Verify()
	TokenOutcomeMissingGrants TokenOutcome = "missing_grant"
	TokenOutcomeMissingRoom   TokenOutcome = "missing_room"
	TokenOutcomeWrongTenant   TokenOutcome = "wrong_tenant"
	TokenOutcomeMissingTenant TokenOutcome = "missing_tenant"
	TokenOutcomeRoomMalformed TokenOutcome = "room_malformed"
	TokenOutcomeOther         TokenOutcome = "other"
)

// NewMetrics returns an empty metrics registry.
func NewMetrics() *Metrics {
	return &Metrics{
		adminRequests:   make(map[int]uint64),
		tokenValidation: make(map[string]uint64),
		activeRooms:     make(map[string]int),
		egressRequests:  make(map[string]uint64),
	}
}

// EgressOutcome is the small, bounded set of labels on the egress-requests
// counter. Bounded by construction so the metric never explodes cardinality.
type EgressOutcome string

const (
	EgressOutcomeOK           EgressOutcome = "ok"           // forwarded; upstream answered
	EgressOutcomeUnauthorized EgressOutcome = "unauthorized" // missing/invalid token
	EgressOutcomeForbidden    EgressOutcome = "forbidden"    // wrong tenant / missing RoomRecord
	EgressOutcomeBadGateway   EgressOutcome = "bad_gateway"  // upstream unreachable / forward error
	EgressOutcomeRejected     EgressOutcome = "rejected"     // method not allowed / bad request
)

// SetRoomLimits records the box's configured per-room participant cap and
// per-box room ceiling as static info gauges. Call once at startup. A zero
// value leaves the gauge at zero (meaning "unset/unbounded" in the scrape).
func (m *Metrics) SetRoomLimits(maxParticipants, maxRooms int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxParticipants = maxParticipants
	m.maxRooms = maxRooms
}

// ObserveEgress records one egress-proxy request outcome. Safe from any
// goroutine; tolerant of a nil receiver.
func (m *Metrics) ObserveEgress(outcome EgressOutcome) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.egressRequests[string(outcome)]++
}

// ObserveAdmin records one admin response with the given HTTP status code.
// Safe to call from any goroutine.
func (m *Metrics) ObserveAdmin(status int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adminRequests[status]++
}

// ObserveTokenValidation records the outcome of a Validator.Validate call.
// Pass nil err for the success path; otherwise pass the returned error and
// we translate it into a stable label via TokenOutcomeFromErr.
func (m *Metrics) ObserveTokenValidation(err error) {
	if m == nil {
		return
	}
	outcome := TokenOutcomeFromErr(err)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenValidation[string(outcome)]++
}

// SetActiveRooms updates the active-rooms gauge for a single tenant. We
// expose this on the admin list path: a list call gives us a fresh count
// for a tenant we have already authenticated, with no extra LiveKit RPCs.
func (m *Metrics) SetActiveRooms(tenant string, n int) {
	if m == nil || tenant == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRooms[tenant] = n
}

// SetTotalRooms records the box-wide total room count (across all tenants) and
// recomputes the rooms_at_capacity flag against the configured maxRooms ceiling.
// The admin list handler is the natural place to call it: it already lists every
// room on the box before filtering to a tenant. A maxRooms of 0 ("unbounded")
// never trips the capacity flag.
func (m *Metrics) SetTotalRooms(total int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalRooms = total
	if m.maxRooms > 0 && total >= m.maxRooms {
		m.roomsAtCapacity = 1
	} else {
		m.roomsAtCapacity = 0
	}
}

// TokenOutcomeFromErr maps a Validator.Validate error to a stable outcome
// label. Unknown errors fold into TokenOutcomeOther so we never emit an
// unbounded label set.
func TokenOutcomeFromErr(err error) TokenOutcome {
	switch {
	case err == nil:
		return TokenOutcomeOK
	case errors.Is(err, ErrTokenMalformed):
		return TokenOutcomeMalformed
	case errors.Is(err, ErrTokenWrongAPIKey):
		return TokenOutcomeWrongAPIKey
	case errors.Is(err, ErrTokenSignatureBad):
		return TokenOutcomeSignatureBad
	case errors.Is(err, ErrTokenMissingGrants):
		return TokenOutcomeMissingGrants
	case errors.Is(err, ErrTokenMissingRoom):
		return TokenOutcomeMissingRoom
	case errors.Is(err, ErrTokenWrongTenant):
		return TokenOutcomeWrongTenant
	case errors.Is(err, ErrTokenMissingTenant):
		return TokenOutcomeMissingTenant
	case errors.Is(err, ErrTokenRoomMalformed):
		return TokenOutcomeRoomMalformed
	default:
		return TokenOutcomeOther
	}
}

// Handler returns the /metrics HTTP handler emitting Prometheus text-format
// exposition. The handler is read-only and side-effect-free; it is safe to
// expose on an internal-only listener.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.writeText(w)
	})
}

func (m *Metrics) writeText(w io.Writer) {
	m.mu.Lock()
	// Copy under the lock so write doesn't hold the mutex.
	admin := make(map[int]uint64, len(m.adminRequests))
	for k, v := range m.adminRequests {
		admin[k] = v
	}
	tok := make(map[string]uint64, len(m.tokenValidation))
	for k, v := range m.tokenValidation {
		tok[k] = v
	}
	rooms := make(map[string]int, len(m.activeRooms))
	for k, v := range m.activeRooms {
		rooms[k] = v
	}
	egress := make(map[string]uint64, len(m.egressRequests))
	for k, v := range m.egressRequests {
		egress[k] = v
	}
	maxParticipants := m.maxParticipants
	maxRooms := m.maxRooms
	totalRooms := m.totalRooms
	roomsAtCapacity := m.roomsAtCapacity
	m.mu.Unlock()

	// admin_requests_total
	_, _ = io.WriteString(w, "# HELP vulos_meet_admin_requests_total Count of admin HTTP responses by status code.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_admin_requests_total counter\n")
	statuses := make([]int, 0, len(admin))
	for s := range admin {
		statuses = append(statuses, s)
	}
	sort.Ints(statuses)
	for _, s := range statuses {
		fmt.Fprintf(w, "vulos_meet_admin_requests_total{status=\"%d\"} %d\n", s, admin[s])
	}

	// token_validation_total
	_, _ = io.WriteString(w, "# HELP vulos_meet_token_validation_total Count of VULOS-MEET/1 token validations by outcome.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_token_validation_total counter\n")
	outs := make([]string, 0, len(tok))
	for o := range tok {
		outs = append(outs, o)
	}
	sort.Strings(outs)
	for _, o := range outs {
		fmt.Fprintf(w, "vulos_meet_token_validation_total{outcome=%q} %d\n", o, tok[o])
	}

	// active_rooms (gauge, tenant-labelled)
	_, _ = io.WriteString(w, "# HELP vulos_meet_active_rooms Active rooms per tenant, refreshed on admin list.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_active_rooms gauge\n")
	tenants := make([]string, 0, len(rooms))
	for t := range rooms {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, t := range tenants {
		fmt.Fprintf(w, "vulos_meet_active_rooms{tenant=%q} %d\n", escapeLabel(t), rooms[t])
	}

	// egress_requests_total (counter, outcome-labelled)
	_, _ = io.WriteString(w, "# HELP vulos_meet_egress_requests_total Count of egress-proxy requests by outcome.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_egress_requests_total counter\n")
	eouts := make([]string, 0, len(egress))
	for o := range egress {
		eouts = append(eouts, o)
	}
	sort.Strings(eouts)
	for _, o := range eouts {
		fmt.Fprintf(w, "vulos_meet_egress_requests_total{outcome=%q} %d\n", o, egress[o])
	}

	// Room-limit gauges (per-room participant cap + per-box room ceiling + the
	// live total + an at-capacity flag). These let a scrape correlate the
	// configured caps against the observed room count.
	_, _ = io.WriteString(w, "# HELP vulos_meet_max_participants Configured per-room participant cap rendered into LiveKit room.max_participants.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_max_participants gauge\n")
	fmt.Fprintf(w, "vulos_meet_max_participants %d\n", maxParticipants)

	_, _ = io.WriteString(w, "# HELP vulos_meet_max_rooms Configured per-box concurrent-room ceiling enforced at the admin layer.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_max_rooms gauge\n")
	fmt.Fprintf(w, "vulos_meet_max_rooms %d\n", maxRooms)

	_, _ = io.WriteString(w, "# HELP vulos_meet_total_rooms Total rooms across all tenants observed on the most recent admin list.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_total_rooms gauge\n")
	fmt.Fprintf(w, "vulos_meet_total_rooms %d\n", totalRooms)

	_, _ = io.WriteString(w, "# HELP vulos_meet_rooms_at_capacity 1 when the box has reached its configured max_rooms ceiling, else 0.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_rooms_at_capacity gauge\n")
	fmt.Fprintf(w, "vulos_meet_rooms_at_capacity %d\n", roomsAtCapacity)
}

// escapeLabel applies the Prometheus exposition-format escapes required for
// label values. Tenant IDs in vulos-meet are restricted to a safe character
// set by Tenant.validateTenant, so in practice no escaping is needed — but
// we apply it defensively in case the metrics layer is ever fed an
// unsanitised string.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\n\"") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// statusRecorder is an http.ResponseWriter wrapper that captures the final
// status code. The admin server's metrics middleware uses it to record
// counters AFTER the inner handler has chosen a status.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// instrumentAdmin wraps an admin http.Handler so every response feeds the
// admin_requests_total counter. Returns the original handler if m is nil.
func instrumentAdmin(m *Metrics, h http.Handler) http.Handler {
	if m == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		m.ObserveAdmin(rec.status)
	})
}
