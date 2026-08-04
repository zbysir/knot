package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestApplyPlanKeepsMachineryWhenOnlyPeersChange is the regression test for the
// churn that made adding one node disconnect all the others.
//
// Peers and PeerKeys both describe the whole mesh, so every join changes them
// on every node. Rebuilding for that closed the relay port and cancelled every
// session, which showed up on the leaves as "session up / ended: EOF" with a
// doubling backoff -- for a plan change that did not touch them at all.
func TestApplyPlanKeepsMachineryWhenOnlyPeersChange(t *testing.T) {
	a := &Agent{}
	t.Cleanup(a.stopRelay)

	p1 := Plan{
		SelfID:   "self",
		Key:      "selfkey",
		Socks:    freeAddr(t),
		Uplinks:  []string{"10.88.0.1:9997"},
		Peers:    []PlanPeer{{NodeID: "n1", VIP: "10.88.0.2"}},
		PeerKeys: map[string]string{"n1": hashKey("k1")},
	}
	if err := a.applyPlan(p1); err != nil {
		t.Fatal(err)
	}
	first := a.relay
	if first == nil {
		t.Fatal("no relay machinery after applyPlan")
	}

	// A second node joins the mesh: peers, keys and uplinks all grow.
	p2 := p1
	p2.Peers = []PlanPeer{{NodeID: "n1", VIP: "10.88.0.2"}, {NodeID: "n2", VIP: "10.88.0.3"}}
	p2.PeerKeys = map[string]string{"n1": hashKey("k1"), "n2": hashKey("k2")}
	p2.Uplinks = []string{"10.88.0.1:9997", "10.88.0.4:9997"}
	if err := a.applyPlan(p2); err != nil {
		t.Fatal(err)
	}
	if a.relay != first {
		t.Fatal("machinery rebuilt for a peer-list change: every leaf session would have dropped")
	}

	// The live Auth callback and the socks path must both see the new peer.
	if got := a.relay.peerKey("n2"); got != hashKey("k2") {
		t.Fatalf("peerKey(n2) = %q, want the new hash -- Auth would reject it", got)
	}
	if p := a.relay.peerByVIP("10.88.0.3"); p == nil || p.NodeID != "n2" {
		t.Fatalf("peerByVIP(10.88.0.3) = %v, want n2", p)
	}

	a.relay.mu.Lock()
	n := len(a.relay.uplinks)
	_, keptOld := a.relay.uplinks["10.88.0.1:9997"]
	a.relay.mu.Unlock()
	if n != 2 {
		t.Errorf("uplinks = %d, want 2", n)
	}
	if !keptOld {
		t.Error("the pre-existing uplink was restarted instead of left alone")
	}

	// Our own listen address is the one thing that does need a rebuild.
	p3 := p2
	p3.Socks = freeAddr(t)
	if err := a.applyPlan(p3); err != nil {
		t.Fatal(err)
	}
	if a.relay == first {
		t.Fatal("socks address changed but nothing was rebound")
	}
}

// fakeSingBox writes a stand-in that accepts `check` and otherwise just runs.
func fakeSingBox(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "fake-sing-box")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  check) exit 0 ;;\n" +
		"  run)   while :; do sleep 0.05; done ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestRestartWaitsForTheOldChild pins down the fix for the outage. The old code
// sent SIGKILL and immediately re-execed; signals are asynchronous, so the new
// process reached ioctl(TUNSETIFF, "knot0") while the old one still held it and
// died at startup with "device or resource busy" -- on a relay, taking :443 and
// the head behind it down.
//
// The invariant: when a restart returns, the previous child must already be
// reaped. Nothing weaker is enough, and SIGHUP is not a way around it (see
// restartSingBox).
func TestRestartWaitsForTheOldChild(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{SingBox: fakeSingBox(t, dir), DataDir: dir, cfgPath: filepath.Join(dir, "singbox.json")}
	if err := os.WriteFile(a.cfgPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.startSingBox(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.stopSingBox)

	old := a.cur
	oldPid := old.cmd.Process.Pid
	if err := a.restartSingBox(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.done:
	default:
		t.Fatal("the old child was still running when the replacement started: that is the tun race")
	}
	if err := syscall.Kill(oldPid, 0); err == nil {
		t.Errorf("pid %d still exists after the restart", oldPid)
	}
	if !a.singBoxAlive() {
		t.Fatal("no live child after the restart")
	}
	if a.cur.cmd.Process.Pid == oldPid {
		t.Fatal("restart did not actually replace the process")
	}
}

// TestRestartStartsWhenTheChildIsGone covers the other half: after a crash there
// is nothing to stop, so a restart has to just start one.
func TestRestartStartsWhenTheChildIsGone(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{SingBox: fakeSingBox(t, dir), DataDir: dir, cfgPath: filepath.Join(dir, "singbox.json")}
	if err := os.WriteFile(a.cfgPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a.singBoxAlive() {
		t.Fatal("alive before anything was started")
	}
	if err := a.restartSingBox(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.stopSingBox)
	if !a.singBoxAlive() {
		t.Fatal("restart did not start a child")
	}
}

// TestFailedReJoinKeepsTheNodeUsable is the regression test for a node bricking
// itself. reenroll used to delete identity.json and zero the in-memory copy
// before attempting the join, so a rejected token (a consumed one-shot, the usual
// case) left the agent with no identity and no head URL. Every later poll then
// went to `/api/config?id=&key=` and failed with "unsupported protocol scheme",
// forever -- the only way out was re-creating the container.
func TestFailedReJoinKeepsTheNodeUsable(t *testing.T) {
	var joins int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/config": // the node was deleted from the panel
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"unknown node id or wrong key"}`)
		case "/api/join": // ... and its one-shot token is long gone
			joins++
			w.WriteHeader(403)
			io.WriteString(w, `{"error":"invalid or expired token"}`)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	idPath := filepath.Join(dir, "identity.json")
	before := `{"node_id":"abc","key":"k","vip":"10.88.0.9","head":"` + srv.URL + `","name":"n"}`
	if err := os.WriteFile(idPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &Agent{Head: srv.URL, Token: "consumed", Name: "n", DataDir: dir}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if err := a.sync(context.Background()); err == nil {
		t.Fatal("a failed re-join must be reported")
	}
	if joins != 1 {
		t.Fatalf("join attempts = %d, want 1", joins)
	}

	// Nothing may have been thrown away: polling is the only way back.
	if _, err := os.Stat(idPath); err != nil {
		t.Error("identity.json was deleted for a re-join that never succeeded")
	}
	if a.id.NodeID != "abc" || a.id.Key != "k" {
		t.Errorf("in-memory identity was wiped: %+v", a.id)
	}
	if err := a.sync(context.Background()); err == nil ||
		strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Errorf("the next poll no longer reaches the head: %v", err)
	}
}

// TestReJoinAdoptsTheNewIdentity is the other half: when the token does work, the
// new identity has to land on disk and in memory.
func TestReJoinAdoptsTheNewIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/config":
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"unknown node id or wrong key"}`)
		case "/api/join":
			io.WriteString(w, `{"node_id":"new1","key":"k2","vip":"10.88.0.4"}`)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.json"),
		[]byte(`{"node_id":"old","key":"k","vip":"10.88.0.9","head":"`+srv.URL+`","name":"n"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Head: srv.URL, Token: "good", Name: "n", DataDir: dir}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	// The re-join succeeds and then re-syncs, which 401s again against this stub
	// (it does not know the new identity either) -- so an error is expected. What
	// matters is that the new identity was adopted.
	a.sync(context.Background())
	if a.id.NodeID != "new1" || a.id.VIP != "10.88.0.4" {
		t.Fatalf("identity not adopted: %+v", a.id)
	}
	b, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil || !strings.Contains(string(b), "new1") {
		t.Fatalf("new identity not persisted: %s %v", b, err)
	}
}

// TestPlanNamesReachTheLogs: the wire protocol only carries node IDs, so
// without the head's name map every log line about a peer is a hex string
// nobody can place -- "relay: node bcd537f766865c69 online".
func TestPlanNamesReachTheLogs(t *testing.T) {
	a := &Agent{}
	t.Cleanup(a.stopRelay)
	p := Plan{
		SelfID:  "self",
		Key:     "selfkey",
		Socks:   freeAddr(t),
		Uplinks: []string{"10.88.0.1:9997"},
		Peers:   []PlanPeer{{NodeID: "bcd537f766865c69", VIP: "10.88.0.2"}},
		Names: map[string]string{
			"bcd537f766865c69": "yy-hz", // by node ID, as a relay sees it
			"10.88.0.1:9997":   "yy-hk", // by address, as a leaf sees it
		},
	}
	if err := a.applyPlan(p); err != nil {
		t.Fatal(err)
	}
	if got := a.relay.nameOf("bcd537f766865c69"); got != "yy-hz" {
		t.Errorf("nameOf(id) = %q, want yy-hz", got)
	}
	if got := a.relay.nameOf("10.88.0.1:9997"); got != "yy-hk" {
		t.Errorf("nameOf(addr) = %q, want yy-hk", got)
	}
	if got := a.relay.nameByVIP("10.88.0.2"); got != "yy-hz" {
		t.Errorf("nameByVIP = %q, want yy-hz", got)
	}
	// A plan cached by an older node carries no names at all, and every lookup
	// has to fall back to the key rather than print an empty string.
	p.Names = nil
	a.relay.setPlan(p)
	if got := a.relay.nameOf("bcd537f766865c69"); got != "bcd537f766865c69" {
		t.Errorf("without names, nameOf = %q, want the id back", got)
	}
}

// TestLogLinesAreTimestamped: knot's own lines are read interleaved with
// sing-box's dated ones, and an undated line cannot be correlated with the
// sing-box error that explains it.
func TestLogLinesAreTimestamped(t *testing.T) {
	var b bytes.Buffer
	logTo(&b, "relay: node %s online", "abc")
	got := b.String()
	if !strings.HasSuffix(got, " knot: relay: node abc online\n") {
		t.Fatalf("unexpected line: %q", got)
	}
	stamp := got[:len(logTime)]
	if _, err := time.Parse(logTime, stamp); err != nil {
		t.Fatalf("line does not start with a parsable timestamp: %q", got)
	}
}

// TestSingBoxLogSummarisesRealityProbes: a public :443 is scanned continuously
// and sing-box reports every probe at ERROR level, which is how a real FATAL
// went unnoticed in a relay's log for hours. Suppress the probes, keep
// everything else, and never lose the fact that probing happened.
func TestSingBoxLogSummarisesRealityProbes(t *testing.T) {
	const probe = "inbound/vless[reality-in]: process connection from %s:443: TLS handshake: REALITY: processed invalid connection"
	in := strings.Join([]string{
		"\x1b[31mERROR\x1b[0m [1 41ms] " + fmt.Sprintf(probe, "1.2.3.4"),
		"ERROR [2 3s] " + fmt.Sprintf(probe, "1.2.3.4"),
		"ERROR [3 1s] " + fmt.Sprintf(probe, "9.9.9.9"),
		"FATAL[0300] start service: start inbound/tun[tun-in]: open tun: TUNSETIFF: device or resource busy",
		"ERROR [4 2s] " + fmt.Sprintf(probe, "1.2.3.4"),
		// A node with a stale UUID passes Reality and must NOT be swallowed:
		// unlike a probe, this one says something about our own mesh.
		"ERROR [5 4ms] inbound/vless[reality-in]: process connection from 8.8.8.8:1: unknown UUID: 181c61f3",
	}, "\n") + "\n"

	var out bytes.Buffer
	// every=time.Hour, so only the EOF report fires and the output is stable.
	pipeSingBoxLog(strings.NewReader(in), &out, time.Hour)
	got := out.String()

	if strings.Contains(got, "processed invalid connection") {
		t.Errorf("probe lines were passed through:\n%s", got)
	}
	for _, want := range []string{
		"TUNSETIFF",    // the line that matters must survive
		"unknown UUID", // and so must a real client failure
		"rejected 4 unauthenticated handshakes",
		"from 2 addresses",
		"most: 1.2.3.4 x3", // one address over and over = a node of yours
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("colour escapes were not stripped:\n%q", got)
	}
}

func TestWriteIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if changed, err := writeIfChanged(p, []byte("a")); err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	if changed, err := writeIfChanged(p, []byte("a")); err != nil || changed {
		t.Fatalf("identical write reported a change: %v %v", changed, err)
	}
	if changed, err := writeIfChanged(p, []byte("b")); err != nil || !changed {
		t.Fatalf("differing write: changed=%v err=%v", changed, err)
	}
}
