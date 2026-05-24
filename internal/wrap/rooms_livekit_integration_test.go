// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

//go:build livekitintegration

// Integration test for the real LiveKit RoomService client wired against an
// actual livekit-server binary. Guarded by the `livekitintegration` build
// tag AND a runtime PATH check: this file compiles only when the tag is on,
// and skips cleanly when the binary is not on PATH (so CI without the
// binary still passes when the tag is set by mistake).
//
// Run:  go test -tags livekitintegration -run TestIntegration ./internal/wrap

package wrap

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIntegration_LiveKitRoomService(t *testing.T) {
	bin := os.Getenv(LiveKitBinaryEnv)
	if bin == "" {
		bin = "livekit-server"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("integration: %s not on PATH (set %s): %v", bin, LiveKitBinaryEnv, err)
	}

	// Render a minimal LiveKit config to a tempfile.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "livekit.yaml")
	cfg := &Config{
		Region:          "test-region",
		TenantSeparator: ":",
		LiveKit: LiveKitConfig{
			APIKey:            testAPIKey,
			APISecret:         testAPISecret,
			SignalingAddr:     ":17880",
			RTCPortRangeStart: 55000,
			RTCPortRangeEnd:   55100,
		},
		Admin: AdminConfig{Addr: ":17881", Token: "x"},
		Media: MediaConfig{TopNAudioMix: 3},
	}
	if err := writeLiveKitConfig(cfg, cfgPath); err != nil {
		t.Fatalf("render config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--config", cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start livekit-server: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	// Wait until the signaling port accepts a TCP connection (livekit-server
	// is up).
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:17880", 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	rs, err := NewLiveKitRoomService(LiveKitRoomServiceConfig{
		SignalingAddr: "127.0.0.1:17880",
		APIKey:        testAPIKey,
		APISecret:     testAPISecret,
	})
	if err != nil {
		t.Fatalf("new room service: %v", err)
	}
	defer rs.Close()

	// Listing on an empty server returns no rooms but no error.
	ids, err := rs.ListRoomIDs(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected empty list, got %v", ids)
	}

	// Deleting a non-existent room is idempotent in LiveKit semantics; the
	// call should succeed (or return a benign error). We assert "no panic /
	// no hang / no breaker trip".
	_ = rs.DeleteRoom(context.Background(), "acme:does-not-exist")
	if rs.BreakerOpen() {
		t.Fatalf("breaker tripped on benign delete")
	}
}
