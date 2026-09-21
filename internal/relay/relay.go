package relay

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// ---------------------------------------------------------------- registry

// Registry maps node IDs to their live sessions. Only relays keep one.
type Registry struct {
	// NameOf, when set, turns a node ID into the name the panel shows. Optional
	// and log/error decoration only.
	NameOf func(string) string

	mu sync.RWMutex
	m  map[string]*yamux.Session
}

func (r *Registry) name(id string) string { return decorate(r.NameOf, id) }

// decorate is the shared fallback: without a NameOf, or for an ID nobody has a
// name for, the key itself is still the best thing to print.
func decorate(f func(string) string, id string) string {
	if f != nil {
		if n := f(id); n != "" {
			return n
		}
	}
	return id
}

func NewRegistry() *Registry { return &Registry{m: map[string]*yamux.Session{}} }

func (r *Registry) put(id string, s *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A reconnecting node replaces its old session. Closing the stale one
	// frees its streams instead of leaving them to time out.
	if old, ok := r.m[id]; ok && old != s {
		old.Close()
	}
	r.m[id] = s
}

func (r *Registry) drop(id string, s *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[id]; ok && cur == s {
		delete(r.m, id)
	}
}

func (r *Registry) get(id string) *yamux.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[id]
}

// Dial opens a connection to dstAddr through the session nodeID holds with
// this relay. It is the local twin of Client.Open, for when the caller IS the
// relay and does not need to bounce off anyone.
func (r *Registry) Dial(nodeID, dstAddr string) (net.Conn, error) {
	sess := r.get(nodeID)
	if sess == nil {
		return nil, fmt.Errorf("relay: node %s not connected here", r.name(nodeID))
	}
	st, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := WriteOpen(st, Open{DstNode: nodeID, DstAddr: dstAddr}); err != nil {
		st.Close()
		return nil, err
	}
	if err := ReadResult(st); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// CloseUnlisted ends every session whose ID is not in allowed, and returns the
// IDs it closed.
//
// This is what makes a revocation take effect now rather than whenever the far
// side happens to reconnect: Auth only runs once, at Hello, so dropping a key
// from the plan does nothing at all to a session that is already open.
func (r *Registry) CloseUnlisted(allowed map[string]string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var closed []string
	for id, s := range r.m {
		if _, ok := allowed[id]; ok {
			continue
		}
		s.Close()
		delete(r.m, id)
		closed = append(closed, id)
	}
	return closed
}

// Online lists the node IDs with a live session, for the panel.
func (r *Registry) Online() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for id := range r.m {
		out = append(out, id)
	}
	return out
}

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// Detect a dead peer reasonably fast; the default 30s leaves half-open
	// sessions around long enough to matter on a flaky cross-border link.
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 20 * time.Second
	c.LogOutput = io.Discard
	return c
}

// ------------------------------------------------------------------ server

// Server runs on a relay. It accepts sessions from leaves and splices streams
// between them.
type Server struct {
	Reg *Registry
	// SelfID is this relay's node ID. A stream addressed to it is one we dial
	// ourselves instead of forwarding -- that is how an operator client reaches
	// anything this machine can reach, such as a database on a private address
	// that is not a mesh member at all.
	SelfID string
	// Clients holds the sessions of read-only clients.
	//
	// A SEPARATE registry, never consulted when routing a stream. That is the
	// enforcement of "nothing can reach my laptop": it is not a promise the
	// client makes about itself, it is that the relay has nowhere to look up a
	// client to push a stream to. The registry exists only so a revoked
	// credential's session can be closed, and so the logs can say who is on.
	Clients *Registry
	// Auth verifies a Hello from a mesh node. Returning false drops the session.
	Auth func(nodeID, key string) bool
	// AuthClient verifies a Hello from a read-only client. Checked only after
	// Auth has declined, so a node is never mistaken for a client.
	AuthClient func(nodeID, key string) bool
	// Logf is optional.
	Logf func(format string, v ...any)
	// NameOf, when set, turns a node ID into the name the panel shows. The wire
	// protocol only ever carries IDs, so without this every log line about a
	// peer is a hex string nobody can place.
	NameOf func(string) string
}

func (s *Server) name(id string) string { return decorate(s.NameOf, id) }

func (s *Server) logf(f string, v ...any) {
	if s.Logf != nil {
		s.Logf(f, v...)
	}
}

// Serve accepts sessions on ln until the listener closes.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(c)
	}
}

func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	h, err := ReadHello(c)
	if err != nil {
		s.logf("relay: hello from %s failed: %v", c.RemoteAddr(), err)
		return
	}
	c.SetReadDeadline(time.Time{})
	isClient := false
	switch {
	case s.Auth != nil && s.Auth(h.NodeID, h.Key):
	case s.AuthClient != nil && s.AuthClient(h.NodeID, h.Key):
		isClient = true
	default:
		s.logf("relay: rejected %s", s.name(h.NodeID))
		return
	}

	sess, err := yamux.Server(c, muxConfig())
	if err != nil {
		s.logf("relay: mux for %s: %v", s.name(h.NodeID), err)
		return
	}
	defer sess.Close()

	kind := "node"
	reg := s.Reg
	if isClient {
		kind = "client"
		reg = s.Clients
	}
	if reg != nil {
		reg.put(h.NodeID, sess)
		defer reg.drop(h.NodeID, sess)
	}
	s.logf("relay: %s %s online", kind, s.name(h.NodeID))
	defer s.logf("relay: %s %s offline", kind, s.name(h.NodeID))

	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go s.handleStream(st)
	}
}

// localDialTimeout bounds a dial this relay makes on somebody else's behalf.
const localDialTimeout = 10 * time.Second

// handleStream forwards one stream toward its destination node.
func (s *Server) handleStream(st *yamux.Stream) {
	defer st.Close()
	st.SetReadDeadline(time.Now().Add(15 * time.Second))
	o, err := ReadOpen(st)
	if err != nil {
		return
	}
	st.SetReadDeadline(time.Time{})

	// Addressed to us: dial it here. A leaf reaches arbitrary addresses through
	// sing-box already ("final": "direct" on the relay), so this grants nothing
	// new -- but a client has no sing-box path of its own, and this is how it
	// reaches an address that is not a mesh member: a managed database, a host
	// in the relay's VPC, a service on the relay's own loopback.
	if o.DstNode == s.SelfID && s.SelfID != "" {
		s.dialLocal(st, o)
		return
	}

	peer := s.Reg.get(o.DstNode)
	if peer == nil {
		// The destination has no session here. Say so rather than hanging:
		// the caller can then try its next path instead of waiting out a
		// timeout.
		WriteResult(st, fmt.Errorf("node %s not connected to this relay", s.name(o.DstNode)))
		return
	}
	out, err := peer.OpenStream()
	if err != nil {
		WriteResult(st, fmt.Errorf("open stream to %s: %v", s.name(o.DstNode), err))
		return
	}
	defer out.Close()

	// Pass the request on unchanged and let the far end answer. The relay
	// never interprets the payload.
	if err := WriteOpen(out, o); err != nil {
		WriteResult(st, err)
		return
	}
	if err := ReadResult(out); err != nil {
		WriteResult(st, err)
		return
	}
	if err := WriteResult(st, nil); err != nil {
		return
	}
	splice(st, out)
}

// dialLocal connects from this machine and splices.
func (s *Server) dialLocal(st *yamux.Stream, o Open) {
	d := net.Dialer{Timeout: localDialTimeout}
	out, err := d.Dial("tcp", o.DstAddr)
	if err != nil {
		WriteResult(st, fmt.Errorf("dial %s: %v", o.DstAddr, err))
		return
	}
	defer out.Close()
	if err := WriteResult(st, nil); err != nil {
		return
	}
	splice(st, out)
}

// ------------------------------------------------------------------ client

// Client runs on every node. It keeps a session to each relay and serves
// streams the relay pushes back down.
type Client struct {
	NodeID string
	Key    string
	// Dial opens a raw connection to the named relay. On a leaf this goes
	// through sing-box, so it is just a TCP dial to the relay's mesh address.
	Dial func(ctx context.Context, relayID string) (net.Conn, error)
	Logf func(format string, v ...any)
	// NameOf, when set, names a relay address for the logs.
	NameOf func(string) string
	// Backoff is the first retry delay; it doubles from there. Zero means one
	// second, which is right for a relay across the internet and far too slow
	// for one reached over loopback -- a client dials the port its own sing-box
	// listens on, and that either answers or is not up yet.
	Backoff time.Duration
	// NoInbound refuses streams the relay pushes down, for an operator client
	// that must not be reachable.
	//
	// Belt to the relay's braces: a relay does not register clients and so has
	// no way to address one, but a client should not be one server-side bug
	// away from becoming a listener on somebody's laptop.
	NoInbound bool

	mu   sync.RWMutex
	sess map[string]*yamux.Session // relayID -> session
}

func (c *Client) name(id string) string { return decorate(c.NameOf, id) }

func (c *Client) logf(f string, v ...any) {
	if c.Logf != nil {
		c.Logf(f, v...)
	}
}

// acceptedAfter is how long a session has to stay open before we believe it was
// accepted. The protocol has no reply to a Hello -- a relay answers a failed Auth
// by closing, and a successful one by saying nothing at all -- so surviving is the
// only confirmation there is.
const acceptedAfter = 3 * time.Second

// waitingRetryCap bounds the retry wait while a relay has not accepted us yet.
// That state resolves on the relay's own poll of the head, so the usual doubling
// only delays the recovery it is waiting for.
const waitingRetryCap = 5 * time.Second

// noteRepeatAfter re-states a situation that has not changed, so a log opened
// later still says what is going on.
const noteRepeatAfter = 5 * time.Minute

// Maintain keeps a session to relayID alive until ctx is done.
//
// It logs states, not events. A leaf that has just joined is refused by the relay
// until the relay next polls the head, and reporting each of those refusals --
// "session up", "ended: EOF (retry in 1s)", again, and again -- described a broken
// node when nothing was wrong. Now that wait says it is a wait, once.
func (c *Client) Maintain(ctx context.Context, relayID string) {
	name := func() string { return c.name(relayID) }
	backoff := c.firstBackoff()

	// note prints only when the situation changes, or when it has been saying the
	// same thing for a while.
	var last string
	var lastAt time.Time
	note := func(format string, v ...any) {
		msg := fmt.Sprintf(format, v...)
		if msg == last && time.Since(lastAt) < noteRepeatAfter {
			return
		}
		last, lastAt = msg, time.Now()
		c.logf("%s", msg)
	}

	for ctx.Err() == nil {
		start := time.Now()
		opened, err := c.once(ctx, relayID, func() {
			// Says when, because this line is printed acceptedAfter late by
			// design and reads as "just now". Timing a reconnect from it gives
			// an answer three seconds too slow -- which is exactly how one got
			// misdiagnosed as a four-second stall that was really half of one.
			note("relay: session to %s established (confirmed after %s)", name(), acceptedAfter)
			backoff = c.firstBackoff()
		})
		if ctx.Err() != nil {
			return
		}
		switch {
		case opened && time.Since(start) < acceptedAfter:
			// Expected for a few seconds after this node joins the mesh, and the
			// normal state for a node that has been removed from it.
			note("relay: waiting for %s to accept this node -- it learns about new nodes when it polls the head", name())
			if backoff > waitingRetryCap {
				backoff = waitingRetryCap
			}
		case err != nil:
			note("relay: session to %s ended: %v (retrying)", name(), err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// Cap the backoff: a relay that has been down for an hour should still be
		// picked up within a minute of coming back.
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// firstBackoff is the delay before the first retry; it doubles from there.
func (c *Client) firstBackoff() time.Duration {
	if c.Backoff > 0 {
		return c.Backoff
	}
	return time.Second
}

// once holds one session open until it breaks. The bool reports whether the
// session was ever opened, which is what separates "cannot reach the relay" from
// "the relay hung up on us". onAccepted fires once the session has stayed open
// long enough to have been accepted, and not at all otherwise.
func (c *Client) once(ctx context.Context, relayID string, onAccepted func()) (bool, error) {
	conn, err := c.Dial(ctx, relayID)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if err := WriteHello(conn, Hello{NodeID: c.NodeID, Key: c.Key}); err != nil {
		return false, err
	}
	sess, err := yamux.Client(conn, muxConfig())
	if err != nil {
		return false, err
	}
	defer sess.Close()

	c.setSession(relayID, sess)
	defer c.setSession(relayID, nil)

	// Make the context mean what a reader assumes it means. AcceptStream below
	// blocks until the session breaks, and cancelling ctx does not break it: ctx
	// only ever governed the dial. Callers were left relying on an explicit Close
	// to stop us, and anything that forgot waited out yamux's keepalive instead --
	// half a minute of a session nobody wanted.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-stopped:
		}
	}()
	// Announce the session only once it has survived; announcing it here, as this
	// used to, meant "session up" was printed for sessions that had already been
	// rejected.
	accepted := time.AfterFunc(acceptedAfter, onAccepted)
	defer accepted.Stop()

	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return true, err
		}
		go c.serveInbound(st)
	}
}

func (c *Client) setSession(relayID string, s *yamux.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		c.sess = map[string]*yamux.Session{}
	}
	if s == nil {
		delete(c.sess, relayID)
	} else {
		c.sess[relayID] = s
	}
}

// serveInbound handles a stream the relay pushed to us: connect locally and
// splice. This is the direction that plain Reality cannot do.
func (c *Client) serveInbound(st *yamux.Stream) {
	defer st.Close()
	st.SetReadDeadline(time.Now().Add(15 * time.Second))
	o, err := ReadOpen(st)
	if err != nil {
		return
	}
	st.SetReadDeadline(time.Time{})

	if c.NoInbound {
		c.logf("relay: refused an inbound stream to %s -- this is a read-only client", o.DstAddr)
		WriteResult(st, fmt.Errorf("read-only client"))
		return
	}

	d := net.Dialer{Timeout: 10 * time.Second}
	target, err := d.Dial("tcp", o.DstAddr)
	if err != nil {
		WriteResult(st, err)
		return
	}
	defer target.Close()
	if err := WriteResult(st, nil); err != nil {
		return
	}
	splice(st, target)
}

// Open reaches dstNode:dstAddr through relayID.
func (c *Client) Open(relayID, dstNode, dstAddr string) (net.Conn, error) {
	c.mu.RLock()
	sess := c.sess[relayID]
	c.mu.RUnlock()
	if sess == nil {
		return nil, fmt.Errorf("relay: no session to %s", c.name(relayID))
	}
	st, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := WriteOpen(st, Open{DstNode: dstNode, DstAddr: dstAddr}); err != nil {
		st.Close()
		return nil, err
	}
	if err := ReadResult(st); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// Up reports whether the session to relayID is usable, so callers can fail
// over to the next path without paying a dial timeout first.
func (c *Client) Up(relayID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.sess[relayID]
	return s != nil && !s.IsClosed()
}

func splice(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}
