// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vul-os/vulos-apps/appsplatform"
	"github.com/vul-os/vulos-apps/mcp"
)

// This file makes vulos-meet host an "Apps & Bots place" on the shared Vulos
// Apps & Bots platform (github.com/vul-os/vulos-apps). It supplies the Meet
// ProductAdapter (how an app acts/reads in a meeting) and a mount helper that
// wires the platform's HTTP handler set behind Meet's existing admin-bearer auth.
//
// Open-core discipline: this package (internal/wrap) depends only on the
// platform's PUBLIC appsplatform package and NEVER on internal/cp. The registry
// is an interface (appsplatform.Registry); the composition root (cmd/vulos-meet)
// selects the standalone default or, when explicitly env-selected, a cloud
// control-plane implementation. See cmd/vulos-meet/main.go.

// MeetAppScopeSet is the scope set the Meet apps place accepts. Meet honours only
// the generic apps:read (read room/participant metadata) and apps:write (act
// into a room), so the registry rejects any other scope at install time rather
// than appearing to grant a Talk-specific permission that means nothing here.
func MeetAppScopeSet() appsplatform.ScopeSet {
	return appsplatform.NewScopeSet(appsplatform.ScopeAppsRead, appsplatform.ScopeAppsWrite)
}

// Apps & Bots runtime action / read kinds Meet understands. They are the
// product-specific vocabulary carried in the generic act/read envelopes.
const (
	// AppActionBroadcast broadcasts an app event/notification into a room over
	// LiveKit's data channel (target = full room id).
	AppActionBroadcast = "room.broadcast"

	// AppReadParticipants reads a room's roster (target = full room id).
	AppReadParticipants = "participants"
	// AppReadRooms lists the active room ids the SFU currently holds (no target).
	AppReadRooms = "rooms"
)

// MeetSFU is the NARROW seam the Apps & Bots adapter needs from the SFU. It is a
// strict subset of the live RoomService: list active rooms (existence + catalog),
// read a room roster (Read), and broadcast a data packet into a room (Act). It
// deliberately excludes DeleteRoom/RemoveParticipant — an app can observe and
// notify a room, never mutate its membership or tear it down. *LiveKitRoomService
// satisfies it.
type MeetSFU interface {
	ListRoomIDs(ctx context.Context) ([]string, error)
	ListParticipants(ctx context.Context, roomID string) ([]ParticipantMeta, error)
	SendData(ctx context.Context, roomID string, data []byte, topic string) error
}

// appDataTopic is the LiveKit data-channel topic app broadcasts carry, so an
// in-room client can distinguish app events from the whiteboard/chat streams.
const appDataTopic = "vulos.apps"

// MeetAdapter implements appsplatform.ProductAdapter for Vulos Meet. The
// platform owns auth, token hashing, product-targeting and scope enforcement;
// this adapter owns the meet-native semantics (broadcast into a room / read a
// roster). It is intentionally minimal and honest about what the SFU exposes.
type MeetAdapter struct {
	sfu     MeetSFU
	timeout time.Duration
}

var _ appsplatform.ProductAdapter = (*MeetAdapter)(nil)

// NewMeetAdapter builds the adapter over a (narrow) SFU seam.
func NewMeetAdapter(sfu MeetSFU) *MeetAdapter {
	return &MeetAdapter{sfu: sfu, timeout: 5 * time.Second}
}

// Product identifies this adapter as the Meet surface.
func (a *MeetAdapter) Product() string { return appsplatform.ProductMeet }

// RequiredScope maps an action (Act) or kind (Read) to the scope it needs.
// Reads require apps:read; everything else (the act/broadcast path, including the
// incoming-webhook action) requires apps:write.
func (a *MeetAdapter) RequiredScope(actionOrKind string) string {
	switch actionOrKind {
	case AppReadParticipants, AppReadRooms:
		return appsplatform.ScopeAppsRead
	default:
		return appsplatform.ScopeAppsWrite
	}
}

// CanAccessTarget gates a target room. A non-empty target must name a room the
// SFU currently holds: unknown → (false, false) → 404; known → (true, true).
// Standalone Meet has no per-app room ACL beyond existence, so any live room is
// accessible. If the SFU cannot be reached we fail closed (deny without leaking
// existence). An empty target is handled by the platform as target-less.
func (a *MeetAdapter) CanAccessTarget(app *appsplatform.App, target string) (allowed, exists bool) {
	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()
	ids, err := a.sfu.ListRoomIDs(ctx)
	if err != nil {
		// Cannot vet the target; deny. Report exists=false so we do not leak a
		// definitive answer and the caller sees a 404 rather than a 403.
		return false, false
	}
	for _, id := range ids {
		if id == target {
			return true, true
		}
	}
	return false, false
}

// appBroadcast is the envelope an app broadcast is wrapped in before it is sent
// over the room data channel. The app's opaque payload rides inside `payload`.
type appBroadcast struct {
	Type    string          `json:"type"`
	AppID   string          `json:"app_id"`
	App     string          `json:"app"`
	Room    string          `json:"room"`
	Payload json.RawMessage `json:"payload,omitempty"`
	TS      int64           `json:"ts"`
}

// Act performs a meet-native action on behalf of an app. The only action is a
// room broadcast: it wraps the app's payload in an envelope and ships it to every
// participant over the LiveKit data channel (the same transport the in-call
// whiteboard uses), then fans a platform `app_event` out to other subscribed
// apps. The incoming-webhook action (action == "incoming_webhook") is treated as
// a broadcast too, so a `POST /api/apps/hooks/{id}` notifies the app's default
// room.
func (a *MeetAdapter) Act(ctx context.Context, app *appsplatform.App, req appsplatform.ActionRequest, emit appsplatform.EmitFunc) (any, error) {
	switch req.Action {
	case AppActionBroadcast, "incoming_webhook":
		room := strings.TrimSpace(req.Target)
		if room == "" {
			return nil, errors.New("vulos-meet: a target room is required")
		}
		payload := req.Payload
		if len(payload) > 0 && !json.Valid(payload) {
			return nil, errors.New("vulos-meet: payload is not valid json")
		}
		env := appBroadcast{
			Type:    "app_event",
			AppID:   app.ID,
			App:     app.Name,
			Room:    room,
			Payload: payload,
			TS:      time.Now().Unix(),
		}
		data, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		if err := a.sfu.SendData(ctx, room, data, appDataTopic); err != nil {
			return nil, err
		}
		// Fan a platform event out to OTHER apps subscribed to app_event so a
		// bot can react to another app's broadcast (mirrors Talk's message.created).
		if emit != nil {
			emit("app_event", map[string]any{
				"room":   room,
				"app_id": app.ID,
				"app":    app.Name,
			}, func(other *appsplatform.App) bool { return other.ID != app.ID })
		}
		return map[string]any{"delivered": true, "room": room}, nil
	default:
		return nil, fmt.Errorf("vulos-meet: unsupported action %q", req.Action)
	}
}

// Read returns meet content for an app: a room's roster (kind=participants) or
// the catalog of active rooms (kind=rooms). Both are metadata only — never media.
func (a *MeetAdapter) Read(ctx context.Context, app *appsplatform.App, req appsplatform.ReadRequest) (any, error) {
	switch req.Kind {
	case AppReadParticipants:
		room := strings.TrimSpace(req.Target)
		if room == "" {
			return nil, errors.New("vulos-meet: a target room is required")
		}
		parts, err := a.sfu.ListParticipants(ctx, room)
		if err != nil {
			return nil, err
		}
		return map[string]any{"room": room, "participants": parts}, nil
	case AppReadRooms:
		ids, err := a.sfu.ListRoomIDs(ctx)
		if err != nil {
			return nil, err
		}
		if ids == nil {
			ids = []string{}
		}
		return map[string]any{"rooms": ids}, nil
	default:
		return nil, fmt.Errorf("vulos-meet: unsupported read kind %q", req.Kind)
	}
}

// MeetAdapter additionally implements mcp.Descriptor so the Meet MCP server
// publishes a precise, per-action tool/resource surface (instead of the generic
// passthrough). The tools/resources are EXACTLY the adapter's Act actions and
// Read kinds — MCP is just a different shape over the same seam, with the same
// scope and target-access checks applied by the mcp engine.
var _ mcp.Descriptor = (*MeetAdapter)(nil)

// broadcastInputSchema is the JSON Schema for the room.broadcast tool. The whole
// arguments object becomes the action Payload; mcp lifts "target" (the room id,
// auto-injected by the engine because AcceptsTarget is set) into the
// ActionRequest.Target and access-checks it via CanAccessTarget before Act runs.
const broadcastInputSchema = `{
  "type": "object",
  "properties": {
    "payload": {
      "type": "object",
      "description": "Opaque app event body delivered to every participant in the room over the LiveKit data channel (topic vulos.apps). Must be valid JSON."
    }
  }
}`

// MCPTools publishes the Meet action tools (the MUTATING surface, apps:write).
// The only Meet action is a room broadcast, addressed by a target room id.
func (a *MeetAdapter) MCPTools() []mcp.ToolSpec {
	return []mcp.ToolSpec{{
		Action:        AppActionBroadcast,
		Description:   "Broadcast an app event into a live meeting room. Delivered to every participant over the room data channel and fanned out as a platform app_event to other subscribed apps. Set `target` to the full room id.",
		AcceptsTarget: true,
		InputSchema:   json.RawMessage(broadcastInputSchema),
	}}
}

// MCPResources publishes the Meet read kinds (the READ surface, apps:read): a
// room's roster (addressed by a target room id) and the catalog of active rooms.
func (a *MeetAdapter) MCPResources() []mcp.ResourceSpec {
	return []mcp.ResourceSpec{
		{
			Kind:          AppReadParticipants,
			Name:          "Room participants",
			Description:   "Roster (metadata only, never media) for a single live room. Address it with the full room id as the target segment.",
			AcceptsTarget: true,
		},
		{
			Kind:        AppReadRooms,
			Name:        "Active rooms",
			Description: "Catalog of room ids the SFU currently holds. No target.",
		},
	}
}

// AppsConfig configures the Apps & Bots mount.
type AppsConfig struct {
	// Registry stores apps (required). The composition root supplies the
	// standalone default or a cloud control-plane implementation.
	Registry appsplatform.Registry
	// SFU is the narrow LiveKit seam the adapter acts/reads through (required).
	SFU MeetSFU
	// AdminToken guards the management API; the SAME bearer that guards /admin/*.
	// Empty disables the management routes (they respond 401).
	AdminToken string
	// BasePath is the mount prefix (default "/api/apps").
	BasePath string
}

// NewAppsHandler wires the shared Apps & Bots platform for Meet: it builds the
// dispatcher + Meet adapter and returns the platform's mountable handler set
// guarded by Meet's existing admin-bearer auth for the management routes. Mount
// the returned handler at BasePath and BasePath+"/".
func NewAppsHandler(cfg AppsConfig) (*appsplatform.Handler, error) {
	if cfg.Registry == nil {
		return nil, errors.New("vulos-meet: apps handler requires a registry")
	}
	if cfg.SFU == nil {
		return nil, errors.New("vulos-meet: apps handler requires an SFU seam")
	}
	disp := appsplatform.NewDispatcher(cfg.Registry, appsplatform.ProductMeet)
	adminToken := cfg.AdminToken
	admin := func(r *http.Request) (string, bool, bool) {
		if adminToken == "" || !appsBearerEquals(r, adminToken) {
			return "", false, false
		}
		// The admin bearer authenticates the operator: a single privileged
		// owner that manages every app installed on this box.
		return "admin", true, true
	}
	return appsplatform.NewHandler(appsplatform.MountConfig{
		Adapter:    NewMeetAdapter(cfg.SFU),
		Registry:   cfg.Registry,
		Dispatcher: disp,
		Admin:      admin,
		BasePath:   cfg.BasePath,
	})
}

// MCPConfig configures the MCP mount. The MCP server reuses the SAME Meet
// adapter and app registry as the REST Apps & Bots mount — MCP is a different
// shape over the same seam — and authenticates with the SAME per-app Bearer
// tokens (vat_…). It has no management routes and so needs no admin bearer.
type MCPConfig struct {
	// Registry authenticates Bearer app tokens (required) — the SAME registry the
	// REST apps mount uses, so a token works identically over either shape.
	Registry appsplatform.Registry
	// SFU is the narrow LiveKit seam the adapter acts/reads through (required).
	SFU MeetSFU
	// BasePath is the mount prefix (default "/mcp").
	BasePath string
}

// NewMCPHandler wires the shared @vulos/apps MCP layer for Meet: it builds a
// Meet adapter (which implements mcp.Descriptor, so tools/resources are derived
// precisely) over the given SFU and returns the mountable MCP handler. Mount the
// returned handler at BasePath and BasePath+"/" on the SAME mux that serves the
// REST /api/apps routes, behind the same signal-gate.
//
// Open-core discipline: the optional cloud MCP-aggregation gateway
// (mcp.MCPConfig.Gateway) is left nil here — standalone is the only behavior
// this core compiles in. The composition root (cmd/vulos-meet) env-gates any
// cloud gateway selection, mirroring the registry seam; internal/wrap never
// imports a Gateway implementation or internal/cp.
func NewMCPHandler(cfg MCPConfig) (*mcp.Handler, error) {
	if cfg.Registry == nil {
		return nil, errors.New("vulos-meet: mcp handler requires a registry")
	}
	if cfg.SFU == nil {
		return nil, errors.New("vulos-meet: mcp handler requires an SFU seam")
	}
	// A dispatcher over the SAME registry lets tool calls fan a platform event
	// out to other subscribed apps exactly as REST actions do.
	disp := appsplatform.NewDispatcher(cfg.Registry, appsplatform.ProductMeet)
	return mcp.NewHandler(mcp.MCPConfig{
		Adapter:  NewMeetAdapter(cfg.SFU),
		Registry: cfg.Registry,
		Emit:     disp.EmitFunc(),
		BasePath: cfg.BasePath,
	})
}

// appsBearerEquals reports whether the request carries the expected admin bearer
// token, compared in constant time (reuses the admin surface's helper so the two
// auth checks share one timing-safe implementation).
func appsBearerEquals(r *http.Request, token string) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return constantTimeEqualString(h[len(prefix):], token)
}
