// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func readFileForTest(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

func cascadingEnabled(b bool) *bool { return &b }

func TestCluster_RenderBlock_OmittedWhenDisabled(t *testing.T) {
	cfg := &Config{
		Region: "za-jhb",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(false)},
	}
	cfg.Cluster.Region = cfg.Region
	if got := renderClusterBlock(cfg); got != "" {
		t.Fatalf("expected empty block when disabled, got %q", got)
	}
}

func TestCluster_RenderBlock_OmittedWhenRedisAddrEmpty(t *testing.T) {
	cfg := &Config{
		Region: "za-jhb",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(true)},
	}
	cfg.Cluster.Region = cfg.Region
	if got := renderClusterBlock(cfg); got != "" {
		t.Fatalf("expected empty block when redis addr unset, got %q", got)
	}
}

func TestCluster_RenderBlock_RegionMirrorsBoxRegion(t *testing.T) {
	cfg := &Config{
		Region: "za-jhb",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(true)},
	}
	cfg.Cluster.Redis.Addr = "redis.internal:6379"
	cfg.Cluster.NodeID = "meet-1"
	cfg.Cluster.Region = cfg.Region

	got := renderClusterBlock(cfg)
	for _, want := range []string{
		"redis:",
		"address: redis.internal:6379",
		"cluster_id: za-jhb",
		"node_id: meet-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered block missing %q:\n%s", want, got)
		}
	}
}

func TestCluster_RenderBlock_DBAndPasswordRendered(t *testing.T) {
	cfg := &Config{
		Region: "eu-fra",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(true)},
	}
	cfg.Cluster.Redis.Addr = "127.0.0.1:6379"
	cfg.Cluster.Redis.DB = 7
	cfg.Cluster.Redis.Password = "hunter2"
	cfg.Cluster.NodeID = "meet-2"
	cfg.Cluster.Region = cfg.Region

	got := renderClusterBlock(cfg)
	for _, want := range []string{"db: 7", "password: hunter2", "node_id: meet-2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("block missing %q:\n%s", want, got)
		}
	}
}

func TestConfig_ValidateRejectsCascadingWithoutRedis(t *testing.T) {
	// Cascading on, but no redis addr → must fail validation.
	cfg := `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
admin:
  token: tok
media:
  cascading_sfu: true
`
	if _, err := ParseConfig([]byte(cfg)); err == nil {
		t.Fatalf("expected error: cascading on, redis empty")
	}
}

func TestConfig_ValidateAcceptsCascadingWithRedis(t *testing.T) {
	cfg := `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
admin:
  token: tok
media:
  cascading_sfu: true
cluster:
  redis:
    addr: redis.internal:6379
  node_id: meet-1
`
	c, err := ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Cluster.Region != "za-jhb" {
		t.Fatalf("cluster.region should mirror box region, got %q", c.Cluster.Region)
	}
	if c.Cluster.Redis.Addr != "redis.internal:6379" {
		t.Fatalf("redis addr: %q", c.Cluster.Redis.Addr)
	}
	if c.Cluster.NodeID != "meet-1" {
		t.Fatalf("node id: %q", c.Cluster.NodeID)
	}
}

func TestConfig_EnvOverridesRedisPassword(t *testing.T) {
	t.Setenv(ClusterRedisPasswordEnv, "env-password")
	cfg := `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
admin:
  token: tok
media:
  cascading_sfu: true
cluster:
  redis:
    addr: redis.internal:6379
    password: from-file
`
	c, err := ParseConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Cluster.Redis.Password != "env-password" {
		t.Fatalf("env should override file: %q", c.Cluster.Redis.Password)
	}
}

// fakeRedis listens on a free TCP port and replies to PING with +PONG. It
// lets the self-check test run without a real Redis dep. The implementation
// is the smallest valid RESP parser that handles the two commands we send.
func fakeRedis(t *testing.T, respondPong bool, requirePassword string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(conn)
		for {
			args, err := readRESPCommand(br)
			if err != nil {
				return
			}
			if len(args) == 0 {
				return
			}
			switch strings.ToUpper(args[0]) {
			case "AUTH":
				if requirePassword != "" && (len(args) < 2 || args[1] != requirePassword) {
					_, _ = conn.Write([]byte("-ERR wrong password\r\n"))
					return
				}
				_, _ = conn.Write([]byte("+OK\r\n"))
			case "PING":
				if respondPong {
					_, _ = conn.Write([]byte("+PONG\r\n"))
				} else {
					_, _ = conn.Write([]byte("-ERR nope\r\n"))
				}
			default:
				_, _ = conn.Write([]byte("-ERR unknown\r\n"))
				return
			}
		}
	}()
	return ln.Addr().String()
}

// readRESPCommand reads a single *N\r\n$L\r\nARG\r\n... shaped command.
func readRESPCommand(br *bufio.Reader) ([]string, error) {
	head, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	head = strings.TrimRight(head, "\r\n")
	if !strings.HasPrefix(head, "*") {
		return nil, errors.New("bad multi-bulk header")
	}
	var n int
	if _, err := fmtSscan(head[1:], &n); err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if !strings.HasPrefix(hdr, "$") {
			return nil, errors.New("bad bulk header: " + hdr)
		}
		body, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		out = append(out, strings.TrimRight(body, "\r\n"))
	}
	return out, nil
}

func fmtSscan(s string, n *int) (int, error) {
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number: " + s)
		}
		v = v*10 + int(r-'0')
	}
	*n = v
	return 1, nil
}

func TestCluster_PingRedis_DisabledIsNoOp(t *testing.T) {
	cfg := &Config{Region: "za-jhb"}
	// cascading off → must return nil even with no redis configured
	if err := PingClusterRedis(context.Background(), cfg); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

func TestCluster_PingRedis_UnreachableIsErr(t *testing.T) {
	cfg := &Config{
		Region: "za-jhb",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(true)},
	}
	// 127.0.0.1:1 is a guaranteed-closed port.
	cfg.Cluster.Redis.Addr = "127.0.0.1:1"
	if err := PingClusterRedis(context.Background(), cfg); !errors.Is(err, ErrClusterRedisUnreachable) {
		t.Fatalf("expected ErrClusterRedisUnreachable, got %v", err)
	}
}

func TestCluster_PingRedis_ReachablePongs(t *testing.T) {
	addr := fakeRedis(t, true, "")
	cfg := &Config{
		Region: "za-jhb",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(true)},
	}
	cfg.Cluster.Redis.Addr = addr
	if err := PingClusterRedis(context.Background(), cfg); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestCluster_PingRedis_WrongAuthFails(t *testing.T) {
	addr := fakeRedis(t, true, "expected-password")
	cfg := &Config{
		Region: "za-jhb",
		Media:  MediaConfig{CascadingSFU: cascadingEnabled(true)},
	}
	cfg.Cluster.Redis.Addr = addr
	cfg.Cluster.Redis.Password = "wrong"
	if err := PingClusterRedis(context.Background(), cfg); !errors.Is(err, ErrClusterRedisUnreachable) {
		t.Fatalf("expected auth fail, got %v", err)
	}
}

func TestSupervise_WriteConfigIncludesClusterBlock(t *testing.T) {
	cfg := &Config{
		Region:          "za-jhb",
		TenantSeparator: ":",
		LiveKit: LiveKitConfig{
			APIKey: "k", APISecret: "s", SignalingAddr: ":7880",
			RTCPortRangeStart: 50000, RTCPortRangeEnd: 60000,
		},
		Admin: AdminConfig{Addr: ":7881", Token: "x"},
		Media: MediaConfig{Codec: "VP9", TopNAudioMix: 3, CascadingSFU: cascadingEnabled(true)},
	}
	cfg.Cluster.Redis.Addr = "redis.internal:6379"
	cfg.Cluster.NodeID = "meet-1"
	cfg.Cluster.Region = cfg.Region

	dir := t.TempDir()
	if err := writeLiveKitConfig(cfg, dir+"/lk.yaml"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readFileForTest(dir + "/lk.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"redis:", "address: redis.internal:6379", "cluster_id: za-jhb"} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
}

func TestSupervise_WriteConfigOmitsClusterWhenDisabled(t *testing.T) {
	cfg := &Config{
		Region:          "za-jhb",
		TenantSeparator: ":",
		LiveKit: LiveKitConfig{
			APIKey: "k", APISecret: "s", SignalingAddr: ":7880",
			RTCPortRangeStart: 50000, RTCPortRangeEnd: 60000,
		},
		Admin: AdminConfig{Addr: ":7881", Token: "x"},
		Media: MediaConfig{Codec: "VP9", TopNAudioMix: 3, CascadingSFU: cascadingEnabled(false)},
	}
	cfg.Cluster.Region = cfg.Region

	dir := t.TempDir()
	if err := writeLiveKitConfig(cfg, dir+"/lk.yaml"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readFileForTest(dir + "/lk.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(got, "redis:") || strings.Contains(got, "cluster_id:") {
		t.Fatalf("config should NOT contain cluster block when disabled:\n%s", got)
	}
}
