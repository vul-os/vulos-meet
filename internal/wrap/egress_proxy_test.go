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
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"
)

// fakeLiveKitTwirp stands in for livekit-server's /twirp/livekit.Egress/*
// surface during egress-proxy tests. It records every request path + body
// it sees so tests can assert that (a) valid recording-tokened requests
// reach it, (b) bad-tenant / no-RoomRecord requests do not.
type fakeLiveKitTwirp struct {
	hits           []string
	listEgressHits []string // ListEgress calls (incl. the proxy's ownership probe)
	lastBody       []byte
	lastAuth       string
	lastHeader     http.Header
	respBody       string
	respCT         string
	respCode       int
	respDelay      time.Duration
	// ownerRoom is the fully-qualified room the fake reports as the owner of any
	// egress id queried via ListEgress(egress_id) — this is what the proxy's
	// StopEgress/UpdateLayout/UpdateStream ownership probe reads. Defaults to
	// "acme:standup" (same-tenant). Set it to a foreign room to simulate an
	// egress owned by another tenant.
	ownerRoom string
}

func newFakeLiveKitTwirp(t *testing.T, f *fakeLiveKitTwirp) *httptest.Server {
	t.Helper()
	if f.respCode == 0 {
		f.respCode = http.StatusOK
	}
	if f.respCT == "" {
		f.respCT = "application/json"
	}
	if f.respBody == "" {
		f.respBody = `{"egress_id":"EG_test","status":1,"room_name":"acme:standup"}`
	}
	if f.ownerRoom == "" {
		f.ownerRoom = "acme:standup"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The proxy resolves an egress id's owning room via ListEgress(egress_id)
		// before it forwards StopEgress/UpdateLayout/UpdateStream. Answer that
		// probe from ownerRoom, echoing back the requested id.
		if strings.HasSuffix(r.URL.Path, "/ListEgress") {
			f.listEgressHits = append(f.listEgressHits, r.URL.Path)
			var lr livekit.ListEgressRequest
			_ = protojson.Unmarshal(body, &lr)
			resp := &livekit.ListEgressResponse{Items: []*livekit.EgressInfo{{
				EgressId: lr.GetEgressId(),
				RoomName: f.ownerRoom,
			}}}
			out, _ := protojson.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			return
		}
		f.hits = append(f.hits, r.URL.Path)
		f.lastAuth = r.Header.Get("Authorization")
		f.lastHeader = r.Header.Clone()
		f.lastBody = body
		if f.respDelay > 0 {
			time.Sleep(f.respDelay)
		}
		w.Header().Set("Content-Type", f.respCT)
		w.WriteHeader(f.respCode)
		_, _ = io.WriteString(w, f.respBody)
	}))
	return srv
}

// mintEgressToken emulates what vulos-cloud's meetalloc/recording.go does:
// mint a token with RoomRecord=true on the qualified room id, with the
// tenant audience set via SetName.
func mintEgressToken(t *testing.T, tenant, room string, ttl time.Duration) string {
	t.Helper()
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("egress:rec_test")
	at.SetName(tenant)
	at.SetValidFor(ttl)
	at.SetVideoGrant(&auth.VideoGrant{
		Room:       tenant + ":" + room,
		RoomRecord: true,
		RoomJoin:   true,
		Hidden:     true,
		Recorder:   true,
	})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint egress token: %v", err)
	}
	return tok
}

func newEgressProxyForTest(t *testing.T, upstream string) (*EgressProxy, *Validator) {
	t.Helper()
	v := newValidatorForTest(t)
	p, err := NewEgressProxy(v, upstream)
	if err != nil {
		t.Fatalf("egress proxy: %v", err)
	}
	return p, v
}

func TestEgressProxy_ValidRecordingTokenForwarded(t *testing.T) {
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	body := `{"room_name":"acme:standup","layout":"speaker"}`
	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d body=%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "EG_test") {
		t.Fatalf("expected upstream body to flow through, got: %q", respBody)
	}
	if len(f.hits) != 1 || f.hits[0] != "/twirp/livekit.Egress/StartRoomCompositeEgress" {
		t.Fatalf("upstream hits: %v", f.hits)
	}
	// Body MUST be forwarded verbatim — Twirp is opaque to us.
	if string(f.lastBody) != body {
		t.Fatalf("body forwarded: got %q want %q", f.lastBody, body)
	}
	// The bearer token MUST be forwarded too (LiveKit re-verifies it).
	if f.lastAuth != "Bearer "+tok {
		t.Fatalf("upstream auth header: got %q want Bearer <tok>", f.lastAuth)
	}
}

func TestEgressProxy_StopEgressAlsoForwarded(t *testing.T) {
	f := &fakeLiveKitTwirp{respBody: `{"egress_id":"EG_x","status":3}`}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StopEgress", strings.NewReader(`{"egress_id":"EG_x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(f.hits) != 1 || f.hits[0] != "/twirp/livekit.Egress/StopEgress" {
		t.Fatalf("upstream hits: %v", f.hits)
	}
}

func TestEgressProxy_NoTokenRejected_401(t *testing.T) {
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	resp, err := http.Post(gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestEgressProxy_MalformedTokenRejected_401(t *testing.T) {
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer not-a-jwt-egress")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d body=%q", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "not-a-jwt-egress") {
		t.Fatalf("rejection body leaked token: %q", body)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestEgressProxy_CrossTenantTokenRejected_403(t *testing.T) {
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	// Token where the room prefix says "acme" but the audience (`name`)
	// says "evil". Defense-in-depth — LiveKit's signature check would
	// also reject this, but our gate catches it earlier with the VULOS-
	// MEET/1 tenant binding rule.
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("egress:rec_test").SetName("evil").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomRecord: true, RoomJoin: true, Recorder: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(`{"room_name":"acme:standup"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
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
		t.Fatalf("upstream should not have been touched on cross-tenant: %v", f.hits)
	}
}

func TestEgressProxy_TokenWithoutRoomRecordRejected_403(t *testing.T) {
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	// A regular meeting-join token (no RoomRecord grant). MUST NOT be
	// replayable on the egress path — even on the caller's own tenant.
	tok := mintToken(t, "acme", "standup", time.Hour)

	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d (want 403)", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestEgressProxy_MethodNotPost_405(t *testing.T) {
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	resp, err := http.Get(gate.URL + "/twirp/livekit.Egress/StartRoomCompositeEgress")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestEgressProxy_NonEgressTwirpPathFallsThroughToSibling(t *testing.T) {
	// Confirm the documented policy: only /twirp/livekit.Egress/* is
	// auth-checked + proxied; other Twirp namespaces (e.g.
	// /twirp/livekit.RoomService/*) fall through to the sibling handler.
	// Today the sibling is the egress-webhook receiver, which doesn't
	// match those paths either — i.e. they 404. That is fine; the cloud
	// uses its own gRPC client for RoomService.
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, f)
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	siblingHit := false
	sibling := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siblingHit = true
		w.WriteHeader(http.StatusTeapot)
	})
	gate := httptest.NewServer(g.Handler(sibling, p))
	defer gate.Close()

	// Even though this is under /twirp/, it is NOT under /twirp/livekit.Egress/.
	resp, err := http.Post(gate.URL+"/twirp/livekit.RoomService/ListRooms", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !siblingHit {
		t.Fatalf("sibling should have caught non-egress Twirp path")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status: %d (want 418)", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("upstream should not have been touched: %v", f.hits)
	}
}

func TestEgressProxy_UpstreamUnreachable_502(t *testing.T) {
	// Point the proxy at a port that is definitively unbound.
	g, _ := newGateForTest(t, "127.0.0.1:1") // signal-gate /rtc target — irrelevant here
	p, err := NewEgressProxy(g.validator, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("egress proxy: %v", err)
	}
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, gate.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(`{"room_name":"acme:standup"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: %d (want 502)", resp.StatusCode)
	}
}

func TestEgressProxy_QueryStringPreserved(t *testing.T) {
	// Defensive: Twirp doesn't use query strings in v1, but if the cloud
	// ever appends e.g. `?trace_id=…` we should not silently drop it.
	f := &fakeLiveKitTwirp{}
	upstream := newFakeLiveKitTwirp(t, &fakeLiveKitTwirp{
		respBody: `{"egress_id":"EG_q"}`,
	})
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	defer gate.Close()

	// Replace the upstream handler with one that asserts on the query.
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RawQuery; got != "trace_id=abc" {
			t.Errorf("query lost: got %q", got)
		}
		f.hits = append(f.hits, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream2.Close()
	p2, err := NewEgressProxy(g.validator, strings.TrimPrefix(upstream2.URL, "http://"))
	if err != nil {
		t.Fatalf("egress proxy: %v", err)
	}
	gate2 := httptest.NewServer(g.Handler(nil, p2))
	defer gate2.Close()

	tok := mintEgressToken(t, "acme", "standup", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, gate2.URL+"/twirp/livekit.Egress/StartRoomCompositeEgress?trace_id=abc", strings.NewReader(`{"room_name":"acme:standup"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestEgressProxy_RequiresInputs(t *testing.T) {
	v := newValidatorForTest(t)
	if _, err := NewEgressProxy(nil, ":7880"); err == nil {
		t.Fatalf("expected error with nil validator")
	}
	if _, err := NewEgressProxy(v, ""); err == nil {
		t.Fatalf("expected error with empty upstream")
	}
}

// egressGate spins up the fake livekit-server + the egress proxy behind the
// signal gate and returns the gate URL plus the fake (for hit assertions).
func egressGate(t *testing.T, f *fakeLiveKitTwirp) (string, *fakeLiveKitTwirp) {
	t.Helper()
	upstream := newFakeLiveKitTwirp(t, f)
	t.Cleanup(upstream.Close)
	addr := strings.TrimPrefix(upstream.URL, "http://")
	g, _ := newGateForTest(t, addr)
	p, _ := newEgressProxyForTest(t, addr)
	gate := httptest.NewServer(g.Handler(nil, p))
	t.Cleanup(gate.Close)
	return gate.URL, f
}

func doEgress(t *testing.T, gateURL, method, tok, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gateURL+"/twirp/livekit.Egress/"+method, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s: %v", method, err)
	}
	return resp
}

// TestEgressProxy_CrossTenantBodyRoomRejected_403 is the core regression for the
// HIGH cross-tenant BAC bug: a VALID RoomRecord token for tenant "acme" whose
// Twirp body names ANOTHER tenant's room ("victim:secret"). The token check
// passes (it's a legitimate acme recording token) but the body-room guard MUST
// reject it before anything reaches livekit-server.
func TestEgressProxy_CrossTenantBodyRoomRejected_403(t *testing.T) {
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{})
	tok := mintEgressToken(t, "acme", "standup", time.Hour)

	resp := doEgress(t, gateURL, "StartRoomCompositeEgress", tok, `{"room_name":"victim:secret"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d body=%q (want 403)", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "victim") || strings.Contains(string(body), tok) {
		t.Fatalf("rejection body leaked contents: %q", body)
	}
	if len(f.hits) != 0 {
		t.Fatalf("cross-tenant body must NOT reach upstream: %v", f.hits)
	}
}

// TestEgressProxy_SameTenantBodyRoomForwarded confirms the legitimate path still
// works: token room == body room ⇒ forwarded verbatim to livekit-server.
func TestEgressProxy_SameTenantBodyRoomForwarded(t *testing.T) {
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{})
	tok := mintEgressToken(t, "acme", "standup", time.Hour)

	resp := doEgress(t, gateURL, "StartRoomCompositeEgress", tok, `{"room_name":"acme:standup"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d (want 200)", resp.StatusCode)
	}
	if len(f.hits) != 1 || f.hits[0] != "/twirp/livekit.Egress/StartRoomCompositeEgress" {
		t.Fatalf("same-tenant egress should have forwarded: %v", f.hits)
	}
}

// TestEgressProxy_SameTenantWrongRoomRejected_403 shows the binding is to the
// token's FULL room, not just its tenant: an acme token cannot start egress on
// a DIFFERENT acme room it wasn't minted for.
func TestEgressProxy_SameTenantWrongRoomRejected_403(t *testing.T) {
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{})
	tok := mintEgressToken(t, "acme", "standup", time.Hour)

	resp := doEgress(t, gateURL, "StartRoomCompositeEgress", tok, `{"room_name":"acme:other-room"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d (want 403)", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("wrong-room egress must NOT reach upstream: %v", f.hits)
	}
}

// TestEgressProxy_ListEgressCrossTenantRejected_403 blocks the enumeration
// vector: an acme token cannot ListEgress another tenant's room, nor run an
// unscoped (no room filter) list that would enumerate every tenant on the box.
func TestEgressProxy_ListEgressCrossTenantRejected_403(t *testing.T) {
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{})
	tok := mintEgressToken(t, "acme", "standup", time.Hour)

	// Foreign room filter → rejected.
	resp := doEgress(t, gateURL, "ListEgress", tok, `{"room_name":"victim:secret"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign ListEgress status: %d (want 403)", resp.StatusCode)
	}
	// No room filter (global enumeration) → rejected.
	resp = doEgress(t, gateURL, "ListEgress", tok, `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unscoped ListEgress status: %d (want 403)", resp.StatusCode)
	}
	if len(f.listEgressHits) != 0 || len(f.hits) != 0 {
		t.Fatalf("rejected ListEgress must NOT reach upstream: hits=%v list=%v", f.hits, f.listEgressHits)
	}

	// Same-tenant, scoped filter → forwarded to the upstream ListEgress surface.
	resp = doEgress(t, gateURL, "ListEgress", tok, `{"room_name":"acme:standup"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-tenant ListEgress status: %d (want 200)", resp.StatusCode)
	}
	if len(f.listEgressHits) != 1 {
		t.Fatalf("same-tenant ListEgress should have forwarded once: %v", f.listEgressHits)
	}
}

// TestEgressProxy_StopEgressCrossTenantRejected_403 blocks the StopEgress DoS
// vector. StopEgress carries only an opaque egress id, so the proxy resolves the
// owning room via an upstream ListEgress probe: when that room belongs to
// another tenant ("victim:secret") the stop MUST be rejected and never
// forwarded.
func TestEgressProxy_StopEgressCrossTenantRejected_403(t *testing.T) {
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{ownerRoom: "victim:secret"})
	tok := mintEgressToken(t, "acme", "standup", time.Hour)

	resp := doEgress(t, gateURL, "StopEgress", tok, `{"egress_id":"EG_victim"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d (want 403)", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("cross-tenant StopEgress must NOT be forwarded: %v", f.hits)
	}
	if len(f.listEgressHits) == 0 {
		t.Fatalf("proxy should have run an ownership probe")
	}
}

// TestEgressProxy_UpdateLayoutCrossTenantRejected_403 mirrors the StopEgress
// case for the other egress-id-keyed method the cloud uses.
func TestEgressProxy_UpdateLayoutCrossTenantRejected_403(t *testing.T) {
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{ownerRoom: "victim:secret"})
	tok := mintEgressToken(t, "acme", "standup", time.Hour)

	resp := doEgress(t, gateURL, "UpdateLayout", tok, `{"egress_id":"EG_victim","layout":"grid"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d (want 403)", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("cross-tenant UpdateLayout must NOT be forwarded: %v", f.hits)
	}
}

// TestEgressProxy_OverTTLTokenRejected exercises the MAX-TTL clamp end to end:
// a token whose remaining lifetime exceeds the ceiling is refused (mapped to
// 401 on the egress path) and never reaches livekit-server.
func TestEgressProxy_OverTTLTokenRejected(t *testing.T) {
	t.Setenv(MaxTokenTTLEnv, "1h")
	gateURL, f := egressGate(t, &fakeLiveKitTwirp{})
	tok := mintEgressToken(t, "acme", "standup", 24*time.Hour) // 24h ≫ 1h ceiling

	resp := doEgress(t, gateURL, "StartRoomCompositeEgress", tok, `{"room_name":"acme:standup"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d (want 401)", resp.StatusCode)
	}
	if len(f.hits) != 0 {
		t.Fatalf("over-TTL token must NOT reach upstream: %v", f.hits)
	}
}

func TestEgressProxy_PathPrefixConstantStable(t *testing.T) {
	// The cloud's HTTPEgressClient.BaseURL + "/twirp/livekit.Egress/<Method>"
	// MUST match what our proxy is mounted at. Lock the constant.
	if EgressTwirpPathPrefix != "/twirp/livekit.Egress/" {
		t.Fatalf("path prefix drift: %q", EgressTwirpPathPrefix)
	}
}
