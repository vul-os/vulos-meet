// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vul-os/vulos-meet/internal/apikey"
)

// stubAPIKeyIntrospector is a mocked apikey.Introspector for admin-auth tests.
type stubAPIKeyIntrospector struct {
	res apikey.Result
	err error
}

func (s stubAPIKeyIntrospector) Introspect(_ context.Context, _ string) (apikey.Result, error) {
	return s.res, s.err
}

// newTestAdminServerWithIntro builds a test admin server and wires the given
// introspector (nil disables the vk_ path).
func newTestAdminServerWithIntro(t *testing.T, intro apikey.Introspector) (*AdminServer, *MemoryRoomService) {
	t.Helper()
	admin, rooms := newTestAdminServer(t)
	admin.SetIntrospector(intro)
	return admin, rooms
}

// TestAdminAPIKey_ValidKeyGrantsAccess checks that a valid vk_ key with the
// "meet" product is accepted on a guarded tenant route.
func TestAdminAPIKey_ValidKeyGrantsAccess(t *testing.T) {
	intro := stubAPIKeyIntrospector{res: apikey.Result{
		Valid:    true,
		Account:  "alice@vulos.org",
		Products: []string{"meet"},
	}}
	admin, _ := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer vk_live_good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid vk_ key: got %d, want 200", resp.StatusCode)
	}
}

// TestAdminAPIKey_ValidKeyOnInfoRoute checks that a valid vk_ key is also
// accepted on the non-tenant-scoped guarded route (GET /admin/info).
func TestAdminAPIKey_ValidKeyOnInfoRoute(t *testing.T) {
	intro := stubAPIKeyIntrospector{res: apikey.Result{
		Valid:    true,
		Account:  "alice@vulos.org",
		Products: []string{"meet", "mail"},
	}}
	admin, _ := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/info", nil)
	req.Header.Set("Authorization", "Bearer vk_live_good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid vk_ key on /admin/info: got %d, want 200", resp.StatusCode)
	}
}

// TestAdminAPIKey_InvalidKeyReturns401 checks that an invalid (revoked/unknown)
// vk_ key is rejected with 401.
func TestAdminAPIKey_InvalidKeyReturns401(t *testing.T) {
	intro := stubAPIKeyIntrospector{res: apikey.Result{Valid: false}}
	admin, _ := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer vk_revoked")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid vk_ key: got %d, want 401", resp.StatusCode)
	}
}

// TestAdminAPIKey_WrongProductReturns403 checks that a valid key that does NOT
// carry the "meet" product scope is rejected with 403.
func TestAdminAPIKey_WrongProductReturns403(t *testing.T) {
	intro := stubAPIKeyIntrospector{res: apikey.Result{
		Valid:    true,
		Account:  "bob@vulos.org",
		Products: []string{"mail", "office"}, // no "meet"
	}}
	admin, _ := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer vk_wrongproduct")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong product vk_ key: got %d, want 403", resp.StatusCode)
	}
}

// TestAdminAPIKey_CPUnavailableReturns503 checks that when the CP is
// unreachable, the gate fails closed (503) rather than granting access.
func TestAdminAPIKey_CPUnavailableReturns503(t *testing.T) {
	intro := stubAPIKeyIntrospector{err: errors.New("cp down")}
	admin, _ := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer vk_anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("CP unavailable: got %d, want 503", resp.StatusCode)
	}
}

// TestAdminAPIKey_DisabledWhenNoIntrospector checks that a vk_ key presented
// without an introspector wired is NOT treated as a valid credential — it falls
// through to the static admin-token path and is rejected (401) because vk_…
// does not match the static token. Self-host behavior is unchanged.
func TestAdminAPIKey_DisabledWhenNoIntrospector(t *testing.T) {
	admin, _ := newTestAdminServer(t) // no introspector wired
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer vk_ignored")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("vk_ without introspector: got %d, want 401", resp.StatusCode)
	}
}

// TestAdminAPIKey_StaticTokenStillWorks checks that the existing MEET_ADMIN_TOKEN
// auth path continues to work alongside vk_ (regression: adding vk_ support
// must not break operators using the static token).
func TestAdminAPIKey_StaticTokenStillWorks(t *testing.T) {
	intro := stubAPIKeyIntrospector{res: apikey.Result{
		Valid:    true,
		Account:  "alice@vulos.org",
		Products: []string{"meet"},
	}}
	admin, _ := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	// Non-vk_ bearer: falls through to static-token path.
	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("static token with introspector wired: got %d, want 200", resp.StatusCode)
	}
}

// TestAdminAPIKey_DeleteRoomWithVKKey checks that the DELETE room route also
// accepts vk_ keys (guarded route coverage beyond just list).
func TestAdminAPIKey_DeleteRoomWithVKKey(t *testing.T) {
	intro := stubAPIKeyIntrospector{res: apikey.Result{
		Valid:    true,
		Account:  "alice@vulos.org",
		Products: []string{"meet"},
	}}
	admin, rooms := newTestAdminServerWithIntro(t, intro)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	ctx := context.Background()
	rooms.CreateRoom(ctx, "acme:standup")

	req, _ := http.NewRequest("DELETE", srv.URL+"/admin/tenants/acme/rooms/standup", nil)
	req.Header.Set("Authorization", "Bearer vk_live_good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete with vk_ key: got %d, want 204", resp.StatusCode)
	}
}
