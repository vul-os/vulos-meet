// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vul-os/vulos-apps/appsplatform"
)

// fakeSFU is an in-memory MeetSFU for adapter/handler tests.
type fakeSFU struct {
	rooms    []string
	parts    []ParticipantMeta
	sent     []sentData
	listErr  error
	sendErr  error
	partsErr error
}

type sentData struct {
	room  string
	data  []byte
	topic string
}

func (f *fakeSFU) ListRoomIDs(context.Context) ([]string, error) { return f.rooms, f.listErr }
func (f *fakeSFU) ListParticipants(_ context.Context, _ string) ([]ParticipantMeta, error) {
	return f.parts, f.partsErr
}
func (f *fakeSFU) SendData(_ context.Context, room string, data []byte, topic string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sentData{room: room, data: data, topic: topic})
	return nil
}

func TestMeetAdapterProductAndScopes(t *testing.T) {
	a := NewMeetAdapter(&fakeSFU{})
	if a.Product() != appsplatform.ProductMeet {
		t.Fatalf("product = %q, want meet", a.Product())
	}
	if got := a.RequiredScope(AppReadRooms); got != appsplatform.ScopeAppsRead {
		t.Fatalf("rooms scope = %q, want apps:read", got)
	}
	if got := a.RequiredScope(AppReadParticipants); got != appsplatform.ScopeAppsRead {
		t.Fatalf("participants scope = %q, want apps:read", got)
	}
	if got := a.RequiredScope(AppActionBroadcast); got != appsplatform.ScopeAppsWrite {
		t.Fatalf("broadcast scope = %q, want apps:write", got)
	}
}

func TestMeetAdapterCanAccessTarget(t *testing.T) {
	a := NewMeetAdapter(&fakeSFU{rooms: []string{"acme:standup"}})
	if allowed, exists := a.CanAccessTarget(nil, "acme:standup"); !allowed || !exists {
		t.Fatalf("known room: allowed=%v exists=%v, want true/true", allowed, exists)
	}
	if allowed, exists := a.CanAccessTarget(nil, "acme:ghost"); allowed || exists {
		t.Fatalf("unknown room: allowed=%v exists=%v, want false/false", allowed, exists)
	}
	// SFU error → fail closed.
	aErr := NewMeetAdapter(&fakeSFU{listErr: errors.New("down")})
	if allowed, exists := aErr.CanAccessTarget(nil, "acme:standup"); allowed || exists {
		t.Fatalf("sfu error: allowed=%v exists=%v, want false/false", allowed, exists)
	}
}

func TestMeetAdapterActBroadcast(t *testing.T) {
	f := &fakeSFU{rooms: []string{"acme:standup"}}
	a := NewMeetAdapter(f)
	res, err := a.Act(context.Background(), &appsplatform.App{ID: "1", Name: "Echo"}, appsplatform.ActionRequest{
		Action:  AppActionBroadcast,
		Target:  "acme:standup",
		Payload: json.RawMessage(`{"text":"hi"}`),
	}, nil)
	if err != nil {
		t.Fatalf("act: %v", err)
	}
	if len(f.sent) != 1 || f.sent[0].room != "acme:standup" || f.sent[0].topic != appDataTopic {
		t.Fatalf("unexpected send: %+v", f.sent)
	}
	var env appBroadcast
	if err := json.Unmarshal(f.sent[0].data, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Type != "app_event" || env.AppID != "1" || env.Room != "acme:standup" {
		t.Fatalf("bad envelope: %+v", env)
	}
	if m, ok := res.(map[string]any); !ok || m["delivered"] != true {
		t.Fatalf("result = %#v", res)
	}
}

func TestMeetAdapterActRejects(t *testing.T) {
	a := NewMeetAdapter(&fakeSFU{})
	if _, err := a.Act(context.Background(), &appsplatform.App{ID: "1"}, appsplatform.ActionRequest{Action: AppActionBroadcast}, nil); err == nil {
		t.Fatal("expected error for empty target")
	}
	if _, err := a.Act(context.Background(), &appsplatform.App{ID: "1"}, appsplatform.ActionRequest{Action: "nope", Target: "r"}, nil); err == nil {
		t.Fatal("expected error for unsupported action")
	}
}

func TestMeetAdapterRead(t *testing.T) {
	f := &fakeSFU{rooms: []string{"acme:standup"}, parts: []ParticipantMeta{{Identity: "u1", Name: "Ann"}}}
	a := NewMeetAdapter(f)
	out, err := a.Read(context.Background(), nil, appsplatform.ReadRequest{Kind: AppReadParticipants, Target: "acme:standup"})
	if err != nil {
		t.Fatalf("read participants: %v", err)
	}
	if m := out.(map[string]any); m["room"] != "acme:standup" {
		t.Fatalf("bad participants read: %#v", out)
	}
	out, err = a.Read(context.Background(), nil, appsplatform.ReadRequest{Kind: AppReadRooms})
	if err != nil {
		t.Fatalf("read rooms: %v", err)
	}
	if m := out.(map[string]any); len(m["rooms"].([]string)) != 1 {
		t.Fatalf("bad rooms read: %#v", out)
	}
	if _, err := a.Read(context.Background(), nil, appsplatform.ReadRequest{Kind: "bogus"}); err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

// TestNewAppsHandlerListGate exercises the mounted handler end to end: the
// management list route is reachable only with the admin bearer and returns the
// Summary[] consolidation shape.
func TestNewAppsHandlerListGate(t *testing.T) {
	reg := appsplatform.NewMemoryRegistry(appsplatform.WithScopeSet(MeetAppScopeSet()))
	h, err := NewAppsHandler(AppsConfig{Registry: reg, SFU: &fakeSFU{}, AdminToken: "s3cret"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(h.BasePath, h)
	mux.Handle(h.BasePath+"/", h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No token → 401.
	resp, err := http.Get(srv.URL + "/api/apps")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token list status = %d, want 401", resp.StatusCode)
	}

	// With admin bearer → 200 + JSON array.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/apps", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin list status = %d, want 200", resp.StatusCode)
	}
	var apps []appsplatform.Summary
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		t.Fatalf("decode list: %v", err)
	}
}

// TestNewMCPHandlerInitializeAndToolsList drives the MCP handshake
// (initialize → tools/list) over HTTP with a per-app (vat_) token and asserts
// the Meet adapter's Descriptor surface is published: the room.broadcast tool
// and the participants/rooms resources.
func TestNewMCPHandlerInitializeAndToolsList(t *testing.T) {
	reg := appsplatform.NewMemoryRegistry(appsplatform.WithScopeSet(MeetAppScopeSet()))
	created, err := reg.Create(appsplatform.CreateParams{
		Name:     "agent",
		OwnerID:  "owner",
		Products: []string{appsplatform.ProductMeet},
		Scopes:   []string{appsplatform.ScopeAppsRead, appsplatform.ScopeAppsWrite},
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	h, err := NewMCPHandler(MCPConfig{Registry: reg, SFU: &fakeSFU{rooms: []string{"acme:standup"}}})
	if err != nil {
		t.Fatalf("mcp handler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(h.BasePath, h)
	mux.Handle(h.BasePath+"/", h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(method string, params any) map[string]any {
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+h.BasePath, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+created.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s decode: %v", method, err)
		}
		return out
	}

	// No token → 401 (signal-gate auth is not weakened for the MCP shape).
	noTok, err := http.Post(srv.URL+h.BasePath, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	noTok.Body.Close()
	if noTok.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token initialize status = %d, want 401", noTok.StatusCode)
	}

	init := call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["error"] != nil {
		t.Fatalf("initialize error: %v", init["error"])
	}

	tools := call("tools/list", nil)
	tb, _ := json.Marshal(tools["result"])
	if !strings.Contains(string(tb), AppActionBroadcast) {
		t.Fatalf("tools/list missing %q: %s", AppActionBroadcast, tb)
	}

	resources := call("resources/list", nil)
	rb, _ := json.Marshal(resources["result"])
	if !strings.Contains(string(rb), AppReadParticipants) || !strings.Contains(string(rb), AppReadRooms) {
		t.Fatalf("resources/list missing participants/rooms: %s", rb)
	}
}
