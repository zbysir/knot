package sb

import (
	"encoding/json"
	"testing"

	"github.com/zbysir/knot/internal/model"
)

func relayState() (*model.State, *model.Node) {
	relay := &model.Node{
		ID:             "n1",
		Name:           "hk",
		VIP:            "10.88.0.1",
		IsRelay:        true,
		Endpoint:       "hk.example.com:443",
		RealityPrivate: "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA",
		RealityPublic:  "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0",
		ShortID:        "0123456789abcdef",
		ServerName:     "dl.google.com",
		Fallback:       "127.0.0.1:4444",
		UUID:           "b831381d-6324-4d53-ad4f-8cda48b30811",
	}
	return &model.State{MeshCIDR: "10.88.0.0/24", Nodes: []*model.Node{relay}}, relay
}

// handshakeOf digs the reality handshake object out of a generated config.
func handshakeOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var cfg struct {
		Inbounds []struct {
			Type string `json:"type"`
			TLS  struct {
				Reality struct {
					Handshake map[string]any `json:"handshake"`
				} `json:"reality"`
			} `json:"tls"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	for _, in := range cfg.Inbounds {
		if in.Type == "vless" {
			return in.TLS.Reality.Handshake
		}
	}
	t.Fatal("no vless inbound in generated config")
	return nil
}

// TestFallbackProxyProtocolOmittedWhenOff pins the compatibility behavior: a
// sing-box without the patch rejects an unknown field outright instead of
// ignoring it, so the key must be absent rather than 0 when disabled.
func TestFallbackProxyProtocolOmittedWhenOff(t *testing.T) {
	st, relay := relayState()
	raw, err := Generate(st, relay)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	handshake := handshakeOf(t, raw)
	if _, present := handshake["proxy_protocol"]; present {
		t.Errorf("proxy_protocol must be omitted when off, got %v", handshake["proxy_protocol"])
	}
	if handshake["server"] != "127.0.0.1" {
		t.Errorf("server = %v, want 127.0.0.1", handshake["server"])
	}
}

func TestFallbackProxyProtocolEmitted(t *testing.T) {
	for _, version := range []int{1, 2} {
		st, relay := relayState()
		relay.FallbackProxyProtocol = version
		raw, err := Generate(st, relay)
		if err != nil {
			t.Fatalf("Generate(v%d): %v", version, err)
		}
		got := handshakeOf(t, raw)["proxy_protocol"]
		// JSON numbers decode as float64.
		if got != float64(version) {
			t.Errorf("proxy_protocol = %v (%T), want %d", got, got, version)
		}
	}
}

func TestFallbackProxyProtocolRejectsBadVersion(t *testing.T) {
	for _, version := range []int{-1, 3} {
		st, relay := relayState()
		relay.FallbackProxyProtocol = version
		if _, err := Generate(st, relay); err == nil {
			t.Errorf("Generate accepted fallback_proxy_protocol=%d, want error", version)
		}
	}
}
