package client

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// GenerateConfig builds the client's sing-box config.
//
// It is the smallest config that can carry anything: for each relay, one
// loopback listener whose destination is fixed to that relay's knot port, and
// one Reality outbound to reach it. No tun, so this needs no root, creates no
// utun device, touches no routing table and cannot collide with a corporate VPN
// or Tailscale -- the three reasons a mesh agent is the wrong shape for a
// workstation.
//
// The listener is a `direct` inbound with a destination override, which is
// sing-box's own ssh -L. Everything the operator actually configures -- which
// local port reaches which service on which machine -- is knot's, not this
// file's, so adding or removing a forward never rewrites this and never
// restarts sing-box.
//
// The listener needs no password even though it is unauthenticated, because it
// leads exactly one place: the relay's knot port, which answers with a
// credential check. A stray process on the laptop that finds the port gets to
// say hello to a relay and be hung up on.
func GenerateConfig(b Bundle, ports map[string]int) ([]byte, error) {
	if err := b.Valid(); err != nil {
		return nil, err
	}
	var inbounds, outbounds, rules []any
	for _, r := range b.Relays {
		port, ok := ports[r.Name]
		if !ok {
			continue
		}
		host, rport, err := splitHostPort(r.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("中继 %s 的端点 %q: %w", r.Name, r.Endpoint, err)
		}
		in, out := "in-"+r.Name, "dial-"+r.Name
		inbounds = append(inbounds, map[string]any{
			"type": "direct", "tag": in,
			"listen": "127.0.0.1", "listen_port": port,
			"network": "tcp",
			// Where this listener goes, decided here and unchangeable from the
			// laptop side: knot's port on that relay.
			"override_address": r.VIP,
			"override_port":    r.KnotPort,
		})
		outbounds = append(outbounds, map[string]any{
			"type": "vless", "tag": out,
			"server": host, "server_port": rport,
			// The shared door. On its own it reaches nothing but the address
			// overridden above -- the relay's config rejects everything else
			// this user asks for.
			"uuid": b.DoorUUID, "flow": "xtls-rprx-vision",
			"tls": map[string]any{
				"enabled":     true,
				"server_name": r.ServerName,
				// Chrome's TLS fingerprint. The Go runtime's own ClientHello is
				// trivially fingerprintable, which would defeat Reality.
				"utls": map[string]any{"enabled": true, "fingerprint": "chrome"},
				"reality": map[string]any{
					"enabled":    true,
					"public_key": r.PublicKey,
					"short_id":   r.ShortID,
				},
			},
		})
		rules = append(rules, map[string]any{"inbound": []string{in}, "outbound": out})
	}
	if len(inbounds) == 0 {
		return nil, fmt.Errorf("没有分配到本地端口")
	}
	cfg := map[string]any{
		"log":       map[string]any{"level": "warn", "timestamp": true},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route": map[string]any{
			"rules": rules,
			// Every inbound above has its own rule, so this is unreachable.
			// It points at a relay rather than "direct" anyway: if a rule is
			// ever wrong, traffic meant for production should fail or go
			// through the tunnel, never leak onto the local network.
			"final": "dial-" + b.Relays[0].Name,
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func splitHostPort(s string) (string, int, error) {
	i := lastIndexByte(s, ':')
	if i < 0 {
		return "", 0, fmt.Errorf("不是 host:port")
	}
	var port int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil || port <= 0 {
		return "", 0, fmt.Errorf("端口不对")
	}
	return s[:i], port, nil
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// singBoxNames is where a Homebrew or hand-installed sing-box ends up.
var singBoxNames = []string{
	"/opt/homebrew/bin/sing-box",
	"/usr/local/bin/sing-box",
}

// FindSingBox locates the binary, preferring an explicit override, then one
// shipped beside us (an .app bundle can carry its own), then the usual install
// locations.
func FindSingBox(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("找不到 sing-box: %s", override)
		}
		return override, nil
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "sing-box"); fileOK(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("sing-box"); err == nil {
		return p, nil
	}
	for _, p := range singBoxNames {
		if fileOK(p) {
			return p, nil
		}
	}
	hint := "没有找到 sing-box。装一个：brew install sing-box"
	if runtime.GOOS != "darwin" {
		hint = "没有找到 sing-box，请先安装并放进 PATH"
	}
	return "", fmt.Errorf("%s", hint)
}

func fileOK(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
