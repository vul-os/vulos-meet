// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the YAML config shape `vulos-meet --config config.yaml` parses.
// It is intentionally narrow: most LiveKit tuning lives in a generated
// LiveKit config (see the comment block on LiveKitConfig). This struct is the
// VULOS-MEET/1 surface — the knobs Vulos operators touch.
type Config struct {
	// Region this box advertises (e.g. "za-jhb", "eu-fra"). Required.
	Region string `yaml:"region"`

	// TenantSeparator is the single-byte separator used between a tenant ID
	// and the room name in qualified room IDs. Defaults to ":" if empty.
	TenantSeparator string `yaml:"tenant_separator"`

	// LiveKit holds the LiveKit-side credentials + listen addrs. The
	// API key/secret pair MUST match what vulos-cloud MEET-CP-01 uses to
	// MINT tokens — otherwise every token will fail validation.
	LiveKit LiveKitConfig `yaml:"livekit"`

	// Admin guards /admin/*. Token is also overridable via
	// MEET_ADMIN_TOKEN env (env wins).
	Admin AdminConfig `yaml:"admin"`

	// Media defaults applied to the generated LiveKit config. These are the
	// production-relevant tuning knobs (simulcast layers, top-N audio mix,
	// active-speaker, cascading SFU).
	Media MediaConfig `yaml:"media"`

	// Room holds per-room and admin ceiling limits (max participants per room,
	// max rooms on this box). These are rendered into LiveKit's `room` block
	// (max_participants) and enforced at the admin layer (max rooms).
	Room RoomConfig `yaml:"room"`

	// Recording is the hook for vulos-cloud MEET-RECORDING-01. The actual
	// egress driver lives there; this is just the endpoint URL we hand
	// LiveKit Server so it knows where to push egress events.
	Recording RecordingConfig `yaml:"recording"`

	// Cluster is the cascading-SFU discovery layer. When `cascading_sfu` is
	// enabled in Media, vulos-meet renders a Redis-discovery block into the
	// generated LiveKit config (see MEET-CASCADE-CFG-04) and pings the
	// Redis target before exec'ing livekit-server so a misconfigured cluster
	// fails fast instead of half-up.
	Cluster ClusterConfig `yaml:"cluster"`

	// Signal is the front-side WebSocket reverse proxy that enforces
	// VULOS-MEET/1 at the edge. See MEET-SIGNAL-GATE-03.
	Signal SignalConfig `yaml:"signal"`

	// cascadingExplicit tracks whether the YAML explicitly set
	// media.cascading_sfu (as opposed to leaving it for the default). Used
	// only by Validate to distinguish "operator set it on, forgot redis"
	// (a fail-fast error) from "we defaulted it on; tolerate the no-op
	// render". Not serialised — internal bookkeeping.
	cascadingExplicit bool `yaml:"-"`
}

// LiveKitConfig holds the LiveKit Server credentials and listen addrs.
// vulos-meet supervises a livekit-server child process with these values
// rendered into a LiveKit-flavoured config file.
type LiveKitConfig struct {
	// APIKey + APISecret are the shared secret pair used to mint AND verify
	// LiveKit JWTs. vulos-meet uses them only to VERIFY (we never mint —
	// see internal/wrap/auth.go). MEET-CP-01 in vulos-cloud uses them only
	// to MINT.
	APIKey    string `yaml:"api_key"`
	APISecret string `yaml:"api_secret"`

	// SignalingAddr is the LiveKit signaling listen address (default :7880).
	// Only the PORT is rendered into the LiveKit config; the child is always
	// bound to loopback (127.0.0.1:<port>) so the SFU is reachable only via the
	// public signal-gate, never directly on the public interface.
	SignalingAddr string `yaml:"signaling_addr"`
	// RTCPortRangeStart/End is the UDP port range used for RTC media (and
	// TURN/UDP). LiveKit defaults are sane; we expose them because cloud
	// deployments often need to coordinate with the firewall layer.
	//
	// MUTUALLY EXCLUSIVE with RTCUDPPort: when RTCUDPPort is set (single-port
	// UDP mux, shape (b) in fly.toml) the range is NOT rendered, so all media
	// muxes over the one port. Leave RTCUDPPort at 0 to use the range.
	RTCPortRangeStart int `yaml:"rtc_port_range_start"`
	RTCPortRangeEnd   int `yaml:"rtc_port_range_end"`

	// RTCUDPPort, when > 0, enables LiveKit's single-port UDP mux (rtc.udp_port)
	// so ALL WebRTC media for every participant is multiplexed over one UDP
	// port instead of a wide range. This is shape (b) documented in fly.toml:
	// the simplest Fly UDP story (expose one port) and the way to actually
	// sustain hundreds of participants without enumerating hundreds of ports.
	// When set, the port range is omitted from the rendered config. Default 0
	// (use the port range).
	RTCUDPPort int `yaml:"rtc_udp_port"`

	// TURN listen address (e.g. ":3478"). Empty disables TURN.
	TURNAddr string `yaml:"turn_addr"`

	// EgressUpstreamAddr is the host:port of livekit-server's Twirp surface
	// (LiveKit serves /twirp/livekit.Egress/* on the same listener as
	// signaling). Default: SignalingAddr's host. Operators rarely touch
	// this — it exists so a test deployment can point the egress proxy at
	// a stub LiveKit binary without retargeting the signaling reverse
	// proxy. See internal/wrap/egress_proxy.go.
	EgressUpstreamAddr string `yaml:"egress_upstream_addr"`
}

// AdminConfig is the admin HTTP surface config.
type AdminConfig struct {
	// Addr is the admin listen address (e.g. ":7881").
	Addr string `yaml:"addr"`
	// Token is the bearer token; environment variable MEET_ADMIN_TOKEN
	// overrides this if set, so the config file never has to ship secrets.
	Token string `yaml:"token"`
}

// MediaConfig is the production-tuning surface for video meetings.
//
// Defaults (when fields are zero-valued):
//
//	codec               = "VP9"
//	simulcast layers    = ["180p", "360p", "720p"]
//	top-N audio mix     = 3
//	active-speaker      = true
//	cascading SFU       = true
type MediaConfig struct {
	Codec           string   `yaml:"codec"`
	SimulcastLayers []string `yaml:"simulcast_layers"`
	TopNAudioMix    int      `yaml:"top_n_audio_mix"`
	ActiveSpeaker   *bool    `yaml:"active_speaker"`
	CascadingSFU    *bool    `yaml:"cascading_sfu"`
}

// RoomConfig holds the room-level limits this box enforces.
//
// Two distinct ceilings:
//
//	MaxParticipants — rendered into the LiveKit config's `room.max_participants`,
//	  so LiveKit itself rejects a join past the cap (server-side enforcement, not
//	  just a client hint). 0 means "leave to LiveKit's default" (LiveKit treats
//	  0 as unlimited), so we default it to a sane value below.
//	MaxRooms — the admin-layer ceiling on how many concurrent rooms this single
//	  box will tolerate. The admin list/delete surface exposes the live count and
//	  callers (and the metrics surface) can see when the box is at capacity.
//
// The 500-participant tier is achievable only with the single-port UDP mux
// (RTCUDPPort) — a narrow 50000-50200 range (201 ports) cannot sustain it. The
// default here is intentionally conservative; bump MaxParticipants toward 500
// only on a deploy that has set livekit.rtc_udp_port (see fly.toml shape (b)).
type RoomConfig struct {
	// MaxParticipants is the per-room participant cap rendered into LiveKit's
	// room.max_participants. 0 → default (DefaultMaxParticipants).
	MaxParticipants int `yaml:"max_participants"`

	// MaxRooms is the admin-enforced ceiling on concurrent rooms for this box.
	// 0 → default (DefaultMaxRooms). The admin list handler refuses to report a
	// healthy state past this and the metric vulos_meet_rooms_at_capacity flips.
	MaxRooms int `yaml:"max_rooms"`
}

// Default room ceilings. These are deliberately conservative defaults that work
// on the narrow UDP range; raise MaxParticipants toward the 500 tier only with
// the single-port UDP mux enabled (livekit.rtc_udp_port).
const (
	DefaultMaxParticipants = 50
	DefaultMaxRooms        = 100
)

// RecordingConfig is the egress endpoint hook. The actual recording driver
// (S3 sink, transcode, retention) lives in vulos-cloud MEET-RECORDING-01;
// this is just the URL we hand to LiveKit's egress so it knows where to
// dispatch events.
type RecordingConfig struct {
	EgressEndpoint string `yaml:"egress_endpoint"`
}

// ClusterConfig is the cascading-SFU discovery configuration. LiveKit uses
// Redis to discover peer SFU nodes; for a single-box deployment this whole
// block is unset and the renderer omits it. When CascadingSFU is enabled,
// Redis is REQUIRED — the startup self-check fails fast if Redis is
// unreachable.
type ClusterConfig struct {
	// Redis is the Redis target LiveKit uses for node discovery (a single
	// instance is fine; LiveKit doesn't require cluster mode).
	Redis ClusterRedisConfig `yaml:"redis"`

	// NodeID is the unique-per-node identifier advertised to the cluster.
	// MUST be stable across restarts (otherwise peer SFUs see a new node
	// every time we bounce). Falls back to the box hostname when empty.
	NodeID string `yaml:"node_id"`

	// Region intentionally has no YAML tag: it is mirrored from cfg.Region
	// during applyDefaults() so the cluster view cannot drift from the
	// georoute view. Setting it in YAML is a user error we silently
	// override.
	Region string `yaml:"-"`
}

// ClusterRedisConfig is the LiveKit cluster Redis dial spec. We keep this
// minimal — username/password/tls live behind the same address as a URI is
// fine for v1.
type ClusterRedisConfig struct {
	// Addr is the Redis host:port (e.g. "redis.internal:6379"). Empty
	// disables cluster discovery even if CascadingSFU is on.
	Addr string `yaml:"addr"`

	// DB is the optional Redis DB index. Defaults to 0.
	DB int `yaml:"db"`

	// Password is an optional Redis password. Prefer the
	// MEET_CLUSTER_REDIS_PASSWORD env override (applied during applyEnv) so
	// the YAML on disk never holds a secret.
	Password string `yaml:"password"`
}

// SignalConfig is the front-side WebSocket reverse proxy listen address. The
// gate validates VULOS-MEET/1 tokens before forwarding the WebSocket upgrade
// to livekit-server's /rtc endpoint.
type SignalConfig struct {
	// Addr is the listen address the gate exposes (e.g. ":7882"). When
	// empty, the gate is mounted on the same loopback by default in
	// SignalGateAddr().
	Addr string `yaml:"addr"`
}

// LoadConfig reads, parses, and validates a YAML config file. Env overrides
// are applied after parsing so an operator can shove the admin token through
// systemd without committing it to disk.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("vulos-meet: --config path is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vulos-meet: read config %s: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig parses a YAML document. Exposed separately so tests can pass
// inline YAML without writing a tempfile.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // reject unknown fields so typos become errors
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("vulos-meet: parse config: %w", err)
	}
	// Capture "was cascading_sfu explicitly set?" BEFORE applyDefaults, so
	// downstream validation can distinguish "operator turned it on but
	// forgot redis" (fail-fast) from "we defaulted it on; tolerate" (no
	// render).
	c.cascadingExplicit = c.Media.CascadingSFU != nil
	c.applyEnv()
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv(AdminTokenEnv); v != "" {
		c.Admin.Token = v
	}
	if v := os.Getenv(ClusterRedisPasswordEnv); v != "" {
		c.Cluster.Redis.Password = v
	}
}

// ClusterRedisPasswordEnv carries the Redis password used by the cascading-
// SFU cluster discovery. Env-only by convention so the YAML on disk does
// not hold the secret.
const ClusterRedisPasswordEnv = "MEET_CLUSTER_REDIS_PASSWORD"

func (c *Config) applyDefaults() {
	if c.TenantSeparator == "" {
		c.TenantSeparator = DefaultTenantSeparator
	}
	if c.Media.Codec == "" {
		c.Media.Codec = "VP9"
	}
	if len(c.Media.SimulcastLayers) == 0 {
		c.Media.SimulcastLayers = []string{"180p", "360p", "720p"}
	}
	if c.Media.TopNAudioMix == 0 {
		c.Media.TopNAudioMix = 3
	}
	if c.Media.ActiveSpeaker == nil {
		t := true
		c.Media.ActiveSpeaker = &t
	}
	if c.Media.CascadingSFU == nil {
		t := true
		c.Media.CascadingSFU = &t
	}
	if c.LiveKit.SignalingAddr == "" {
		c.LiveKit.SignalingAddr = ":7880"
	}
	if c.LiveKit.EgressUpstreamAddr == "" {
		// LiveKit serves Twirp on the same port as signaling.
		c.LiveKit.EgressUpstreamAddr = c.LiveKit.SignalingAddr
	}
	// Port-range defaults apply ONLY when the single-port UDP mux is off. With
	// rtc_udp_port set, the range is irrelevant (and intentionally not rendered),
	// so we must not silently fabricate one that Validate would then reject.
	if c.LiveKit.RTCUDPPort == 0 {
		if c.LiveKit.RTCPortRangeStart == 0 {
			c.LiveKit.RTCPortRangeStart = 50000
		}
		if c.LiveKit.RTCPortRangeEnd == 0 {
			c.LiveKit.RTCPortRangeEnd = 60000
		}
	}
	if c.Room.MaxParticipants == 0 {
		c.Room.MaxParticipants = DefaultMaxParticipants
	}
	if c.Room.MaxRooms == 0 {
		c.Room.MaxRooms = DefaultMaxRooms
	}
	if c.Admin.Addr == "" {
		c.Admin.Addr = ":7881"
	}
	// Cluster.Region is a mirror of the box region; if the operator sets it
	// in YAML we silently overwrite to keep the cluster + georoute views
	// consistent. There is no "this node is in region X but advertises
	// region Y" mode — it would just be a fan-out attack on debuggability.
	c.Cluster.Region = c.Region
}

// SignalGateAddr returns the address the signaling reverse proxy listens on.
// Falls back to a loopback default when unset so the gate is always reachable
// from main.go without a config tweak.
func (c *Config) SignalGateAddr() string {
	if c.Signal.Addr != "" {
		return c.Signal.Addr
	}
	return "127.0.0.1:7883"
}

// CascadingSFUEnabled reports whether cluster discovery should be rendered.
// Centralises the nil-check on the *bool so other layers don't replicate it.
func (c *Config) CascadingSFUEnabled() bool {
	return c.Media.CascadingSFU != nil && *c.Media.CascadingSFU
}

// Validate enforces the invariants that, if violated, would make the box
// silently unsafe (open admin, no auth, cross-tenant leaks).
func (c *Config) Validate() error {
	if c.Region == "" {
		return errors.New("vulos-meet: config.region is required")
	}
	if len(c.TenantSeparator) != 1 {
		return errors.New("vulos-meet: config.tenant_separator must be a single ASCII byte")
	}
	if c.LiveKit.APIKey == "" {
		return errors.New("vulos-meet: config.livekit.api_key is required")
	}
	if c.LiveKit.APISecret == "" {
		return errors.New("vulos-meet: config.livekit.api_secret is required")
	}
	if c.Admin.Token == "" {
		return errors.New("vulos-meet: admin token is empty (set config.admin.token or " + AdminTokenEnv + ")")
	}
	// UDP transport: EITHER the single-port mux (rtc_udp_port) OR the port
	// range, never both — they are mutually exclusive at the LiveKit layer.
	if c.LiveKit.RTCUDPPort != 0 {
		if c.LiveKit.RTCUDPPort < 1 || c.LiveKit.RTCUDPPort > 65535 {
			return errors.New("vulos-meet: livekit.rtc_udp_port must be a valid port (1-65535)")
		}
		if c.LiveKit.RTCPortRangeStart != 0 || c.LiveKit.RTCPortRangeEnd != 0 {
			return errors.New("vulos-meet: set EITHER livekit.rtc_udp_port (single-port mux) OR rtc_port_range_* (range), not both")
		}
	} else if c.LiveKit.RTCPortRangeStart <= 0 || c.LiveKit.RTCPortRangeEnd <= c.LiveKit.RTCPortRangeStart {
		return errors.New("vulos-meet: invalid rtc_port_range_*")
	}
	if c.Media.TopNAudioMix <= 0 {
		return errors.New("vulos-meet: media.top_n_audio_mix must be > 0")
	}
	if c.Room.MaxParticipants < 0 {
		return errors.New("vulos-meet: room.max_participants must be >= 0")
	}
	if c.Room.MaxRooms < 0 {
		return errors.New("vulos-meet: room.max_rooms must be >= 0")
	}
	// NOTE on cascading SFU validation: when cascading_sfu is explicitly
	// enabled in YAML but cluster.redis.addr is empty, that is a user error
	// and we fail fast. But the default is cascading_sfu=true (operators
	// rarely flip it off), so we tolerate "defaulted true + no redis" by
	// downgrading to a no-op render at supervise time — see
	// CascadingSFUEnabled() and renderClusterBlock(). The explicit-but-
	// unconfigured case is checked downstream when CascadingSFUExplicit is
	// set; that wiring is captured in the parse path below.
	if c.cascadingExplicit && c.CascadingSFUEnabled() && c.Cluster.Redis.Addr == "" {
		return errors.New("vulos-meet: media.cascading_sfu is on but cluster.redis.addr is empty")
	}
	return nil
}
