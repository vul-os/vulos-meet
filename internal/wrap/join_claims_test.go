// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

// Join-token mint (VULOS-MEET/1) tests.
//
// vulos-meet never mints tokens — that is the control-plane's job (MEET-CP-01).
// These tests exercise the VALIDATE side: given a token with specific claim
// combinations, does the validator extract the correct fields and enforce the
// correct boundaries?
//
// Coverage:
//   - All ValidatedToken fields correctly extracted from a well-formed token.
//   - Video-grant scope (RoomJoin, CanPublish) propagated verbatim.
//   - Short-lived tokens rejected after expiry.
//   - Room/tenant auth boundaries:
//     · Token for tenant A cannot validate against tenant B's room (cross-tenant).
//     · Token for room A carries room A's claim, not room B.
//     · Multiple tenants with the same room short-name are namespace-isolated.
//   - Talk-chat transport seam: the validator does not strip or alter the
//     token's identity, which the chat transport uses as the participant identity.
package wrap

import (
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	josejwt "github.com/go-jose/go-jose/v3/jwt"
	"github.com/livekit/protocol/auth"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. Claim field extraction — every field of ValidatedToken is populated.
// ─────────────────────────────────────────────────────────────────────────────

// TestJoinToken_AllClaimsCorrectlyExtracted verifies the validator populates
// all four fields of ValidatedToken from a well-formed VULOS-MEET/1 token.
func TestJoinToken_AllClaimsCorrectlyExtracted(t *testing.T) {
	v := newValidatorForTest(t)
	tok := mintToken(t, "acme", "standup", time.Hour)
	vt, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Identity is the `sub` claim set by the CP minter.
	if vt.Identity != "u_test" {
		t.Errorf("Identity: got %q want u_test", vt.Identity)
	}
	// Tenant is the short tenant name extracted from the room prefix.
	if vt.Tenant != "acme" {
		t.Errorf("Tenant: got %q want acme", vt.Tenant)
	}
	// Room is the per-tenant short room name (after the separator is stripped).
	if vt.Room != "standup" {
		t.Errorf("Room: got %q want standup", vt.Room)
	}
	// FullRoom is the unmodified, fully-qualified room id as it appeared in the token.
	if vt.FullRoom != "acme:standup" {
		t.Errorf("FullRoom: got %q want acme:standup", vt.FullRoom)
	}
	// Grants are present (video grant is not nil).
	if vt.Grants == nil || vt.Grants.Video == nil {
		t.Errorf("Grants.Video is nil — must be propagated")
	}
}

// TestJoinToken_VideoGrantScopePreserved verifies that the video-grant scope
// flags (RoomJoin, CanPublish, CanSubscribe) are passed through verbatim. The
// validator must not modify or drop the grant.
func TestJoinToken_VideoGrantScopePreserved(t *testing.T) {
	v := newValidatorForTest(t)
	trueP := true
	falseP := false
	tok := mintToken(t, "acme", "boardroom", time.Hour,
		func(at *auth.AccessToken) {
			at.SetVideoGrant(&auth.VideoGrant{
				Room:         "acme:boardroom",
				RoomJoin:     true,
				CanPublish:   &trueP,
				CanSubscribe: &falseP,
				RoomRecord:   false,
			})
		},
	)
	vt, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	g := vt.Grants.Video
	if !g.RoomJoin {
		t.Errorf("RoomJoin not preserved")
	}
	if g.CanPublish == nil || !*g.CanPublish {
		t.Errorf("CanPublish not preserved")
	}
	if g.CanSubscribe == nil || *g.CanSubscribe {
		t.Errorf("CanSubscribe not preserved (should be false)")
	}
	if g.RoomRecord {
		t.Errorf("RoomRecord should be false on a join token")
	}
}

// TestJoinToken_RoomClaimIsAuthoritative verifies the room name extracted from
// the ValidatedToken equals exactly what was minted. The gate does not infer
// the room from the URL or any other source; the token's room claim is the
// ground truth.
func TestJoinToken_RoomClaimIsAuthoritative(t *testing.T) {
	v := newValidatorForTest(t)
	tok := mintToken(t, "globex", "planning-2026", time.Hour)
	vt, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if vt.Room != "planning-2026" {
		t.Fatalf("room claim: got %q want planning-2026", vt.Room)
	}
	if vt.Tenant != "globex" {
		t.Fatalf("tenant claim: got %q want globex", vt.Tenant)
	}
	if vt.FullRoom != "globex:planning-2026" {
		t.Fatalf("full room: got %q want globex:planning-2026", vt.FullRoom)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Expiry enforcement — tokens with past/future validity windows rejected.
// ─────────────────────────────────────────────────────────────────────────────

// TestJoinToken_ShortTTLExpiredRejected mints a token with a 1-second TTL
// and verifies it is rejected once well past the go-jose leeway (1 minute).
// We bypass livekit/protocol's SetValidFor floor by signing directly.
func TestJoinToken_ShortTTLExpiredRejected(t *testing.T) {
	v := newValidatorForTest(t)
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: []byte(testAPISecret)},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	rj := true
	grants := auth.ClaimGrants{
		Name: "acme",
		Video: &auth.VideoGrant{
			Room: "acme:standup", RoomJoin: true, CanPublish: &rj,
		},
	}
	cl := josejwt.Claims{
		Issuer:    testAPIKey,
		Subject:   "u_test",
		NotBefore: josejwt.NewNumericDate(time.Now().Add(-90 * time.Second)), // 1m30s ago
		Expiry:    josejwt.NewNumericDate(time.Now().Add(-70 * time.Second)), // expired 1m10s ago (> 1m leeway)
	}
	tok, err := josejwt.Signed(sig).Claims(cl).Claims(&grants).CompactSerialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = v.Validate(tok)
	if err == nil {
		t.Fatalf("expired token (70s past leeway) was ACCEPTED — must be rejected")
	}
}

// TestJoinToken_FutureNBFRejected mints a token whose nbf is in the future
// (well past the go-jose 1-minute leeway). A not-yet-valid token is refused.
func TestJoinToken_FutureNBFRejected(t *testing.T) {
	v := newValidatorForTest(t)
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: []byte(testAPISecret)},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	rj := true
	grants := auth.ClaimGrants{
		Name:  "acme",
		Video: &auth.VideoGrant{Room: "acme:standup", RoomJoin: true, CanPublish: &rj},
	}
	cl := josejwt.Claims{
		Issuer:    testAPIKey,
		Subject:   "u_test",
		NotBefore: josejwt.NewNumericDate(time.Now().Add(90 * time.Second)), // valid 1m30s from now (> 1m leeway)
		Expiry:    josejwt.NewNumericDate(time.Now().Add(2 * time.Hour)),
	}
	tok, err := josejwt.Signed(sig).Claims(cl).Claims(&grants).CompactSerialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = v.Validate(tok)
	if err == nil {
		t.Fatalf("future-nbf token (90s ahead of leeway) was ACCEPTED — must be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Room/tenant auth boundaries — isolation between tenants and rooms.
// ─────────────────────────────────────────────────────────────────────────────

// TestJoinToken_SameTenantDifferentRoomsHaveSeparateClaims verifies that two
// tokens minted for the SAME tenant but DIFFERENT rooms produce ValidatedTokens
// with distinct Room fields. The token's room claim is what controls which room
// a participant may join; LiveKit enforces this server-side. Here we confirm the
// validator correctly extracts the per-room claim from each token.
func TestJoinToken_SameTenantDifferentRoomsHaveSeparateClaims(t *testing.T) {
	v := newValidatorForTest(t)

	tokA := mintToken(t, "acme", "room-alpha", time.Hour)
	tokB := mintToken(t, "acme", "room-beta", time.Hour)

	vtA, err := v.Validate(tokA)
	if err != nil {
		t.Fatalf("token A validate: %v", err)
	}
	vtB, err := v.Validate(tokB)
	if err != nil {
		t.Fatalf("token B validate: %v", err)
	}

	// Each token's room must match what it was minted for.
	if vtA.Room != "room-alpha" {
		t.Errorf("token A room: got %q want room-alpha", vtA.Room)
	}
	if vtB.Room != "room-beta" {
		t.Errorf("token B room: got %q want room-beta", vtB.Room)
	}
	// Same tenant.
	if vtA.Tenant != "acme" || vtB.Tenant != "acme" {
		t.Errorf("tenant: A=%q B=%q want both acme", vtA.Tenant, vtB.Tenant)
	}
	// Full room IDs must differ.
	if vtA.FullRoom == vtB.FullRoom {
		t.Errorf("two distinct rooms must have distinct FullRoom: both %q", vtA.FullRoom)
	}
}

// TestJoinToken_CrossTenantRoomRejected is the headline boundary test:
// a token minted for tenant A CANNOT validate against tenant B's room. The
// tenant-audience binding (name claim == room prefix) is the gate.
func TestJoinToken_CrossTenantRoomRejected(t *testing.T) {
	v := newValidatorForTest(t)
	// Audience says "evil", room says "victim:boardroom". Must be rejected.
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_evil").SetName("evil").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "victim:boardroom", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); err == nil {
		t.Fatalf("LIVE VULN: cross-tenant room token (aud=evil, room=victim:*) ACCEPTED")
	}
}

// TestJoinToken_SameShortRoomDifferentTenantsAreSeparate verifies that two
// tenants can each have a room with the same short name ("standup") without
// any confusion. This is the namespace isolation invariant: the full room id
// is `<tenant><sep><short>`, so "acme:standup" ≠ "globex:standup".
func TestJoinToken_SameShortRoomDifferentTenantsAreSeparate(t *testing.T) {
	v := newValidatorForTest(t)

	tokAcme := mintToken(t, "acme", "standup", time.Hour)
	tokGlobex := mintToken(t, "globex", "standup", time.Hour)

	vtAcme, err := v.Validate(tokAcme)
	if err != nil {
		t.Fatalf("acme token: %v", err)
	}
	vtGlobex, err := v.Validate(tokGlobex)
	if err != nil {
		t.Fatalf("globex token: %v", err)
	}

	if vtAcme.Tenant == vtGlobex.Tenant {
		t.Errorf("same-short-name tenants must be distinct: both %q", vtAcme.Tenant)
	}
	if vtAcme.FullRoom == vtGlobex.FullRoom {
		t.Errorf("same-short-name rooms must have different full IDs: both %q", vtAcme.FullRoom)
	}
}

// TestJoinToken_TenantFromRoomPrefixIsCanonical verifies that the tenant in
// the ValidatedToken is always parsed from the room's prefix (the canonical
// source of truth), not from the name claim alone. If the two disagree the
// validator must reject; if they agree the room prefix wins in the returned
// tenant field.
func TestJoinToken_TenantFromRoomPrefixIsCanonical(t *testing.T) {
	v := newValidatorForTest(t)
	// Consistent token: name="contoso", room="contoso:weekly" → should pass.
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u1").SetName("contoso").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "contoso:weekly", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	vt, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("consistent token rejected: %v", err)
	}
	if vt.Tenant != "contoso" {
		t.Errorf("tenant: got %q want contoso", vt.Tenant)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Talk-chat transport seam — the identity claim flows to the chat layer.
// ─────────────────────────────────────────────────────────────────────────────

// TestJoinToken_IdentityFlowsToTalkChatSeam verifies that the token's identity
// claim (the participant's user ID) is available in ValidatedToken.Identity.
// The Go chat-transport seam (in the web client) uses the participant identity
// to mark outbound messages as "self". If identity were lost or altered at the
// validator, the chat transport would misidentify the sender.
func TestJoinToken_IdentityFlowsToTalkChatSeam(t *testing.T) {
	v := newValidatorForTest(t)
	// Custom identity: mimic a real CP-minted token where sub is a Vulos user ID.
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("usr_7f3a2b").SetName("acme").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:daily", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	vt, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if vt.Identity != "usr_7f3a2b" {
		t.Fatalf("identity not preserved through validator: got %q want usr_7f3a2b", vt.Identity)
	}
}

// TestJoinToken_EmptyIdentityTolerated verifies a token with no sub claim
// is still accepted (identity is optional in LiveKit; the server assigns a
// random ID when absent). The chat transport gracefully handles empty identity
// (shows "Guest").
func TestJoinToken_EmptyIdentityTolerated(t *testing.T) {
	v := newValidatorForTest(t)
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetName("acme").SetValidFor(time.Hour) // no SetIdentity
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:demo", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Must validate without error even if sub is empty.
	vt, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("empty-identity token rejected: %v", err)
	}
	// The identity field should be empty (or unset) — not some random value.
	if strings.Contains(vt.Identity, "u_") {
		t.Fatalf("identity should be empty for no-sub token, got %q", vt.Identity)
	}
}

// TestJoinToken_UniqueTokensPerRoomAreDistinct verifies that minting two tokens
// for the SAME room (e.g. two participants) produces tokens with the same room
// claim but distinct identity claims. This is the expected shape for a
// multi-participant room.
func TestJoinToken_UniqueTokensPerRoomAreDistinct(t *testing.T) {
	v := newValidatorForTest(t)

	tok1 := mintToken(t, "acme", "standup", time.Hour, func(at *auth.AccessToken) {
		at.SetIdentity("alice@acme")
	})
	tok2 := mintToken(t, "acme", "standup", time.Hour, func(at *auth.AccessToken) {
		at.SetIdentity("bob@acme")
	})

	vt1, err := v.Validate(tok1)
	if err != nil {
		t.Fatalf("tok1: %v", err)
	}
	vt2, err := v.Validate(tok2)
	if err != nil {
		t.Fatalf("tok2: %v", err)
	}

	// Same room.
	if vt1.Room != vt2.Room || vt1.Tenant != vt2.Tenant {
		t.Errorf("same-room tokens should share room/tenant: %+v vs %+v", vt1, vt2)
	}
	// Distinct identities.
	if vt1.Identity == vt2.Identity {
		t.Errorf("two participants should have distinct identities, both = %q", vt1.Identity)
	}
}
