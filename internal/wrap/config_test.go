// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

const sampleConfigYAML = `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
  signaling_addr: ":7880"
  rtc_port_range_start: 50000
  rtc_port_range_end: 60000
  turn_addr: ":3478"
admin:
  addr: ":7881"
  token: file-supplied-token
media:
  codec: VP9
  simulcast_layers: ["180p", "360p", "720p"]
  top_n_audio_mix: 3
  active_speaker: true
  cascading_sfu: true
cluster:
  redis:
    addr: redis.internal:6379
  node_id: meet-test-1
recording:
  egress_endpoint: https://meet-egress.vulos.example/v1/webhook
`

func TestConfig_ParsesSampleYAML(t *testing.T) {
	// Make sure the env var doesn't leak from the test environment.
	_ = os.Unsetenv(AdminTokenEnv)

	c, err := ParseConfig([]byte(sampleConfigYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Region != "za-jhb" {
		t.Fatalf("region: got %q", c.Region)
	}
	if c.LiveKit.APIKey != "APItestkey" || c.LiveKit.APISecret == "" {
		t.Fatalf("livekit creds not parsed")
	}
	if c.Admin.Token != "file-supplied-token" {
		t.Fatalf("admin token: got %q", c.Admin.Token)
	}
	if c.Media.TopNAudioMix != 3 {
		t.Fatalf("top n: got %d", c.Media.TopNAudioMix)
	}
	if !reflect.DeepEqual(c.Media.SimulcastLayers, []string{"180p", "360p", "720p"}) {
		t.Fatalf("simulcast layers: got %v", c.Media.SimulcastLayers)
	}
	if c.Recording.EgressEndpoint == "" {
		t.Fatalf("egress endpoint not parsed")
	}
}

func TestConfig_AppliesDefaults(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	// Minimal config: only required fields. Defaults should fill the rest.
	minimal := `
region: eu-fra
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
admin:
  token: tok
`
	c, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.TenantSeparator != ":" {
		t.Fatalf("tenant sep default: got %q", c.TenantSeparator)
	}
	if c.Media.Codec != "VP9" {
		t.Fatalf("codec default: got %q", c.Media.Codec)
	}
	if c.Media.TopNAudioMix != 3 {
		t.Fatalf("top n default: got %d", c.Media.TopNAudioMix)
	}
	if c.Media.ActiveSpeaker == nil || !*c.Media.ActiveSpeaker {
		t.Fatalf("active speaker default should be true")
	}
	if c.Media.CascadingSFU == nil || !*c.Media.CascadingSFU {
		t.Fatalf("cascading SFU default should be true")
	}
	if !reflect.DeepEqual(c.Media.SimulcastLayers, []string{"180p", "360p", "720p"}) {
		t.Fatalf("simulcast default: got %v", c.Media.SimulcastLayers)
	}
	if c.LiveKit.SignalingAddr != ":7880" || c.Admin.Addr != ":7881" {
		t.Fatalf("addr defaults: got signaling=%q admin=%q", c.LiveKit.SignalingAddr, c.Admin.Addr)
	}
}

func TestConfig_RejectsMissingRequired(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	cases := map[string]string{
		"missing region":  `livekit: { api_key: k, api_secret: s }` + "\nadmin: { token: t }\n",
		"missing api key": "region: x\nlivekit:\n  api_secret: s\nadmin:\n  token: t\n",
		"missing secret":  "region: x\nlivekit:\n  api_key: k\nadmin:\n  token: t\n",
		"missing admin":   "region: x\nlivekit:\n  api_key: k\n  api_secret: s\n",
		"bad separator":   "region: x\ntenant_separator: \":-\"\nlivekit:\n  api_key: k\n  api_secret: s\nadmin:\n  token: t\n",
		"unknown field":   "region: x\nlivekit:\n  api_key: k\n  api_secret: s\nadmin:\n  token: t\nshoes: red\n",
	}
	for name, raw := range cases {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestConfig_EnvOverridesAdminToken(t *testing.T) {
	t.Setenv(AdminTokenEnv, "env-supplied-token")
	c, err := ParseConfig([]byte(sampleConfigYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Admin.Token != "env-supplied-token" {
		t.Fatalf("env should override file: got %q", c.Admin.Token)
	}
}

func TestLoadConfig_RequiresPath(t *testing.T) {
	if _, err := LoadConfig(""); err == nil {
		t.Fatalf("expected error with empty path")
	}
	if _, err := LoadConfig("/no/such/path.yaml"); err == nil {
		t.Fatalf("expected error with non-existent path")
	}
}

func TestLoadConfig_ReadsFromDisk(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	dir := t.TempDir()
	path := dir + "/cfg.yaml"
	if err := os.WriteFile(path, []byte(sampleConfigYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Region != "za-jhb" || !strings.Contains(c.Recording.EgressEndpoint, "vulos.example") {
		t.Fatalf("round-trip from disk lost data: %+v", c)
	}
}
