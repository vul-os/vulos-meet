// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"sync"
)

// MemoryRoomService is an in-memory RoomService implementation used as a
// bootstrap stand-in for the real LiveKit RoomService client. It exists so
// the admin surface, tenant gate, and end-to-end wiring can be exercised
// (and tested) without a live LiveKit Server child process.
//
// Production deployments will replace this with a thin wrapper around
// github.com/livekit/protocol's RoomServiceClient (gRPC over the LiveKit
// signaling port). That swap is local to cmd/vulos-meet — nothing in
// internal/wrap depends on the concrete implementation, only on the
// RoomService interface.
type MemoryRoomService struct {
	mu    sync.Mutex
	rooms map[string]struct{}
}

// NewMemoryRoomService constructs an empty in-memory room service.
func NewMemoryRoomService() *MemoryRoomService {
	return &MemoryRoomService{rooms: make(map[string]struct{})}
}

// CreateRoom seeds a room ID. Used by tests + by the (eventual) "create
// room on first join" upstream signal from LiveKit.
func (m *MemoryRoomService) CreateRoom(_ context.Context, roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rooms[roomID] = struct{}{}
}

// ListRoomIDs returns every known room ID. The tenant gate is responsible
// for narrowing this to a single tenant before the result reaches an admin
// client.
func (m *MemoryRoomService) ListRoomIDs(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.rooms))
	for r := range m.rooms {
		out = append(out, r)
	}
	return out, nil
}

// DeleteRoom forgets a room. Returns nil whether or not the room existed —
// this matches the LiveKit RoomService semantics (idempotent delete).
func (m *MemoryRoomService) DeleteRoom(_ context.Context, roomID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rooms, roomID)
	return nil
}
