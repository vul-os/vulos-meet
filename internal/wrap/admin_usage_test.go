// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubStatter is a fixed UsageStatter for the admin read test.
type stubStatter struct {
	byTenant map[string][]RoomUsageSnapshot
}

func (s stubStatter) Snapshot(tenant string) []RoomUsageSnapshot {
	return s.byTenant[tenant]
}

func TestAdminUsage_NotFoundWhenUnwired(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/usage", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unwired usage: got %d, want 404", resp.StatusCode)
	}
}

func TestAdminUsage_ReturnsSnapshotWhenWired(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	admin.SetUsageStatter(stubStatter{byTenant: map[string][]RoomUsageSnapshot{
		"acme": {{Tenant: "acme", Room: "standup", ParticipantMinutes: 12.5, LiveParticipants: 2}},
	}})
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/admin/tenants/acme/usage", nil)
	req.Header.Set("Authorization", "Bearer supersecrettoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wired usage: got %d, want 200", resp.StatusCode)
	}
	var body usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Tenant != "acme" || len(body.Rooms) != 1 {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body.Rooms[0].Room != "standup" || body.Rooms[0].ParticipantMinutes != 12.5 {
		t.Fatalf("snapshot: %+v", body.Rooms[0])
	}
}

func TestAdminUsage_RequiresAdminToken(t *testing.T) {
	admin, _ := newTestAdminServer(t)
	admin.SetUsageStatter(stubStatter{})
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/tenants/acme/usage")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", resp.StatusCode)
	}
}
