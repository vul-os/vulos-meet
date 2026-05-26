// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Recording retention / lifecycle (MEET-RECORDING-RETENTION-06).
//
// Boundary, stated up front so it is impossible to misread the seam:
//
//   - vulos-meet is the ONLY LiveKit-talking surface. It already verifies and
//     forwards egress webhooks to vulos-cloud (see egress_proxy.go). The actual
//     recording BLOB (the .mp4/.ogg in object storage) is owned by the cloud
//     MEET-RECORDING-01 S3/Tigris sink — vulos-meet never holds the bytes.
//
//   - What vulos-meet CAN own is the POLICY and the LIFECYCLE LEDGER: per-room
//     / per-tenant retention rules, a state machine for each egress
//     (recording → available → expired → deleted), and a cleanup DRIVER that
//     decides what is past-retention and issues a deletion against a seam.
//
//   - The deletion seam is RecordingBlobDeleter. The in-tree default
//     (NoopBlobDeleter) records the lifecycle transition but does NOT touch any
//     blob — it is what a self-host box with no central sink uses. A cloud
//     deployment wires a CloudBlobDeleter (an HTTP call back to the
//     MEET-RECORDING-01 sink), which is the genuinely-external piece: the cloud
//     repo owns the bytes and the actual S3 DeleteObject. We FLAG that here and
//     give it a clean, tested hook (see CloudBlobDeleter below) so the cloud
//     side is a thin wire-up, not a redesign.
//
// In short: the policy + the lifecycle ledger + the cleanup driver are REAL and
// live here; the blob delete is delegated through a one-method interface that
// the cloud implements.

// RecordingState is the lifecycle state of a single egress recording.
type RecordingState string

const (
	// RecordingStateRecording — egress_started seen; capture in progress.
	RecordingStateRecording RecordingState = "recording"
	// RecordingStateAvailable — egress_ended/egress_updated(complete) seen; the
	// blob is finalised in the cloud sink and downloadable.
	RecordingStateAvailable RecordingState = "available"
	// RecordingStateFailed — egress_updated with a failed status; no usable blob.
	RecordingStateFailed RecordingState = "failed"
	// RecordingStateExpired — past its retention deadline; cleanup has marked it
	// but the blob delete has not yet been confirmed.
	RecordingStateExpired RecordingState = "expired"
	// RecordingStateDeleted — the blob delete was confirmed by the deleter seam.
	RecordingStateDeleted RecordingState = "deleted"
)

// Recording is one egress's lifecycle ledger entry. It is metadata ONLY — the
// bytes live in the cloud sink. Sizes/durations are populated from the egress
// webhook payload when present (LiveKit reports file results on completion).
type Recording struct {
	EgressID string
	Tenant   string
	Room     string // per-tenant short room name (sep-stripped)
	State    RecordingState

	// StartedAt / EndedAt are server-receive times of the start/end webhooks
	// (Unix seconds). EndedAt is the retention anchor — TTL counts from the
	// moment the recording became Available, not from when it started.
	StartedAt int64
	EndedAt   int64

	// DurationMs / SizeBytes come from the LiveKit egress file result when the
	// completion webhook carries it. Zero until the completion event arrives.
	DurationMs uint64
	SizeBytes  uint64

	// ExpiredAt / DeletedAt are set by the cleanup driver as it walks the
	// state machine. Zero until the relevant transition occurs.
	ExpiredAt int64
	DeletedAt int64
}

// retainedFrom is the timestamp retention is measured from: the end time when
// the recording is finalised, else the start time as a conservative fallback.
func (r *Recording) retainedFrom() int64 {
	if r.EndedAt > 0 {
		return r.EndedAt
	}
	return r.StartedAt
}

// RecordingStore is the lifecycle ledger seam. Production uses the in-memory
// store below; the cloud could later back this with its own durable store
// (the interface is intentionally small so that swap is mechanical). All
// methods MUST be safe for concurrent use — the egress receiver writes from
// the webhook path while the cleanup driver reads/writes from its own loop.
type RecordingStore interface {
	// Upsert records or advances a recording's lifecycle from a webhook event.
	// It is idempotent: replaying the same event must not regress state.
	Upsert(ctx context.Context, r Recording) error
	// List returns a snapshot of all recordings (cleanup driver walks this).
	List(ctx context.Context) ([]Recording, error)
	// SetState transitions one recording to a new state, stamping the
	// transition time. Returns ErrRecordingNotFound if the egress is unknown.
	SetState(ctx context.Context, egressID string, state RecordingState, at int64) error
}

// ErrRecordingNotFound is returned by a store when an egress ID is unknown.
var ErrRecordingNotFound = errors.New("vulos-meet: recording not found")

// RecordingBlobDeleter is the deletion seam. The cleanup driver calls Delete
// for each past-retention recording; the implementation owns the actual blob.
//
//   - NoopBlobDeleter: self-host default — no central sink, nothing to delete.
//   - CloudBlobDeleter: FLAG — the cloud MEET-RECORDING-01 sink owns the bytes;
//     this issues the delete request back to it. The cloud is the blob owner.
type RecordingBlobDeleter interface {
	// Delete removes the underlying blob for an egress. Returning nil means the
	// blob is gone (or was already gone) and the ledger may advance to Deleted.
	// A non-nil error leaves the recording in Expired so the next cleanup pass
	// retries — deletion must be at-least-once, never silently dropped.
	Delete(ctx context.Context, r Recording) error
}

// NoopBlobDeleter is the self-host default: there is no central blob to delete,
// so it always succeeds and the ledger advances straight to Deleted. This is
// the correct behaviour for a box whose recording.egress_endpoint is unset
// (the egress events were never forwarded to a sink).
type NoopBlobDeleter struct{}

// Delete is a no-op success.
func (NoopBlobDeleter) Delete(context.Context, Recording) error { return nil }

// MemRecordingStore is the in-memory lifecycle ledger. It is the production
// default for a single box: recordings are short-lived ledger entries and the
// authoritative blob lives in the cloud sink, so durability of the LEDGER
// across a box restart is not required (a restarted box re-learns state from
// subsequent webhooks, and the cloud sink can run its own backstop sweep).
type MemRecordingStore struct {
	mu  sync.Mutex
	rec map[string]*Recording
}

// NewMemRecordingStore returns an empty in-memory ledger.
func NewMemRecordingStore() *MemRecordingStore {
	return &MemRecordingStore{rec: make(map[string]*Recording)}
}

// Upsert records or advances a recording. Idempotent and monotonic-ish: a
// terminal state (deleted) is never regressed by a late duplicate webhook, and
// completion metadata (size/duration) is filled in but not zeroed by replays.
func (s *MemRecordingStore) Upsert(_ context.Context, in Recording) error {
	if in.EgressID == "" {
		return errors.New("vulos-meet: recording upsert requires an egress id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.rec[in.EgressID]
	if !ok {
		cp := in
		s.rec[in.EgressID] = &cp
		return nil
	}
	// Merge: never regress a terminal/expired state from an out-of-order event.
	if !stateRegresses(cur.State, in.State) {
		cur.State = in.State
	}
	if in.Tenant != "" {
		cur.Tenant = in.Tenant
	}
	if in.Room != "" {
		cur.Room = in.Room
	}
	if in.StartedAt > 0 && cur.StartedAt == 0 {
		cur.StartedAt = in.StartedAt
	}
	if in.EndedAt > 0 {
		cur.EndedAt = in.EndedAt
	}
	if in.DurationMs > 0 {
		cur.DurationMs = in.DurationMs
	}
	if in.SizeBytes > 0 {
		cur.SizeBytes = in.SizeBytes
	}
	return nil
}

// List returns a copy of every ledger entry (stable order by egress id).
func (s *MemRecordingStore) List(_ context.Context) ([]Recording, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Recording, 0, len(s.rec))
	for _, r := range s.rec {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EgressID < out[j].EgressID })
	return out, nil
}

// SetState transitions a recording, stamping ExpiredAt/DeletedAt as relevant.
func (s *MemRecordingStore) SetState(_ context.Context, egressID string, state RecordingState, at int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.rec[egressID]
	if !ok {
		return ErrRecordingNotFound
	}
	cur.State = state
	switch state {
	case RecordingStateExpired:
		cur.ExpiredAt = at
	case RecordingStateDeleted:
		if cur.ExpiredAt == 0 {
			cur.ExpiredAt = at
		}
		cur.DeletedAt = at
	}
	return nil
}

// stateRegresses reports whether moving from->to would walk the lifecycle
// backwards (e.g. an out-of-order egress_started after egress_ended). We keep a
// simple rank: recording < available/failed < expired < deleted.
func stateRegresses(from, to RecordingState) bool {
	return stateRank(to) < stateRank(from)
}

func stateRank(s RecordingState) int {
	switch s {
	case RecordingStateRecording:
		return 0
	case RecordingStateAvailable, RecordingStateFailed:
		return 1
	case RecordingStateExpired:
		return 2
	case RecordingStateDeleted:
		return 3
	default:
		return 0
	}
}

// RetentionPolicy is the configurable retention rule set. A recording is
// past-retention when EITHER the TTL has elapsed OR the per-scope count cap is
// exceeded (oldest-first). Both are optional; a zero policy retains forever
// (the cleanup driver then never deletes anything).
//
// Scope: caps apply per (tenant, room) by default — a busy room does not evict
// another room's recordings. Set MaxPerTenant to additionally cap a tenant's
// total across all its rooms.
type RetentionPolicy struct {
	// TTL is the max age (from the recording's finalisation time) before it is
	// eligible for deletion. Zero disables the TTL rule.
	TTL time.Duration

	// MaxPerRoom caps how many recordings to keep per (tenant, room); the
	// oldest beyond the cap are eligible for deletion. Zero disables.
	MaxPerRoom int

	// MaxPerTenant caps a tenant's total recordings across all its rooms. Zero
	// disables. Applied after MaxPerRoom.
	MaxPerTenant int
}

// Enabled reports whether the policy would ever delete anything.
func (p RetentionPolicy) Enabled() bool {
	return p.TTL > 0 || p.MaxPerRoom > 0 || p.MaxPerTenant > 0
}

// expiredEgressIDs returns the set of egress IDs that are past retention given
// the policy and the current time. Only Available/Failed/Expired recordings are
// eligible — an in-progress (recording) entry is never deleted, and an already
// Deleted entry is skipped. The returned set is deterministic for a given
// input so the driver and tests agree.
func (p RetentionPolicy) expiredEgressIDs(recs []Recording, now time.Time) map[string]bool {
	out := make(map[string]bool)
	if !p.Enabled() {
		return out
	}
	eligible := make([]Recording, 0, len(recs))
	for _, r := range recs {
		switch r.State {
		case RecordingStateRecording, RecordingStateDeleted:
			continue // never delete in-progress; skip already-gone
		}
		eligible = append(eligible, r)
	}

	// TTL rule.
	if p.TTL > 0 {
		cutoff := now.Add(-p.TTL).Unix()
		for _, r := range eligible {
			if r.retainedFrom() > 0 && r.retainedFrom() <= cutoff {
				out[r.EgressID] = true
			}
		}
	}

	// Count rules. Group by (tenant, room) for MaxPerRoom and by tenant for
	// MaxPerTenant; within a group keep the newest N, mark the rest.
	if p.MaxPerRoom > 0 {
		byRoom := groupBy(eligible, func(r Recording) string { return r.Tenant + "\x00" + r.Room })
		for _, grp := range byRoom {
			markOldestBeyond(grp, p.MaxPerRoom, out)
		}
	}
	if p.MaxPerTenant > 0 {
		byTenant := groupBy(eligible, func(r Recording) string { return r.Tenant })
		for _, grp := range byTenant {
			markOldestBeyond(grp, p.MaxPerTenant, out)
		}
	}
	return out
}

// groupBy partitions recordings by a key function, preserving nothing about
// order (the caller sorts).
func groupBy(recs []Recording, key func(Recording) string) map[string][]Recording {
	m := make(map[string][]Recording)
	for _, r := range recs {
		k := key(r)
		m[k] = append(m[k], r)
	}
	return m
}

// markOldestBeyond sorts a group newest-first and marks everything past `keep`
// for deletion. Newness is by finalisation time, tie-broken by egress id so the
// result is deterministic.
func markOldestBeyond(grp []Recording, keep int, out map[string]bool) {
	if keep <= 0 || len(grp) <= keep {
		return
	}
	sort.Slice(grp, func(i, j int) bool {
		ai, aj := grp[i].retainedFrom(), grp[j].retainedFrom()
		if ai != aj {
			return ai > aj // newest first
		}
		return grp[i].EgressID > grp[j].EgressID
	})
	for _, r := range grp[keep:] {
		out[r.EgressID] = true
	}
}

// RetentionDriver is the cleanup job. On each Run it lists the ledger, asks the
// policy which recordings are past retention, marks them Expired, issues the
// blob delete through the deleter seam, and on success advances them to
// Deleted. A delete failure leaves the entry Expired so the next pass retries.
type RetentionDriver struct {
	store   RecordingStore
	policy  RetentionPolicy
	deleter RecordingBlobDeleter
	metrics *Metrics // optional
	now     func() time.Time
}

// NewRetentionDriver builds a cleanup driver. A nil deleter falls back to the
// no-op deleter (self-host: nothing central to delete).
func NewRetentionDriver(store RecordingStore, policy RetentionPolicy, deleter RecordingBlobDeleter) (*RetentionDriver, error) {
	if store == nil {
		return nil, errors.New("vulos-meet: retention driver requires a store")
	}
	if deleter == nil {
		deleter = NoopBlobDeleter{}
	}
	return &RetentionDriver{
		store:   store,
		policy:  policy,
		deleter: deleter,
		now:     time.Now,
	}, nil
}

// SetMetrics attaches a metrics registry (optional; nil clears it).
func (d *RetentionDriver) SetMetrics(m *Metrics) { d.metrics = m }

// RetentionSweepResult summarises one cleanup pass for logging/tests.
type RetentionSweepResult struct {
	Examined    int // recordings considered
	Expired     int // newly marked expired this pass
	Deleted     int // blob delete confirmed and advanced to deleted
	DeleteErrs  int // delete attempts that failed (left expired for retry)
	BytesFreed  uint64
	DurationFreed time.Duration
}

// Run performs one cleanup pass. Safe to call from a ticker; it does not retain
// any state between calls beyond what the store holds.
func (d *RetentionDriver) Run(ctx context.Context) (RetentionSweepResult, error) {
	var res RetentionSweepResult
	if !d.policy.Enabled() {
		return res, nil // no policy → nothing to do (retain forever)
	}
	recs, err := d.store.List(ctx)
	if err != nil {
		return res, err
	}
	res.Examined = len(recs)
	now := d.now()
	expired := d.policy.expiredEgressIDs(recs, now)
	// Index for size/duration accounting.
	byID := make(map[string]Recording, len(recs))
	for _, r := range recs {
		byID[r.EgressID] = r
	}
	at := now.Unix()
	for id := range expired {
		rec := byID[id]
		// Mark expired first so a crash mid-delete leaves a retry-able state.
		if rec.State != RecordingStateExpired {
			if err := d.store.SetState(ctx, id, RecordingStateExpired, at); err != nil {
				if errors.Is(err, ErrRecordingNotFound) {
					continue
				}
				return res, err
			}
			res.Expired++
		}
		rec.State = RecordingStateExpired
		if err := d.deleter.Delete(ctx, rec); err != nil {
			res.DeleteErrs++
			continue // stays expired; next pass retries
		}
		if err := d.store.SetState(ctx, id, RecordingStateDeleted, at); err != nil {
			if errors.Is(err, ErrRecordingNotFound) {
				continue
			}
			return res, err
		}
		res.Deleted++
		res.BytesFreed += rec.SizeBytes
		res.DurationFreed += time.Duration(rec.DurationMs) * time.Millisecond
	}
	d.metrics.ObserveRetentionSweep(res)
	return res, nil
}

// RunLoop runs Run on a ticker until ctx is cancelled. Intended to be launched
// in its own goroutine from main.go. A non-positive interval disables the loop
// (returns immediately) so an operator can turn the sweeper off by leaving the
// interval unset.
func (d *RetentionDriver) RunLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 || !d.policy.Enabled() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = d.Run(ctx)
		}
	}
}
