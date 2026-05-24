// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
)

// fakeLiveKitSignal stands in for livekit-server's /rtc endpoint during
// gate tests. It records every request it sees so tests can assert that
// (a) valid tokens reach it and (b) invalid tokens never do.
type fakeLiveKitSignal struct {
	hits []string
}

func newFakeLiveKitSignal(t *testing.T, f *fakeLiveKitSignal) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.URL.Path)
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	return srv
}

func newGateForTest(t *testing.T, upstream string) (*SignalGate, *Validator) {
	t.Helper()
	v := newValidatorForTest(t)
	g, err := NewSignalGate(v, upstream)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	return g, v
}

func TestSignalGate_ValidTokenForwarded(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()

	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)

	gate := httptest.NewServer(g.Handler(nil))
	defer gate.Close()

	tok := mintToken(t, "acme", "standup", time.Hour)
	resp, err := http.Get(gate.URL + "/rtc?access_token=" + tok)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d body=%s", resp.StatusCode, body)
	}
	if string(body) != "upstream-ok" {
		t.Fatalf("body: %q", body)
	}
	if len(f.hits) != 1 || f.hits[0] != "/rtc" {
		t.Fatalf("upstream hits: %v", f.hits)
	}
}

func TestSignalGate_NoTokenRejected(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)

	gate := httptest.NewServer(g.Handler(nil))
	defer gate.Close()

	resp, err := http.Get(gate.URL + "/rtc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestSignalGate_MalformedTokenRejected(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)

	gate := httptest.NewServer(g.Handler(nil))
	defer gate.Close()

	resp, err := http.Get(gate.URL + "/rtc?access_token=not-a-jwt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d body=%q", resp.StatusCode, body)
	}
	// Critical: the response body MUST NOT contain the token contents.
	if strings.Contains(string(body), "not-a-jwt") {
		t.Fatalf("rejection body leaked token contents: %q", body)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestSignalGate_WrongTenantRejected_403(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)

	gate := httptest.NewServer(g.Handler(nil))
	defer gate.Close()

	// Token where room prefix says "acme" but the audience (`name`) says
	// "evil". This is the canonical replay-attempt shape.
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test").SetName("evil").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	resp, err := http.Get(gate.URL + "/rtc?access_token=" + tok)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d body=%q (want 403)", resp.StatusCode, body)
	}
	if strings.Contains(string(body), tok) || strings.Contains(string(body), "evil") || strings.Contains(string(body), "acme:standup") {
		t.Fatalf("rejection body leaked token contents: %q", body)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestSignalGate_BearerHeaderAlsoAccepted(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)

	gate := httptest.NewServer(g.Handler(nil))
	defer gate.Close()

	tok := mintToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest("GET", gate.URL+"/rtc", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(f.hits) != 1 {
		t.Fatalf("upstream hits: %v", f.hits)
	}
}

func TestSignalGate_NonRTCRouteSentToSibling(t *testing.T) {
	f := &fakeLiveKitSignal{}
	upstream := newFakeLiveKitSignal(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)

	siblingHit := false
	sibling := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siblingHit = true
		w.WriteHeader(http.StatusTeapot)
	})
	gate := httptest.NewServer(g.Handler(sibling))
	defer gate.Close()

	resp, err := http.Get(gate.URL + "/anything")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if !siblingHit {
		t.Fatalf("sibling handler was not reached")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestSignalGate_RequiresInputs(t *testing.T) {
	v := newValidatorForTest(t)
	if _, err := NewSignalGate(nil, ":7880"); err == nil {
		t.Fatalf("expected error with nil validator")
	}
	if _, err := NewSignalGate(v, ""); err == nil {
		t.Fatalf("expected error with empty upstream")
	}
}

func TestExtractTokenFromRequest_Variants(t *testing.T) {
	// query param
	req, _ := http.NewRequest("GET", "/rtc?access_token=abc", nil)
	if got := extractTokenFromRequest(req); got != "abc" {
		t.Fatalf("query: %q", got)
	}
	// bearer header
	req, _ = http.NewRequest("GET", "/rtc", nil)
	req.Header.Set("Authorization", "Bearer xyz")
	if got := extractTokenFromRequest(req); got != "xyz" {
		t.Fatalf("bearer: %q", got)
	}
	// none
	req, _ = http.NewRequest("GET", "/rtc", nil)
	if got := extractTokenFromRequest(req); got != "" {
		t.Fatalf("none: %q", got)
	}
	// non-bearer
	req, _ = http.NewRequest("GET", "/rtc", nil)
	req.Header.Set("Authorization", "Basic ZGV2OnAxYg==")
	if got := extractTokenFromRequest(req); got != "" {
		t.Fatalf("basic: %q", got)
	}
}
