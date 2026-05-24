// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"errors"
	"fmt"

	"github.com/livekit/protocol/auth"
)

// SubprotocolVersion is the Vulos sub-protocol identifier carried by every
// token vulos-meet accepts. Versioning rules: see spec/VERSIONS.md.
const SubprotocolVersion = "VULOS-MEET/1"

// Token-validation errors. Callers (HTTP gate, signaling reverse proxy) MUST
// map these to 401/403 with no token contents in the response body.
var (
	ErrTokenMalformed      = errors.New("vulos-meet: token is malformed")
	ErrTokenWrongAPIKey    = errors.New("vulos-meet: token was minted with an unknown API key")
	ErrTokenSignatureBad   = errors.New("vulos-meet: token signature does not verify")
	ErrTokenMissingGrants  = errors.New("vulos-meet: token carries no video grants")
	ErrTokenMissingRoom    = errors.New("vulos-meet: token grants no room")
	ErrTokenWrongTenant    = errors.New("vulos-meet: token tenant does not match token room prefix")
	ErrTokenMissingTenant  = errors.New("vulos-meet: token has no tenant audience")
	ErrTokenRoomMalformed  = errors.New("vulos-meet: token room id is malformed")
)

// Validator validates VULOS-MEET/1 tokens.
//
// IMPORTANT: vulos-meet does NOT mint tokens. The cloud control plane
// (MEET-CP-01) is the sole token issuer; this validator is the *only*
// admission seam on the SFU side. The validator:
//
//  1. Asserts the JWT is well-formed and was minted with the API key we know.
//  2. Verifies the JWT signature against the shared API secret (delegated to
//     livekit/protocol so the verification path is byte-identical to what
//     LiveKit Server itself does — there is no "two implementations, two bugs"
//     risk).
//  3. Asserts the embedded video grants name a room.
//  4. Asserts the room ID carries the tenant prefix and that the prefix
//     matches the token's audience tenant — this is the per-tenant namespace
//     binding. A token for tenant A cannot be replayed against a room owned
//     by tenant B because the prefix wouldn't match the audience.
type Validator struct {
	apiKey    string
	apiSecret string
	tenant    *Tenant
}

// NewValidator builds a validator bound to the shared LiveKit API key/secret
// pair (minted-side and validated-side MUST be identical) and the tenant gate.
func NewValidator(apiKey, apiSecret string, tenant *Tenant) (*Validator, error) {
	if apiKey == "" {
		return nil, errors.New("vulos-meet: validator requires non-empty api key")
	}
	if apiSecret == "" {
		return nil, errors.New("vulos-meet: validator requires non-empty api secret")
	}
	if tenant == nil {
		return nil, errors.New("vulos-meet: validator requires a tenant gate")
	}
	return &Validator{apiKey: apiKey, apiSecret: apiSecret, tenant: tenant}, nil
}

// ValidatedToken is the result of a successful Validator.Validate call.
// Identity is the token subject (the Vulos user ID that minted-side embedded
// as sub). Tenant + Room are the parsed tenant prefix and the per-tenant
// room name.
type ValidatedToken struct {
	Identity string
	Tenant   string
	Room     string // room name AFTER the tenant prefix (the "rest")
	FullRoom string // the full <tenant><sep><rest> room id
	Grants   *auth.ClaimGrants
}

// Validate parses + verifies a token, then enforces the Vulos tenant
// invariant. On success the returned token is safe to forward to LiveKit
// Server. On any failure no information about the token's contents is
// returned to the caller — only the typed sentinel error.
func (v *Validator) Validate(raw string) (*ValidatedToken, error) {
	parsed, err := auth.ParseAPIToken(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenMalformed, err)
	}
	if parsed.APIKey() != v.apiKey {
		return nil, ErrTokenWrongAPIKey
	}
	// Verify also checks `iss == apiKey`, `exp`, and `nbf` (Time = now).
	_, grants, err := parsed.Verify(v.apiSecret)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenSignatureBad, err)
	}
	if grants == nil || grants.Video == nil {
		return nil, ErrTokenMissingGrants
	}
	if grants.Video.Room == "" {
		return nil, ErrTokenMissingRoom
	}
	tenantFromRoom, rest, err := v.tenant.SplitRoom(grants.Video.Room)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenRoomMalformed, err)
	}
	// The tenant binding: the token's `aud` (carried by livekit/protocol as
	// the audience claim, but for VULOS-MEET/1 we accept it via either
	// (a) the `aud` JWT claim copied into a custom grant, or (b) the
	// implicit prefix in the room. We require BOTH to be present and to
	// agree, so a leaked token can't be replayed against an alternate room
	// in another tenant.
	tenantFromAud := extractTenantAudience(grants)
	if tenantFromAud == "" {
		return nil, ErrTokenMissingTenant
	}
	if tenantFromAud != tenantFromRoom {
		return nil, ErrTokenWrongTenant
	}
	return &ValidatedToken{
		Identity: grants.Identity,
		Tenant:   tenantFromRoom,
		Room:     rest,
		FullRoom: grants.Video.Room,
		Grants:   grants,
	}, nil
}

// extractTenantAudience pulls the Vulos tenant out of a token's grants. We
// piggyback on ClaimGrants.Name as the tenant-audience carrier (the
// MEET-CP-01 minter is responsible for setting it). This lets us pass the
// VULOS-MEET/1 binding through LiveKit's grant shape without forking the
// JWT claim schema — important because LiveKit Server will re-verify the
// token using the very same claim set on the signaling path.
//
// The CP-side minter sets:
//
//	at.SetName(tenant) // tenant audience (Vulos profile)
//	at.SetIdentity(userID).SetVideoGrant(&auth.VideoGrant{Room: tenant + ":" + roomName, RoomJoin: true})
//
// If we ever need a richer claim shape we should bump VULOS-MEET to /2 and
// add a custom claim in ClaimGrants.Metadata; doing it via Name today keeps
// the validator dependency-clean.
func extractTenantAudience(g *auth.ClaimGrants) string {
	if g == nil {
		return ""
	}
	return g.Name
}
