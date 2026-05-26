// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
)

// EgressReceiverConfig configures the egress-webhook receiver.
type EgressReceiverConfig struct {
	// Tenant is the namespace gate used to extract a tenant from the room
	// name on each event.
	Tenant *Tenant

	// APIKey + APISecret are the shared LiveKit key/secret pair. The
	// receiver verifies inbound webhooks against this pair using
	// livekit/protocol/webhook.Receive. SAME pair as the validator.
	APIKey    string
	APISecret string

	// CloudURL is the vulos-cloud MEET-RECORDING-01 endpoint we forward
	// the Vulos-shaped envelope to. Empty disables forwarding (the
	// receiver still verifies + parses, useful for self-host deployments
	// that don't need a central recording store).
	CloudURL string

	// CloudAuthTok is the bearer token used on the forward leg. We use a
	// separate token from LiveKit's webhook secret so the cloud can rotate
	// its inbound auth without dragging livekit-server's webhook config
	// behind it. Read from env (MEET_RECORDING_CLOUD_TOKEN) in main.go.
	CloudAuthTok string

	// HTTPClient is the upstream client used to push to CloudURL. Defaults
	// to a 10s-timeout client.
	HTTPClient *http.Client

	// MaxAttempts caps the retry count for cloud forwarding. Defaults to 4
	// (the initial attempt + 3 retries, ~7s total at the default backoff).
	MaxAttempts int

	// BaseBackoff is the initial retry backoff. Defaults to 250ms;
	// doubles each retry up to MaxAttempts.
	BaseBackoff time.Duration

	// Store is the recording lifecycle ledger (MEET-RECORDING-RETENTION-06).
	// When set, the receiver advances each egress's lifecycle state from the
	// webhook event (started → recording, ended/complete → available, failed →
	// failed) so the retention driver has something to sweep. Nil disables
	// ledger tracking (the receiver still verifies + forwards). The blob bytes
	// remain owned by the cloud sink — the ledger is metadata only.
	Store RecordingStore

	// Metrics, when set, receives per-egress lifecycle counters/gauges as the
	// receiver advances recordings. Optional; nil disables emission.
	Metrics *Metrics
}

// EgressReceiver is the LiveKit egress webhook receiver. It verifies the
// LiveKit-side signature, parses the WebhookEvent, scopes it to a tenant
// from the room-name prefix, and forwards a Vulos-shaped envelope to the
// cloud egress driver with bounded retries.
//
// Why this exists (vs. pointing LiveKit directly at the cloud):
//
//   - LiveKit signs webhooks with the API key/secret pair we already share
//     with this box; routing through us means the cloud doesn't need that
//     secret in its own rotation loop.
//   - We can attach the Vulos tenant identifier on the forward leg, so the
//     cloud doesn't have to re-derive the tenant from a LiveKit room name.
//   - We get an obvious cross-tenant audit point: any event whose room
//     prefix does not parse into a recognised tenant is dropped here, not
//     in the cloud.
type EgressReceiver struct {
	cfg     EgressReceiverConfig
	keyProv auth.KeyProvider
	httpc   *http.Client
	store   RecordingStore // optional lifecycle ledger
	metrics *Metrics       // optional
}

// NewEgressReceiver constructs a receiver. CloudURL may be empty (drops
// forwarding entirely — verification + parse still happen).
func NewEgressReceiver(cfg EgressReceiverConfig) (*EgressReceiver, error) {
	if cfg.Tenant == nil {
		return nil, errors.New("vulos-meet: egress receiver requires a tenant gate")
	}
	if cfg.APIKey == "" || cfg.APISecret == "" {
		return nil, errors.New("vulos-meet: egress receiver requires api_key/api_secret")
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.BaseBackoff == 0 {
		cfg.BaseBackoff = 250 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &EgressReceiver{
		cfg:     cfg,
		keyProv: auth.NewSimpleKeyProvider(cfg.APIKey, cfg.APISecret),
		httpc:   cfg.HTTPClient,
		store:   cfg.Store,
		metrics: cfg.Metrics,
	}, nil
}

// WebhookPath is the path LiveKit posts egress events to.
const WebhookPath = "/v1/egress/webhook"

// Handler returns an http.Handler that mounts the receiver at WebhookPath.
func (r *EgressReceiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(WebhookPath, r.handle)
	return mux
}

// VulosEgressEnvelope is the cloud-facing shape. It is intentionally narrow:
// the cloud doesn't need every LiveKit field, only the seam-level metadata
// it needs to file the recording against a tenant. The full LiveKit
// WebhookEvent JSON is included verbatim under raw so the cloud retains the
// option to peek if a debug round-trip is ever required.
type VulosEgressEnvelope struct {
	Schema   string          `json:"schema"`    // "vulos-meet/egress/v1"
	Event    string          `json:"event"`     // e.g. "egress_started"
	Tenant   string          `json:"tenant"`    // parsed from room-name prefix
	Room     string          `json:"room"`      // per-tenant short name (sep-stripped)
	FullRoom string          `json:"full_room"` // <tenant><sep><rest>
	EgressID string          `json:"egress_id"` // EgressInfo.EgressId when present
	At       int64           `json:"at"`        // Unix seconds (server time on receive)
	Raw      json.RawMessage `json:"raw"`       // verbatim LiveKit webhook event JSON
}

func (r *EgressReceiver) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// webhook.Receive verifies the Authorization JWT against our keyProvider
	// AND verifies the sha256 of the body matches the JWT's `sha256` claim.
	// It closes req.Body for us.
	raw, err := webhook.Receive(req, r.keyProv)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	envelope, parsed, err := r.envelopeFromWebhookBody(raw)
	if err != nil {
		// Body verified OK but content was unusable. Return 400 not 500.
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	// Advance the lifecycle ledger BEFORE forwarding. The ledger is local
	// metadata; even if the cloud forward later fails (and LiveKit redelivers),
	// recording the state we just verified is correct and idempotent. The blob
	// itself stays owned by the cloud sink — we track state, not bytes.
	r.recordLifecycle(req.Context(), envelope, parsed)
	if r.cfg.CloudURL == "" {
		// Verified + parsed; nothing to forward. Self-host mode is a
		// supported deployment shape — accept and ack.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := r.forward(req.Context(), envelope); err != nil {
		// Forward failed after retries. Return 502 so LiveKit will retry
		// the webhook itself (it implements its own redelivery).
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// envelopeFromWebhookBody parses the JSON event body, drops events whose
// room name does not carry a recognisable tenant prefix, and builds the
// Vulos-shaped envelope. The body is JSON because LiveKit's webhook
// notifier emits JSON over the wire (the protobuf is only the in-memory
// representation).
func (r *EgressReceiver) envelopeFromWebhookBody(raw []byte) (*VulosEgressEnvelope, *parsedEgress, error) {
	// Minimal struct mirroring the fields we care about; lets us avoid
	// pulling in proto-json unmarshalling. We additionally read the egress
	// status + file results so the lifecycle ledger can record size/duration
	// and the available/failed terminal state.
	var ev struct {
		Event string `json:"event"`
		Room  struct {
			Name string `json:"name"`
		} `json:"room"`
		EgressInfo struct {
			EgressId string `json:"egress_id"`
			RoomName string `json:"room_name"`
			Status   string `json:"status"`
			File     struct {
				Size     json.Number `json:"size"`
				Duration json.Number `json:"duration"`
			} `json:"file"`
			FileResults []struct {
				Size     json.Number `json:"size"`
				Duration json.Number `json:"duration"`
			} `json:"file_results"`
		} `json:"egress_info"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, nil, fmt.Errorf("vulos-meet: parse webhook body: %w", err)
	}
	// Egress events carry the room name on EgressInfo.RoomName; non-egress
	// events use Room.Name. We accept either; the receiver is "any LiveKit
	// event" — the cloud filters by event name on its side.
	fullRoom := ev.EgressInfo.RoomName
	if fullRoom == "" {
		fullRoom = ev.Room.Name
	}
	if fullRoom == "" {
		return nil, nil, errors.New("vulos-meet: webhook event has no room name")
	}
	tenant, rest, err := r.cfg.Tenant.SplitRoom(fullRoom)
	if err != nil {
		// The room does not parse into a (tenant, rest) — drop. This is
		// the cross-tenant audit point: LiveKit was somehow tricked into
		// emitting an event for a room that does not belong to any Vulos
		// tenant, and we refuse to launder it to the cloud.
		return nil, nil, fmt.Errorf("vulos-meet: webhook room %q does not parse: %w", fullRoom, err)
	}
	pe := &parsedEgress{
		event:    ev.Event,
		status:   ev.EgressInfo.Status,
		egressID: ev.EgressInfo.EgressId,
	}
	// Prefer the single file block; fall back to the first file_results entry
	// (LiveKit emits file_results for room-composite egress). Sizes/durations
	// are best-effort — a missing/zero value just leaves the ledger field 0.
	pe.sizeBytes = parseUint(ev.EgressInfo.File.Size)
	pe.durationMs = parseUint(ev.EgressInfo.File.Duration)
	if pe.sizeBytes == 0 && len(ev.EgressInfo.FileResults) > 0 {
		pe.sizeBytes = parseUint(ev.EgressInfo.FileResults[0].Size)
	}
	if pe.durationMs == 0 && len(ev.EgressInfo.FileResults) > 0 {
		pe.durationMs = parseUint(ev.EgressInfo.FileResults[0].Duration)
	}
	return &VulosEgressEnvelope{
		Schema:   "vulos-meet/egress/v1",
		Event:    ev.Event,
		Tenant:   tenant,
		Room:     rest,
		FullRoom: fullRoom,
		EgressID: ev.EgressInfo.EgressId,
		At:       time.Now().Unix(),
		Raw:      json.RawMessage(raw),
	}, pe, nil
}

// parsedEgress carries the lifecycle-relevant fields extracted from a webhook
// event (separate from the cloud-facing envelope, which is intentionally
// narrow). It is only used to feed the local lifecycle ledger.
type parsedEgress struct {
	event      string
	status     string
	egressID   string
	sizeBytes  uint64
	durationMs uint64
}

// parseUint converts a json.Number (LiveKit serialises file sizes/durations as
// numeric strings or numbers depending on the proto-json codec) to uint64,
// returning 0 on any parse failure.
func parseUint(n json.Number) uint64 {
	if n == "" {
		return 0
	}
	v, err := n.Int64()
	if err != nil || v < 0 {
		return 0
	}
	return uint64(v)
}

// recordLifecycle advances the recording ledger from one webhook event. It is
// a no-op when no store is configured. Mapping:
//
//	egress_started                          → recording
//	egress_ended / egress_updated(complete) → available (with size/duration)
//	egress_updated(failed/aborted)          → failed
//
// LiveKit's egress status enum values are EGRESS_STARTING, EGRESS_ACTIVE,
// EGRESS_ENDING, EGRESS_COMPLETE, EGRESS_FAILED, EGRESS_ABORTED, EGRESS_LIMIT_
// REACHED. We map COMPLETE → available, FAILED/ABORTED → failed, and use the
// event name as a fallback when status is absent.
func (r *EgressReceiver) recordLifecycle(ctx context.Context, env *VulosEgressEnvelope, pe *parsedEgress) {
	if r.store == nil || pe == nil || pe.egressID == "" {
		return
	}
	now := time.Now().Unix()
	rec := Recording{
		EgressID: pe.egressID,
		Tenant:   env.Tenant,
		Room:     env.Room,
	}
	switch {
	case statusIsComplete(pe.status):
		rec.State = RecordingStateAvailable
		rec.EndedAt = now
		rec.SizeBytes = pe.sizeBytes
		rec.DurationMs = pe.durationMs
	case statusIsFailed(pe.status):
		rec.State = RecordingStateFailed
		rec.EndedAt = now
	case pe.event == "egress_ended":
		rec.State = RecordingStateAvailable
		rec.EndedAt = now
		rec.SizeBytes = pe.sizeBytes
		rec.DurationMs = pe.durationMs
	case pe.event == "egress_started":
		rec.State = RecordingStateRecording
		rec.StartedAt = now
	default:
		// Some intermediate egress_updated (active/ending) — record presence
		// but don't force a state we can't justify. Use recording as the
		// floor; Upsert won't regress a later available/failed.
		rec.State = RecordingStateRecording
		rec.StartedAt = now
	}
	if err := r.store.Upsert(ctx, rec); err == nil {
		r.metrics.ObserveRecordingLifecycle(rec.State)
		if rec.State == RecordingStateAvailable && (rec.SizeBytes > 0 || rec.DurationMs > 0) {
			r.metrics.ObserveRecordingBytes(rec.SizeBytes, rec.DurationMs)
		}
	}
}

// statusIsComplete / statusIsFailed classify a LiveKit egress status string.
func statusIsComplete(s string) bool {
	return s == "EGRESS_COMPLETE"
}

func statusIsFailed(s string) bool {
	switch s {
	case "EGRESS_FAILED", "EGRESS_ABORTED", "EGRESS_LIMIT_REACHED":
		return true
	default:
		return false
	}
}

// forward POSTs the envelope to the cloud with bounded exponential-backoff
// retries. Returns nil on first 2xx, or the last error if every attempt
// fails.
func (r *EgressReceiver) forward(ctx context.Context, env *VulosEgressEnvelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	var lastErr error
	backoff := r.cfg.BaseBackoff
	for attempt := 0; attempt < r.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.CloudURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Vulos-Schema", env.Schema)
		req.Header.Set("X-Vulos-Tenant", env.Tenant)
		if r.cfg.CloudAuthTok != "" {
			req.Header.Set("Authorization", "Bearer "+r.cfg.CloudAuthTok)
		}
		resp, err := r.httpc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("vulos-meet: cloud egress returned %d", resp.StatusCode)
		// Don't retry on 4xx — those are caller bugs and the cloud will
		// keep rejecting. Only 5xx + transport errors retry.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("vulos-meet: cloud egress: no attempts succeeded")
	}
	return lastErr
}
