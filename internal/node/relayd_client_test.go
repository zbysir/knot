package node

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/zbysir/knot/internal/relay"
)

// planWithClient is what the head hands a relay: its own identity, plus the
// two credential lists.
func planWithClient(t *testing.T, clientKey string) Plan {
	t.Helper()
	p := Plan{
		SelfID:  "relay1",
		Key:     "relaykey",
		IsRelay: true,
		Listen:  freeAddr(t),
		Names:   map[string]string{"client1": "laptop"},
		PeerKeys: map[string]string{
			"leaf1": hashKey("leafkey"),
		},
	}
	if clientKey != "" {
		p.ClientKeys = map[string]string{"client1": hashKey(clientKey)}
	}
	return p
}

// hello opens a session to the relay as the given identity and reports whether
// the relay kept it.
func hello(t *testing.T, addr, id, key string) (*yamux.Session, bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, false
	}
	if err := relay.WriteHello(c, relay.Hello{NodeID: id, Key: key}); err != nil {
		c.Close()
		return nil, false
	}
	sess, err := yamux.Client(c, yamux.DefaultConfig())
	if err != nil {
		c.Close()
		return nil, false
	}
	t.Cleanup(func() { sess.Close() })

	// The protocol answers a rejected Hello by closing and an accepted one by
	// saying nothing, so opening a stream is how a caller finds out.
	st, err := sess.OpenStream()
	if err != nil {
		return sess, false
	}
	defer st.Close()
	if err := relay.WriteOpen(st, relay.Open{DstNode: "relay1", DstAddr: "127.0.0.1:0"}); err != nil {
		return sess, false
	}
	st.SetReadDeadline(time.Now().Add(2 * time.Second))
	// A refusal to dial 127.0.0.1:0 still proves the session is live; a closed
	// session gives an io error instead.
	if err := relay.ReadResult(st); err != nil {
		return sess, isRefusal(err)
	}
	return sess, true
}

// isRefusal separates "the relay answered and declined this dial" -- which
// proves the session is alive -- from "the session is gone".
func isRefusal(err error) bool {
	return err != nil &&
		(strings.Contains(err.Error(), "remote refused") || strings.Contains(err.Error(), "dial"))
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("relayd never bound %s", addr)
}

// TestClientKeysReachTheRelay covers the wiring between the head's plan and the
// relay server: a client listed in ClientKeys gets in, one that is not does not,
// and a node key is not accepted as a client key.
func TestClientKeysReachTheRelay(t *testing.T) {
	a := &Agent{DataDir: t.TempDir()}
	p := planWithClient(t, "clientkey")
	if err := a.applyPlan(p); err != nil {
		t.Fatal(err)
	}
	defer a.stopRelay()
	waitListening(t, p.Listen)

	if _, ok := hello(t, p.Listen, "client1", "clientkey"); !ok {
		t.Error("an authorised client was refused")
	}
	if _, ok := hello(t, p.Listen, "client1", "wrong"); ok {
		t.Error("a client with the wrong key got in")
	}
	if _, ok := hello(t, p.Listen, "nobody", "clientkey"); ok {
		t.Error("an unknown id got in with a valid key")
	}
	if _, ok := hello(t, p.Listen, "leaf1", "leafkey"); !ok {
		t.Error("a mesh node was refused")
	}
}

// TestRevokingAClientCostsNoRebuild is the operational promise: a plan that
// differs only in ClientKeys must not restart the relay machinery, because
// that would drop every leaf's session.
func TestRevokingAClientCostsNoRebuild(t *testing.T) {
	a := &Agent{DataDir: t.TempDir()}
	p := planWithClient(t, "clientkey")
	if err := a.applyPlan(p); err != nil {
		t.Fatal(err)
	}
	defer a.stopRelay()
	waitListening(t, p.Listen)
	before := a.relay

	sess, ok := hello(t, p.Listen, "client1", "clientkey")
	if !ok {
		t.Fatal("client could not connect")
	}

	revoked := p
	revoked.ClientKeys = map[string]string{}
	if err := a.applyPlan(revoked); err != nil {
		t.Fatal(err)
	}
	if a.relay != before {
		t.Error("revoking a client rebuilt the relay -- every leaf's session just dropped")
	}

	// The live session must be closed, not left to expire: Auth runs once, so
	// without this the revoked laptop keeps working until it reconnects.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sess.IsClosed() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sess.IsClosed() {
		t.Fatal("a revoked client's session was left open")
	}
	if _, ok := hello(t, p.Listen, "client1", "clientkey"); ok {
		t.Error("a revoked client reconnected")
	}

	// A mesh node is untouched by any of it.
	if _, ok := hello(t, p.Listen, "leaf1", "leafkey"); !ok {
		t.Error("revoking a client locked a mesh node out")
	}
}
