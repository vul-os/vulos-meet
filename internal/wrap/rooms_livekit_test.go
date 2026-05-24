// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

// fakeLiveKitServer is a tiny Twirp-compatible RoomService impl used by the
// unit tests. It speaks the Protobuf content type because that is the codec
// our client constructs (NewRoomServiceProtobufClient).
type fakeLiveKitServer struct {
	rooms       []*livekit.Room
	failNext    int // when > 0, fail the next N requests
	deleteCount int
	listCount   int
	requireAuth bool
}

func newFakeLiveKitServer() *httptest.Server {
	f := &fakeLiveKitServer{requireAuth: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/twirp/livekit.RoomService/ListRooms", func(w http.ResponseWriter, r *http.Request) {
		f.listCount++
		if !checkAuth(w, r, f.requireAuth) {
			return
		}
		if f.failNext > 0 {
			f.failNext--
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		respond(w, &livekit.ListRoomsResponse{Rooms: f.rooms})
	})
	mux.HandleFunc("/twirp/livekit.RoomService/DeleteRoom", func(w http.ResponseWriter, r *http.Request) {
		f.deleteCount++
		if !checkAuth(w, r, f.requireAuth) {
			return
		}
		if f.failNext > 0 {
			f.failNext--
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		respond(w, &livekit.DeleteRoomResponse{})
	})
	srv := httptest.NewServer(mux)
	// stash the fake on the server for tests to mutate
	srv.Config.Handler = withFakeRef(mux, f)
	return srv
}

// fakeRefKey lets tests reach the fake state via the server's Handler.
type fakeRefHandler struct {
	http.Handler
	f *fakeLiveKitServer
}

func withFakeRef(h http.Handler, f *fakeLiveKitServer) http.Handler {
	return &fakeRefHandler{Handler: h, f: f}
}

func grabFake(srv *httptest.Server) *fakeLiveKitServer {
	if h, ok := srv.Config.Handler.(*fakeRefHandler); ok {
		return h.f
	}
	return nil
}

func checkAuth(w http.ResponseWriter, r *http.Request, required bool) bool {
	if !required {
		return true
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || len(h) < len("Bearer ")+10 {
		http.Error(w, "no auth", http.StatusUnauthorized)
		return false
	}
	return true
}

// respond encodes a protobuf body in the shape Twirp expects.
func respond(w http.ResponseWriter, m proto.Message) {
	b, err := proto.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/protobuf")
	_, _ = w.Write(b)
}

func newLKRoomServiceForTest(t *testing.T, srv *httptest.Server) *LiveKitRoomService {
	t.Helper()
	// httptest.NewServer returns "http://127.0.0.1:port". Strip scheme to feed
	// into our SignalingAddr (which is host:port).
	addr := strings.TrimPrefix(srv.URL, "http://")
	rs, err := NewLiveKitRoomService(LiveKitRoomServiceConfig{
		SignalingAddr:    addr,
		APIKey:           testAPIKey,
		APISecret:        testAPISecret,
		CallTimeout:      2 * time.Second,
		BreakerCooldown:  100 * time.Millisecond,
		BreakerThreshold: 3,
		HTTPScheme:       "http",
	})
	if err != nil {
		t.Fatalf("new lk room service: %v", err)
	}
	return rs
}

func TestLKRoomService_ListRooms_HappyPath(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	f := grabFake(srv)
	f.rooms = []*livekit.Room{{Name: "acme:standup"}, {Name: "evil:weekly"}}

	rs := newLKRoomServiceForTest(t, srv)
	got, err := rs.ListRoomIDs(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0] != "acme:standup" {
		t.Fatalf("rooms: %v", got)
	}
	if f.listCount != 1 {
		t.Fatalf("list count: %d", f.listCount)
	}
}

func TestLKRoomService_DeleteRoom_HappyPath(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	f := grabFake(srv)

	rs := newLKRoomServiceForTest(t, srv)
	if err := rs.DeleteRoom(context.Background(), "acme:standup"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if f.deleteCount != 1 {
		t.Fatalf("delete count: %d", f.deleteCount)
	}
}

func TestLKRoomService_DeleteRoom_EmptyRejected(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	rs := newLKRoomServiceForTest(t, srv)
	if err := rs.DeleteRoom(context.Background(), ""); err == nil {
		t.Fatalf("expected error on empty room id")
	}
}

func TestLKRoomService_AttachesBearerToken(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	rs := newLKRoomServiceForTest(t, srv)
	// requireAuth defaults to true; if the bearer wasn't attached, the call
	// would 401 and the protobuf decode would fail.
	if _, err := rs.ListRoomIDs(context.Background()); err != nil {
		t.Fatalf("list with auth: %v", err)
	}
}

func TestLKRoomService_Breaker_OpensAfterConsecutiveFailures(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	f := grabFake(srv)
	f.failNext = 5 // more than the threshold of 3

	rs := newLKRoomServiceForTest(t, srv)

	// First three calls fail (and trip the breaker).
	for i := 0; i < 3; i++ {
		if _, err := rs.ListRoomIDs(context.Background()); err == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}
	if !rs.BreakerOpen() {
		t.Fatalf("breaker should be open after %d failures", 3)
	}
	// Fourth call returns the breaker error immediately, without touching
	// the fake server.
	listCountBefore := f.listCount
	if _, err := rs.ListRoomIDs(context.Background()); !errors.Is(err, ErrRoomServiceBreakerOpen) {
		t.Fatalf("expected breaker open, got %v", err)
	}
	if f.listCount != listCountBefore {
		t.Fatalf("breaker did not short-circuit: listCount went %d -> %d", listCountBefore, f.listCount)
	}
}

func TestLKRoomService_Breaker_HalfOpenAfterCooldown(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	f := grabFake(srv)
	f.failNext = 3 // trip the breaker

	rs := newLKRoomServiceForTest(t, srv)
	// rs is configured with BreakerCooldown = 100ms in newLKRoomServiceForTest.

	for i := 0; i < 3; i++ {
		_, _ = rs.ListRoomIDs(context.Background())
	}
	if !rs.BreakerOpen() {
		t.Fatalf("expected breaker open")
	}

	time.Sleep(150 * time.Millisecond)

	// Half-open probe should succeed and close the breaker.
	if _, err := rs.ListRoomIDs(context.Background()); err != nil {
		t.Fatalf("half-open probe failed: %v", err)
	}
	if rs.BreakerOpen() {
		t.Fatalf("breaker should have closed after successful probe")
	}
}

func TestLKRoomService_RequiresAPIKey(t *testing.T) {
	if _, err := NewLiveKitRoomService(LiveKitRoomServiceConfig{
		SignalingAddr: ":7880",
		APIKey:        "",
		APISecret:     "x",
	}); err == nil {
		t.Fatalf("expected error with empty key")
	}
	if _, err := NewLiveKitRoomService(LiveKitRoomServiceConfig{
		SignalingAddr: "",
		APIKey:        "k",
		APISecret:     "s",
	}); err == nil {
		t.Fatalf("expected error with empty addr")
	}
}

func TestLKRoomService_HostFromSignalingAddr(t *testing.T) {
	cases := map[string]string{
		":7880":          "127.0.0.1:7880",
		"":               "127.0.0.1:7880",
		"0.0.0.0:7880":   "127.0.0.1:7880",
		"127.0.0.1:9999": "127.0.0.1:9999",
		"livekit:7880":   "livekit:7880",
	}
	for in, want := range cases {
		if got := hostFromSignalingAddr(in); got != want {
			t.Fatalf("hostFromSignalingAddr(%q): got %q want %q", in, got, want)
		}
	}
}

// AdminServer + LiveKitRoomService end-to-end: the admin surface, when
// pointed at a real (faked) RoomService, returns tenant-scoped lists.
func TestLKRoomService_WithAdminServer_TenantScoped(t *testing.T) {
	srv := newFakeLiveKitServer()
	defer srv.Close()
	f := grabFake(srv)
	f.rooms = []*livekit.Room{
		{Name: "acme:standup"},
		{Name: "acme:retro"},
		{Name: "evil:weekly"},
	}

	rs := newLKRoomServiceForTest(t, srv)
	tenant := NewTenant("")
	geo, _ := NewGeoRouter("za-jhb")
	admin, err := NewAdminServer(tenant, rs, geo, "supersecrettoken", "0.0.0-test")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	adminSrv := httptest.NewServer(admin.Handler())
	defer adminSrv.Close()

	req, _ := http.NewRequest("GET", adminSrv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	// We trust the existing admin tests to cover the body shape; this test
	// only confirms the wiring round-trips successfully through the real
	// Twirp client.
}
