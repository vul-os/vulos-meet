// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"

	"github.com/vul-os/vulos-meet/internal/cp"
	"github.com/vul-os/vulos-meet/internal/wrap"
)

// e2e fixtures: the shared LiveKit key/secret the usage receiver verifies
// webhook signatures against.
const (
	e2eAPIKey    = "APItestkey"
	e2eAPISecret = "supersecretvalueof32bytesplus_padding"
)

// signUsageWebhook reproduces LiveKit's notifier signing for the given event
// body so the usage receiver accepts it.
func signUsageWebhook(t *testing.T, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	at := auth.NewAccessToken(e2eAPIKey, e2eAPISecret).
		SetValidFor(5 * time.Minute).
		SetSha256(base64.StdEncoding.EncodeToString(sum[:]))
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("sign webhook: %v", err)
	}
	return tok
}

// capturedUsage is one /api/usage POST observed by the stub control plane.
type capturedUsage struct {
	Product        string `json:"product"`
	AccountID      string `json:"account_id"`
	Kind           string `json:"kind"`
	Count          int64  `json:"count"`
	IdempotencyKey string `json:"idempotency_key"`
	relayAuth      string
	idemHeader     string
	path           string
}

// TestE2E_RoomFinishedDrivesUsagePOST is the end-to-end seam test the metering
// pipeline previously lacked: a signed room_finished webhook delivered to the
// real wrap.UsageReceiver — wired to the real cp.UsageClient — must result in a
// single /api/usage POST to the (stub) control plane carrying the expected
// {product:meet, kind:meet_minutes, count, idempotency_key} body, the
// X-Relay-Auth header, and a matching Idempotency-Key header.
//
// This is the load-bearing assurance that the cloud minutes-budget gate (which
// reads the externally-written usage bucket) can actually trip: if reporting
// silently stopped driving POSTs, this test fails.
func TestE2E_RoomFinishedDrivesUsagePOST(t *testing.T) {
	var mu sync.Mutex
	var got []capturedUsage
	cpStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var c capturedUsage
		_ = json.Unmarshal(raw, &c)
		c.relayAuth = r.Header.Get("X-Relay-Auth")
		c.idemHeader = r.Header.Get("Idempotency-Key")
		c.path = r.URL.Path
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer cpStub.Close()

	// Real cp client, pointed at the stub CP — the same construction main.go
	// performs when CP_URL is set.
	client, err := cp.NewUsageClient(cp.Config{
		URL:          cpStub.URL,
		SharedSecret: "relay-secret",
		BaseBackoff:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new cp client: %v", err)
	}

	// Controllable clock so the lifecycle accrues a deterministic, whole minute
	// (a real-time test span would round to zero minutes and report nothing).
	var clkMu sync.Mutex
	clk := time.Unix(1_700_000_000, 0)
	now := func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		return clk
	}
	advance := func(d time.Duration) {
		clkMu.Lock()
		defer clkMu.Unlock()
		clk = clk.Add(d)
	}

	// Real usage receiver wired to the real cp client through the wrap seam.
	rx, err := wrap.NewUsageReceiver(wrap.UsageReceiverConfig{
		Tenant:    wrap.NewTenant(""),
		APIKey:    e2eAPIKey,
		APISecret: e2eAPISecret,
		Reporter:  client,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("new usage receiver: %v", err)
	}
	meetSrv := httptest.NewServer(rx.Handler())
	defer meetSrv.Close()

	post := func(body string) {
		tok := signUsageWebhook(t, []byte(body))
		req, _ := http.NewRequest(http.MethodPost, meetSrv.URL+wrap.UsageWebhookPath, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", tok)
		req.Header.Set("Content-Type", "application/webhook+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post webhook: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("webhook status: %d", resp.StatusCode)
		}
	}

	// Drive a minimal room lifecycle: start, one participant joins, then the
	// room finishes with the participant still live — accruing real minutes.
	post(`{"event":"room_started","room":{"name":"acme:standup"}}`)
	post(`{"event":"participant_joined","room":{"name":"acme:standup"},"participant":{"identity":"alice"}}`)
	// Advance the injected clock a full 5 minutes so the room accrues a
	// deterministic 5 participant-minutes before it finishes.
	advance(5 * time.Minute)
	post(`{"event":"room_finished","room":{"name":"acme:standup"}}`)

	// Close flushes the cp client's queue and waits for the background POST.
	if err := client.Close(); err != nil {
		t.Fatalf("close cp client: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 /api/usage POST from room_finished, got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.path != "/api/usage" {
		t.Fatalf("path: %q", c.path)
	}
	if c.relayAuth != "relay-secret" {
		t.Fatalf("X-Relay-Auth: %q", c.relayAuth)
	}
	if c.Product != "meet" {
		t.Fatalf("product: %q", c.Product)
	}
	if c.Kind != "meet_minutes" {
		t.Fatalf("kind: %q", c.Kind)
	}
	if c.AccountID != "acme" {
		t.Fatalf("account_id: %q", c.AccountID)
	}
	if c.Count != 5 {
		t.Fatalf("count: %d (expected 5 metered participant-minutes)", c.Count)
	}
	if c.IdempotencyKey == "" {
		t.Fatalf("missing idempotency_key in body")
	}
	if c.idemHeader != c.IdempotencyKey {
		t.Fatalf("Idempotency-Key header %q != body key %q", c.idemHeader, c.IdempotencyKey)
	}
}
