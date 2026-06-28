// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

// Package wrap holds the Vulos-specific layer that surrounds LiveKit Server:
// token validation, per-tenant room-namespace enforcement, admin HTTP surface,
// geo-routing hook, and YAML config.
package wrap

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultTenantSeparator is the byte we use between tenant ID and per-tenant
// room name in a room ID. It is configurable (TenantSeparator on Config) but
// MUST remain a single ASCII byte that cannot appear in a tenant ID.
const DefaultTenantSeparator = ":"

// Errors returned by the tenant gate. Callers (admin handlers, token
// validators) MUST check these and return 403/404 — not 500.
var (
	ErrEmptyTenant       = errors.New("vulos-meet: tenant is empty")
	ErrEmptyRoom         = errors.New("vulos-meet: room id is empty")
	ErrRoomMissingPrefix = errors.New("vulos-meet: room id is missing tenant prefix")
	ErrTenantMismatch    = errors.New("vulos-meet: room id belongs to a different tenant")
	ErrInvalidTenant     = errors.New("vulos-meet: tenant id contains invalid characters")
)

// Tenant is the per-tenant room-namespace gate. It is the single place every
// admin / token-validator must funnel through before reading or writing a room.
//
// Cross-tenant isolation invariant (FROZEN):
//
//	Every room ID is `<tenant><sep><rest>`. A caller claiming tenant T may
//	NEVER list, get, or delete a room whose prefix is not T<sep>. This is the
//	difference between "multi-tenant SFU" and "shared SFU with leaks".
type Tenant struct {
	sep string
}

// NewTenant returns a tenant gate using the provided separator. Empty separator
// falls back to DefaultTenantSeparator. The separator MUST be a single ASCII
// byte; multi-byte separators are rejected because they break the
// "prefix-by-bytes" property the gate depends on.
func NewTenant(sep string) *Tenant {
	if sep == "" {
		sep = DefaultTenantSeparator
	}
	if len(sep) != 1 {
		// In practice this is a config error; panic at construction is
		// preferable to a misconfigured SFU that silently allows leaks.
		panic("vulos-meet: tenant separator must be a single ASCII byte")
	}
	return &Tenant{sep: sep}
}

// Separator returns the configured tenant separator.
func (t *Tenant) Separator() string { return t.sep }

// QualifyRoom builds the canonical room ID `<tenant><sep><rest>`. It returns
// an error if either argument is empty or if `rest` already looks like a
// qualified room ID (i.e. it already contains the separator) — re-qualifying a
// qualified ID is almost always a caller bug.
func (t *Tenant) QualifyRoom(tenant, rest string) (string, error) {
	if err := t.validateTenant(tenant); err != nil {
		return "", err
	}
	if rest == "" {
		return "", ErrEmptyRoom
	}
	if strings.Contains(rest, t.sep) {
		return "", fmt.Errorf("vulos-meet: room rest %q already contains separator %q", rest, t.sep)
	}
	return tenant + t.sep + rest, nil
}

// SplitRoom parses a qualified room ID into (tenant, rest). It rejects any
// room ID that does not carry the tenant prefix; this is the gate that stops a
// caller from claiming a room whose prefix is somebody else's tenant.
func (t *Tenant) SplitRoom(roomID string) (tenant, rest string, err error) {
	if roomID == "" {
		return "", "", ErrEmptyRoom
	}
	i := strings.Index(roomID, t.sep)
	if i < 0 {
		return "", "", ErrRoomMissingPrefix
	}
	tenant = roomID[:i]
	rest = roomID[i+len(t.sep):]
	if tenant == "" || rest == "" {
		return "", "", ErrRoomMissingPrefix
	}
	if err := t.validateTenant(tenant); err != nil {
		return "", "", err
	}
	return tenant, rest, nil
}

// EnforceRoom asserts that roomID belongs to wantTenant. Returns
// ErrTenantMismatch on cross-tenant access. This is the call that gates
// every admin operation that takes (tenant, room) — caller wants to act on
// room R as tenant T, we say yes only if R's prefix is T.
func (t *Tenant) EnforceRoom(wantTenant, roomID string) error {
	if err := t.validateTenant(wantTenant); err != nil {
		return err
	}
	got, _, err := t.SplitRoom(roomID)
	if err != nil {
		return err
	}
	if got != wantTenant {
		return ErrTenantMismatch
	}
	return nil
}

// FilterRooms returns the subset of the input that belongs to wantTenant.
// Malformed room IDs are silently dropped — LiveKit may carry rooms created
// by other tenants in the same Redis store, and a list-rooms call MUST never
// return rows that belong to anyone but the caller. Returning an error here
// would let one tenant's bad data DoS another tenant's list view.
func (t *Tenant) FilterRooms(wantTenant string, roomIDs []string) []string {
	if err := t.validateTenant(wantTenant); err != nil {
		return nil
	}
	out := roomIDs[:0:0]
	prefix := wantTenant + t.sep
	for _, r := range roomIDs {
		if strings.HasPrefix(r, prefix) && len(r) > len(prefix) {
			out = append(out, r)
		}
	}
	return out
}

// MaxTenantIDLen is the maximum allowed length (in bytes) of a tenant
// identifier. 63 mirrors the DNS label limit and is a practical upper bound
// for any real tenant slug; longer values are rejected as malformed to prevent
// DoS via unbounded path parameters.
const MaxTenantIDLen = 63

// validateTenant enforces the tenant-ID character set and length. We
// deliberately keep the rule strict: ASCII letters, digits, hyphen, underscore
// only. No dots (invite confusion with DNS names); no separator byte (would
// corrupt the prefix invariant); no whitespace; length ≤ MaxTenantIDLen.
func (t *Tenant) validateTenant(tenant string) error {
	if tenant == "" {
		return ErrEmptyTenant
	}
	if len(tenant) > MaxTenantIDLen {
		return ErrInvalidTenant
	}
	for i := 0; i < len(tenant); i++ {
		c := tenant[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_'
		if !ok {
			return ErrInvalidTenant
		}
	}
	if strings.Contains(tenant, t.sep) {
		return ErrInvalidTenant
	}
	return nil
}
