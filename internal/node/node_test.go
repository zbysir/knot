package node

import (
	"context"
	"net"
	"os"
	"path/filepath"
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

// fakeSingBox writes a stand-in that accepts `check` and survives SIGHUP, the
// way the real one does.
func fakeSingBox(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "fake-sing-box")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  check) exit 0 ;;\n" +
		"  run)   trap '' HUP; while :; do sleep 0.05; done ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestReloadKeepsTheSameProcess pins down the fix for the outage: a config
// change must be delivered by SIGHUP, which sing-box answers by closing its
// instance and only then rebuilding it from the file, in one process.
//
// The old path killed the child and immediately re-execed. SIGKILL is
// asynchronous, so the new process reached ioctl(TUNSETIFF, "knot0") while the
// old one still held it and died with "device or resource busy".
func TestReloadKeepsTheSameProcess(t *testing.T) {
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

	pid := a.cur.cmd.Process.Pid
	if err := a.reloadSingBox(ctx); err != nil {
		t.Fatal(err)
	}
	// Give the signal time to land and, if it were mishandled, to kill it.
	time.Sleep(200 * time.Millisecond)
	if !a.singBoxAlive() {
		t.Fatal("child did not survive the reload")
	}
	if got := a.cur.cmd.Process.Pid; got != pid {
		t.Fatalf("reload replaced the process (pid %d -> %d): that is the tun race", pid, got)
	}
}

// TestReloadStartsWhenTheChildIsGone covers the other half: after a crash there
// is nothing to signal, so a reload has to start one.
func TestReloadStartsWhenTheChildIsGone(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{SingBox: fakeSingBox(t, dir), DataDir: dir, cfgPath: filepath.Join(dir, "singbox.json")}
	if err := os.WriteFile(a.cfgPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a.singBoxAlive() {
		t.Fatal("alive before anything was started")
	}
	if err := a.reloadSingBox(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.stopSingBox)
	if !a.singBoxAlive() {
		t.Fatal("reload did not start a child")
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
