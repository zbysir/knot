package client

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Bundle is what the head hands an client: the credential material for
// the relays it may dial, and the list of machines a forward can point at.
//
// It comes from /api/client, an endpoint that exists for exactly this. An
// earlier version of this app read the sing-box config the head generates for
// nodes and picked the relay outbounds out of it -- which worked, but coupled a
// laptop to a format produced for somebody else, and that format carries a
// relay's Reality PRIVATE key when the reader happens to be a relay.
type Bundle struct {
	Name     string       `json:"name"`
	DoorUUID string       `json:"door_uuid"`
	Expires  time.Time    `json:"expires"`
	Relays   []Relay      `json:"relays"`
	Nodes    []BundleNode `json:"nodes"`
}

// Relay is one dialable exit.
type Relay struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	VIP        string `json:"vip"`
	KnotPort   int    `json:"knot_port"`
	Endpoint   string `json:"endpoint"`
	ServerName string `json:"server_name"`
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
}

// BundleNode is a machine a forward can be pointed at -- any mesh node, relay
// or leaf. What a forward names is the machine that will do the dialling.
type BundleNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	VIP     string `json:"vip"`
	IsRelay bool   `json:"is_relay"`
}

func (b Bundle) Relay(name string) (Relay, bool) {
	for _, r := range b.Relays {
		if r.Name == name {
			return r, true
		}
	}
	return Relay{}, false
}

func (b Bundle) Node(name string) (BundleNode, bool) {
	for _, n := range b.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return BundleNode{}, false
}

// Valid reports whether this bundle can actually be used to connect.
func (b Bundle) Valid() error {
	if b.DoorUUID == "" {
		return fmt.Errorf("head 没有下发客户端凭证")
	}
	if len(b.Relays) == 0 {
		return fmt.Errorf("这个网络里没有可拨的中继，客户端无法连接")
	}
	for _, r := range b.Relays {
		if r.Endpoint == "" || r.PublicKey == "" || r.ShortID == "" || r.VIP == "" || r.KnotPort == 0 {
			return fmt.Errorf("中继 %s 的信息不完整", r.Name)
		}
	}
	return nil
}

func ParseBundle(body []byte) (Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(body, &b); err != nil {
		return Bundle{}, fmt.Errorf("head 的回应看不懂: %w", err)
	}
	if err := b.Valid(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// freePort asks the kernel for an unused loopback port and gives it straight
// back. Racy in theory; in practice the window is microseconds and the only
// competitor for a random high port on a workstation is this same function.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func insecureTransport() http.RoundTripper {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
}
