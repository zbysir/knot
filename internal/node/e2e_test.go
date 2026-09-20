package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zbysir/knot/internal/client"
	"github.com/zbysir/knot/internal/head"
	"github.com/zbysir/knot/internal/model"
	"github.com/zbysir/knot/internal/sb"
)

// TestHelperFakeSingBox is not a test. It is the body of the fake sing-box the
// end-to-end test below runs, re-executing this same binary because a shell
// script cannot forward TCP.
//
// It implements the only thing the client asks sing-box for: each `direct`
// inbound becomes a listener that connects to that inbound's override address.
// In the real thing the hop in between is Reality; here it is a plain dial,
// which is exactly the part the rest of this test is not trying to prove.
func TestHelperFakeSingBox(t *testing.T) {
	if os.Getenv("KNOT_FAKE_SINGBOX") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) < 3 || args[1] != "-c" {
		os.Exit(2)
	}
	raw, err := os.ReadFile(args[2])
	if err != nil {
		os.Exit(1)
	}
	var cfg struct {
		Inbounds []struct {
			Type            string `json:"type"`
			ListenPort      int    `json:"listen_port"`
			OverrideAddress string `json:"override_address"`
			OverridePort    int    `json:"override_port"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(raw, &cfg) != nil || len(cfg.Inbounds) == 0 {
		os.Exit(1)
	}
	if args[0] == "check" {
		return
	}
	for _, in := range cfg.Inbounds {
		if in.Type != "direct" || in.OverrideAddress == "" {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", in.ListenPort))
		if err != nil {
			os.Exit(1)
		}
		dst := fmt.Sprintf("%s:%d", in.OverrideAddress, in.OverridePort)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					out, err := net.DialTimeout("tcp", dst, 3*time.Second)
					if err != nil {
						return
					}
					defer out.Close()
					go io.Copy(out, c)
					io.Copy(c, out)
				}(c)
			}
		}()
	}
	select {}
}

func forwardingSingBox(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "fake-sing-box")
	script := fmt.Sprintf("#!/bin/sh\nKNOT_FAKE_SINGBOX=1 exec %q -test.run='^TestHelperFakeSingBox$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func postJSON(t *testing.T, url string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// relayPlanFor fetches a node's plan the way the node agent does.
func relayPlanFor(t *testing.T, headURL, id, key string) Plan {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/config?id=%s&key=%s", headURL, id, key))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("config: HTTP %d %s", resp.StatusCode, b)
	}
	var cr struct {
		Relay Plan `json:"relay"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	return cr.Relay
}

// TestEndToEndOperatorClient runs the whole chain in one process: a real head,
// a real relay (this package's relayd), and the real client app driven through
// its own panel API.
//
// Only Reality is missing, and only because in this arrangement it would be a
// transparent pipe: the client's listener has a fixed destination, so pointing
// it straight at the relay's knot port is what sing-box would have produced.
//
// The mesh CIDR is loopback so relayd can bind its address without a tun
// device, which is the one thing a test on a laptop cannot have.
func TestEndToEndOperatorClient(t *testing.T) {
	// knot's own ports are fixed, so a relay running on this machine already
	// owns them. Skip rather than fail: "address already in use" on a port the
	// test never chose is a confusing way to learn that.
	for _, p := range []int{sb.RelayPort, sb.SocksPort} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Skipf("127.0.0.1:%d is in use -- is a knot relay running here?", p)
		}
		ln.Close()
	}

	dir := t.TempDir()
	store, err := model.NewStore(filepath.Join(dir, "head.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(func(st *model.State) error {
		st.MeshCIDR = "127.0.0.0/24"
		st.Tokens = append(st.Tokens,
			&model.JoinToken{Token: "nodetok", Reusable: true, Expires: time.Now().Add(time.Hour)},
			&model.JoinToken{Token: "clienttok", Reusable: true, Expires: time.Now().Add(time.Hour),
				Role: model.RoleClient},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(head.New(store).Handler())
	defer ts.Close()

	// --- the relay ---------------------------------------------------------
	code, body := postJSON(t, ts.URL+"/api/join",
		map[string]string{"token": "nodetok", "name": "hk", "endpoint": "127.0.0.1:443"})
	if code != 200 {
		t.Fatalf("relay join: %d %s", code, body)
	}
	var relayID struct {
		NodeID string `json:"node_id"`
		Key    string `json:"key"`
		VIP    string `json:"vip"`
	}
	json.Unmarshal([]byte(body), &relayID)
	if relayID.VIP != "127.0.0.1" {
		t.Fatalf("relay got %q, the test needs the loopback address", relayID.VIP)
	}

	agent := &Agent{DataDir: t.TempDir()}
	plan := relayPlanFor(t, ts.URL, relayID.NodeID, relayID.Key)
	if err := agent.applyPlan(plan); err != nil {
		t.Fatal(err)
	}
	defer agent.stopRelay()
	waitListening(t, plan.Listen)

	// --- something to reach ------------------------------------------------
	db, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	go func() {
		for {
			c, err := db.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 64)
				n, err := c.Read(b)
				if err != nil {
					return
				}
				c.Write([]byte("db:" + string(b[:n])))
			}(c)
		}
	}()
	dbHost, dbPort, _ := net.SplitHostPort(db.Addr().String())

	// --- the client app ----------------------------------------------------
	app := client.New()
	app.DataDir = t.TempDir()
	app.SingBox = forwardingSingBox(t, dir)
	app.UIAddr = freeAddr(t)
	app.OpenUI = false
	app.LogTo = io.Discard // the panel keeps its own copy; this test does not need it
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.Run(ctx)
	ui := "http://" + app.UIAddr
	waitHTTP(t, ui+"/api/state")

	if code, body := postJSON(t, ui+"/api/join", map[string]any{
		"head": ts.URL, "token": "clienttok", "name": "laptop",
	}); code != 200 {
		t.Fatalf("client join: %d %s", code, body)
	}

	// The relay only learns about a new client when it next polls the head --
	// 10 seconds in production, and one explicit refresh here. This is the
	// whole cost of issuing a client: a map entry in the plan, no restart.
	if err := agent.applyPlan(relayPlanFor(t, ts.URL, relayID.NodeID, relayID.Key)); err != nil {
		t.Fatal(err)
	}

	local := freePort(t)
	if code, body := postJSON(t, ui+"/api/forward", map[string]any{
		"local": local, "node": "hk", "host": dbHost, "port": atoiT(t, dbPort),
		"enabled": true, "label": "db",
	}); code != 200 {
		t.Fatalf("add forward: %d %s", code, body)
	}

	// --- the whole point ---------------------------------------------------
	got := waitEcho(t, fmt.Sprintf("127.0.0.1:%d", local), "hello", 10*time.Second)
	if got != "db:hello" {
		t.Fatalf("through the forward: got %q, want %q", got, "db:hello")
	}

	// The client must not have become part of the mesh.
	store.Read(func(st *model.State) {
		n := st.NodeByName("laptop")
		if n == nil || !n.IsClient() {
			t.Fatalf("client record wrong: %+v", n)
		}
		if n.VIP != "" {
			t.Errorf("the client took a mesh address: %q", n.VIP)
		}
	})
	if h := relayPlanFor(t, ts.URL, relayID.NodeID, relayID.Key); len(h.ClientKeys) != 1 {
		t.Errorf("the relay's plan does not carry exactly one client: %+v", h.ClientKeys)
	}

	// --- revoke ------------------------------------------------------------
	store.Write(func(st *model.State) error {
		for i, n := range st.Nodes {
			if n.Name == "laptop" {
				st.Nodes = append(st.Nodes[:i], st.Nodes[i+1:]...)
				break
			}
		}
		return nil
	})
	if err := agent.applyPlan(relayPlanFor(t, ts.URL, relayID.NodeID, relayID.Key)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	cut := false
	for time.Now().Before(deadline) {
		if _, err := echoOnce(fmt.Sprintf("127.0.0.1:%d", local), "again"); err != nil {
			cut = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !cut {
		t.Fatal("a revoked client could still reach the database")
	}

	// --- re-issue ----------------------------------------------------------
	//
	// Re-joining hands out a new key while the relay list stays the same, so
	// the generated sing-box config is byte-identical and nothing about the
	// transport changes. The sessions still have to be rebuilt: without that
	// they keep presenting the revoked credential, the relay keeps refusing it,
	// and the app sits there looking connected. That is a real bug this test
	// exists to keep out.
	if code, body := postJSON(t, ui+"/api/join", map[string]any{
		"head": ts.URL, "token": "clienttok", "name": "laptop",
	}); code != 200 {
		t.Fatalf("re-join: %d %s", code, body)
	}
	if err := agent.applyPlan(relayPlanFor(t, ts.URL, relayID.NodeID, relayID.Key)); err != nil {
		t.Fatal(err)
	}
	if got := waitEcho(t, fmt.Sprintf("127.0.0.1:%d", local), "back", 10*time.Second); got != "db:back" {
		t.Fatalf("after re-issuing the credential: got %q", got)
	}
}

// --- small helpers ---------------------------------------------------------

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never answered", url)
}

func waitEcho(t *testing.T, addr, msg string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		got, err := echoOnce(addr, msg)
		if err == nil {
			return got
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing came back through %s: %v", addr, last)
	return ""
}

func echoOnce(addr, msg string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	b := make([]byte, 64)
	n, err := c.Read(b)
	if err != nil {
		return "", err
	}
	return string(b[:n]), nil
}

func freePort(t *testing.T) int {
	t.Helper()
	_, p, _ := net.SplitHostPort(freeAddr(t))
	return atoiT(t, p)
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}
