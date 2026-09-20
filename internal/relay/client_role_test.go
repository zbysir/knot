package relay

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// echo answers every read with "echo:" + what it got.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 128)
				for {
					n, err := c.Read(b)
					if err != nil {
						return
					}
					c.Write(append([]byte("echo:"), b[:n]...))
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// testRelay starts a relay that knows one node key and one client key.
func testRelay(t *testing.T) (addr string, srv *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv = &Server{
		Reg:        NewRegistry(),
		Clients:    NewRegistry(),
		SelfID:     "relay1",
		Auth:       func(id, key string) bool { return id == "node1" && key == "nodekey" },
		AuthClient: func(id, key string) bool { return id == "client1" && key == "clientkey" },
		Logf:       func(string, ...any) {},
	}
	go srv.Serve(ln)
	return ln.Addr().String(), srv
}

// dialSession opens a session to the relay as the given identity.
func dialSession(t *testing.T, addr, id, key string) *yamux.Session {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHello(c, Hello{NodeID: id, Key: key}); err != nil {
		t.Fatal(err)
	}
	sess, err := yamux.Client(c, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func open(t *testing.T, sess *yamux.Session, dstNode, dstAddr string) (net.Conn, error) {
	t.Helper()
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

// TestClientReachesTheRelayItself covers the case the whole client role exists
// for: a database that is not a mesh member, on an address only the relay can
// reach.
func TestClientReachesTheRelayItself(t *testing.T) {
	addr, _ := testRelay(t)
	target := echo(t)
	sess := dialSession(t, addr, "client1", "clientkey")

	conn, err := open(t, sess, "relay1", target)
	if err != nil {
		t.Fatalf("client could not reach a local address through the relay: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write([]byte("hi"))
	b := make([]byte, 64)
	n, err := conn.Read(b)
	if err != nil || string(b[:n]) != "echo:hi" {
		t.Fatalf("got %q, %v", b[:n], err)
	}
}

// TestClientIsNotAddressable is the enforcement of "nothing can reach my
// laptop": the relay keeps client sessions somewhere routing never looks.
func TestClientIsNotAddressable(t *testing.T) {
	addr, srv := testRelay(t)
	dialSession(t, addr, "client1", "clientkey")

	// Give the relay a moment to finish the handshake.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(srv.Clients.Online()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(srv.Clients.Online()) != 1 {
		t.Fatal("the client session was never registered as a client")
	}
	if got := srv.Reg.Online(); len(got) != 0 {
		t.Fatalf("a client landed in the routable registry: %v", got)
	}

	// A legitimate node now asks the relay to push a stream to the client.
	node := dialSession(t, addr, "node1", "nodekey")
	if _, err := open(t, node, "client1", "127.0.0.1:1"); err == nil {
		t.Fatal("a node opened a connection to a read-only client")
	}
}

func TestBadCredentialsAreRefused(t *testing.T) {
	addr, _ := testRelay(t)
	for _, c := range []struct{ id, key string }{
		{"client1", "wrong"},
		{"unknown", "clientkey"},
		{"node1", "clientkey"}, // a node id with a client's key
	} {
		sess := dialSession(t, addr, c.id, c.key)
		// The protocol answers a rejected Hello by closing, so the first
		// stream is what discovers it.
		_, err := open(t, sess, "relay1", "127.0.0.1:1")
		if err == nil {
			t.Errorf("relay accepted %s/%s", c.id, c.key)
		}
	}
}

// TestRevokeClosesLiveSessions: Auth runs once, so dropping a key from the plan
// does nothing to a session that is already open unless we close it.
func TestRevokeClosesLiveSessions(t *testing.T) {
	addr, srv := testRelay(t)
	target := echo(t)
	sess := dialSession(t, addr, "client1", "clientkey")
	conn, err := open(t, sess, "relay1", target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	closed := srv.Clients.CloseUnlisted(map[string]string{}) // revoke everything
	if len(closed) != 1 || closed[0] != "client1" {
		t.Fatalf("revoke closed %v, want [client1]", closed)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err == nil {
		// ReadAll returning nil error means a clean EOF, which is what a
		// closed session looks like -- that is a pass.
		return
	}
}

// TestNoInboundRefusesPushedStreams is the client-side half of the same
// guarantee, for the day a relay has a bug.
func TestNoInboundRefusesPushedStreams(t *testing.T) {
	target := echo(t)
	a, b := net.Pipe()
	c := &Client{NodeID: "client1", Key: "k", NoInbound: true, Logf: func(string, ...any) {}}

	server, err := yamux.Server(a, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := yamux.Client(b, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	go func() {
		st, err := client.AcceptStream()
		if err != nil {
			return
		}
		c.serveInbound(st)
	}()

	st, err := server.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := WriteOpen(st, Open{DstNode: "client1", DstAddr: target}); err != nil {
		t.Fatal(err)
	}
	st.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ReadResult(st); err == nil {
		t.Fatal("a read-only client accepted a stream pushed at it")
	}
}
