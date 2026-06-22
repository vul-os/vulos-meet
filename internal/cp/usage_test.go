// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package cp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// receivedUsage captures the parsed body + headers of one /api/usage POST.
type receivedUsage struct {
	body       usageEvent
	relayAuth  string
	idemHeader string
	path       string
}

// newCapturingCP spins up an httptest server that records every /api/usage POST.
func newCapturingCP(t *testing.T, status int) (*httptest.Server, *[]receivedUsage, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var got []receivedUsage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var ev usageEvent
		_ = json.Unmarshal(raw, &ev)
		mu.Lock()
		got = append(got, receivedUsage{
			body:       ev,
			relayAuth:  r.Header.Get("X-Relay-Auth"),
			idemHeader: r.Header.Get("Idempotency-Key"),
			path:       r.URL.Path,
		})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	return srv, &got, &mu
}

func TestUsageClient_PostShapeAndHeader(t *testing.T) {
	srv, got, mu := newCapturingCP(t, http.StatusOK)
	defer srv.Close()

	c, err := NewUsageClient(Config{
		URL:          srv.URL,
		SharedSecret: "topsecret",
		BaseBackoff:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	c.ReportMeetMinutes("acme", 42, "meet:acme:standup:100:200")
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("expected 1 POST, got %d", len(*got))
	}
	r := (*got)[0]
	if r.path != "/api/usage" {
		t.Fatalf("path: %q", r.path)
	}
	if r.relayAuth != "topsecret" {
		t.Fatalf("X-Relay-Auth: %q", r.relayAuth)
	}
	if r.body.Product != "meet" {
		t.Fatalf("product: %q", r.body.Product)
	}
	if r.body.AccountID != "acme" {
		t.Fatalf("account_id: %q", r.body.AccountID)
	}
	if r.body.Kind != "meet_minutes" {
		t.Fatalf("kind: %q", r.body.Kind)
	}
	if r.body.Count != 42 {
		t.Fatalf("count: %d", r.body.Count)
	}
	if r.body.IdempotencyKey != "meet:acme:standup:100:200" {
		t.Fatalf("body idempotency_key: %q", r.body.IdempotencyKey)
	}
	if r.idemHeader != "meet:acme:standup:100:200" {
		t.Fatalf("Idempotency-Key header: %q", r.idemHeader)
	}
}

func TestUsageClient_GenericReportShape(t *testing.T) {
	srv, got, mu := newCapturingCP(t, http.StatusOK)
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, SharedSecret: "s", BaseBackoff: time.Millisecond})
	c.Report("globex", KindMeetMinutes, 7, "meet:globex:r:1:2")
	_ = c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 || (*got)[0].body.AccountID != "globex" || (*got)[0].body.Count != 7 {
		t.Fatalf("unexpected capture: %+v", *got)
	}
}

func TestUsageClient_RequiresURL(t *testing.T) {
	if _, err := NewUsageClient(Config{}); err == nil {
		t.Fatalf("expected error when CP_URL is empty (seam must be off, not silently dropping)")
	}
}

func TestUsageClient_IgnoresNonPositiveAndEmpty(t *testing.T) {
	srv, got, mu := newCapturingCP(t, http.StatusOK)
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, BaseBackoff: time.Millisecond})
	c.ReportMeetMinutes("acme", 0, "k1")
	c.ReportMeetMinutes("acme", -5, "k2")
	c.ReportMeetMinutes("", 10, "k3")
	_ = c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("expected no POSTs for non-positive/empty inputs, got %d", len(*got))
	}
}

func TestUsageClient_RetriesOn5xxThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, BaseBackoff: time.Millisecond, MaxAttempts: 5})
	c.ReportMeetMinutes("acme", 3, "k")
	_ = c.Close()

	if attempts.Load() < 3 {
		t.Fatalf("expected at least 3 attempts (retry on 5xx), got %d", attempts.Load())
	}
	if c.Dropped() != 0 {
		t.Fatalf("expected 0 drops on eventual success, got %d", c.Dropped())
	}
}

func TestUsageClient_DoesNotRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, BaseBackoff: time.Millisecond, MaxAttempts: 5})
	c.ReportMeetMinutes("acme", 3, "k")
	_ = c.Close()

	if attempts.Load() != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry on 4xx), got %d", attempts.Load())
	}
	if c.Dropped() != 1 {
		t.Fatalf("expected 1 drop on 4xx reject, got %d", c.Dropped())
	}
}

// TestUsageClient_FireAndForgetDoesNotBlock asserts Report returns immediately
// even when the cp endpoint is slow: the call must not sit on the webhook hot
// path.
func TestUsageClient_FireAndForgetDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the request open
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, BaseBackoff: time.Millisecond})

	start := time.Now()
	c.ReportMeetMinutes("acme", 1, "k")
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Report blocked for %s — must be fire-and-forget", elapsed)
	}
	// Release the held request, THEN close so the worker can drain promptly
	// instead of sitting in Close() while the background POST is parked.
	close(release)
	_ = c.Close()
}

// TestUsageClient_IdempotencyKeyStableAcrossRetries asserts that every delivery
// attempt for one logical event carries the SAME idempotency key (body +
// header), so a 5xx-forced retry cannot double-count: the cp dedupes on a stable
// key, not a per-attempt-fresh one.
func TestUsageClient_IdempotencyKeyStableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var bodyKeys, headerKeys []string
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var ev usageEvent
		_ = json.Unmarshal(raw, &ev)
		mu.Lock()
		bodyKeys = append(bodyKeys, ev.IdempotencyKey)
		headerKeys = append(headerKeys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, BaseBackoff: time.Millisecond, MaxAttempts: 5})
	c.Report("acme", KindMeetMinutes, 9, "meet:acme:standup:100:200")
	_ = c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodyKeys) < 3 {
		t.Fatalf("expected >=3 attempts, got %d", len(bodyKeys))
	}
	for i := range bodyKeys {
		if bodyKeys[i] != "meet:acme:standup:100:200" {
			t.Fatalf("attempt %d body key drifted: %q", i, bodyKeys[i])
		}
		if headerKeys[i] != "meet:acme:standup:100:200" {
			t.Fatalf("attempt %d header key drifted: %q", i, headerKeys[i])
		}
	}
}

// TestUsageClient_DropCounterOnQueueFull asserts that when the bounded queue is
// full the overflow events are counted (not silently lost). We stall the worker
// on a blocking endpoint, fill the size-1 queue + in-flight slot, then overflow.
func TestUsageClient_DropCounterOnQueueFull(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-release // park the worker so the queue cannot drain
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewUsageClient(Config{URL: srv.URL, BaseBackoff: time.Millisecond, QueueSize: 1})
	// 1 event gets pulled by the worker and parks on the held request; 1 sits in
	// the size-1 queue; every further enqueue overflows and is counted dropped.
	for i := 0; i < 20; i++ {
		c.Report("acme", KindMeetMinutes, 1, "k")
	}
	// Give the worker a moment to pull the first event off the queue.
	deadline := time.Now().Add(2 * time.Second)
	for c.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Dropped() == 0 {
		t.Fatalf("expected overflow events to be counted as dropped, got 0")
	}
	close(release)
	_ = c.Close()
}

// TestUsageClient_NilSafe confirms a nil client is a no-op (so the seam being
// off is never a crash).
func TestUsageClient_NilSafe(t *testing.T) {
	var c *UsageClient
	c.ReportMeetMinutes("acme", 5, "k") // must not panic
	c.Report("acme", KindMeetMinutes, 5, "k")
	if c.Dropped() != 0 {
		t.Fatalf("nil Dropped should be 0, got %d", c.Dropped())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}
