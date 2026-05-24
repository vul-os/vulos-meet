// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

func TestTenant_QualifyAndSplit(t *testing.T) {
	gate := NewTenant("")
	full, err := gate.QualifyRoom("acme", "standup")
	if err != nil {
		t.Fatalf("qualify: %v", err)
	}
	if full != "acme:standup" {
		t.Fatalf("qualify: got %q, want %q", full, "acme:standup")
	}
	got, rest, err := gate.SplitRoom(full)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if got != "acme" || rest != "standup" {
		t.Fatalf("split: got (%q,%q), want (acme,standup)", got, rest)
	}
}

func TestTenant_RejectsCrossTenantRoom(t *testing.T) {
	gate := NewTenant("")
	// Caller claims tenant "acme" but room belongs to "evil".
	if err := gate.EnforceRoom("acme", "evil:standup"); !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("expected ErrTenantMismatch, got %v", err)
	}
	// Caller claims tenant "acme" with a room missing any prefix.
	if err := gate.EnforceRoom("acme", "standup"); !errors.Is(err, ErrRoomMissingPrefix) {
		t.Fatalf("expected ErrRoomMissingPrefix, got %v", err)
	}
	// Empty room.
	if err := gate.EnforceRoom("acme", ""); !errors.Is(err, ErrEmptyRoom) {
		t.Fatalf("expected ErrEmptyRoom, got %v", err)
	}
	// Empty tenant.
	if err := gate.EnforceRoom("", "acme:standup"); !errors.Is(err, ErrEmptyTenant) {
		t.Fatalf("expected ErrEmptyTenant, got %v", err)
	}
}

func TestTenant_FilterRooms_NoLeak(t *testing.T) {
	gate := NewTenant("")
	all := []string{
		"acme:standup",
		"acme:retro",
		"evil:weekly",
		"globex:planning",
		"acme:", // malformed (empty rest); MUST be filtered out
		"acme",  // malformed (no sep); MUST be filtered out
	}
	got := gate.FilterRooms("acme", all)
	sort.Strings(got)
	want := []string{"acme:retro", "acme:standup"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterRooms leaked or dropped: got %v, want %v", got, want)
	}
}

func TestTenant_InvalidTenantCharsRejected(t *testing.T) {
	gate := NewTenant("")
	bad := []string{"acme.com", "a/b", "a b", "a:b", ""}
	for _, b := range bad {
		if err := gate.validateTenant(b); err == nil {
			t.Fatalf("tenant %q: expected validation error, got nil", b)
		}
	}
	good := []string{"acme", "a-b", "a_b", "tenant1", "TENANT", "x"}
	for _, g := range good {
		if err := gate.validateTenant(g); err != nil {
			t.Fatalf("tenant %q: expected no error, got %v", g, err)
		}
	}
}

func TestTenant_QualifyRejectsDoubleQualification(t *testing.T) {
	gate := NewTenant("")
	if _, err := gate.QualifyRoom("acme", "evil:standup"); err == nil {
		t.Fatalf("expected error when re-qualifying an already-qualified room")
	}
}

func TestTenant_NonDefaultSeparator(t *testing.T) {
	gate := NewTenant("|")
	full, err := gate.QualifyRoom("acme", "standup")
	if err != nil || full != "acme|standup" {
		t.Fatalf("qualify with custom sep: %q err=%v", full, err)
	}
	if err := gate.EnforceRoom("acme", "acme|standup"); err != nil {
		t.Fatalf("enforce with custom sep: %v", err)
	}
	// A ":" room should be REJECTED when the configured sep is "|".
	if err := gate.EnforceRoom("acme", "acme:standup"); err == nil {
		t.Fatalf("expected error for wrong separator")
	}
}

func TestTenant_MultiByteSeparatorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for multi-byte separator")
		}
	}()
	_ = NewTenant("--")
}
