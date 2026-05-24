// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// LiveKitBinaryEnv lets an operator override the path to the livekit-server
// binary. Default is "livekit-server" on PATH. (Choice (b) in the README:
// supervise out-of-process rather than embed.)
const LiveKitBinaryEnv = "VULOS_MEET_LIVEKIT_BIN"

// SuperviseLiveKit launches livekit-server as a child process with a config
// file rendered from cfg. It blocks until the child exits or ctx is cancelled
// (in which case SIGTERM is forwarded to the child, then SIGKILL after a
// short grace period). Returns the child's exit error.
//
// Why a child process and not an embedded import:
//
//	github.com/livekit/livekit-server pulls a very large dep graph (full
//	Pion stack, Redis client, NATS, multiple zap variants, OTel, ...) and
//	upstream's documented deployment mode is the standalone binary. Staying
//	out of the embedding business keeps vulos-meet's binary tiny and lets
//	us track upstream security releases via simple binary swaps rather than
//	dep-bump merges. See README "Architecture: vendor or supervise?".
func SuperviseLiveKit(ctx context.Context, cfg *Config, lkConfigPath string) error {
	bin := os.Getenv(LiveKitBinaryEnv)
	if bin == "" {
		bin = "livekit-server"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("vulos-meet: livekit-server binary not found (set %s or install livekit-server): %w", LiveKitBinaryEnv, err)
	}
	if err := writeLiveKitConfig(cfg, lkConfigPath); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "--config", lkConfigPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Send SIGTERM (not SIGKILL) on ctx cancel so LiveKit can drain rooms.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("vulos-meet: start livekit-server: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		// A clean SIGTERM exits with a non-nil error too; convert to nil
		// only if the parent ctx was the cancellation source.
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return fmt.Errorf("vulos-meet: livekit-server exited: %w", err)
	}
	return nil
}

// writeLiveKitConfig renders the Vulos defaults into a LiveKit-flavoured YAML
// config that the child binary consumes. We keep this rendering trivial and
// well-commented because LiveKit's config schema is large and we deliberately
// only set the fields we care about — everything else is left to LiveKit's
// own defaults.
func writeLiveKitConfig(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vulos-meet: mkdir livekit config dir: %w", err)
	}
	// Render the LiveKit config inline. We do this with a string template
	// (not the LiveKit Go types) so we don't pull livekit-server as a Go
	// dep. The schema below matches LiveKit Server's documented YAML.
	doc := "" +
		"port: " + portFromAddr(cfg.LiveKit.SignalingAddr) + "\n" +
		"rtc:\n" +
		"  port_range_start: " + strconv.Itoa(cfg.LiveKit.RTCPortRangeStart) + "\n" +
		"  port_range_end: " + strconv.Itoa(cfg.LiveKit.RTCPortRangeEnd) + "\n" +
		"  use_external_ip: true\n" +
		"keys:\n" +
		"  " + cfg.LiveKit.APIKey + ": " + cfg.LiveKit.APISecret + "\n" +
		"room:\n" +
		"  auto_create: true\n" +
		"  enabled_codecs:\n" +
		"    - mime: video/" + cfg.Media.Codec + "\n" +
		"    - mime: video/H264\n" +
		"    - mime: audio/opus\n" +
		"audio:\n" +
		// LiveKit's top-N active-speaker mix is governed by the
		// active_level + smooth_intervals; the actual N is enforced
		// on the publish side by client SDKs. We set the server-side
		// detection knobs here.
		"  active_level: 30\n" +
		"  min_percentile: 40\n" +
		"  update_interval: 400\n" +
		"  smooth_intervals: 2\n"
	if cfg.LiveKit.TURNAddr != "" {
		doc += "turn:\n" +
			"  enabled: true\n" +
			"  udp_port: " + portFromAddr(cfg.LiveKit.TURNAddr) + "\n"
	}
	if cfg.Recording.EgressEndpoint != "" {
		// Egress hook: vulos-cloud MEET-RECORDING-01 sits behind this URL.
		doc += "egress:\n" +
			"  webhook_url: " + cfg.Recording.EgressEndpoint + "\n"
	}
	return os.WriteFile(path, []byte(doc), 0o600)
}

// portFromAddr extracts the port from ":7880" or "0.0.0.0:7880". Returns the
// raw input if it can't be parsed — that mode lets bad config surface as a
// LiveKit startup error rather than a silent default.
func portFromAddr(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}
