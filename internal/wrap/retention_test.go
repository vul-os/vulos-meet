// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeBlobDeleter records the recordings it was asked to delete and can be told
// to fail a given egress id (to exercise the at-least-once retry path).
type fakeBlobDeleter struct {
	mu      sync.Mutex
	deleted []string
	failIDs map[string]bool
}

func newFakeBlobDeleter() *fakeBlobDeleter {
	return &fakeBlobDeleter{failIDs: make(map[string]bool)}
}

func (f *fakeBlobDeleter) Delete(_ context.Context, r Recording) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIDs[r.EgressID] {
		return errors.New("simulated blob delete failure")
	}
	f.deleted = append(f.deleted, r.EgressID)
	return nil
}

func (f *fakeBlobDeleter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

// seedRecording is a small helper that upserts a finalised (available)
// recording at a given finalisation time.
func seedRecording(t *testing.T, s RecordingStore, egressID, tenant, room string, endedAt int64, size uint64) {
	t.Helper()
	if err := s.Upsert(context.Background(), Recording{
		EgressID:  egressID,
		Tenant:    tenant,
		Room:      room,
		State:     RecordingStateAvailable,
		StartedAt: endedAt - 60,
		EndedAt:   endedAt,
		SizeBytes: size,
	}); err != nil {
		t.Fatalf("seed %s: %v", egressID, err)
	}
}

func TestRetention_TTLExpires(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	store := NewMemRecordingStore()
	// old: ended 48h ago → past a 24h TTL. fresh: ended 1h ago → kept.
	seedRecording(t, store, "EG_old", "acme", "standup", now.Add(-48*time.Hour).Unix(), 100)
	seedRecording(t, store, "EG_fresh", "acme", "standup", now.Add(-1*time.Hour).Unix(), 200)

	pol := RetentionPolicy{TTL: 24 * time.Hour}
	recs, _ := store.List(context.Background())
	expired := pol.expiredEgressIDs(recs, now)
	if !expired["EG_old"] {
		t.Fatalf("EG_old should be past 24h TTL")
	}
	if expired["EG_fresh"] {
		t.Fatalf("EG_fresh (1h old) must NOT be expired under a 24h TTL")
	}
}

func TestRetention_NeverExpiresInProgress(t *testing.T) {
	now := time.Now()
	store := NewMemRecordingStore()
	// An in-progress recording that started long ago must never be deleted,
	// even past the TTL — we don't delete a live capture.
	_ = store.Upsert(context.Background(), Recording{
		EgressID:  "EG_live",
		Tenant:    "acme",
		Room:      "standup",
		State:     RecordingStateRecording,
		StartedAt: now.Add(-100 * time.Hour).Unix(),
	})
	pol := RetentionPolicy{TTL: time.Hour}
	recs, _ := store.List(context.Background())
	if pol.expiredEgressIDs(recs, now)["EG_live"] {
		t.Fatalf("an in-progress (recording) entry must never be expired")
	}
}

func TestRetention_MaxPerRoomEvictsOldest(t *testing.T) {
	now := time.Now()
	store := NewMemRecordingStore()
	// 3 recordings in acme/standup; keep 2 newest, evict the oldest.
	seedRecording(t, store, "EG_1", "acme", "standup", now.Add(-3*time.Hour).Unix(), 1)
	seedRecording(t, store, "EG_2", "acme", "standup", now.Add(-2*time.Hour).Unix(), 1)
	seedRecording(t, store, "EG_3", "acme", "standup", now.Add(-1*time.Hour).Unix(), 1)
	// A different room must be unaffected by acme/standup's cap.
	seedRecording(t, store, "EG_other", "acme", "retro", now.Add(-9*time.Hour).Unix(), 1)

	pol := RetentionPolicy{MaxPerRoom: 2}
	recs, _ := store.List(context.Background())
	expired := pol.expiredEgressIDs(recs, now)
	if !expired["EG_1"] {
		t.Fatalf("oldest (EG_1) should be evicted by MaxPerRoom=2")
	}
	if expired["EG_2"] || expired["EG_3"] {
		t.Fatalf("the 2 newest must be kept: %+v", expired)
	}
	if expired["EG_other"] {
		t.Fatalf("a different room must not be evicted by another room's cap")
	}
}

func TestRetention_MaxPerTenantEvictsOldest(t *testing.T) {
	now := time.Now()
	store := NewMemRecordingStore()
	seedRecording(t, store, "EG_a", "acme", "r1", now.Add(-4*time.Hour).Unix(), 1)
	seedRecording(t, store, "EG_b", "acme", "r2", now.Add(-3*time.Hour).Unix(), 1)
	seedRecording(t, store, "EG_c", "acme", "r3", now.Add(-2*time.Hour).Unix(), 1)
	// globex is a separate tenant; its single recording is never touched.
	seedRecording(t, store, "EG_g", "globex", "r1", now.Add(-99*time.Hour).Unix(), 1)

	pol := RetentionPolicy{MaxPerTenant: 2}
	recs, _ := store.List(context.Background())
	expired := pol.expiredEgressIDs(recs, now)
	if !expired["EG_a"] {
		t.Fatalf("oldest of acme (EG_a) should be evicted by MaxPerTenant=2")
	}
	if expired["EG_g"] {
		t.Fatalf("globex must not be affected by acme's per-tenant cap")
	}
}

func TestRetention_ZeroPolicyRetainsForever(t *testing.T) {
	now := time.Now()
	store := NewMemRecordingStore()
	seedRecording(t, store, "EG_ancient", "acme", "standup", now.Add(-1000*time.Hour).Unix(), 1)
	var pol RetentionPolicy // zero
	if pol.Enabled() {
		t.Fatalf("zero policy must report disabled")
	}
	recs, _ := store.List(context.Background())
	if len(pol.expiredEgressIDs(recs, now)) != 0 {
		t.Fatalf("zero policy must never expire anything")
	}
}

func TestRetentionDriver_CleanupDeletesExpired(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	store := NewMemRecordingStore()
	seedRecording(t, store, "EG_old", "acme", "standup", now.Add(-48*time.Hour).Unix(), 4096)
	seedRecording(t, store, "EG_fresh", "acme", "standup", now.Add(-1*time.Hour).Unix(), 8192)

	deleter := newFakeBlobDeleter()
	drv, err := NewRetentionDriver(store, RetentionPolicy{TTL: 24 * time.Hour}, deleter)
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	drv.now = func() time.Time { return now } // freeze time

	res, err := drv.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Deleted != 1 || res.Expired != 1 {
		t.Fatalf("expected 1 expired + 1 deleted, got %+v", res)
	}
	if res.BytesFreed != 4096 {
		t.Fatalf("expected 4096 bytes freed, got %d", res.BytesFreed)
	}
	if deleter.count() != 1 {
		t.Fatalf("expected blob deleter called once, got %d", deleter.count())
	}

	// Ledger: EG_old → deleted, EG_fresh untouched (available).
	recs, _ := store.List(context.Background())
	byID := map[string]Recording{}
	for _, r := range recs {
		byID[r.EgressID] = r
	}
	if byID["EG_old"].State != RecordingStateDeleted {
		t.Fatalf("EG_old should be deleted, got %q", byID["EG_old"].State)
	}
	if byID["EG_old"].DeletedAt == 0 {
		t.Fatalf("EG_old should have a DeletedAt stamp")
	}
	if byID["EG_fresh"].State != RecordingStateAvailable {
		t.Fatalf("EG_fresh should remain available, got %q", byID["EG_fresh"].State)
	}
}

func TestRetentionDriver_DeleteFailureLeavesExpiredForRetry(t *testing.T) {
	now := time.Now()
	store := NewMemRecordingStore()
	seedRecording(t, store, "EG_x", "acme", "standup", now.Add(-100*time.Hour).Unix(), 10)

	deleter := newFakeBlobDeleter()
	deleter.failIDs["EG_x"] = true // first sweep fails the delete

	drv, _ := NewRetentionDriver(store, RetentionPolicy{TTL: time.Hour}, deleter)
	res, err := drv.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Deleted != 0 || res.DeleteErrs != 1 || res.Expired != 1 {
		t.Fatalf("failed delete should leave expired w/ 1 err, got %+v", res)
	}
	recs, _ := store.List(context.Background())
	if recs[0].State != RecordingStateExpired {
		t.Fatalf("after a failed delete the recording must stay expired (retry-able), got %q", recs[0].State)
	}

	// Next sweep: deleter recovers → delete succeeds, advances to deleted.
	delete(deleter.failIDs, "EG_x")
	res2, _ := drv.Run(context.Background())
	if res2.Deleted != 1 {
		t.Fatalf("retry sweep should delete, got %+v", res2)
	}
	if res2.Expired != 0 {
		t.Fatalf("already-expired entry must not be re-counted as newly expired, got %+v", res2)
	}
	recs2, _ := store.List(context.Background())
	if recs2[0].State != RecordingStateDeleted {
		t.Fatalf("retry should advance to deleted, got %q", recs2[0].State)
	}
}

func TestRetentionDriver_NoopWhenPolicyDisabled(t *testing.T) {
	store := NewMemRecordingStore()
	seedRecording(t, store, "EG_x", "acme", "standup", 1, 10)
	drv, _ := NewRetentionDriver(store, RetentionPolicy{}, nil) // disabled + nil deleter
	res, err := drv.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Deleted != 0 || res.Expired != 0 {
		t.Fatalf("disabled policy must be a no-op, got %+v", res)
	}
}

func TestMemRecordingStore_UpsertIsMonotonic(t *testing.T) {
	store := NewMemRecordingStore()
	ctx := context.Background()
	// started → available, then a late duplicate started arrives.
	_ = store.Upsert(ctx, Recording{EgressID: "EG", Tenant: "acme", Room: "r", State: RecordingStateRecording, StartedAt: 100})
	_ = store.Upsert(ctx, Recording{EgressID: "EG", Tenant: "acme", Room: "r", State: RecordingStateAvailable, EndedAt: 200, SizeBytes: 50})
	_ = store.Upsert(ctx, Recording{EgressID: "EG", Tenant: "acme", Room: "r", State: RecordingStateRecording, StartedAt: 100})
	recs, _ := store.List(ctx)
	if len(recs) != 1 {
		t.Fatalf("expected 1 ledger entry, got %d", len(recs))
	}
	if recs[0].State != RecordingStateAvailable {
		t.Fatalf("a late started event must not regress available, got %q", recs[0].State)
	}
	if recs[0].SizeBytes != 50 {
		t.Fatalf("size should be retained, got %d", recs[0].SizeBytes)
	}
}

func TestMemRecordingStore_SetStateUnknownReturnsNotFound(t *testing.T) {
	store := NewMemRecordingStore()
	err := store.SetState(context.Background(), "nope", RecordingStateDeleted, 1)
	if !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("expected ErrRecordingNotFound, got %v", err)
	}
}

// TestCloudBlobDeleter_DeleteContract exercises the FLAGGED external seam: the
// deleter must issue the documented DELETE to the cloud sink with the bearer +
// tenant headers, and treat 2xx/204/404 as "blob gone".
func TestCloudBlobDeleter_DeleteContract(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotTenant, gotSchema string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Vulos-Tenant")
		gotSchema = r.Header.Get("X-Vulos-Schema")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d, err := NewCloudBlobDeleter(srv.URL, "cloud-token", nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	err = d.Delete(context.Background(), Recording{EgressID: "EG_42", Tenant: "acme", Room: "standup"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method: %q", gotMethod)
	}
	if gotPath != "/v1/recordings/EG_42" {
		t.Fatalf("path: %q", gotPath)
	}
	if gotAuth != "Bearer cloud-token" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if gotTenant != "acme" {
		t.Fatalf("tenant header: %q", gotTenant)
	}
	if gotSchema != CloudDeleteSchema {
		t.Fatalf("schema header: %q", gotSchema)
	}
}

func TestCloudBlobDeleter_404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	d, _ := NewCloudBlobDeleter(srv.URL, "tok", nil)
	if err := d.Delete(context.Background(), Recording{EgressID: "EG_gone"}); err != nil {
		t.Fatalf("404 must be idempotent success, got %v", err)
	}
}

func TestCloudBlobDeleter_5xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	d, _ := NewCloudBlobDeleter(srv.URL, "tok", nil)
	if err := d.Delete(context.Background(), Recording{EgressID: "EG_x"}); err == nil {
		t.Fatalf("5xx must surface as error so the driver retries")
	}
}

// TestRetentionDriver_CloudDeleterEndToEnd wires a real MemRecordingStore + the
// CloudBlobDeleter against a stub cloud sink and confirms the full
// available→expired→deleted walk dispatches to the cloud endpoint.
func TestRetentionDriver_CloudDeleterEndToEnd(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deletes++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now()
	store := NewMemRecordingStore()
	seedRecording(t, store, "EG_old", "acme", "standup", now.Add(-100*time.Hour).Unix(), 123)

	deleter, _ := NewCloudBlobDeleter(srv.URL, "tok", nil)
	drv, _ := NewRetentionDriver(store, RetentionPolicy{TTL: time.Hour}, deleter)
	res, err := drv.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deleted via cloud, got %+v", res)
	}
	if deletes != 1 {
		t.Fatalf("expected 1 cloud delete call, got %d", deletes)
	}
}
