// Package sb turns the head's node/route model into a sing-box config.
//
// The data plane is a proxy mesh, not a real L3 VPN: a TUN device owns the
// mesh CIDR so the kernel hands us every packet addressed to a peer, and each
// TCP/UDP stream is carried inside VLESS+Reality. That means ICMP between
// nodes does NOT work -- VLESS carries streams, not raw IP. Verify links with
// nc/curl, not ping.
package sb

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zbysir/knot/internal/model"
)

// Generate builds the sing-box config for one node.
func Generate(st *model.State, self *model.Node) ([]byte, error) {
	if self.VIP == "" {
		return nil, fmt.Errorf("node %s has no VIP", self.Name)
	}
	// No dns block: peer names resolve through /etc/hosts, which the node
	// agent maintains from Hosts(). That keeps us off sing-box's DNS schema,
	// which changes between releases, and means name resolution still works
	// even if sing-box is down.
	cfg := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": true},
	}

	inbounds := []any{tunInbound(st, self)}
	if self.IsRelay {
		in, err := realityInbound(st, self)
		if err != nil {
			return nil, err
		}
		inbounds = append(inbounds, in)
	}
	cfg["inbounds"] = inbounds

	outbounds, rules, err := routing(st, self)
	if err != nil {
		return nil, err
	}
	cfg["outbounds"] = outbounds
	cfg["route"] = map[string]any{
		"rules": rules,
		// Anything we did not explicitly match leaves the box normally.
		// Without this a misconfigured rule would black-hole the node.
		"final":                 "direct",
		"auto_detect_interface": true,
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// tunInbound gives the node its mesh address. The /24 (or whatever MeshCIDR
// says) is what makes the kernel route peer traffic into us -- we deliberately
// do NOT set auto_route, because hijacking the default route would break the
// host's normal networking.
func tunInbound(st *model.State, self *model.Node) map[string]any {
	prefix := maskOf(st.MeshCIDR)
	return map[string]any{
		"type":           "tun",
		"tag":            "tun-in",
		"interface_name": "knot0",
		"address":        []string{self.VIP + "/" + prefix},
		"auto_route":     false,
		"stack":          "gvisor",
		"mtu":            1420,
	}
}

// realityInbound terminates Reality on a relay.
//
// handshake points at a REAL site (Fallback). Anything that is not an
// authenticated tunnel connection gets transparently forwarded there, so
// active probing of this port sees a genuine website with a genuine
// certificate.
//
// The dest MUST have a small certificate chain. Reality relays the dest's
// handshake through a fixed buffer, and a large chain overruns it: with
// www.microsoft.com (8273-byte cert) every connection dies AFTER successful
// Reality auth, reported as the same useless
// "REALITY: processed invalid connection". dl.google.com (2732 bytes) works.
// Check with:
//
//	openssl s_client -connect HOST:443 -servername HOST -tls1_3 </dev/null |
//	  awk '/BEGIN CERT/,/END CERT/' | wc -c
func realityInbound(st *model.State, n *model.Node) (map[string]any, error) {
	if n.RealityPrivate == "" || n.ShortID == "" {
		return nil, fmt.Errorf("relay %s missing reality material", n.Name)
	}
	host, port, err := splitHostPort(n.Fallback)
	if err != nil {
		return nil, fmt.Errorf("relay %s fallback: %w", n.Name, err)
	}
	_, listenPort, err := splitHostPort(n.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("relay %s endpoint: %w", n.Name, err)
	}
	users := make([]any, 0, len(st.Nodes))
	for _, p := range st.Nodes {
		users = append(users, map[string]any{
			"name": p.Name,
			"uuid": p.UUID,
			"flow": "xtls-rprx-vision",
		})
	}
	return map[string]any{
		"type":        "vless",
		"tag":         "reality-in",
		"listen":      "::",
		"listen_port": listenPort,
		// Every node dials with its OWN uuid, so the relay has to accept all of
		// them. Listing only the relay's own uuid here fails the handshake for
		// everyone else -- and Reality reports that as a generic
		// "processed invalid connection", which points nowhere near the cause.
		"users": users,
		"tls": map[string]any{
			"enabled":     true,
			"server_name": n.ServerName,
			"reality": map[string]any{
				"enabled": true,
				"handshake": map[string]any{
					"server":      host,
					"server_port": port,
				},
				"private_key": n.RealityPrivate,
				"short_id":    []string{n.ShortID},
			},
		},
	}, nil
}

// realityOutbound dials a relay.
func realityOutbound(tag string, peer *model.Node, selfUUID string) (map[string]any, error) {
	host, port, err := splitHostPort(peer.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("peer %s endpoint: %w", peer.Name, err)
	}
	return map[string]any{
		"type":        "vless",
		"tag":         tag,
		"server":      host,
		"server_port": port,
		"uuid":        selfUUID,
		"flow":        "xtls-rprx-vision",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": peer.ServerName,
			"utls": map[string]any{
				// Impersonate Chrome's TLS fingerprint. Without this the Go
				// runtime's own ClientHello is trivially fingerprintable
				// (JA3/JA4), which defeats the point of Reality.
				"enabled":     true,
				"fingerprint": "chrome",
			},
			"reality": map[string]any{
				"enabled":    true,
				"public_key": peer.RealityPublic,
				"short_id":   peer.ShortID,
			},
		},
	}, nil
}

// routing builds one outbound per reachable peer plus the route rules that
// steer mesh traffic into them.
//
// For each destination we resolve an ordered path list. A single path becomes
// a plain outbound; several become a urltest group, which probes its members
// and prefers the first healthy one -- that is the primary/backup behaviour.
func routing(st *model.State, self *model.Node) ([]any, []any, error) {
	outbounds := []any{
		map[string]any{"type": "direct", "tag": "direct"},
		map[string]any{"type": "block", "tag": "block"},
		// Peers we cannot dial (leaves have no endpoint) go to knot's local
		// SOCKS listener, which carries them over a relay session. sing-box
		// only does transport; the relay semantics live in knot.
		map[string]any{
			"type": "socks", "tag": "knot-relay",
			"server": "127.0.0.1", "server_port": SocksPort, "version": "5",
		},
	}
	var rules []any

	// Deduplicate the physical dials: several destinations may share one relay.
	dialed := map[string]bool{}
	addDial := func(peer *model.Node) (string, error) {
		tag := "dial-" + peer.Name
		if dialed[tag] {
			return tag, nil
		}
		ob, err := realityOutbound(tag, peer, self.UUID)
		if err != nil {
			return "", err
		}
		outbounds = append(outbounds, ob)
		dialed[tag] = true
		return tag, nil
	}

	for _, dst := range st.Nodes {
		if dst.ID == self.ID {
			continue
		}
		paths := st.RouteFor(self.ID, dst.ID)
		var members []string
		for _, p := range paths {
			var hop *model.Node
			switch p.Kind {
			case "direct":
				hop = dst
			case "relay":
				hop = st.NodeByID(p.RelayID)
			}
			if hop == nil || !hop.IsRelay || hop.Endpoint == "" || hop.ID == self.ID {
				continue // unusable path, silently skipped
			}
			tag, err := addDial(hop)
			if err != nil {
				return nil, nil, err
			}
			members = append(members, tag)
		}
		if len(members) == 0 {
			// Not directly dialable. If it is a leaf we still reach it through
			// a relay session, so hand it to knot rather than dropping it.
			if !dst.IsRelay || dst.Endpoint == "" {
				rules = append(rules,
					map[string]any{
						"ip_cidr":  []string{dst.VIP + "/32"},
						"network":  "tcp",
						"outbound": "knot-relay",
					},
					// The relay session carries streams, not datagrams, so UDP
					// to a relayed peer cannot work. Drop it here rather than
					// handing sing-box a SOCKS outbound that answers every
					// UDP-ASSOCIATE with "rejected, code=7" -- that fills the
					// log with errors for traffic we were never going to carry.
					// In practice the source is another overlay (tailscaled
					// advertises knot0's address as a WireGuard endpoint and
					// probes it every 5s); it fails over on its own.
					map[string]any{
						"ip_cidr":  []string{dst.VIP + "/32"},
						"network":  "udp",
						"outbound": "block",
					},
				)
			}
			continue
		}

		target := members[0]
		if len(members) > 1 {
			target = "to-" + dst.Name
			outbounds = append(outbounds, map[string]any{
				"type":      "urltest",
				"tag":       target,
				"outbounds": members,
				"url":       "https://www.gstatic.com/generate_204",
				"interval":  "3m",
				// Bias toward the earlier (higher priority) member: a later
				// one has to be meaningfully faster before we switch.
				"tolerance": 200,
			})
		}
		// No inbound filter on purpose. On a relay these same rules have to
		// match traffic arriving from reality-in as well, otherwise forwarded
		// packets fall through to "final": "direct" and the relay tries to
		// dial the peer's mesh address on itself, where nothing listens. The
		// symptom is a TCP connect that succeeds and then returns no data.
		rules = append(rules, map[string]any{
			"ip_cidr":  []string{dst.VIP + "/32"},
			"outbound": target,
		})
	}
	return outbounds, rules, nil
}

// Hosts returns the /etc/hosts lines the node agent should maintain.
func Hosts(st *model.State) string {
	var sb strings.Builder
	for _, n := range st.Nodes {
		fmt.Fprintf(&sb, "%s %s.knot %s\n", n.VIP, n.Name, n.Name)
	}
	return sb.String()
}

// Ports knot itself owns on every node.
const (
	SocksPort = 9998 // sing-box -> knot, for peers reachable only via a relay
	RelayPort = 9997 // relays only: where leaf sessions land
)

func splitHostPort(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("%q is not host:port", s)
	}
	host := s[:i]
	var port int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil || port <= 0 {
		return "", 0, fmt.Errorf("%q has no valid port", s)
	}
	return host, port, nil
}

func maskOf(cidr string) string {
	if i := strings.Index(cidr, "/"); i >= 0 {
		return cidr[i+1:]
	}
	return "24"
}
