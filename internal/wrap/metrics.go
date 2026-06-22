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

	// room_admission_total{outcome="new_room"|"existing"|"rejected_capacity"|"list_error"}
	// — one per token-valid /rtc join that reached the MaxRooms admission
	// decision, recorded by the signal-gate. "rejected_capacity" counts joins
	// refused because the box was already at its concurrent-room ceiling.
	roomAdmission map[string]uint64

	// Static limit gauges, set once at startup via SetRoomLimits. They make the
	// configured per-room participant cap and per-box room ceiling visible on
	// the metrics surface so a scrape can correlate active_rooms against the cap.
	maxParticipants int
	maxRooms        int

	// rooms_at_capacity — 1 when the latest observed total room count across all
	// tenants reached/exceeded maxRooms, else 0. Refreshed on each admin list.
	totalRooms      int
	roomsAtCapacity int

	// participants_in_room{tenant,room} — gauge of current participant count per
	// room, refreshed on each admin list (which already enumerates rooms with
	// their participant counts). Cardinality is bounded by the MaxRooms ceiling.
	participantsInRoom map[roomKey]int

	// egress_lifecycle_total{event="started"|"completed"|"failed"} — one per
	// egress lifecycle transition observed on the webhook receiver. This is the
	// in-progress/completed/failed surface the egress_requests_total counter
	// (which counts PROXY requests) does not provide.
	egressLifecycle map[string]uint64

	// egress_in_progress — gauge of egresses currently in the recording state
	// (started minus completed/failed). Derived from egressLifecycle counters.
	egressInProgress int

	// recording_lifecycle_total{state="recording"|"available"|"failed"|"expired"|"deleted"}
	// — one per recording-ledger state transition recorded by the receiver and
	// the retention driver.
	recordingLifecycle map[string]uint64

	// Recording byte/duration accounting, accumulated as recordings finalise
	// (available) and as the retention sweep frees them (deleted).
	recordingBytesTotal    uint64 // cumulative bytes seen on available recordings
	recordingDurationMsTotal uint64 // cumulative duration ms on available recordings

	// Retention sweep counters, accumulated across cleanup passes.
	retentionExpiredTotal   uint64
	retentionDeletedTotal   uint64
	retentionDeleteErrTotal uint64
	retentionBytesFreed     uint64

	// meet_rooms_total{event="started"|"finished"} — room lifecycle transitions
	// observed on the usage webhook receiver (the cp metering surface).
	meetRooms map[string]uint64

	// meet_minutes_total{tenant="<id>"} — cumulative participant-minutes metered
	// per tenant as rooms finish. This is the value reported to cp; exposing it
	// here lets a scrape reconcile against the central bill.
	meetMinutes map[string]uint64
}

// roomKey is the (tenant, room) composite used as the participants-in-room
// gauge label set. Bounded cardinality: rooms are capped by MaxRooms.
type roomKey struct {
	tenant string
	room   string
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
		adminRequests:      make(map[int]uint64),
		tokenValidation:    make(map[string]uint64),
		activeRooms:        make(map[string]int),
		egressRequests:     make(map[string]uint64),
		roomAdmission:      make(map[string]uint64),
		participantsInRoom: make(map[roomKey]int),
		egressLifecycle:    make(map[string]uint64),
		recordingLifecycle: make(map[string]uint64),
		meetRooms:          make(map[string]uint64),
		meetMinutes:        make(map[string]uint64),
	}
}

// MeetRoom is the bounded label set on the meet_rooms_total counter recorded by
// the usage webhook receiver.
type MeetRoom string

const (
	MeetRoomStarted  MeetRoom = "started"
	MeetRoomFinished MeetRoom = "finished"
)

// ObserveMeetRoom records one room lifecycle transition (started/finished) seen
// on the usage webhook receiver. Nil-tolerant.
func (m *Metrics) ObserveMeetRoom(ev MeetRoom) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meetRooms[string(ev)]++
}

// ObserveMeetMinutes accumulates participant-minutes metered for a tenant as a
// room finishes (the value reported to cp). Nil-tolerant; ignores non-positive
// counts and empty tenants.
func (m *Metrics) ObserveMeetMinutes(tenant string, minutes int64) {
	if m == nil || tenant == "" || minutes <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meetMinutes[tenant] += uint64(minutes)
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

// RoomAdmission is the small, bounded set of labels on the room-admission
// counter recorded by the signal-gate's MaxRooms enforcement.
type RoomAdmission string

const (
	RoomAdmissionNewRoom          RoomAdmission = "new_room"          // allowed; created a new room under the cap
	RoomAdmissionExisting         RoomAdmission = "existing"          // allowed; joined an already-active room
	RoomAdmissionRejectedCapacity RoomAdmission = "rejected_capacity" // refused; box at MaxRooms and this is a new room
	RoomAdmissionListError        RoomAdmission = "list_error"        // refused; could not list rooms to decide
)

// ObserveRoomAdmission records one /rtc join admission decision. Safe from any
// goroutine; tolerant of a nil receiver.
func (m *Metrics) ObserveRoomAdmission(outcome RoomAdmission) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomAdmission[string(outcome)]++
}

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

// EgressLifecycle is the bounded label set on the egress-lifecycle counter. It
// tracks the in-progress/completed/failed transitions of egress jobs (distinct
// from egress_requests_total, which counts PROXY requests not job outcomes).
type EgressLifecycle string

const (
	EgressLifecycleStarted   EgressLifecycle = "started"
	EgressLifecycleCompleted EgressLifecycle = "completed"
	EgressLifecycleFailed    EgressLifecycle = "failed"
)

// SetParticipantsInRoom records the current participant count for one room. The
// admin list handler is the natural place to call it (it can enumerate rooms
// with their participant counts in the same RPC). A count <= 0 removes the
// room's gauge series so a finished room does not linger as a stale 0.
func (m *Metrics) SetParticipantsInRoom(tenant, room string, n int) {
	if m == nil || tenant == "" || room == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := roomKey{tenant: tenant, room: room}
	if n <= 0 {
		delete(m.participantsInRoom, k)
		return
	}
	m.participantsInRoom[k] = n
}

// ResetParticipantsForTenant clears all per-room participant gauges for a tenant
// before a fresh admin-list refresh repopulates them. This keeps the gauge from
// retaining rooms that have since closed. Called by the admin list handler.
func (m *Metrics) ResetParticipantsForTenant(tenant string) {
	if m == nil || tenant == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.participantsInRoom {
		if k.tenant == tenant {
			delete(m.participantsInRoom, k)
		}
	}
}

// ObserveEgressLifecycle records one egress job lifecycle transition and keeps
// the in-progress gauge consistent (started increments it; completed/failed
// decrement it, floored at 0). Safe from any goroutine; nil-tolerant.
func (m *Metrics) ObserveEgressLifecycle(ev EgressLifecycle) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.egressLifecycle[string(ev)]++
	switch ev {
	case EgressLifecycleStarted:
		m.egressInProgress++
	case EgressLifecycleCompleted, EgressLifecycleFailed:
		if m.egressInProgress > 0 {
			m.egressInProgress--
		}
	}
}

// ObserveRecordingLifecycle records one recording-ledger state transition and,
// when a recording becomes available, mirrors it onto the egress-lifecycle
// counters + the cumulative bytes/duration totals. recording → started,
// available → completed, failed → failed. expired/deleted feed only the
// recording counter (the egress job already terminated). Nil-tolerant.
func (m *Metrics) ObserveRecordingLifecycle(state RecordingState) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.recordingLifecycle[string(state)]++
	m.mu.Unlock()
	switch state {
	case RecordingStateRecording:
		m.ObserveEgressLifecycle(EgressLifecycleStarted)
	case RecordingStateAvailable:
		m.ObserveEgressLifecycle(EgressLifecycleCompleted)
	case RecordingStateFailed:
		m.ObserveEgressLifecycle(EgressLifecycleFailed)
	}
}

// ObserveRecordingBytes accumulates the byte/duration totals for a finalised
// recording. Called by the receiver when a recording becomes available with a
// known size/duration. Nil-tolerant.
func (m *Metrics) ObserveRecordingBytes(sizeBytes, durationMs uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordingBytesTotal += sizeBytes
	m.recordingDurationMsTotal += durationMs
}

// ObserveRetentionSweep folds one retention cleanup pass into the cumulative
// retention counters. Nil-tolerant.
func (m *Metrics) ObserveRetentionSweep(res RetentionSweepResult) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retentionExpiredTotal += uint64(res.Expired)
	m.retentionDeletedTotal += uint64(res.Deleted)
	m.retentionDeleteErrTotal += uint64(res.DeleteErrs)
	m.retentionBytesFreed += res.BytesFreed
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
	roomAdm := make(map[string]uint64, len(m.roomAdmission))
	for k, v := range m.roomAdmission {
		roomAdm[k] = v
	}
	parts := make(map[roomKey]int, len(m.participantsInRoom))
	for k, v := range m.participantsInRoom {
		parts[k] = v
	}
	egLife := make(map[string]uint64, len(m.egressLifecycle))
	for k, v := range m.egressLifecycle {
		egLife[k] = v
	}
	recLife := make(map[string]uint64, len(m.recordingLifecycle))
	for k, v := range m.recordingLifecycle {
		recLife[k] = v
	}
	maxParticipants := m.maxParticipants
	maxRooms := m.maxRooms
	totalRooms := m.totalRooms
	roomsAtCapacity := m.roomsAtCapacity
	egressInProgress := m.egressInProgress
	recBytes := m.recordingBytesTotal
	recDurationMs := m.recordingDurationMsTotal
	retExpired := m.retentionExpiredTotal
	retDeleted := m.retentionDeletedTotal
	retDeleteErr := m.retentionDeleteErrTotal
	retBytesFreed := m.retentionBytesFreed
	meetRooms := make(map[string]uint64, len(m.meetRooms))
	for k, v := range m.meetRooms {
		meetRooms[k] = v
	}
	meetMinutes := make(map[string]uint64, len(m.meetMinutes))
	for k, v := range m.meetMinutes {
		meetMinutes[k] = v
	}
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

	// room_admission_total (counter, outcome-labelled) — the MaxRooms decision.
	_, _ = io.WriteString(w, "# HELP vulos_meet_room_admission_total Count of token-valid /rtc join admission decisions against the MaxRooms ceiling.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_room_admission_total counter\n")
	raouts := make([]string, 0, len(roomAdm))
	for o := range roomAdm {
		raouts = append(raouts, o)
	}
	sort.Strings(raouts)
	for _, o := range raouts {
		fmt.Fprintf(w, "vulos_meet_room_admission_total{outcome=%q} %d\n", o, roomAdm[o])
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

	// participants_in_room (gauge, tenant+room-labelled) — current participant
	// count per room, refreshed on admin list. Bounded by MaxRooms.
	_, _ = io.WriteString(w, "# HELP vulos_meet_participants_in_room Current participant count per room, refreshed on admin list.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_participants_in_room gauge\n")
	pkeys := make([]roomKey, 0, len(parts))
	for k := range parts {
		pkeys = append(pkeys, k)
	}
	sort.Slice(pkeys, func(i, j int) bool {
		if pkeys[i].tenant != pkeys[j].tenant {
			return pkeys[i].tenant < pkeys[j].tenant
		}
		return pkeys[i].room < pkeys[j].room
	})
	for _, k := range pkeys {
		fmt.Fprintf(w, "vulos_meet_participants_in_room{tenant=%q,room=%q} %d\n", escapeLabel(k.tenant), escapeLabel(k.room), parts[k])
	}

	// egress_lifecycle_total (counter, event-labelled) — egress JOB outcomes,
	// distinct from egress_requests_total which counts proxy requests.
	_, _ = io.WriteString(w, "# HELP vulos_meet_egress_lifecycle_total Count of egress job lifecycle transitions by event (started/completed/failed).\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_egress_lifecycle_total counter\n")
	elKeys := make([]string, 0, len(egLife))
	for k := range egLife {
		elKeys = append(elKeys, k)
	}
	sort.Strings(elKeys)
	for _, k := range elKeys {
		fmt.Fprintf(w, "vulos_meet_egress_lifecycle_total{event=%q} %d\n", k, egLife[k])
	}

	_, _ = io.WriteString(w, "# HELP vulos_meet_egress_in_progress Egress jobs currently in progress (started minus completed/failed).\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_egress_in_progress gauge\n")
	fmt.Fprintf(w, "vulos_meet_egress_in_progress %d\n", egressInProgress)

	// recording_lifecycle_total (counter, state-labelled) — recording-ledger
	// state transitions across the receiver + retention driver.
	_, _ = io.WriteString(w, "# HELP vulos_meet_recording_lifecycle_total Count of recording-ledger state transitions by state.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_recording_lifecycle_total counter\n")
	rlKeys := make([]string, 0, len(recLife))
	for k := range recLife {
		rlKeys = append(rlKeys, k)
	}
	sort.Strings(rlKeys)
	for _, k := range rlKeys {
		fmt.Fprintf(w, "vulos_meet_recording_lifecycle_total{state=%q} %d\n", k, recLife[k])
	}

	// Recording byte/duration accounting (counters).
	_, _ = io.WriteString(w, "# HELP vulos_meet_recording_bytes_total Cumulative bytes of finalised (available) recordings observed.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_recording_bytes_total counter\n")
	fmt.Fprintf(w, "vulos_meet_recording_bytes_total %d\n", recBytes)

	_, _ = io.WriteString(w, "# HELP vulos_meet_recording_duration_ms_total Cumulative duration (ms) of finalised (available) recordings observed.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_recording_duration_ms_total counter\n")
	fmt.Fprintf(w, "vulos_meet_recording_duration_ms_total %d\n", recDurationMs)

	// Retention sweep accounting (counters).
	_, _ = io.WriteString(w, "# HELP vulos_meet_retention_expired_total Cumulative recordings marked expired by the retention sweep.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_retention_expired_total counter\n")
	fmt.Fprintf(w, "vulos_meet_retention_expired_total %d\n", retExpired)

	_, _ = io.WriteString(w, "# HELP vulos_meet_retention_deleted_total Cumulative recordings whose blob delete was confirmed by the retention sweep.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_retention_deleted_total counter\n")
	fmt.Fprintf(w, "vulos_meet_retention_deleted_total %d\n", retDeleted)

	_, _ = io.WriteString(w, "# HELP vulos_meet_retention_delete_errors_total Cumulative blob-delete failures (left expired for retry) during retention sweeps.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_retention_delete_errors_total counter\n")
	fmt.Fprintf(w, "vulos_meet_retention_delete_errors_total %d\n", retDeleteErr)

	_, _ = io.WriteString(w, "# HELP vulos_meet_retention_bytes_freed_total Cumulative bytes freed by confirmed retention deletions.\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_retention_bytes_freed_total counter\n")
	fmt.Fprintf(w, "vulos_meet_retention_bytes_freed_total %d\n", retBytesFreed)

	// meet_rooms_total (counter, event-labelled) — room lifecycle transitions
	// observed on the usage webhook receiver (the cp metering surface).
	_, _ = io.WriteString(w, "# HELP vulos_meet_meet_rooms_total Count of meet room lifecycle transitions by event (started/finished).\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_meet_rooms_total counter\n")
	mrKeys := make([]string, 0, len(meetRooms))
	for k := range meetRooms {
		mrKeys = append(mrKeys, k)
	}
	sort.Strings(mrKeys)
	for _, k := range mrKeys {
		fmt.Fprintf(w, "vulos_meet_meet_rooms_total{event=%q} %d\n", k, meetRooms[k])
	}

	// meet_minutes_total (counter, tenant-labelled) — cumulative participant-
	// minutes metered per tenant as rooms finish. This is the value reported to
	// the cp control plane.
	_, _ = io.WriteString(w, "# HELP vulos_meet_meet_minutes_total Cumulative participant-minutes metered per tenant as rooms finish (reported to cp).\n")
	_, _ = io.WriteString(w, "# TYPE vulos_meet_meet_minutes_total counter\n")
	mmKeys := make([]string, 0, len(meetMinutes))
	for k := range meetMinutes {
		mmKeys = append(mmKeys, k)
	}
	sort.Strings(mmKeys)
	for _, k := range mmKeys {
		fmt.Fprintf(w, "vulos_meet_meet_minutes_total{tenant=%q} %d\n", escapeLabel(k), meetMinutes[k])
	}
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
