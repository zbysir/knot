package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/zbysir/knot/internal/relay"
)

func testBundle() Bundle {
	return Bundle{
		Name:     "laptop",
		DoorUUID: "11111111-2222-4333-8444-555555555555",
		Relays: []Relay{{
			ID: "relay1", Name: "hk", VIP: "10.88.0.1", KnotPort: 9997,
			Endpoint: "hk.example.com:443", ServerName: "dl.google.com",
			PublicKey: "pk", ShortID: "ab12",
		}},
		Nodes: []BundleNode{
			{ID: "relay1", Name: "hk", VIP: "10.88.0.1", IsRelay: true},
			{ID: "leaf1", Name: "pg", VIP: "10.88.0.2"},
		},
	}
}

func TestParseBundleRejectsUnusableOnes(t *testing.T) {
	good, _ := json.Marshal(testBundle())
	if _, err := ParseBundle(good); err != nil {
		t.Fatalf("a complete bundle was rejected: %v", err)
	}

	for name, mangle := range map[string]func(*Bundle){
		"no door":     func(b *Bundle) { b.DoorUUID = "" },
		"no relay":    func(b *Bundle) { b.Relays = nil },
		"no key":      func(b *Bundle) { b.Relays[0].PublicKey = "" },
		"no knotport": func(b *Bundle) { b.Relays[0].KnotPort = 0 },
		"no vip":      func(b *Bundle) { b.Relays[0].VIP = "" },
	} {
		b := testBundle()
		mangle(&b)
		raw, _ := json.Marshal(b)
		if _, err := ParseBundle(raw); err == nil {
			t.Errorf("%s: accepted a bundle that cannot be used to connect", name)
		}
	}
}

// TestGenerateConfigIsAFixedPipe: the client's sing-box must be able to reach
// exactly one thing per relay -- knot's port on that relay. Anything more and
// the shared door would be worth stealing.
func TestGenerateConfigIsAFixedPipe(t *testing.T) {
	b := testBundle()
	raw, err := GenerateConfig(b, map[string]int{"hk": 19901})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"tun"`) {
		t.Fatalf("client config must never contain a tun inbound:\n%s", raw)
	}

	var cfg struct {
		Inbounds []struct {
			Type            string `json:"type"`
			Tag             string `json:"tag"`
			Listen          string `json:"listen"`
			ListenPort      int    `json:"listen_port"`
			OverrideAddress string `json:"override_address"`
			OverridePort    int    `json:"override_port"`
		} `json:"inbounds"`
		Outbounds []struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
			UUID string `json:"uuid"`
		} `json:"outbounds"`
		Route struct {
			Rules []struct {
				Inbound  []string `json:"inbound"`
				Outbound string   `json:"outbound"`
			} `json:"rules"`
			Final string `json:"final"`
		} `json:"route"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("generated config is not valid json: %v", err)
	}
	if len(cfg.Inbounds) != 1 {
		t.Fatalf("want one inbound per relay, got %d", len(cfg.Inbounds))
	}
	in := cfg.Inbounds[0]
	if in.Type != "direct" || in.Listen != "127.0.0.1" || in.ListenPort != 19901 {
		t.Errorf("inbound is not a loopback fixed-destination listener: %+v", in)
	}
	if in.OverrideAddress != "10.88.0.1" || in.OverridePort != 9997 {
		t.Errorf("inbound does not point at the relay's knot port: %+v", in)
	}
	if cfg.Outbounds[0].UUID != b.DoorUUID {
		t.Errorf("outbound does not present the shared door: %q", cfg.Outbounds[0].UUID)
	}
	if len(cfg.Route.Rules) != 1 || cfg.Route.Rules[0].Outbound != "dial-hk" {
		t.Errorf("rules do not wire the listener to its relay: %+v", cfg.Route.Rules)
	}
	if cfg.Route.Final != "dial-hk" {
		t.Errorf("final = %q, stray traffic must not leak onto the local network", cfg.Route.Final)
	}
}

// ------------------------------------------------------------- integration

// echoServer answers every read with "<tag>:<what it got>".
func echoServer(t *testing.T, tag string) string {
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
				b := make([]byte, 256)
				for {
					n, err := c.Read(b)
					if err != nil {
						return
					}
					c.Write([]byte(tag + ":" + string(b[:n])))
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func say(t *testing.T, addr, msg string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := make([]byte, 256)
	n, err := c.Read(b)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b[:n])
}

// testRelay starts a real relay server on loopback.
//
// sing-box is the only thing missing from the chain, and only because in this
// arrangement it is a transparent pipe: the client's listener has a fixed
// destination, so pointing the client's port map straight at the relay is
// exactly what sing-box would have produced.
func testRelay(t *testing.T, clientID, clientKey string) (addr string, srv *relay.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv = &relay.Server{
		Reg:        relay.NewRegistry(),
		Clients:    relay.NewRegistry(),
		SelfID:     "relay1",
		Auth:       func(id, key string) bool { return id == "leaf1" && key == "leafkey" },
		AuthClient: func(id, key string) bool { return id == clientID && key == clientKey },
		Logf:       func(string, ...any) {},
	}
	go srv.Serve(ln)
	return ln.Addr().String(), srv
}

// newTestAgent wires an agent to a relay without sing-box or a head.
func newTestAgent(t *testing.T, relayAddr string) (*Agent, context.Context) {
	t.Helper()
	_, port, _ := net.SplitHostPort(relayAddr)
	a := New()
	a.DataDir = t.TempDir()
	a.log = newRing(50)
	a.st = State{Head: "http://head", NodeID: "client1", Key: "clientkey", Name: "laptop"}
	a.bundle = testBundle()
	a.ports = map[string]int{"hk": atoi(port)}
	a.fwd = NewManager(a.dialForward, func(string, ...any) {})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		a.fwd.CloseAll()
	})
	a.startSessions(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		rc := a.rc
		a.mu.Unlock()
		if rc != nil && rc.Up("hk") {
			return a, ctx
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the relay session never came up")
	return nil, nil
}

// TestForwardToTheRelayItself is the "managed database in the relay's VPC"
// case: an address that is not a mesh member at all.
func TestForwardToTheRelayItself(t *testing.T) {
	relayAddr, _ := testRelay(t, "client1", "clientkey")
	db := echoServer(t, "db")
	a, ctx := newTestAgent(t, relayAddr)

	host, port, _ := net.SplitHostPort(db)
	local, _ := freePort()
	a.st.Forwards = []Forward{{
		ID: "f1", Local: local, Node: "hk", Host: host, Port: atoi(port), Enabled: true,
	}}
	a.applyForwards()

	if got := say(t, fmt.Sprintf("127.0.0.1:%d", local), "ping"); got != "db:ping" {
		t.Fatalf("through the forward: got %q", got)
	}
	_ = ctx
}

// TestForwardToALeaf is the "database on another machine" case: the relay
// pushes the stream down the leaf's own session, which is the part plain
// Reality cannot do.
func TestForwardToALeaf(t *testing.T) {
	relayAddr, _ := testRelay(t, "client1", "clientkey")
	db := echoServer(t, "pgdb")
	a, ctx := newTestAgent(t, relayAddr)

	// A leaf homes itself on the relay, exactly as a node does.
	leaf := &relay.Client{
		NodeID: "leaf1", Key: "leafkey", Logf: func(string, ...any) {},
		Dial: func(ctx context.Context, id string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", relayAddr)
		},
	}
	go leaf.Maintain(ctx, "hk")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !leaf.Up("hk") {
		time.Sleep(20 * time.Millisecond)
	}
	if !leaf.Up("hk") {
		t.Fatal("the leaf never homed on the relay")
	}

	host, port, _ := net.SplitHostPort(db)
	local, _ := freePort()
	a.st.Forwards = []Forward{{
		ID: "f1", Local: local, Node: "pg", Host: host, Port: atoi(port), Enabled: true,
	}}
	a.applyForwards()

	if got := say(t, fmt.Sprintf("127.0.0.1:%d", local), "hello"); got != "pgdb:hello" {
		t.Fatalf("through the forward to a leaf: got %q", got)
	}
}

func TestForwardToAnUnknownNodeSaysSo(t *testing.T) {
	relayAddr, _ := testRelay(t, "client1", "clientkey")
	a, _ := newTestAgent(t, relayAddr)
	_, err := a.dialForward(context.Background(), "nosuch", "127.0.0.1", 1)
	if err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("want an error naming the node, got %v", err)
	}
}

func TestWrongCredentialNeverConnects(t *testing.T) {
	relayAddr, _ := testRelay(t, "client1", "theRightKey")
	_, port, _ := net.SplitHostPort(relayAddr)
	a := New()
	a.DataDir = t.TempDir()
	a.log = newRing(50)
	a.st = State{Head: "http://head", NodeID: "client1", Key: "theWrongKey"}
	a.bundle = testBundle()
	a.ports = map[string]int{"hk": atoi(port)}
	a.fwd = NewManager(a.dialForward, func(string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startSessions(ctx)

	// The relay answers a rejected Hello by closing, so a session never settles.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := a.dialForward(ctx, "hk", "127.0.0.1", 1); err == nil {
			t.Fatal("a wrong key still opened a connection")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ------------------------------------------------------------- forwards

func TestForwardLifecycle(t *testing.T) {
	one, two := echoServer(t, "one"), echoServer(t, "two")
	h1, p1, _ := net.SplitHostPort(one)
	h2, p2, _ := net.SplitHostPort(two)

	dial := func(ctx context.Context, node, host string, port int) (netConn, error) {
		if node != "hk" {
			return nil, fmt.Errorf("unknown node %q", node)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	}
	m := NewManager(dial, func(string, ...any) {})
	defer m.CloseAll()

	local, _ := freePort()
	addr := fmt.Sprintf("127.0.0.1:%d", local)
	f := Forward{ID: "f1", Local: local, Node: "hk", Host: h1, Port: atoi(p1), Enabled: true}
	m.Apply([]Forward{f})

	if got := say(t, addr, "ping"); got != "one:ping" {
		t.Fatalf("got %q", got)
	}

	// A connection held across an edit, like a database session an operator is
	// in the middle of, must survive it.
	live, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.SetDeadline(time.Now().Add(3 * time.Second))
	live.Write([]byte("before"))
	buf := make([]byte, 64)
	if n, err := live.Read(buf); err != nil || string(buf[:n]) != "one:before" {
		t.Fatalf("live connection before edit: %q %v", buf[:n], err)
	}

	f.Host, f.Port = h2, atoi(p2)
	m.Apply([]Forward{f})

	live.Write([]byte("after"))
	if n, err := live.Read(buf); err != nil || string(buf[:n]) != "one:after" {
		t.Fatalf("the edit disturbed a live connection: %q %v", buf[:n], err)
	}
	if got := say(t, addr, "hi"); got != "two:hi" {
		t.Fatalf("new connection after edit: got %q", got)
	}

	// Removing it must free the port, or a later forward on the same one fails
	// with a bind error nobody can explain.
	m.Apply(nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d still accepting after the forward was removed", local)
}

func TestFailedBindIsRetried(t *testing.T) {
	echo := echoServer(t, "e")
	h, p, _ := net.SplitHostPort(echo)
	dial := func(ctx context.Context, node, host string, port int) (netConn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	}
	m := NewManager(dial, func(string, ...any) {})
	defer m.CloseAll()

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := blocker.Addr().(*net.TCPAddr).Port
	f := Forward{ID: "f1", Local: local, Node: "hk", Host: h, Port: atoi(p), Enabled: true}

	m.Apply([]Forward{f})
	if s := m.Stats()["f1"]; s.Listening || s.LastErr == "" {
		t.Fatalf("a bind that failed should be reported: %+v", s)
	}

	blocker.Close()
	m.Apply([]Forward{f})
	if s := m.Stats()["f1"]; !s.Listening {
		t.Fatalf("forward was not retried after the port freed up: %+v", s)
	}
	if got := say(t, fmt.Sprintf("127.0.0.1:%d", local), "x"); got != "e:x" {
		t.Fatalf("retried forward does not carry traffic: %q", got)
	}

	// And removing one that never bound must not panic.
	m.Apply([]Forward{{ID: "f2", Local: 1, Node: "hk", Host: h, Port: atoi(p), Enabled: true}})
	m.Apply(nil)
}

func TestForwardValidate(t *testing.T) {
	ok := Forward{Local: 15432, Node: "hk", Host: "db", Port: 5432}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid forward rejected: %v", err)
	}
	for _, f := range []Forward{
		{Local: 80, Node: "hk", Host: "db", Port: 5432}, // privileged port
		{Local: 15432, Host: "db", Port: 5432},          // no node
		{Local: 15432, Node: "hk", Port: 5432},          // no host
		{Local: 15432, Node: "hk", Host: "db"},          // no port
		{Local: 0, Node: "hk", Host: "db", Port: 5432},  // no local port
	} {
		if err := f.Validate(); err == nil {
			t.Errorf("accepted an invalid forward: %+v", f)
		}
	}
}

// TestMigrateForwards: the first version named an exit relay and a target it
// would reach, which is what Node and Host mean now.
func TestMigrateForwards(t *testing.T) {
	got := migrateForwards([]Forward{
		{ID: "old", Local: 15432, Exit: "hk", Host: "10.0.1.5", Port: 5432, Enabled: true},
		{ID: "new", Local: 15433, Node: "pg", Host: "127.0.0.1", Port: 5432},
	})
	if got[0].Node != "hk" || got[0].Host != "10.0.1.5" {
		t.Errorf("old forward not migrated: %+v", got[0])
	}
	if got[0].Exit != "" || got[1].Exit != "" {
		t.Error("Exit should be cleared once migrated, so it is written back as the new shape")
	}
	if got[1].Node != "pg" {
		t.Errorf("a current forward was disturbed: %+v", got[1])
	}
}

func atoi(s string) int {
	n := 0
	fmt.Sscanf(s, "%d", &n)
	return n
}
