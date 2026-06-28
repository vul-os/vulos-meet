// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

// Metering / usage-emit tests.
//
// The usage receiver meters participant-minutes per room and reports them to
// cp via the UsageReporter seam when each room finishes. These tests cover:
//
//   - Emit happens exactly once per room lifecycle (on room_finished), not on
//     individual participant events.
//   - Peak participant tracking is accurate across joins/leaves.
//   - Multiple rooms tracked simultaneously; they don't cross-contaminate.
//   - Cross-tenant usage isolation: tenant A's usage is never reported to B.
//   - Metrics counters (room started/finished, meet-minutes gauge) are
//     updated at the right lifecycle moments.
//   - A zero-minute room (nobody joined) emits no usage report.
package wrap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMeteringEmit_EmitExactlyOnceOnRoomFinished verifies the reporter is
// called exactly once per room lifecycle — on room_finished — and not on
// individual participant_joined or participant_left events.
func TestMeteringEmit_EmitExactlyOnceOnRoomFinished(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := newMeteringTestServer(t, rx.Handler())

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }

	post(`{"event":"room_started","room":{"name":"acme:daily"}}`)
	// participant_joined and participant_left must NOT trigger a report.
	post(`{"event":"participant_joined","room":{"name":"acme:daily"},"participant":{"identity":"alice"}}`)
	clk.advance(5 * time.Minute)
	post(`{"event":"participant_left","room":{"name":"acme:daily"},"participant":{"identity":"alice"}}`)

	// No reports yet.
	if reps := rep.all(); len(reps) != 0 {
		t.Fatalf("expected no reports before room_finished, got %+v", reps)
	}

	// room_finished triggers exactly one report.
	post(`{"event":"room_finished","room":{"name":"acme:daily"}}`)
	if reps := rep.all(); len(reps) != 1 {
		t.Fatalf("expected exactly 1 report on room_finished, got %d: %+v", len(reps), reps)
	}

	// A second room_finished for the SAME room (e.g. spurious re-delivery)
	// must not double-report; the room is removed from the tracker on finish.
	post(`{"event":"room_finished","room":{"name":"acme:daily"}}`)
	if reps := rep.all(); len(reps) != 1 {
		t.Fatalf("spurious second room_finished caused an extra report: %+v", reps)
	}
}

// TestMeteringEmit_PeakParticipantTracking verifies the peak concurrent
// participant count is correctly tracked as participants join and leave.
func TestMeteringEmit_PeakParticipantTracking(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, nil, clk)
	srv := newMeteringTestServer(t, rx.Handler())

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }

	post(`{"event":"room_started","room":{"name":"acme:peaktest"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:peaktest"},"participant":{"identity":"a"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:peaktest"},"participant":{"identity":"b"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:peaktest"},"participant":{"identity":"c"}}`)
	// Peak so far: 3.
	post(`{"event":"participant_left","room":{"name":"acme:peaktest"},"participant":{"identity":"a"}}`)
	post(`{"event":"participant_left","room":{"name":"acme:peaktest"},"participant":{"identity":"b"}}`)
	// Drop to 1 — peak remains 3.
	post(`{"event":"participant_joined","room":{"name":"acme:peaktest"},"participant":{"identity":"d"}}`)
	// Now at 2 (c and d). Peak remains 3.

	snaps := rx.Snapshot("acme")
	if len(snaps) != 1 {
		t.Fatalf("expected 1 tracked room, got %d", len(snaps))
	}
	s := snaps[0]
	if s.PeakParticipants != 3 {
		t.Errorf("PeakParticipants: got %d want 3", s.PeakParticipants)
	}
	if s.LiveParticipants != 2 { // c and d are live
		t.Errorf("LiveParticipants: got %d want 2", s.LiveParticipants)
	}
	if s.TotalJoins != 4 { // a, b, c, d each joined once
		t.Errorf("TotalJoins: got %d want 4", s.TotalJoins)
	}
}

// TestMeteringEmit_MultipleRoomsSimultaneous verifies that two rooms tracked
// at the same time have independent participant-minute accumulators.
func TestMeteringEmit_MultipleRoomsSimultaneous(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := newMeteringTestServer(t, rx.Handler())

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }

	// Start two rooms and add one participant each.
	post(`{"event":"room_started","room":{"name":"acme:room1"}}`)
	post(`{"event":"room_started","room":{"name":"acme:room2"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:room1"},"participant":{"identity":"p1"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:room2"},"participant":{"identity":"p2"}}`)

	clk.advance(10 * time.Minute) // both accrue 10 participant-minutes each

	post(`{"event":"room_finished","room":{"name":"acme:room1"}}`)
	post(`{"event":"room_finished","room":{"name":"acme:room2"}}`)

	reps := rep.all()
	if len(reps) != 2 {
		t.Fatalf("expected 2 reports (one per room), got %d: %+v", len(reps), reps)
	}
	for _, r := range reps {
		if r.count != 10 {
			t.Errorf("expected 10 participant-minutes per room, got %d (idemKey=%s)", r.count, r.idemKey)
		}
		if r.account != "acme" {
			t.Errorf("account: got %q want acme", r.account)
		}
	}
}

// TestMeteringEmit_CrossTenantIsolation verifies that participant events for
// tenant A do not pollute tenant B's snapshot or report.
func TestMeteringEmit_CrossTenantIsolation(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := newMeteringTestServer(t, rx.Handler())

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }

	post(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	post(`{"event":"room_started","room":{"name":"globex:weekly"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:standup"},"participant":{"identity":"alice"}}`)
	post(`{"event":"participant_joined","room":{"name":"globex:weekly"},"participant":{"identity":"bob"}}`)

	clk.advance(20 * time.Minute)

	post(`{"event":"room_finished","room":{"name":"acme:standup"}}`)
	post(`{"event":"room_finished","room":{"name":"globex:weekly"}}`)

	reps := rep.all()
	if len(reps) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reps))
	}

	acmeMinutes, globexMinutes := int64(0), int64(0)
	for _, r := range reps {
		switch r.account {
		case "acme":
			acmeMinutes += r.count
		case "globex":
			globexMinutes += r.count
		default:
			t.Fatalf("unexpected account %q in report", r.account)
		}
	}
	if acmeMinutes != 20 {
		t.Errorf("acme participant-minutes: got %d want 20", acmeMinutes)
	}
	if globexMinutes != 20 {
		t.Errorf("globex participant-minutes: got %d want 20", globexMinutes)
	}

	// Snapshot isolation: after room_finished the rooms are removed.
	if snaps := rx.Snapshot("acme"); len(snaps) != 0 {
		t.Errorf("expected empty acme snapshot after room_finished, got %+v", snaps)
	}
	if snaps := rx.Snapshot("globex"); len(snaps) != 0 {
		t.Errorf("expected empty globex snapshot after room_finished, got %+v", snaps)
	}
}

// TestMeteringEmit_MetricsRoomCounters verifies that the Metrics registry
// records room started/finished events. This exercises the metrics seam so
// vulos_meet_meet_rooms_total is observable on the /metrics scrape target.
func TestMeteringEmit_MetricsRoomCounters(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	m := NewMetrics()
	cfg := UsageReceiverConfig{
		Tenant:    NewTenant(""),
		APIKey:    testAPIKey,
		APISecret: testAPISecret,
		Metrics:   m,
		Now:       clk.now,
	}
	rx, err := NewUsageReceiver(cfg)
	if err != nil {
		t.Fatalf("new usage receiver: %v", err)
	}
	srv := newMeteringTestServer(t, rx.Handler())

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }
	post(`{"event":"room_started","room":{"name":"acme:m1"}}`)
	post(`{"event":"room_finished","room":{"name":"acme:m1"}}`)

	// Verify the metrics surface contains the room-count entries.
	metricsSrv := newMeteringTestServer(t, m.Handler())
	resp, err := http.Get(metricsSrv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)

	if !strings.Contains(body, "vulos_meet_meet_rooms_total") {
		t.Errorf("metrics missing vulos_meet_meet_rooms_total:\n%s", body)
	}
}

// TestMeteringEmit_MultipleZeroMinuteRoomsNoReport verifies that multiple
// empty rooms (no participants) finish without generating any usage reports.
func TestMeteringEmit_MultipleZeroMinuteRoomsNoReport(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := newMeteringTestServer(t, rx.Handler())

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }

	for _, name := range []string{"acme:e1", "acme:e2", "acme:e3"} {
		post(`{"event":"room_started","room":{"name":"` + name + `"}}`)
		post(`{"event":"room_finished","room":{"name":"` + name + `"}}`) // no participants → 0 minutes
	}

	if reps := rep.all(); len(reps) != 0 {
		t.Fatalf("expected no reports for zero-minute rooms, got %d: %+v", len(reps), reps)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers shared by metering_emit_test.go
// ─────────────────────────────────────────────────────────────────────────────

// newMeteringTestServer wraps httptest.NewServer with automatic Close cleanup.
func newMeteringTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
