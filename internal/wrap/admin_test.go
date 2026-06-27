// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func newTestAdminServer(t *testing.T) (*AdminServer, *MemoryRoomService) {
	t.Helper()
	tenant := NewTenant("")
	geo, err := NewGeoRouter("za-jhb")
	if err != nil {
		t.Fatalf("geo: %v", err)
	}
	rooms := NewMemoryRoomService()
	admin, err := NewAdminServer(tenant, rooms, geo, "supersecrettoken", "0.0.0-test")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	return admin, rooms
}

func TestAdmin_RequiresAdminToken(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/tenants/acme/rooms")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", resp.StatusCode)
	}
}

func TestAdmin_AcceptsCorrectToken(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: got %d, want 200", resp.StatusCode)
	}
}

func TestAdmin_RejectsWrongTokenScheme(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	// "Basic" not "Bearer".
	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Basic supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong scheme: got %d, want 401", resp.StatusCode)
	}
}

func TestConstantTimeEqualString(t *testing.T) {
	// Same length, equal.
	if !constantTimeEqualString("abcdef", "abcdef") {
		t.Fatalf("equal strings should match")
	}
	// Same length, different.
	if constantTimeEqualString("abcdef", "abcdez") {
		t.Fatalf("different strings should not match")
	}
	// Different length.
	if constantTimeEqualString("abc", "abcdef") {
		t.Fatalf("different-length strings should not match")
	}
	if constantTimeEqualString("abcdef", "abc") {
		t.Fatalf("different-length strings should not match (reverse)")
	}
	// Empty.
	if constantTimeEqualString("", "abc") {
		t.Fatalf("empty vs non-empty should not match")
	}
	if !constantTimeEqualString("", "") {
		t.Fatalf("two empty strings should match")
	}
}

func TestAdmin_HealthExposesRegionAndProto(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	// Public health does NOT require the admin token, but it is minimal: a 200
	// + {"status":"ok"} and NOTHING that fingerprints the box.
	resp, err := http.Get(srv.URL + "/admin/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	pubBody, _ := readAll(resp.Body)
	for _, leak := range []string{"region", "protocol", "version", "separator", "za-jhb"} {
		if strings.Contains(string(pubBody), leak) {
			t.Fatalf("public health leaked %q: %s", leak, pubBody)
		}
	}
	if !strings.Contains(string(pubBody), `"status":"ok"`) {
		t.Fatalf("public health missing status ok: %s", pubBody)
	}

	// Build/region detail moved behind the admin token at /admin/info.
	req, _ := http.NewRequest("GET", srv.URL+"/admin/info", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get info: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("info status: got %d, want 200", resp2.StatusCode)
	}
	var h AdminHealth
	if err := json.NewDecoder(resp2.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Region != "za-jhb" {
		t.Fatalf("region: got %q, want za-jhb", h.Region)
	}
	if h.Protocol != SubprotocolVersion {
		t.Fatalf("protocol: got %q, want %s", h.Protocol, SubprotocolVersion)
	}
	if h.Status != "ok" {
		t.Fatalf("status: got %q, want ok", h.Status)
	}
}

// TestAdmin_InfoRequiresAdminToken locks that the build/region detail endpoint
// is NOT anonymously readable — that was the whole point of moving it off the
// public health probe.
func TestAdmin_InfoRequiresAdminToken(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/info")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /admin/info: got %d, want 401", resp.StatusCode)
	}
}

func TestAdmin_ListRoomsScopedToTenant(t *testing.T) {
	admin, rooms := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	ctx := context.Background()
	rooms.CreateRoom(ctx, "acme:standup")
	rooms.CreateRoom(ctx, "acme:retro")
	rooms.CreateRoom(ctx, "evil:weekly")
	rooms.CreateRoom(ctx, "globex:planning")

	// As tenant "acme" — MUST see only acme rooms.
	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got listRoomsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sort.Strings(got.Rooms)
	want := []string{"retro", "standup"}
	if got.Tenant != "acme" || !equalStrings(got.Rooms, want) {
		t.Fatalf("list leaked or dropped: tenant=%q rooms=%v want=%v", got.Tenant, got.Rooms, want)
	}
}

func TestAdmin_DeleteRoomScopedToTenant(t *testing.T) {
	admin, rooms := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	ctx := context.Background()
	rooms.CreateRoom(ctx, "acme:standup")
	rooms.CreateRoom(ctx, "evil:weekly")

	// Delete acme:standup as acme — allowed.
	req, _ := http.NewRequest("DELETE", srv.URL+"/admin/tenants/acme/rooms/standup", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", resp.StatusCode)
	}

	// Verify evil:weekly is untouched.
	all, _ := rooms.ListRoomIDs(ctx)
	if len(all) != 1 || all[0] != "evil:weekly" {
		t.Fatalf("delete leaked across tenants or dropped wrong row: %v", all)
	}
}

func TestAdmin_DeleteRoomCrossTenantRejected(t *testing.T) {
	admin, rooms := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	ctx := context.Background()
	rooms.CreateRoom(ctx, "evil:weekly")

	// Try to delete evil:weekly while CLAIMING to be tenant acme. Note:
	// the URL is /admin/tenants/acme/rooms/weekly. The server qualifies it
	// to "acme:weekly" (a non-existent room owned by acme) — the delete
	// silently succeeds-but-doesn't-find-it. The CRITICAL property is that
	// evil:weekly is STILL THERE afterward. That is what we check.
	req, _ := http.NewRequest("DELETE", srv.URL+"/admin/tenants/acme/rooms/weekly", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()

	all, _ := rooms.ListRoomIDs(ctx)
	if len(all) != 1 || all[0] != "evil:weekly" {
		t.Fatalf("cross-tenant delete LEAKED: %v", all)
	}
}

func TestAdmin_BadTenantRejected(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	// Tenant with dot (forbidden char).
	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme.com/rooms", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad tenant: got %d, want 400", resp.StatusCode)
	}
}

func TestNewAdminServer_RequiresInputs(t *testing.T) {
	tenant := NewTenant("")
	geo, _ := NewGeoRouter("za-jhb")
	rooms := NewMemoryRoomService()

	if _, err := NewAdminServer(nil, rooms, geo, "tok", "v"); err == nil {
		t.Fatalf("expected error with nil tenant")
	}
	if _, err := NewAdminServer(tenant, nil, geo, "tok", "v"); err == nil {
		t.Fatalf("expected error with nil rooms")
	}
	if _, err := NewAdminServer(tenant, rooms, nil, "tok", "v"); err == nil {
		t.Fatalf("expected error with nil geo")
	}
	if _, err := NewAdminServer(tenant, rooms, geo, "", "v"); err == nil {
		t.Fatalf("expected error with empty admin token")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Sanity check we haven't broken JSON contracts: the separator byte is still
// exposed, but only on the authenticated /admin/info endpoint.
func TestAdmin_InfoSeparatorByteIsExposed(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/info", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := readAll(resp.Body)
	if !strings.Contains(string(body), `"separator":":"`) {
		t.Fatalf("info did not expose separator byte: %s", body)
	}
}

func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
