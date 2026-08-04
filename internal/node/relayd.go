package node

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
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
	// Names maps node ID -> name AND relay address -> name, for the logs. A plan
	// cached by an older node has none, and every lookup then falls back to the
	// key it was given.
	Names map[string]string `json:"names,omitempty"`
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
	client *relay.Client
	reg    *relay.Registry
	socks  net.Listener
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	plan    Plan                          // replaced wholesale by setPlan
	srv     net.Listener                  // rebound by listenAndServe, so guarded
	uplinks map[string]context.CancelFunc // relay addr -> stop its Maintain loop
}

// applyPlan brings the relay machinery in line with a new plan, rebuilding only
// what the change actually requires.
//
// Peers and PeerKeys both describe the whole mesh, so they change whenever ANY
// node joins or leaves. Rebuilding for that meant closing the relay port and
// cancelling every session, so adding one leaf disconnected all the others and
// made each reconnect on a 1s/2s/4s backoff. Only this node's own identity or
// its two listen addresses genuinely need a rebuild.
func (a *Agent) applyPlan(p Plan) error {
	if p.SelfID == "" {
		a.stopRelay()
		return nil
	}
	r := a.relay
	if r == nil || r.needsRebuild(p) {
		return a.startRelay(p)
	}
	r.setPlan(p)
	r.syncUplinks(p.Uplinks)
	return nil
}

func (r *relayd) needsRebuild(p Plan) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.plan
	return cur.SelfID != p.SelfID || cur.Key != p.Key ||
		cur.IsRelay != p.IsRelay || cur.Listen != p.Listen || cur.Socks != p.Socks
}

// startRelay brings the relay machinery up from scratch. It is safe to call
// repeatedly; the previous instance is torn down first.
func (a *Agent) startRelay(p Plan) error {
	a.stopRelay()
	if p.SelfID == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &relayd{
		plan:    p,
		ctx:     ctx,
		cancel:  cancel,
		uplinks: map[string]context.CancelFunc{},
	}

	// A relay accepts sessions from leaves. The listener is plain TCP: the
	// Reality wrapping already happened in sing-box before we see the bytes.
	if p.IsRelay && p.Listen != "" {
		r.reg = relay.NewRegistry()
		r.reg.NameOf = r.nameOf
		srv := &relay.Server{
			Reg:    r.reg,
			NameOf: r.nameOf,
			// The head issues each node's key and tells the relay the hashes.
			// Accepting any non-empty key would let anyone who reaches this
			// port register as any node. Read through peerKey rather than
			// capturing the map: a new plan replaces it while sessions are
			// being accepted on this very callback.
			Auth: func(nodeID, key string) bool {
				want := r.peerKey(nodeID)
				return want != "" && hashKey(key) == want
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
		NameOf: r.nameOf,
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			// addr is the relay's mesh address; sing-box turns this into a
			// Reality connection on the way out.
			d := net.Dialer{Timeout: 15 * time.Second}
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	r.syncUplinks(p.Uplinks)

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

// nameByVIP names a mesh address for the logs.
func (r *relayd) nameByVIP(vip string) string {
	if p := r.peerByVIP(vip); p != nil {
		return r.nameOf(p.NodeID)
	}
	return vip
}

// nameOf turns a node ID or a relay address into the name the panel shows,
// falling back to the key itself so callers can use the result unconditionally.
func (r *relayd) nameOf(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.plan.Names[key]; n != "" {
		return n
	}
	return key
}

func (r *relayd) setPlan(p Plan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plan = p
}

// peerKey returns the expected key hash for a node, or "" if it is not allowed
// on this relay.
func (r *relayd) peerKey(nodeID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plan.PeerKeys[nodeID]
}

// syncUplinks starts a session to each relay that is new in the plan and stops
// the ones that have gone, leaving the sessions that are still wanted alone.
func (r *relayd) syncUplinks(addrs []string) {
	want := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		want[addr] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for addr, stop := range r.uplinks {
		if want[addr] {
			continue
		}
		stop()
		// Cancelling is not enough to end the session: the context only governs
		// the dial, while Maintain is parked in AcceptStream on a live
		// connection. Close it so the goroutine actually returns.
		r.client.Close(addr)
		delete(r.uplinks, addr)
		logf("relay: uplink %s dropped (no longer in plan)", addr)
	}
	for addr := range want {
		if _, ok := r.uplinks[addr]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(r.ctx)
		r.uplinks[addr] = cancel
		go r.client.Maintain(ctx, addr)
	}
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
	r := a.relay
	a.relay = nil
	r.cancel()
	if r.socks != nil {
		r.socks.Close()
	}
	r.mu.Lock()
	if r.srv != nil {
		r.srv.Close()
	}
	for addr := range r.uplinks {
		if r.client != nil {
			r.client.Close(addr) // see syncUplinks: cancelling alone leaves it parked
		}
	}
	r.uplinks = nil
	r.mu.Unlock()
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
		logf("relay: %s (%s:%d) unreachable: %v", r.nameByVIP(host), host, port, err)
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

// peerByVIP returns a copy: the plan it comes from is replaced under us on every
// sync, and the caller goes on to use this across a dial.
func (r *relayd) peerByVIP(vip string) *PlanPeer {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.plan.Peers {
		if p.VIP == vip {
			return &p
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
