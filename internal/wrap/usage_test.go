// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeReporter records the usage reports the receiver emits.
type fakeReporter struct {
	mu      sync.Mutex
	reports []reportCall
}

type reportCall struct {
	account string
	kind    string
	count   int64
	idemKey string
}

func (f *fakeReporter) Report(account, kind string, count int64, idempotencyKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, reportCall{account, kind, count, idempotencyKey})
}

func (f *fakeReporter) all() []reportCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]reportCall, len(f.reports))
	copy(out, f.reports)
	return out
}

// fakeClock is a controllable clock for deterministic minute math.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newUsageReceiverForTest(t *testing.T, rep UsageReporter, clk *fakeClock) *UsageReceiver {
	t.Helper()
	cfg := UsageReceiverConfig{
		Tenant:    NewTenant(""),
		APIKey:    testAPIKey,
		APISecret: testAPISecret,
		Reporter:  rep,
	}
	if clk != nil {
		cfg.Now = clk.now
	}
	rx, err := NewUsageReceiver(cfg)
	if err != nil {
		t.Fatalf("new usage receiver: %v", err)
	}
	return rx
}

// postSignedUsage posts a signed webhook event to the usage receiver server.
func postSignedUsage(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	tok := signLiveKitWebhook(t, body)
	req, _ := http.NewRequest("POST", url+UsageWebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	req.Header.Set("Content-Type", "application/webhook+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func TestUsage_VerifiesSignature(t *testing.T) {
	rx := newUsageReceiverForTest(t, nil, nil)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	resp := postSignedUsage(t, srv.URL, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestUsage_RejectsBadSignature(t *testing.T) {
	rx := newUsageReceiverForTest(t, nil, nil)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	wrongTok := signLiveKitWebhook(t, []byte(`{"event":"other"}`))
	req, _ := http.NewRequest("POST", srv.URL+UsageWebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", wrongTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d (expected 401)", resp.StatusCode)
	}
}

func TestUsage_RejectsMissingAuth(t *testing.T) {
	rx := newUsageReceiverForTest(t, nil, nil)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	resp, err := http.Post(srv.URL+UsageWebhookPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestUsage_DropsEventWithoutTenantPrefix(t *testing.T) {
	rx := newUsageReceiverForTest(t, nil, nil)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"room_started","room":{"name":"standup"}}`)
	resp := postSignedUsage(t, srv.URL, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %d (expected 400 — must not meter a no-tenant event)", resp.StatusCode)
	}
}

// TestUsage_ComputesParticipantMinutes drives a full room lifecycle and checks
// that the reported minutes equal the sum of each participant's join→leave span.
//
//	t=0   room_started
//	t=0   alice joins
//	t=10m bob joins
//	t=20m alice leaves   -> alice = 20m (t=0..20)
//	t=30m room_finished  -> bob = 20m   (t=10..30, still live at finish)
//	total = 40 participant-minutes
func TestUsage_ComputesParticipantMinutes(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	post := func(body string) {
		resp := postSignedUsage(t, srv.URL, []byte(body))
		resp.Body.Close()
	}

	post(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:standup"},"participant":{"identity":"alice"}}`)
	clk.advance(10 * time.Minute)
	post(`{"event":"participant_joined","room":{"name":"acme:standup"},"participant":{"identity":"bob"}}`)
	clk.advance(10 * time.Minute)
	post(`{"event":"participant_left","room":{"name":"acme:standup"},"participant":{"identity":"alice"}}`)
	clk.advance(10 * time.Minute)
	post(`{"event":"room_finished","room":{"name":"acme:standup"}}`)

	reps := rep.all()
	if len(reps) != 1 {
		t.Fatalf("expected 1 report on room_finished, got %d: %+v", len(reps), reps)
	}
	r := reps[0]
	if r.account != "acme" || r.kind != UsageKindMeetMinutes {
		t.Fatalf("report account/kind: %+v", r)
	}
	if r.count != 40 {
		t.Fatalf("participant-minutes: got %d want 40", r.count)
	}
	// The receiver must attach a deterministic idempotency key so a reporter
	// retry cannot double-count. Key is meet:<tenant>:<short>:<start>:<finish>.
	wantKey := idempotencyKeyFor("acme", "standup", time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000+30*60, 0))
	if r.idemKey != wantKey {
		t.Fatalf("idempotency key: got %q want %q", r.idemKey, wantKey)
	}
}

// TestUsage_IdempotencyKeyDeterministicAndDistinct asserts the key is stable for
// a given room lifecycle (so retries dedupe) yet distinct across separate
// lifecycles of the same short room (so a later meeting is not deduped away).
func TestUsage_IdempotencyKeyDeterministicAndDistinct(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	a := idempotencyKeyFor("acme", "standup", t0, t0.Add(10*time.Minute))
	b := idempotencyKeyFor("acme", "standup", t0, t0.Add(10*time.Minute))
	if a != b {
		t.Fatalf("same lifecycle must produce the same key: %q vs %q", a, b)
	}
	// A second meeting in the same short room (different start/finish) must NOT
	// collide with the first.
	later := idempotencyKeyFor("acme", "standup", t0.Add(time.Hour), t0.Add(time.Hour+10*time.Minute))
	if later == a {
		t.Fatalf("distinct lifecycles must produce distinct keys, both = %q", a)
	}
	// Cross-tenant must not collide either.
	if other := idempotencyKeyFor("globex", "standup", t0, t0.Add(10*time.Minute)); other == a {
		t.Fatalf("cross-tenant keys must differ, both = %q", a)
	}
}

// TestUsage_DuplicateJoinDoesNotDoubleCount asserts a redelivered join for a
// still-live participant does not reset the span or double the joins.
func TestUsage_DuplicateJoinDoesNotDoubleCount(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }

	post(`{"event":"room_started","room":{"name":"acme:r"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:r"},"participant":{"identity":"alice"}}`)
	clk.advance(5 * time.Minute)
	// redelivered join — must NOT reset alice's join span back to now.
	post(`{"event":"participant_joined","room":{"name":"acme:r"},"participant":{"identity":"alice"}}`)
	clk.advance(5 * time.Minute)
	post(`{"event":"room_finished","room":{"name":"acme:r"}}`)

	reps := rep.all()
	if len(reps) != 1 || reps[0].count != 10 {
		t.Fatalf("expected 10 minutes from a single 10m span, got %+v", reps)
	}
}

// TestUsage_NoReporterStillTracks confirms that with no cp reporter wired the
// receiver still verifies + tracks (admin snapshot works), reporting nothing.
func TestUsage_NoReporterStillTracks(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, nil, clk)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }
	post(`{"event":"room_started","room":{"name":"acme:live"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:live"},"participant":{"identity":"alice"}}`)
	clk.advance(3 * time.Minute)

	snaps := rx.Snapshot("acme")
	if len(snaps) != 1 {
		t.Fatalf("expected 1 tracked room, got %d", len(snaps))
	}
	s := snaps[0]
	if s.Room != "live" || s.LiveParticipants != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
	if s.ParticipantMinutes < 2.99 || s.ParticipantMinutes > 3.01 {
		t.Fatalf("live participant-minutes: got %v want ~3", s.ParticipantMinutes)
	}
	// Cross-tenant isolation: another tenant sees nothing.
	if other := rx.Snapshot("globex"); len(other) != 0 {
		t.Fatalf("cross-tenant snapshot leak: %+v", other)
	}
}

// TestUsage_RoomFinishedWithNoMinutesReportsNothing confirms a room that
// finishes with zero accrued minutes does not POST a zero-count report.
func TestUsage_RoomFinishedWithNoMinutesReportsNothing(t *testing.T) {
	rep := &fakeReporter{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rx := newUsageReceiverForTest(t, rep, clk)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	post := func(body string) { postSignedUsage(t, srv.URL, []byte(body)).Body.Close() }
	post(`{"event":"room_started","room":{"name":"acme:empty"}}`)
	post(`{"event":"room_finished","room":{"name":"acme:empty"}}`)

	if reps := rep.all(); len(reps) != 0 {
		t.Fatalf("expected no report for a zero-minute room, got %+v", reps)
	}
}

func TestUsage_RequiresInputs(t *testing.T) {
	if _, err := NewUsageReceiver(UsageReceiverConfig{}); err == nil {
		t.Fatalf("expected error with empty config")
	}
	if _, err := NewUsageReceiver(UsageReceiverConfig{Tenant: NewTenant("")}); err == nil {
		t.Fatalf("expected error with no api creds")
	}
}
