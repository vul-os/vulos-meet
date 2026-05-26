// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
)

// roomAdmissionCount is a test-only accessor for the unexported room-admission
// counter, used by the MaxRooms pentest to assert a capacity rejection was
// recorded on the metrics surface.
func (m *Metrics) roomAdmissionCount(outcome RoomAdmission) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.roomAdmission[string(outcome)]
}

func TestMetrics_TokenOutcomeMapping(t *testing.T) {
	cases := map[error]TokenOutcome{
		nil:                   TokenOutcomeOK,
		ErrTokenMalformed:     TokenOutcomeMalformed,
		ErrTokenWrongAPIKey:   TokenOutcomeWrongAPIKey,
		ErrTokenSignatureBad:  TokenOutcomeSignatureBad,
		ErrTokenMissingGrants: TokenOutcomeMissingGrants,
		ErrTokenMissingRoom:   TokenOutcomeMissingRoom,
		ErrTokenWrongTenant:   TokenOutcomeWrongTenant,
		ErrTokenMissingTenant: TokenOutcomeMissingTenant,
		ErrTokenRoomMalformed: TokenOutcomeRoomMalformed,
		errors.New("nope"):    TokenOutcomeOther,
	}
	for in, want := range cases {
		if got := TokenOutcomeFromErr(in); got != want {
			t.Fatalf("TokenOutcomeFromErr(%v): got %q want %q", in, got, want)
		}
	}
}

func TestMetrics_AdminAndTokenCountersExposed(t *testing.T) {
	m := NewMetrics()
	m.ObserveAdmin(200)
	m.ObserveAdmin(200)
	m.ObserveAdmin(401)
	m.ObserveTokenValidation(nil)
	m.ObserveTokenValidation(ErrTokenMalformed)
	m.ObserveTokenValidation(ErrTokenWrongTenant)
	m.SetActiveRooms("acme", 4)
	m.SetActiveRooms("globex", 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	body := rec.Body.String()
	wantSubs := []string{
		`vulos_meet_admin_requests_total{status="200"} 2`,
		`vulos_meet_admin_requests_total{status="401"} 1`,
		`vulos_meet_token_validation_total{outcome="ok"} 1`,
		`vulos_meet_token_validation_total{outcome="malformed"} 1`,
		`vulos_meet_token_validation_total{outcome="wrong_tenant"} 1`,
		`vulos_meet_active_rooms{tenant="acme"} 4`,
		`vulos_meet_active_rooms{tenant="globex"} 0`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(body, s) {
			t.Fatalf("missing %q in:\n%s", s, body)
		}
	}
}

func TestMetrics_NilSafeCalls(t *testing.T) {
	var m *Metrics
	// All these must be no-ops, not crashes.
	m.ObserveAdmin(200)
	m.ObserveTokenValidation(nil)
	m.SetActiveRooms("acme", 1)
	m.ObserveEgress(EgressOutcomeOK)
	m.SetRoomLimits(50, 100)
	m.SetTotalRooms(3)
}

func TestMetrics_EgressAndRoomLimitsExposed(t *testing.T) {
	m := NewMetrics()
	m.SetRoomLimits(500, 200) // per-room cap + per-box ceiling
	m.ObserveEgress(EgressOutcomeOK)
	m.ObserveEgress(EgressOutcomeOK)
	m.ObserveEgress(EgressOutcomeForbidden)
	m.ObserveEgress(EgressOutcomeUnauthorized)
	m.SetTotalRooms(199) // below the 200 ceiling

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, s := range []string{
		`vulos_meet_egress_requests_total{outcome="ok"} 2`,
		`vulos_meet_egress_requests_total{outcome="forbidden"} 1`,
		`vulos_meet_egress_requests_total{outcome="unauthorized"} 1`,
		`vulos_meet_max_participants 500`,
		`vulos_meet_max_rooms 200`,
		`vulos_meet_total_rooms 199`,
		`vulos_meet_rooms_at_capacity 0`,
	} {
		if !strings.Contains(body, s) {
			t.Fatalf("missing %q in:\n%s", s, body)
		}
	}
}

func TestMetrics_RoomsAtCapacityFlips(t *testing.T) {
	m := NewMetrics()
	m.SetRoomLimits(50, 100)

	scrape := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	m.SetTotalRooms(99)
	if !strings.Contains(scrape(), "vulos_meet_rooms_at_capacity 0") {
		t.Fatalf("below ceiling should not be at capacity")
	}
	m.SetTotalRooms(100) // reaches the ceiling
	if !strings.Contains(scrape(), "vulos_meet_rooms_at_capacity 1") {
		t.Fatalf("at ceiling should flip rooms_at_capacity to 1")
	}

	// Unbounded ceiling (0) never trips.
	m2 := NewMetrics()
	m2.SetRoomLimits(50, 0)
	m2.SetTotalRooms(100000)
	rec := httptest.NewRecorder()
	m2.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "vulos_meet_rooms_at_capacity 0") {
		t.Fatalf("unbounded ceiling must never flip at-capacity")
	}
}

func TestMetrics_AdminInstrumentation_CountsResponses(t *testing.T) {
	admin, rooms := newTestAdminServer(t)
	m := NewMetrics()
	admin.SetMetrics(m)

	rooms.CreateRoom(context.Background(), "acme:standup")
	rooms.CreateRoom(context.Background(), "acme:retro")

	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	// One auth-failed admin call.
	resp, err := http.Get(srv.URL + "/admin/tenants/acme/rooms")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	// One success.
	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	// One health call (no auth required).
	resp, err = http.Get(srv.URL + "/admin/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `vulos_meet_admin_requests_total{status="401"} 1`) {
		t.Fatalf("missing 401 counter:\n%s", body)
	}
	if !strings.Contains(body, `vulos_meet_admin_requests_total{status="200"} 2`) {
		t.Fatalf("missing 200 counter:\n%s", body)
	}
	if !strings.Contains(body, `vulos_meet_active_rooms{tenant="acme"} 2`) {
		t.Fatalf("missing per-tenant gauge:\n%s", body)
	}
}

func TestMetrics_ValidatorObservesOutcomes(t *testing.T) {
	v := newValidatorForTest(t)
	m := NewMetrics()
	v.SetMetrics(m)

	// Success.
	if _, err := v.Validate(mintToken(t, "acme", "standup", time.Hour)); err != nil {
		t.Fatalf("expected ok: %v", err)
	}
	// Malformed.
	_, _ = v.Validate("not-a-jwt")
	// Wrong tenant in audience.
	_, _ = v.Validate(mintTokenWrongTenant(t))

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`vulos_meet_token_validation_total{outcome="ok"} 1`,
		`vulos_meet_token_validation_total{outcome="malformed"} 1`,
		`vulos_meet_token_validation_total{outcome="wrong_tenant"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}

func TestMetrics_RecordingAndEgressLifecycleExposed(t *testing.T) {
	m := NewMetrics()
	// Per-room participant gauges.
	m.SetParticipantsInRoom("acme", "standup", 7)
	m.SetParticipantsInRoom("acme", "retro", 0) // 0 → removed, must not appear
	m.SetParticipantsInRoom("globex", "all-hands", 3)

	// Recording lifecycle: one started, one available (mirrors to egress
	// lifecycle started/completed + in-progress gauge), one failed.
	m.ObserveRecordingLifecycle(RecordingStateRecording) // started, in-progress=1
	m.ObserveRecordingLifecycle(RecordingStateAvailable) // completed, in-progress=0
	m.ObserveRecordingLifecycle(RecordingStateRecording) // started, in-progress=1
	m.ObserveRecordingLifecycle(RecordingStateFailed)    // failed, in-progress=0
	m.ObserveRecordingBytes(2048, 600000)

	// A retention sweep result folds into the retention counters.
	m.ObserveRetentionSweep(RetentionSweepResult{Expired: 2, Deleted: 1, DeleteErrs: 1, BytesFreed: 4096})
	m.ObserveRecordingLifecycle(RecordingStateExpired)
	m.ObserveRecordingLifecycle(RecordingStateDeleted)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, s := range []string{
		`vulos_meet_participants_in_room{tenant="acme",room="standup"} 7`,
		`vulos_meet_participants_in_room{tenant="globex",room="all-hands"} 3`,
		`vulos_meet_egress_lifecycle_total{event="started"} 2`,
		`vulos_meet_egress_lifecycle_total{event="completed"} 1`,
		`vulos_meet_egress_lifecycle_total{event="failed"} 1`,
		`vulos_meet_egress_in_progress 0`,
		`vulos_meet_recording_lifecycle_total{state="recording"} 2`,
		`vulos_meet_recording_lifecycle_total{state="available"} 1`,
		`vulos_meet_recording_lifecycle_total{state="failed"} 1`,
		`vulos_meet_recording_lifecycle_total{state="expired"} 1`,
		`vulos_meet_recording_lifecycle_total{state="deleted"} 1`,
		`vulos_meet_recording_bytes_total 2048`,
		`vulos_meet_recording_duration_ms_total 600000`,
		`vulos_meet_retention_expired_total 2`,
		`vulos_meet_retention_deleted_total 1`,
		`vulos_meet_retention_delete_errors_total 1`,
		`vulos_meet_retention_bytes_freed_total 4096`,
	} {
		if !strings.Contains(body, s) {
			t.Fatalf("missing %q in:\n%s", s, body)
		}
	}
	// The zeroed room must NOT appear as a stale series.
	if strings.Contains(body, `room="retro"`) {
		t.Fatalf("a zero-participant room must be removed from the gauge:\n%s", body)
	}
}

func TestMetrics_ParticipantGaugeFedByAdminList(t *testing.T) {
	admin, rooms := newTestAdminServer(t)
	m := NewMetrics()
	admin.SetMetrics(m)

	rooms.CreateRoom(context.Background(), "acme:standup")
	rooms.SetParticipants("acme:standup", 5)
	rooms.CreateRoom(context.Background(), "acme:retro")
	rooms.SetParticipants("acme:retro", 2)
	rooms.CreateRoom(context.Background(), "globex:secret") // other tenant — must not leak
	rooms.SetParticipants("globex:secret", 9)

	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `vulos_meet_participants_in_room{tenant="acme",room="standup"} 5`) {
		t.Fatalf("missing acme/standup participant gauge:\n%s", body)
	}
	if !strings.Contains(body, `vulos_meet_participants_in_room{tenant="acme",room="retro"} 2`) {
		t.Fatalf("missing acme/retro participant gauge:\n%s", body)
	}
	// A list for tenant acme must NOT expose globex's rooms on the gauge.
	if strings.Contains(body, "globex") {
		t.Fatalf("cross-tenant room leaked into participant gauge:\n%s", body)
	}
}

func TestMetrics_NilSafeNewCalls(t *testing.T) {
	var m *Metrics
	m.SetParticipantsInRoom("acme", "r", 1)
	m.ResetParticipantsForTenant("acme")
	m.ObserveEgressLifecycle(EgressLifecycleStarted)
	m.ObserveRecordingLifecycle(RecordingStateAvailable)
	m.ObserveRecordingBytes(1, 1)
	m.ObserveRetentionSweep(RetentionSweepResult{})
}

// mintTokenWrongTenant produces a token whose `name` (tenant audience) does
// not match the room prefix — the canonical replay-attempt shape
// Validator.Validate must reject as ErrTokenWrongTenant.
func mintTokenWrongTenant(t *testing.T) string {
	t.Helper()
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test").SetName("evil").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}
