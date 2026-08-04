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
	// Auth verifies a Hello. Returning false drops the session.
	Auth func(nodeID, key string) bool
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
	if s.Auth != nil && !s.Auth(h.NodeID, h.Key) {
		s.logf("relay: rejected node %s", s.name(h.NodeID))
		return
	}

	sess, err := yamux.Server(c, muxConfig())
	if err != nil {
		s.logf("relay: mux for %s: %v", s.name(h.NodeID), err)
		return
	}
	defer sess.Close()

	s.Reg.put(h.NodeID, sess)
	defer s.Reg.drop(h.NodeID, sess)
	s.logf("relay: node %s online", s.name(h.NodeID))
	defer s.logf("relay: node %s offline", s.name(h.NodeID))

	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go s.handleStream(st)
	}
}

// handleStream forwards one stream toward its destination node.
func (s *Server) handleStream(st *yamux.Stream) {
	defer st.Close()
	st.SetReadDeadline(time.Now().Add(15 * time.Second))
	o, err := ReadOpen(st)
	if err != nil {
		return
	}
	st.SetReadDeadline(time.Time{})

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

	mu   sync.RWMutex
	sess map[string]*yamux.Session // relayID -> session
}

func (c *Client) name(id string) string { return decorate(c.NameOf, id) }

func (c *Client) logf(f string, v ...any) {
	if c.Logf != nil {
		c.Logf(f, v...)
	}
}

// Maintain keeps a session to relayID alive until ctx is done.
func (c *Client) Maintain(ctx context.Context, relayID string) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.once(ctx, relayID)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logf("relay: session to %s ended: %v (retry in %s)", c.name(relayID), err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// Cap the backoff: a relay that has been down for an hour should still
		// be picked up within a minute of coming back.
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) once(ctx context.Context, relayID string) error {
	conn, err := c.Dial(ctx, relayID)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := WriteHello(conn, Hello{NodeID: c.NodeID, Key: c.Key}); err != nil {
		return err
	}
	sess, err := yamux.Client(conn, muxConfig())
	if err != nil {
		return err
	}
	defer sess.Close()

	c.setSession(relayID, sess)
	defer c.setSession(relayID, nil)
	c.logf("relay: session to %s up", c.name(relayID))

	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return err
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

// Close tears down the session to relayID, if any. Cancelling a Maintain
// context does not do this on its own: the context only governs the dial, and
// the loop is otherwise parked in AcceptStream on a live connection.
func (c *Client) Close(relayID string) {
	c.mu.RLock()
	s := c.sess[relayID]
	c.mu.RUnlock()
	if s != nil {
		s.Close()
	}
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
