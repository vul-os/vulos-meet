// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"errors"
	"testing"
	"time"

	josejwt "github.com/go-jose/go-jose/v3/jwt"
	jose "github.com/go-jose/go-jose/v3"
	"github.com/livekit/protocol/auth"
)

const (
	testAPIKey    = "APItestkey"
	testAPISecret = "supersecretvalueof32bytesplus_padding"
)

// mintToken is a TEST-ONLY helper that emulates what vulos-cloud's
// MEET-CP-01 minter does. Production vulos-meet never mints.
func mintToken(t *testing.T, tenant, room string, ttl time.Duration, opts ...func(*auth.AccessToken)) string {
	t.Helper()
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test")
	at.SetName(tenant) // tenant audience carrier per VULOS-MEET/1
	at.SetValidFor(ttl)
	rj := true
	at.SetVideoGrant(&auth.VideoGrant{
		Room:       tenant + ":" + room,
		RoomJoin:   true,
		CanPublish: &rj,
	})
	for _, opt := range opts {
		opt(at)
	}
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

func newValidatorForTest(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator(testAPIKey, testAPISecret, NewTenant(""))
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	return v
}

func TestValidator_ValidTokenPasses(t *testing.T) {
	v := newValidatorForTest(t)
	tok := mintToken(t, "acme", "standup", time.Hour)
	got, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("expected valid token to pass, got: %v", err)
	}
	if got.Tenant != "acme" || got.Room != "standup" {
		t.Fatalf("validated: got tenant=%q room=%q, want (acme, standup)", got.Tenant, got.Room)
	}
	if got.Identity != "u_test" {
		t.Fatalf("identity: got %q, want u_test", got.Identity)
	}
}

func TestValidator_ExpiredTokenRejected(t *testing.T) {
	v := newValidatorForTest(t)
	// Mint a token that expired WELL outside the go-jose default 1-minute
	// leeway by bypassing livekit/protocol's SetValidFor floor and signing
	// the JWT directly with the test secret.
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
			Room:       "acme:standup",
			RoomJoin:   true,
			CanPublish: &rj,
		},
	}
	cl := josejwt.Claims{
		Issuer:    testAPIKey,
		Subject:   "u_test",
		NotBefore: josejwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		Expiry:    josejwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
	}
	tok, err := josejwt.Signed(sig).Claims(cl).Claims(&grants).CompactSerialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenSignatureBad) {
		t.Fatalf("expected ErrTokenSignatureBad (covers exp), got %v", err)
	}
}

func TestValidator_WrongAPISecretRejected(t *testing.T) {
	v := newValidatorForTest(t)
	// Mint with a DIFFERENT secret than the validator expects.
	at := auth.NewAccessToken(testAPIKey, "not-the-right-secret-padding-1234")
	at.SetIdentity("u_test").SetName("acme").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenSignatureBad) {
		t.Fatalf("expected ErrTokenSignatureBad, got %v", err)
	}
}

func TestValidator_WrongAPIKeyRejected(t *testing.T) {
	v := newValidatorForTest(t)
	at := auth.NewAccessToken("APIotherkey", testAPISecret)
	at.SetIdentity("u_test").SetName("acme").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenWrongAPIKey) {
		t.Fatalf("expected ErrTokenWrongAPIKey, got %v", err)
	}
}

func TestValidator_WrongTenantInAudRejected(t *testing.T) {
	v := newValidatorForTest(t)
	// Room prefix says "acme", audience (`name`) says "evil". This is the
	// classic replay attempt: leaked token from tenant evil pointed at an
	// acme room. MUST be rejected.
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test").SetName("evil").SetValidFor(time.Hour)
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenWrongTenant) {
		t.Fatalf("expected ErrTokenWrongTenant, got %v", err)
	}
}

func TestValidator_MissingTenantAudienceRejected(t *testing.T) {
	v := newValidatorForTest(t)
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test").SetValidFor(time.Hour) // no SetName
	at.SetVideoGrant(&auth.VideoGrant{Room: "acme:standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenMissingTenant) {
		t.Fatalf("expected ErrTokenMissingTenant, got %v", err)
	}
}

func TestValidator_RoomMissingPrefixRejected(t *testing.T) {
	v := newValidatorForTest(t)
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test").SetName("acme").SetValidFor(time.Hour)
	// Room has NO tenant prefix.
	at.SetVideoGrant(&auth.VideoGrant{Room: "standup", RoomJoin: true})
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenRoomMalformed) {
		t.Fatalf("expected ErrTokenRoomMalformed, got %v", err)
	}
}

func TestValidator_MalformedTokenRejected(t *testing.T) {
	v := newValidatorForTest(t)
	if _, err := v.Validate("not-a-jwt"); !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("expected ErrTokenMalformed, got %v", err)
	}
}

func TestValidator_NoVideoGrantRejected(t *testing.T) {
	v := newValidatorForTest(t)
	at := auth.NewAccessToken(testAPIKey, testAPISecret)
	at.SetIdentity("u_test").SetName("acme").SetValidFor(time.Hour)
	// No SetVideoGrant.
	tok, err := at.ToJWT()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := v.Validate(tok); !errors.Is(err, ErrTokenMissingGrants) {
		t.Fatalf("expected ErrTokenMissingGrants, got %v", err)
	}
}

func TestValidator_RequiresInputs(t *testing.T) {
	if _, err := NewValidator("", testAPISecret, NewTenant("")); err == nil {
		t.Fatalf("expected error with empty api key")
	}
	if _, err := NewValidator(testAPIKey, "", NewTenant("")); err == nil {
		t.Fatalf("expected error with empty api secret")
	}
	if _, err := NewValidator(testAPIKey, testAPISecret, nil); err == nil {
		t.Fatalf("expected error with nil tenant gate")
	}
}
