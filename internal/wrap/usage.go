// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
)

// Meet usage metering (cp seam).
//
// vulos-meet meters meet usage from LiveKit's room/participant lifecycle
// webhooks so the central Vulos control plane (cp) can bill meet as part of the
// suite billing model. The metering surface is OPTIONAL and removable:
//
//   - The receiver verifies the LiveKit webhook signature (same key/secret pair
//     as the egress receiver) and tracks each participant's joined→left span to
//     compute participant-minutes per room, plus room count/duration.
//   - Usage is attributed to a Vulos account via the tenant prefix on the room
//     name (reusing the Tenant gate — the same parsing the egress receiver and
//     signal-gate already trust).
//   - When a room finishes (room_finished), the accumulated participant-minutes
//     are reported through the UsageReporter seam. The reporter is an OPTIONAL
//     interface; when nil, the receiver still verifies + tracks + exposes the
//     /admin stats read, but reports nothing. The cp client that satisfies the
//     reporter lives in package internal/cp, which THIS package never imports.
//
// The import boundary: wrap defines UsageReporter; internal/cp implements it;
// main.go is the only place they meet, and only when CP_URL is configured. With
// no reporter wired, vulos-meet is the standalone, cp-free product it has always
// been.

// UsageReporter is the OPTIONAL cp metering seam. It is implemented in package
// internal/cp by *cp.UsageClient (structural match — wrap does not import cp).
// A nil reporter disables reporting; the usage receiver still verifies events
// and tracks minutes for the admin stats read.
//
// account is the Vulos tenant/account the usage is billed to; kind is the usage
// kind (UsageKindMeetMinutes); count is the magnitude. idempotencyKey is a
// stable identifier for this logical usage event: the SAME key is presented on
// every delivery attempt (including reporter-internal retries) so the cp can
// dedupe and a momentary blip retried doesn't double-count minutes. The receiver
// derives a deterministic key per room lifecycle (see idempotencyKeyFor).
// Implementations MUST be fire-and-forget — Report must not block the webhook
// hot path.
type UsageReporter interface {
	Report(account, kind string, count int64, idempotencyKey string)
}

// UsageKindMeetMinutes is the usage kind reported for participant-minutes. It
// mirrors cp.KindMeetMinutes but is duplicated here so wrap stays free of any
// cp import.
const UsageKindMeetMinutes = "meet_minutes"

// participantSession tracks one participant's active span within a room.
type participantSession struct {
	joinedAt time.Time
}

// roomUsage accumulates a single room's metering state across the events of its
// lifetime. participant-minutes are summed as each participant leaves (or as the
// room finishes with participants still present).
type roomUsage struct {
	tenant     string
	shortRoom  string
	startedAt  time.Time
	live       map[string]participantSession // identity -> join span
	accumMin   float64                       // accumulated participant-minutes
	peakLive   int                           // peak concurrent participants
	joinsTotal int                           // total joins seen
}

// RoomUsageSnapshot is the read-only stats projection for the admin/stats read.
type RoomUsageSnapshot struct {
	Tenant             string  `json:"tenant"`
	Room               string  `json:"room"`
	StartedAtUnix      int64   `json:"started_at"`
	ParticipantMinutes float64 `json:"participant_minutes"`
	LiveParticipants   int     `json:"live_participants"`
	PeakParticipants   int     `json:"peak_participants"`
	TotalJoins         int     `json:"total_joins"`
}

// UsageReceiverConfig configures the meet-usage webhook receiver.
type UsageReceiverConfig struct {
	// Tenant is the namespace gate used to attribute each event to an account
	// via the room-name prefix.
	Tenant *Tenant

	// APIKey + APISecret are the shared LiveKit key/secret pair used to verify
	// inbound webhook signatures. SAME pair as the validator/egress receiver.
	APIKey    string
	APISecret string

	// Reporter is the OPTIONAL cp metering seam. Nil disables reporting (the
	// receiver still verifies + tracks + serves the admin stats read).
	Reporter UsageReporter

	// Metrics, when set, receives meet-usage counters/gauges. Optional.
	Metrics *Metrics

	// Now overrides the clock for tests. Defaults to time.Now.
	Now func() time.Time
}

// UsageReceiver is the LiveKit room/participant webhook receiver that meters
// participant-minutes per room and reports them to cp through the (optional)
// UsageReporter seam when a room finishes.
type UsageReceiver struct {
	tenant   *Tenant
	keyProv  auth.KeyProvider
	reporter UsageReporter
	metrics  *Metrics
	now      func() time.Time

	mu    sync.Mutex
	rooms map[string]*roomUsage // keyed by full room id (<tenant><sep><rest>)
}

// NewUsageReceiver constructs a usage receiver. Reporter may be nil (reporting
// disabled; tracking + admin read still work).
func NewUsageReceiver(cfg UsageReceiverConfig) (*UsageReceiver, error) {
	if cfg.Tenant == nil {
		return nil, errors.New("vulos-meet: usage receiver requires a tenant gate")
	}
	if cfg.APIKey == "" || cfg.APISecret == "" {
		return nil, errors.New("vulos-meet: usage receiver requires api_key/api_secret")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &UsageReceiver{
		tenant:   cfg.Tenant,
		keyProv:  auth.NewSimpleKeyProvider(cfg.APIKey, cfg.APISecret),
		reporter: cfg.Reporter,
		metrics:  cfg.Metrics,
		now:      now,
		rooms:    make(map[string]*roomUsage),
	}, nil
}

// UsageWebhookPath is the path LiveKit posts room/participant lifecycle events
// to for metering. Distinct from the egress webhook path so an operator can
// point LiveKit's webhook config at it independently.
const UsageWebhookPath = "/v1/usage/webhook"

// Handler returns an http.Handler mounting the receiver at UsageWebhookPath.
func (u *UsageReceiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(UsageWebhookPath, u.handle)
	return mux
}

// usageWebhookBody mirrors the subset of LiveKit's webhook event JSON the
// metering path needs. LiveKit emits JSON over the wire; we avoid pulling in
// proto-json by reading only these fields.
type usageWebhookBody struct {
	Event string `json:"event"`
	Room  struct {
		Name string `json:"name"`
	} `json:"room"`
	Participant struct {
		Identity string `json:"identity"`
	} `json:"participant"`
}

func (u *UsageReceiver) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Cap the body BEFORE signature verification so a multi-GB POST from an
	// unauthenticated caller cannot buffer into memory. 1 MiB matches the
	// egress_proxy.go cap; any legitimate LiveKit webhook is well under 1 KiB.
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
	// webhook.Receive verifies the Authorization JWT against our key provider
	// AND that the body sha256 matches the JWT claim. It closes req.Body.
	raw, err := webhook.Receive(req, u.keyProv)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev usageWebhookBody
	if err := json.Unmarshal(raw, &ev); err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	if ev.Room.Name == "" {
		// Some events (e.g. track-level) may arrive without a room name we
		// can attribute. Ack so LiveKit does not redeliver; nothing to meter.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Attribute to a tenant. An event whose room does not parse into a
	// recognised tenant is the cross-tenant audit point — drop with 400 rather
	// than meter it against nobody.
	tenant, short, err := u.tenant.SplitRoom(ev.Room.Name)
	if err != nil {
		http.Error(w, "bad room", http.StatusBadRequest)
		return
	}
	u.apply(ev.Event, ev.Room.Name, tenant, short, ev.Participant.Identity)
	w.WriteHeader(http.StatusNoContent)
}

// apply folds one verified, tenant-attributed event into the room's metering
// state, reporting to cp when the room finishes.
func (u *UsageReceiver) apply(event, fullRoom, tenant, short, identity string) {
	now := u.now()
	u.mu.Lock()
	defer u.mu.Unlock()

	switch event {
	case "room_started":
		ru := u.rooms[fullRoom]
		if ru == nil {
			ru = newRoomUsage(tenant, short)
			u.rooms[fullRoom] = ru
		}
		ru.startedAt = now
		u.metrics.ObserveMeetRoom(MeetRoomStarted)

	case "participant_joined":
		ru := u.roomFor(fullRoom, tenant, short)
		if identity == "" {
			return
		}
		// Idempotency: a redelivered join for an already-live participant must
		// not reset the join span (which would lose accrued minutes).
		if _, ok := ru.live[identity]; !ok {
			ru.live[identity] = participantSession{joinedAt: now}
			ru.joinsTotal++
		}
		if len(ru.live) > ru.peakLive {
			ru.peakLive = len(ru.live)
		}

	case "participant_left":
		ru := u.roomFor(fullRoom, tenant, short)
		if identity == "" {
			return
		}
		if sess, ok := ru.live[identity]; ok {
			ru.accumMin += minutesBetween(sess.joinedAt, now)
			delete(ru.live, identity)
		}

	case "room_finished":
		ru := u.rooms[fullRoom]
		if ru == nil {
			// room_finished with no tracked start — nothing accrued. Still
			// count the room transition.
			u.metrics.ObserveMeetRoom(MeetRoomFinished)
			return
		}
		// Close out any participants still live at room finish.
		for _, sess := range ru.live {
			ru.accumMin += minutesBetween(sess.joinedAt, now)
		}
		ru.live = map[string]participantSession{}
		minutes := roundMinutes(ru.accumMin)
		u.metrics.ObserveMeetRoom(MeetRoomFinished)
		u.metrics.ObserveMeetMinutes(ru.tenant, minutes)
		if u.reporter != nil && minutes > 0 {
			// Fire-and-forget; the reporter (cp client) must not block. The
			// idempotency key is deterministic per room lifecycle so any
			// reporter-internal retry presents the same key and the cp can
			// dedupe rather than double-count minutes.
			key := idempotencyKeyFor(ru.tenant, ru.shortRoom, ru.startedAt, now)
			u.reporter.Report(ru.tenant, UsageKindMeetMinutes, minutes, key)
		}
		delete(u.rooms, fullRoom)
	}
}

// roomFor returns the tracked room, creating it if a join/leave arrived before
// (or without) a room_started event — LiveKit does not guarantee strict event
// ordering on redelivery, so we tolerate a missing start.
func (u *UsageReceiver) roomFor(fullRoom, tenant, short string) *roomUsage {
	ru := u.rooms[fullRoom]
	if ru == nil {
		ru = newRoomUsage(tenant, short)
		ru.startedAt = u.now()
		u.rooms[fullRoom] = ru
	}
	return ru
}

func newRoomUsage(tenant, short string) *roomUsage {
	return &roomUsage{
		tenant:    tenant,
		shortRoom: short,
		live:      make(map[string]participantSession),
	}
}

// Snapshot returns a read-only view of the currently-tracked rooms for the
// given tenant, for the admin stats read. Live participant-minutes include the
// in-progress accrual of participants still present.
func (u *UsageReceiver) Snapshot(tenant string) []RoomUsageSnapshot {
	now := u.now()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]RoomUsageSnapshot, 0)
	for _, ru := range u.rooms {
		if ru.tenant != tenant {
			continue
		}
		live := ru.accumMin
		for _, sess := range ru.live {
			live += minutesBetween(sess.joinedAt, now)
		}
		out = append(out, RoomUsageSnapshot{
			Tenant:             ru.tenant,
			Room:               ru.shortRoom,
			StartedAtUnix:      unixOrZero(ru.startedAt),
			ParticipantMinutes: roundFloat(live),
			LiveParticipants:   len(ru.live),
			PeakParticipants:   ru.peakLive,
			TotalJoins:         ru.joinsTotal,
		})
	}
	return out
}

// idempotencyKeyFor derives a stable identifier for the usage event emitted
// when a room finishes. It is deterministic in the room's identity and its
// start/finish boundary, so:
//
//   - reporter-internal retries of the SAME finish present the SAME key (the cp
//     dedupes instead of double-counting minutes), and
//   - two distinct rooms — or the same short room reused after it finished and a
//     fresh one started — produce DISTINCT keys (no accidental dedupe).
//
// startedAt may be zero for a room whose start we never saw; the finished
// timestamp still disambiguates it from any other lifecycle.
func idempotencyKeyFor(tenant, shortRoom string, startedAt, finishedAt time.Time) string {
	return fmt.Sprintf("meet:%s:%s:%d:%d", tenant, shortRoom, unixOrZero(startedAt), finishedAt.Unix())
}

// minutesBetween returns the whole-and-fractional minutes between two times,
// clamped at 0 for non-monotonic clocks / out-of-order events.
func minutesBetween(start, end time.Time) float64 {
	d := end.Sub(start)
	if d <= 0 {
		return 0
	}
	return d.Minutes()
}

// roundMinutes rounds accumulated participant-minutes to the nearest whole
// minute for the cp count (cp meters integer minutes). We round rather than
// floor so a 90s span (1.5m) bills as 2 minutes, not 1 — closer to honest.
func roundMinutes(m float64) int64 {
	if m <= 0 {
		return 0
	}
	return int64(m + 0.5)
}

// roundFloat rounds a float to 2 decimals for the human-facing admin read.
func roundFloat(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
