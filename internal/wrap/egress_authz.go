// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// errEgressLookupFailed is returned when the proxy cannot resolve an egress
// id's owning room from the upstream (unreachable, non-200, unparseable, or the
// id was not found). It is an internal sentinel — the caller fails closed (403).
var errEgressLookupFailed = errors.New("vulos-meet: egress ownership lookup failed")

// authorizeEgressTenant enforces the VULOS-MEET/1 room↔tenant binding on the
// egress request BODY.
//
// Why this is separate from token validation: Validate only proves the caller
// holds *a* valid RoomRecord token for *some* room in their tenant. LiveKit's
// RoomRecord video grant is NOT room-scoped, so a token holder can name ANOTHER
// tenant's room (or another tenant's egress id) in the Twirp body and — absent
// this check — the proxy would forward it verbatim. That is the cross-tenant
// broken-access-control hole (record a victim's meeting, enumerate/stop another
// tenant's egress). Here we bind the body's target room/egress to the token's
// validated tenant.
//
// Fail closed: an unparseable body, an unrecognised method, a Start whose room
// is not the token's room, a List with no (or a foreign) room filter, and an
// egress-id method whose id cannot be proven to belong to the token's tenant
// are ALL rejected (return false → 403, nothing forwarded upstream).
func (p *EgressProxy) authorizeEgressTenant(ctx context.Context, method, contentType string, body []byte, vt *ValidatedToken, authHeader string) bool {
	switch method {
	case "StartRoomCompositeEgress", "StartParticipantEgress", "StartTrackCompositeEgress", "StartTrackEgress":
		// Start egress is inherently per-room and the token is minted for
		// exactly one room. Require the body's room to equal the token's full
		// room — the tightest binding, which also implies the tenant matches.
		room, ok := egressBodyRoomName(method, contentType, body)
		if !ok {
			return false // undecodable body ⇒ fail closed
		}
		return room != "" && room == vt.FullRoom

	case "StartWebEgress":
		// Web egress records an arbitrary URL and carries NO room, so it cannot
		// be bound to the token's tenant. Fail closed (the cloud does not route
		// web egress through this proxy).
		return false

	case "ListEgress":
		// A ListEgress with no room filter enumerates every tenant's egress on
		// the box. Require a room filter scoped to the token's tenant.
		room, ok := egressBodyRoomName(method, contentType, body)
		if !ok || room == "" {
			return false
		}
		return p.roomTenant(room) == vt.Tenant

	case "StopEgress", "UpdateLayout", "UpdateStream":
		// These key off an opaque egress id that carries no tenant. Resolve the
		// egress's owning room from the upstream (ListEgress by id) and check
		// its tenant. Fail closed when it cannot be resolved.
		id, ok := egressBodyID(method, contentType, body)
		if !ok || id == "" {
			return false
		}
		room, err := p.resolveEgressRoom(ctx, id, authHeader)
		if err != nil || room == "" {
			return false
		}
		return p.roomTenant(room) == vt.Tenant

	default:
		// Unknown egress method: fail closed. The known set above is the whole
		// livekit.Egress Twirp surface; anything else is unexpected here.
		return false
	}
}

// roomTenant returns the tenant prefix of a fully-qualified room id, or "" when
// the room is empty/malformed. An empty result never matches a validated token
// tenant (which is always non-empty), so a parse failure fails closed.
func (p *EgressProxy) roomTenant(room string) string {
	tenant, _, err := p.validator.tenant.SplitRoom(room)
	if err != nil {
		return ""
	}
	return tenant
}

// egressBodyRoomName decodes a room-bearing egress request body and returns its
// room_name. ok is false on a decode failure (caller fails closed). Methods
// with no room_name field return ("", false).
func egressBodyRoomName(method, contentType string, body []byte) (room string, ok bool) {
	switch method {
	case "StartRoomCompositeEgress":
		m := &livekit.RoomCompositeEgressRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetRoomName(), true
	case "StartParticipantEgress":
		m := &livekit.ParticipantEgressRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetRoomName(), true
	case "StartTrackCompositeEgress":
		m := &livekit.TrackCompositeEgressRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetRoomName(), true
	case "StartTrackEgress":
		m := &livekit.TrackEgressRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetRoomName(), true
	case "ListEgress":
		m := &livekit.ListEgressRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetRoomName(), true
	default:
		return "", false
	}
}

// egressBodyID decodes an egress-id-keyed request body and returns its
// egress_id. ok is false on a decode failure (caller fails closed).
func egressBodyID(method, contentType string, body []byte) (id string, ok bool) {
	switch method {
	case "StopEgress":
		m := &livekit.StopEgressRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetEgressId(), true
	case "UpdateLayout":
		m := &livekit.UpdateLayoutRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetEgressId(), true
	case "UpdateStream":
		m := &livekit.UpdateStreamRequest{}
		if !decodeEgressBody(m, contentType, body) {
			return "", false
		}
		return m.GetEgressId(), true
	default:
		return "", false
	}
}

// decodeEgressBody unmarshals a Twirp egress body (proto-JSON or protobuf, per
// content-type) into msg. Unknown fields are tolerated (DiscardUnknown) so we
// never reject a legitimate body just because it carries a field we don't read.
// Returns false on a decode error.
func decodeEgressBody(msg proto.Message, contentType string, body []byte) bool {
	if bodyIsJSON(contentType) {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, msg) == nil
	}
	return proto.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, msg) == nil
}

// resolveEgressRoom asks the upstream livekit.Egress surface which room a given
// egress id belongs to, by issuing a ListEgress filtered on that id and reading
// back the owning room_name. It talks to the upstream DIRECTLY (not back through
// serve, so there is no recursion) using the caller's own bearer, which LiveKit
// re-verifies. Any failure (transport, non-200, unparseable, id-not-found)
// returns errEgressLookupFailed so the caller fails closed.
func (p *EgressProxy) resolveEgressRoom(ctx context.Context, egressID, authHeader string) (string, error) {
	payload, err := protojson.Marshal(&livekit.ListEgressRequest{EgressId: egressID})
	if err != nil {
		return "", err
	}
	url := p.upstream.String() + EgressTwirpPathPrefix + "ListEgress"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errEgressLookupFailed
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var list livekit.ListEgressResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &list); err != nil {
		return "", err
	}
	for _, it := range list.GetItems() {
		if it.GetEgressId() == egressID {
			return it.GetRoomName(), nil
		}
	}
	return "", errEgressLookupFailed
}
