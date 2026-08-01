package node

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/zbysir/knot/internal/relay"
)

// Plan is the relay half of the config the head hands out.
type Plan struct {
	SelfID  string     `json:"self_id"`
	Key     string     `json:"key"`
	IsRelay bool       `json:"is_relay"`
	Listen  string     `json:"listen"`  // relay only, its own mesh addr e.g. "10.88.0.1:9997"
	Socks   string     `json:"socks"`   // local SOCKS5 sing-box forwards peer traffic to
	Uplinks []string   `json:"uplinks"` // relay addresses to stay homed on
	Peers   []PlanPeer `json:"peers"`
	// PeerKeys maps node ID -> sha256(key). Relays only.
	PeerKeys map[string]string `json:"peer_keys,omitempty"`
}

// PlanPeer says how to reach one peer that cannot be dialled directly.
type PlanPeer struct {
	NodeID string   `json:"node_id"`
	VIP    string   `json:"vip"`
	Relays []string `json:"relays"` // ordered: relay node IDs to try, and their mesh addresses
	Addrs  []string `json:"addrs"`  // parallel to Relays: "<relayVIP>:<relayListenPort>"
}

// relayd owns everything on the relay data path for one node.
type relayd struct {
	plan   Plan
	client *relay.Client
	reg    *relay.Registry
	socks  net.Listener
	cancel context.CancelFunc

	mu  sync.Mutex
	srv net.Listener // rebound by listenAndServe, so guarded
}

func logf(f string, v ...any) { fmt.Fprintf(os.Stderr, "knot: "+f+"\n", v...) }

// start brings the relay machinery up for a new plan. It is safe to call
// repeatedly; the previous instance is torn down first.
func (a *Agent) startRelay(p Plan) error {
	a.stopRelay()
	if p.SelfID == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &relayd{plan: p, cancel: cancel}

	// A relay accepts sessions from leaves. The listener is plain TCP: the
	// Reality wrapping already happened in sing-box before we see the bytes.
	if p.IsRelay && p.Listen != "" {
		r.reg = relay.NewRegistry()
		keys := p.PeerKeys
		srv := &relay.Server{
			Reg: r.reg,
			// The head issues each node's key and tells the relay the hashes.
			// Accepting any non-empty key would let anyone who reaches this
			// port register as any node.
			Auth: func(nodeID, key string) bool {
				want, ok := keys[nodeID]
				return ok && want != "" && hashKey(key) == want
			},
			Logf: logf,
		}
		go r.listenAndServe(ctx, p.Listen, srv)
	}

	// Every node -- relay or leaf -- keeps sessions to the relays it uses, so
	// that traffic can be pushed back down to it.
	r.client = &relay.Client{
		NodeID: p.SelfID,
		Key:    p.Key,
		Logf:   logf,
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			// addr is the relay's mesh address; sing-box turns this into a
			// Reality connection on the way out.
			d := net.Dialer{Timeout: 15 * time.Second}
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	for _, addr := range p.Uplinks {
		go r.client.Maintain(ctx, addr)
	}

	if p.Socks != "" {
		ln, err := net.Listen("tcp", p.Socks)
		if err != nil {
			cancel()
			return fmt.Errorf("socks listen: %w", err)
		}
		r.socks = ln
		go r.serveSocks(ln)
		logf("relay: socks on %s", p.Socks)
	}
	a.relay = r
	return nil
}

// hashKey mirrors the head's hashKey. The two must stay in step; the salt is
// duplicated rather than shared to keep the node from importing the head.
func hashKey(k string) string {
	sum := sha256.Sum256([]byte("knot-node\x00" + k))
	return hex.EncodeToString(sum[:])
}

// listenAndServe binds the relay port and keeps it bound.
//
// The address is the node's own mesh IP, which sing-box owns -- so at startup
// it does not exist yet (we run before sing-box) and it disappears whenever
// sing-box restarts. Retrying instead of failing once is what makes the relay
// survive both.
func (r *relayd) listenAndServe(ctx context.Context, addr string, srv *relay.Server) {
	for ctx.Err() == nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		r.mu.Lock()
		r.srv = ln
		r.mu.Unlock()
		logf("relay: serving on %s", addr)
		srv.Serve(ln) // returns when the listener closes
		if ctx.Err() != nil {
			return
		}
		logf("relay: listener on %s went away, rebinding", addr)
	}
}

func (a *Agent) stopRelay() {
	if a.relay == nil {
		return
	}
	a.relay.cancel()
	if a.relay.socks != nil {
		a.relay.socks.Close()
	}
	a.relay.mu.Lock()
	if a.relay.srv != nil {
		a.relay.srv.Close()
	}
	a.relay.mu.Unlock()
	a.relay = nil
}

// serveSocks accepts the connections sing-box hands us for peer VIPs and
// forwards each through the first healthy relay for that peer.
func (r *relayd) serveSocks(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go r.handleSocks(c)
	}
}

func (r *relayd) handleSocks(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	host, port, err := socksRequest(c)
	if err != nil {
		return
	}

	// Everything below runs BEFORE the SOCKS reply. Answering "succeeded" and
	// only then discovering we cannot reach the peer turns every routing
	// mistake into "the TCP connect worked but no bytes ever came" -- which is
	// exactly the symptom that sent us hunting in the wrong place before. It
	// also makes `nc -z` lie: a probe that only checks the handshake reports
	// the peer as up when nothing can actually get through.
	conn, err := r.dialPeer(host, port)
	if err != nil {
		logf("relay: %s:%d unreachable: %v", host, port, err)
		socksReply(c, socksHostUnreachable)
		return
	}
	defer conn.Close()
	if err := socksReply(c, socksOK); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	spliceConn(c, conn)
}

// dialPeer resolves a mesh address to a live relay stream.
func (r *relayd) dialPeer(host string, port int) (net.Conn, error) {
	peer := r.peerByVIP(host)
	if peer == nil {
		return nil, fmt.Errorf("no relay path configured for %s", host)
	}
	dst := net.JoinHostPort(host, strconv.Itoa(port))
	var lastErr error

	// If we are the relay this peer is homed on, use that session directly --
	// there is no reason to bounce our own traffic off another relay.
	if r.reg != nil {
		conn, err := r.reg.Dial(peer.NodeID, dst)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}

	// Otherwise try the relays in order. Skipping ones with no live session
	// means a dead primary costs nothing instead of a dial timeout per
	// connection.
	for i, addr := range peer.Addrs {
		if !r.client.Up(addr) {
			lastErr = fmt.Errorf("relay %d (%s) not connected", i, addr)
			continue
		}
		conn, err := r.client.Open(addr, peer.NodeID, dst)
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable relay")
	}
	return nil, lastErr
}

func (r *relayd) peerByVIP(vip string) *PlanPeer {
	for i := range r.plan.Peers {
		if r.plan.Peers[i].VIP == vip {
			return &r.plan.Peers[i]
		}
	}
	return nil
}

// SOCKS5 reply codes we actually use.
const (
	socksOK              = 0
	socksHostUnreachable = 4
	socksCmdUnsupported  = 7
)

// socksReply sends the CONNECT reply. The bound address is not meaningful
// here and sing-box ignores it.
func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

// socksRequest implements just enough SOCKS5 to learn the destination.
// sing-box is the only client, so no auth methods and no BIND/UDP.
//
// It deliberately stops short of replying: the caller answers only once it
// knows whether the destination is actually reachable.
func socksRequest(c net.Conn) (string, int, error) {
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return "", 0, err
	}
	if buf[0] != 5 {
		return "", 0, fmt.Errorf("not socks5")
	}
	n := int(buf[1])
	if _, err := io.ReadFull(c, buf[:n]); err != nil {
		return "", 0, err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil { // no auth
		return "", 0, err
	}
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return "", 0, err
	}
	if buf[1] != 1 { // CONNECT only
		socksReply(c, socksCmdUnsupported)
		return "", 0, fmt.Errorf("unsupported command %d", buf[1])
	}
	var host string
	switch buf[3] {
	case 1: // IPv4
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return "", 0, err
		}
		host = net.IP(buf[:4]).String()
	case 3: // domain
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return "", 0, err
		}
		l := int(buf[0])
		if _, err := io.ReadFull(c, buf[:l]); err != nil {
			return "", 0, err
		}
		host = string(buf[:l])
	case 4: // IPv6
		if _, err := io.ReadFull(c, buf[:16]); err != nil {
			return "", 0, err
		}
		host = net.IP(buf[:16]).String()
	default:
		return "", 0, fmt.Errorf("bad address type %d", buf[3])
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return "", 0, err
	}
	port := int(binary.BigEndian.Uint16(buf[:2]))
	return host, port, nil
}

func spliceConn(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}
