// Copyright (c) 2026 Vulos contributors
// SPDX-License-Identifier: MIT

package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderConfig is a small helper: build a Config from inline YAML (so defaults
// + validation run exactly as in production), render the LiveKit child config,
// and return the rendered text.
func renderConfig(t *testing.T, yaml string) string {
	t.Helper()
	_ = os.Unsetenv(AdminTokenEnv)
	c, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "livekit.yaml")
	if err := writeLiveKitConfig(c, path); err != nil {
		t.Fatalf("write livekit config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	return string(raw)
}

const baseMeetYAML = `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
admin:
  token: tok
`

// ── Item 1: participant cap rendered + admin ceiling configurable ─────────────

func TestRender_ParticipantCapDefault(t *testing.T) {
	doc := renderConfig(t, baseMeetYAML)
	want := "max_participants: " + itoaTest(DefaultMaxParticipants)
	if !strings.Contains(doc, want) {
		t.Fatalf("rendered config missing default participant cap %q; got:\n%s", want, doc)
	}
}

func TestRender_ParticipantCapConfigured(t *testing.T) {
	// With the single-port mux on, the 500 tier is allowed.
	doc := renderConfig(t, `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
  rtc_udp_port: 50000
admin:
  token: tok
room:
  max_participants: 500
  max_rooms: 200
`)
	if !strings.Contains(doc, "max_participants: 500") {
		t.Fatalf("rendered config missing configured participant cap 500; got:\n%s", doc)
	}
}

func TestConfig_RoomCeilingsParsedAndDefaulted(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	// Defaults when unset.
	c, err := ParseConfig([]byte(baseMeetYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Room.MaxParticipants != DefaultMaxParticipants {
		t.Fatalf("max_participants default: got %d want %d", c.Room.MaxParticipants, DefaultMaxParticipants)
	}
	if c.Room.MaxRooms != DefaultMaxRooms {
		t.Fatalf("max_rooms default: got %d want %d", c.Room.MaxRooms, DefaultMaxRooms)
	}

	// Explicit values survive.
	c2, err := ParseConfig([]byte(baseMeetYAML + "\nroom:\n  max_participants: 12\n  max_rooms: 7\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c2.Room.MaxParticipants != 12 || c2.Room.MaxRooms != 7 {
		t.Fatalf("explicit room ceilings not honoured: %+v", c2.Room)
	}
}

func TestConfig_RejectsNegativeRoomCeilings(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	for name, raw := range map[string]string{
		"neg participants": baseMeetYAML + "\nroom:\n  max_participants: -1\n",
		"neg rooms":        baseMeetYAML + "\nroom:\n  max_rooms: -5\n",
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// ── Item 2: single-port UDP mux renders rtc.udp_port (and not the range) ──────

func TestRender_UDPMuxRendersSinglePort(t *testing.T) {
	doc := renderConfig(t, `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
  rtc_udp_port: 50000
admin:
  token: tok
`)
	if !strings.Contains(doc, "udp_port: 50000") {
		t.Fatalf("expected rtc.udp_port: 50000 in mux mode; got:\n%s", doc)
	}
	if strings.Contains(doc, "port_range_start") || strings.Contains(doc, "port_range_end") {
		t.Fatalf("port range must NOT be rendered when udp_port is set; got:\n%s", doc)
	}
}

func TestRender_RangeModeRendersRangeNotMux(t *testing.T) {
	doc := renderConfig(t, baseMeetYAML) // defaults to the range
	if !strings.Contains(doc, "port_range_start: 50000") || !strings.Contains(doc, "port_range_end: 60000") {
		t.Fatalf("expected default port range; got:\n%s", doc)
	}
	if strings.Contains(doc, "udp_port:") {
		t.Fatalf("udp_port must NOT be rendered in range mode; got:\n%s", doc)
	}
}

func TestConfig_RejectsBothUDPShapes(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	raw := `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
  rtc_udp_port: 50000
  rtc_port_range_start: 50000
  rtc_port_range_end: 50200
admin:
  token: tok
`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("expected rejection when both rtc_udp_port and rtc_port_range_* are set")
	}
}

func TestConfig_RejectsBadUDPMuxPort(t *testing.T) {
	_ = os.Unsetenv(AdminTokenEnv)
	raw := `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
  rtc_udp_port: 70000
admin:
  token: tok
`
	if _, err := ParseConfig([]byte(raw)); err == nil {
		t.Fatal("expected rejection of out-of-range rtc_udp_port")
	}
}

// ── Item 3: the livekit child binds loopback, not 0.0.0.0 ─────────────────────

func TestRender_ChildBindsLoopback(t *testing.T) {
	// Even when the operator writes a wildcard signaling addr, the child must be
	// pinned to loopback so only the signal-gate is public.
	doc := renderConfig(t, `
region: za-jhb
livekit:
  api_key: APItestkey
  api_secret: supersecretsigningvalueof32bytes
  signaling_addr: "0.0.0.0:7880"
admin:
  token: tok
`)
	if !strings.Contains(doc, "bind_addresses:") || !strings.Contains(doc, "- 127.0.0.1") {
		t.Fatalf("rendered config must pin bind_addresses to loopback; got:\n%s", doc)
	}
	if strings.Contains(doc, "0.0.0.0") {
		t.Fatalf("rendered config must NOT bind the wildcard; got:\n%s", doc)
	}
	// The port is still the one the operator chose.
	if !strings.Contains(doc, "port: 7880") {
		t.Fatalf("rendered config must keep the configured signaling port; got:\n%s", doc)
	}
}

// itoaTest avoids importing strconv just for the test assertions.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
