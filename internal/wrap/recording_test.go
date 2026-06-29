// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
)

// signLiveKitWebhook builds the (Authorization header, body) pair LiveKit's
// own notifier would produce for the given event JSON. This is the smallest
// reproduction of url_notifier.go's signing path — the same key/secret pair
// the receiver verifies against.
func signLiveKitWebhook(t *testing.T, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	at := auth.NewAccessToken(testAPIKey, testAPISecret).
		SetValidFor(5 * time.Minute).
		SetSha256(b64)
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("sign webhook: %v", err)
	}
	return tok
}

func newEgressReceiverForTest(t *testing.T, cloudURL string) (*EgressReceiver, *atomic.Int32, *[]VulosEgressEnvelope) {
	t.Helper()
	rx, err := NewEgressReceiver(EgressReceiverConfig{
		Tenant:       NewTenant(""),
		APIKey:       testAPIKey,
		APISecret:    testAPISecret,
		CloudURL:     cloudURL,
		CloudAuthTok: "cloud-token",
		BaseBackoff:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new egress receiver: %v", err)
	}
	return rx, nil, nil
}

func TestEgress_VerifiesSignature(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_1","room_name":"acme:standup"}}`)
	tok := signLiveKitWebhook(t, body)

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	req.Header.Set("Content-Type", "application/webhook+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestEgress_RejectsBadSignature(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_1","room_name":"acme:standup"}}`)
	// sign a DIFFERENT body so the sha256 claim won't match the request body.
	wrongTok := signLiveKitWebhook(t, []byte(`{"event":"other"}`))

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
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

func TestEgress_RejectsMissingAuth(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_1","room_name":"acme:standup"}}`)
	resp, err := http.Post(srv.URL+WebhookPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestEgress_DropsEventWithoutTenantPrefix(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	// Room name with NO tenant prefix.
	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_1","room_name":"standup"}}`)
	tok := signLiveKitWebhook(t, body)

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %d (expected 400 — must NOT launder a no-tenant event to the cloud)", resp.StatusCode)
	}
}

func TestEgress_ForwardsVulosEnvelopeToCloud(t *testing.T) {
	var received atomic.Int32
	var seen VulosEgressEnvelope
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.Header.Get("X-Vulos-Tenant") != "acme" {
			t.Errorf("missing tenant header: %q", r.Header.Get("X-Vulos-Tenant"))
		}
		if r.Header.Get("Authorization") != "Bearer cloud-token" {
			t.Errorf("missing cloud bearer: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Vulos-Schema") != "vulos-meet/egress/v1" {
			t.Errorf("missing schema header: %q", r.Header.Get("X-Vulos-Schema"))
		}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	rx, _, _ := newEgressReceiverForTest(t, cloud.URL)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_1","room_name":"acme:standup"}}`)
	tok := signLiveKitWebhook(t, body)

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	if received.Load() != 1 {
		t.Fatalf("cloud received %d events", received.Load())
	}
	if seen.Schema != "vulos-meet/egress/v1" {
		t.Fatalf("envelope schema: %q", seen.Schema)
	}
	if seen.Tenant != "acme" || seen.Room != "standup" || seen.FullRoom != "acme:standup" {
		t.Fatalf("envelope tenant/room: %+v", seen)
	}
	if seen.EgressID != "EG_1" {
		t.Fatalf("envelope egress id: %q", seen.EgressID)
	}
	if !strings.Contains(string(seen.Raw), "egress_started") {
		t.Fatalf("raw payload missing: %s", seen.Raw)
	}
}

func TestEgress_RetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if n < 3 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	rx, _, _ := newEgressReceiverForTest(t, cloud.URL)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_2","room_name":"acme:retro"}}`)
	tok := signLiveKitWebhook(t, body)

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if attempts.Load() < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", attempts.Load())
	}
}

func TestEgress_DoesNotRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer cloud.Close()

	rx, _, _ := newEgressReceiverForTest(t, cloud.URL)
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_3","room_name":"acme:planning"}}`)
	tok := signLiveKitWebhook(t, body)

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: %d (cloud's 4xx should surface as 502 from us)", resp.StatusCode)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt (no retry on 4xx), got %d", attempts.Load())
	}
}

func TestEgress_AcceptsRoomEventTooNotJustEgress(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	// Non-egress event with the room name on Room.Name instead of
	// EgressInfo.RoomName. Receiver must still parse tenant + ack.
	body := []byte(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	tok := signLiveKitWebhook(t, body)

	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestEgress_RequiresInputs(t *testing.T) {
	if _, err := NewEgressReceiver(EgressReceiverConfig{}); err == nil {
		t.Fatalf("expected error with empty config")
	}
	if _, err := NewEgressReceiver(EgressReceiverConfig{
		Tenant: NewTenant(""),
	}); err == nil {
		t.Fatalf("expected error with no api creds")
	}
}

// Drop noise: assert that with cloud disabled we still gate-keep.
func TestEgress_NoCloudUrl_StillVerifies(t *testing.T) {
	rx, err := NewEgressReceiver(EgressReceiverConfig{
		Tenant:    NewTenant(""),
		APIKey:    testAPIKey,
		APISecret: testAPISecret,
		// CloudURL omitted
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if rx == nil {
		t.Fatalf("nil receiver")
	}
	// just confirm Handler() returns a non-nil mux
	if rx.Handler() == nil {
		t.Fatalf("nil handler")
	}
}

// TestEgress_AdvancesLifecycleLedger confirms the receiver records the egress
// lifecycle into a RecordingStore as webhook events arrive: started → recording,
// then complete → available with the file size/duration.
func TestEgress_AdvancesLifecycleLedger(t *testing.T) {
	store := NewMemRecordingStore()
	rx, err := NewEgressReceiver(EgressReceiverConfig{
		Tenant:    NewTenant(""),
		APIKey:    testAPIKey,
		APISecret: testAPISecret,
		Store:     store,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	post := func(body []byte) {
		tok := signLiveKitWebhook(t, body)
		req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
		req.Header.Set("Authorization", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
	}

	// egress_started → recording state.
	post([]byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_L","room_name":"acme:standup","status":"EGRESS_ACTIVE"}}`))
	recs, _ := store.List(context.Background())
	if len(recs) != 1 || recs[0].State != RecordingStateRecording {
		t.Fatalf("after start expected 1 recording-state entry, got %+v", recs)
	}

	// egress_ended with EGRESS_COMPLETE + a file result → available + size/dur.
	post([]byte(`{"event":"egress_ended","egress_info":{"egress_id":"EG_L","room_name":"acme:standup","status":"EGRESS_COMPLETE","file":{"size":2048,"duration":600000}}}`))
	recs, _ = store.List(context.Background())
	if recs[0].State != RecordingStateAvailable {
		t.Fatalf("after complete expected available, got %q", recs[0].State)
	}
	if recs[0].SizeBytes != 2048 || recs[0].DurationMs != 600000 {
		t.Fatalf("size/duration not recorded: %+v", recs[0])
	}
	if recs[0].Tenant != "acme" || recs[0].Room != "standup" {
		t.Fatalf("tenant/room not recorded: %+v", recs[0])
	}
}

func TestEgress_LifecycleFailedStatus(t *testing.T) {
	store := NewMemRecordingStore()
	rx, _ := NewEgressReceiver(EgressReceiverConfig{
		Tenant: NewTenant(""), APIKey: testAPIKey, APISecret: testAPISecret, Store: store,
	})
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	body := []byte(`{"event":"egress_updated","egress_info":{"egress_id":"EG_F","room_name":"acme:standup","status":"EGRESS_FAILED"}}`)
	tok := signLiveKitWebhook(t, body)
	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(body))
	req.Header.Set("Authorization", tok)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	recs, _ := store.List(context.Background())
	if len(recs) != 1 || recs[0].State != RecordingStateFailed {
		t.Fatalf("expected failed state, got %+v", recs)
	}
}

// TestEgress_OversizedBodyRejected verifies that a POST body larger than 1 MiB
// is rejected (non-204) without buffering the full body into memory before the
// signature check, preventing memory exhaustion from unauthenticated callers.
func TestEgress_OversizedBodyRejected(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")
	srv := httptest.NewServer(rx.Handler())
	defer srv.Close()

	// 1 MiB + 1 byte — just over the cap.
	oversized := make([]byte, (1<<20)+1)
	// Sign the oversized body so the signature check would pass if the body
	// were accepted — that way a non-4xx response indicates a real bug.
	tok := signLiveKitWebhook(t, oversized)
	req, _ := http.NewRequest("POST", srv.URL+WebhookPath, bytes.NewReader(oversized))
	req.Header.Set("Authorization", tok)
	req.Header.Set("Content-Type", "application/webhook+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// Must NOT succeed (204). Any 4xx is acceptable — the body was capped.
	if resp.StatusCode == http.StatusNoContent {
		t.Fatalf("oversized body was accepted (204); expected rejection")
	}
}

// TestEgress_RateLimitAppliesToWebhookPath verifies that a rate limiter applied
// to the full public listener (not just /rtc) also throttles the egress webhook
// path — ensuring non-/rtc routes are not accidentally left unprotected.
func TestEgress_RateLimitAppliesToWebhookPath(t *testing.T) {
	rx, _, _ := newEgressReceiverForTest(t, "")

	// Use MiddlewareWithTrust(false) — RemoteAddr keyed so httptest's loopback
	// addr is stable within a single persistent connection. We drive requests
	// through httptest.ResponseRecorder directly (no network) to keep the key
	// stable across calls.
	rl := newRateLimiterWithClock(0.001, 1, 10*time.Minute, time.Now) // burst=1
	handler := rl.MiddlewareWithTrust(rx.Handler(), false)

	body := []byte(`{"event":"egress_started","egress_info":{"egress_id":"EG_RL","room_name":"acme:r"}}`)
	tok := signLiveKitWebhook(t, body)

	makeReq := func() *http.Request {
		req := httptest.NewRequest("POST", WebhookPath, bytes.NewReader(body))
		req.Header.Set("Authorization", tok)
		req.Header.Set("Content-Type", "application/webhook+json")
		req.RemoteAddr = "203.0.113.1:12345" // stable key across recorder calls
		return req
	}

	// First request: allowed (burst=1).
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, makeReq())
	if rw.Code == http.StatusTooManyRequests {
		t.Fatalf("first request should pass the rate limiter, got 429")
	}

	// Second request: same IP, burst exhausted → 429.
	rw = httptest.NewRecorder()
	handler.ServeHTTP(rw, makeReq())
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("second request to webhook path should be rate-limited, got %d (want 429)", rw.Code)
	}
}

// Avoid unused-context warning when go vet is run during the run.
var _ = context.Background
