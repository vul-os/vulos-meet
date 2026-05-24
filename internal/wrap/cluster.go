// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// ClusterPingTimeout caps how long the startup self-check waits for Redis
// to answer PING. Kept short — a stalled Redis on boot is a deploy bug, not
// a transient outage, and we want fail-fast not patience.
const ClusterPingTimeout = 3 * time.Second

// ErrClusterRedisUnreachable is returned when the startup Redis self-check
// fails. The supervisor maps this to a fatal startup error so a misconfigured
// cluster never reaches LiveKit (which would otherwise come up half-broken).
var ErrClusterRedisUnreachable = errors.New("vulos-meet: cluster redis unreachable")

// PingClusterRedis verifies that the configured Redis target answers a PING
// before we exec livekit-server. Returns nil when cluster discovery is
// disabled (no cascading SFU configured) so the call is safe to invoke
// unconditionally from supervise.
//
// We speak raw RESP rather than pulling go-redis here because the dep would
// be the largest in our tree just to send one command. The protocol is
// trivial and stable.
func PingClusterRedis(ctx context.Context, cfg *Config) error {
	if !cfg.CascadingSFUEnabled() || cfg.Cluster.Redis.Addr == "" {
		return nil
	}
	dialer := &net.Dialer{Timeout: ClusterPingTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Cluster.Redis.Addr)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %v", ErrClusterRedisUnreachable, cfg.Cluster.Redis.Addr, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(ClusterPingTimeout)
	_ = conn.SetDeadline(deadline)
	br := bufio.NewReader(conn)
	if pw := cfg.Cluster.Redis.Password; pw != "" {
		if _, err := writeRedisCmd(conn, "AUTH", pw); err != nil {
			return fmt.Errorf("%w: auth write: %v", ErrClusterRedisUnreachable, err)
		}
		if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+OK") {
			return fmt.Errorf("%w: auth rejected (line=%q err=%v)", ErrClusterRedisUnreachable, line, err)
		}
	}
	if _, err := writeRedisCmd(conn, "PING"); err != nil {
		return fmt.Errorf("%w: ping write: %v", ErrClusterRedisUnreachable, err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("%w: ping read: %v", ErrClusterRedisUnreachable, err)
	}
	if !strings.HasPrefix(line, "+PONG") {
		return fmt.Errorf("%w: unexpected ping reply %q", ErrClusterRedisUnreachable, line)
	}
	return nil
}

// writeRedisCmd writes a RESP-encoded command. Stays argv-shaped so we
// never have to worry about embedded \r\n in args.
func writeRedisCmd(w interface{ Write([]byte) (int, error) }, parts ...string) (int, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return w.Write([]byte(b.String()))
}

// renderClusterBlock emits the LiveKit cluster discovery YAML fragment when
// cascading SFU is enabled. Returns an empty string when disabled — the
// rendered LiveKit config then omits the cluster block entirely and LiveKit
// behaves as a single-node SFU.
//
// LiveKit's schema:
//
//	redis:
//	  address: host:port
//	  username: ""
//	  password: ""
//	  db: 0
//	cluster_id: <region>
//	node_id: <node>
//
// We feed the box region into cluster_id so the cluster view stays consistent
// with the georoute view (CLOUD-REGION-01). Mixing regions in one cluster_id
// is operationally a bug: cascading SFU expects intra-region peering, not
// global mesh.
func renderClusterBlock(cfg *Config) string {
	if !cfg.CascadingSFUEnabled() || cfg.Cluster.Redis.Addr == "" {
		return ""
	}
	nodeID := cfg.Cluster.NodeID
	if nodeID == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			nodeID = h
		} else {
			nodeID = "vulos-meet"
		}
	}
	region := cfg.Cluster.Region
	if region == "" {
		region = cfg.Region
	}
	var b strings.Builder
	b.WriteString("redis:\n")
	b.WriteString("  address: ")
	b.WriteString(cfg.Cluster.Redis.Addr)
	b.WriteString("\n")
	if cfg.Cluster.Redis.Password != "" {
		b.WriteString("  password: ")
		b.WriteString(cfg.Cluster.Redis.Password)
		b.WriteString("\n")
	}
	if cfg.Cluster.Redis.DB != 0 {
		fmt.Fprintf(&b, "  db: %d\n", cfg.Cluster.Redis.DB)
	}
	fmt.Fprintf(&b, "cluster_id: %s\n", region)
	fmt.Fprintf(&b, "node_id: %s\n", nodeID)
	return b.String()
}
