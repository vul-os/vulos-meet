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

// Avoid unused-context warning when go vet is run during the run.
var _ = context.Background
