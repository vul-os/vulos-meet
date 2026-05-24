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

	// Recording is the hook for vulos-cloud MEET-RECORDING-01. The actual
	// egress driver lives there; this is just the endpoint URL we hand
	// LiveKit Server so it knows where to push egress events.
	Recording RecordingConfig `yaml:"recording"`
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
	SignalingAddr string `yaml:"signaling_addr"`
	// RTCPortRangeStart/End is the UDP port range used for RTC media (and
	// TURN/UDP). LiveKit defaults are sane; we expose them because cloud
	// deployments often need to coordinate with the firewall layer.
	RTCPortRangeStart int `yaml:"rtc_port_range_start"`
	RTCPortRangeEnd   int `yaml:"rtc_port_range_end"`

	// TURN listen address (e.g. ":3478"). Empty disables TURN.
	TURNAddr string `yaml:"turn_addr"`
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

// RecordingConfig is the egress endpoint hook. The actual recording driver
// (S3 sink, transcode, retention) lives in vulos-cloud MEET-RECORDING-01;
// this is just the URL we hand to LiveKit's egress so it knows where to
// dispatch events.
type RecordingConfig struct {
	EgressEndpoint string `yaml:"egress_endpoint"`
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
}

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
	if c.LiveKit.RTCPortRangeStart == 0 {
		c.LiveKit.RTCPortRangeStart = 50000
	}
	if c.LiveKit.RTCPortRangeEnd == 0 {
		c.LiveKit.RTCPortRangeEnd = 60000
	}
	if c.Admin.Addr == "" {
		c.Admin.Addr = ":7881"
	}
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
	if c.LiveKit.RTCPortRangeStart <= 0 || c.LiveKit.RTCPortRangeEnd <= c.LiveKit.RTCPortRangeStart {
		return errors.New("vulos-meet: invalid rtc_port_range_*")
	}
	if c.Media.TopNAudioMix <= 0 {
		return errors.New("vulos-meet: media.top_n_audio_mix must be > 0")
	}
	return nil
}
