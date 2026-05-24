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
	envelope, err := r.envelopeFromWebhookBody(raw)
	if err != nil {
		// Body verified OK but content was unusable. Return 400 not 500.
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
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
func (r *EgressReceiver) envelopeFromWebhookBody(raw []byte) (*VulosEgressEnvelope, error) {
	// Minimal struct mirroring the fields we care about; lets us avoid
	// pulling in proto-json unmarshalling.
	var ev struct {
		Event string `json:"event"`
		Room  struct {
			Name string `json:"name"`
		} `json:"room"`
		EgressInfo struct {
			EgressId string `json:"egress_id"`
			RoomName string `json:"room_name"`
		} `json:"egress_info"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("vulos-meet: parse webhook body: %w", err)
	}
	// Egress events carry the room name on EgressInfo.RoomName; non-egress
	// events use Room.Name. We accept either; the receiver is "any LiveKit
	// event" — the cloud filters by event name on its side.
	fullRoom := ev.EgressInfo.RoomName
	if fullRoom == "" {
		fullRoom = ev.Room.Name
	}
	if fullRoom == "" {
		return nil, errors.New("vulos-meet: webhook event has no room name")
	}
	tenant, rest, err := r.cfg.Tenant.SplitRoom(fullRoom)
	if err != nil {
		// The room does not parse into a (tenant, rest) — drop. This is
		// the cross-tenant audit point: LiveKit was somehow tricked into
		// emitting an event for a room that does not belong to any Vulos
		// tenant, and we refuse to launder it to the cloud.
		return nil, fmt.Errorf("vulos-meet: webhook room %q does not parse: %w", fullRoom, err)
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
	}, nil
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
