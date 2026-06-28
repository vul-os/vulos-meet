// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz_Returns200AndBody(t *testing.T) {
	h := NewHealthzHandler("0.0.1-test")
	req := httptest.NewRequest("GET", "/healthz", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rw.Code)
	}
	var resp HealthzResponse
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status field: got %q want ok", resp.Status)
	}
	if resp.Version != "0.0.1-test" {
		t.Fatalf("version field: got %q want 0.0.1-test", resp.Version)
	}
}

func TestHealthz_EmptyVersionDefaulted(t *testing.T) {
	h := NewHealthzHandler("")
	req := httptest.NewRequest("GET", "/healthz", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rw.Code)
	}
	var resp HealthzResponse
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version == "" {
		t.Fatalf("version must not be empty when not provided; got empty string")
	}
}

// TestHealthz_ContentTypeJSON verifies the response carries the JSON content-type
// so clients can parse it without content-sniffing.
func TestHealthz_ContentTypeJSON(t *testing.T) {
	h := NewHealthzHandler("v1")
	req := httptest.NewRequest("GET", "/healthz", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	ct := rw.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("Content-Type: got %q want application/json", ct)
	}
}

// TestHealthz_MountedOnSignalGate verifies /healthz is reachable on the
// signal-gate listener's sibling handler and returns the expected body.
// This mirrors how main.go mounts it via siblingMux.Handle("GET /healthz", ...).
func TestHealthz_MountedOnSignalGate(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)

	sibling := http.NewServeMux()
	sibling.Handle("GET /healthz", NewHealthzHandler("0.0.2-gate"))

	gate := httptest.NewServer(g.Handler(sibling, nil))
	defer gate.Close()

	resp, err := http.Get(gate.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var body HealthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.Version != "0.0.2-gate" {
		t.Fatalf("body: %+v", body)
	}
	// /healthz must NOT forward anything to the LiveKit upstream.
	if len(f.hits) != 0 {
		t.Fatalf("healthz should not reach the livekit upstream: %v", f.hits)
	}
}

// TestHealthz_NoTokenRequired verifies /healthz is unauthenticated — no admin
// token is needed. This is intentional: load balancers probe without creds.
func TestHealthz_NoTokenRequired(t *testing.T) {
	h := NewHealthzHandler("v1")
	// No Authorization header at all.
	req := httptest.NewRequest("GET", "/healthz", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("unauthenticated healthz: got %d want 200", rw.Code)
	}
}
