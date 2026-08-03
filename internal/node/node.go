// Package node is the agent that runs on every machine in the mesh.
//
// It joins the head once with a token, then polls for its sing-box config and
// keeps a sing-box child process in sync with it. Everything it needs to
// persist lives in one small identity file, so re-creating the container is
// harmless: it re-joins under the same name and gets the same VIP back.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Identity struct {
	NodeID string `json:"node_id"`
	Key    string `json:"key"`
	VIP    string `json:"vip"`
	Head   string `json:"head"`
	Name   string `json:"name"`
}

type Agent struct {
	Head     string // https://head.example.com
	Token    string
	Name     string
	Endpoint string // set on relays: the host:port peers dial
	DataDir  string
	SingBox  string // path to the sing-box binary
	Insecure bool   // skip TLS verification against the head

	id       Identity
	etag     string
	cfgPath  string
	planPath string
	relay    *relayd

	// Set when a part of the config was written but not yet applied, so a
	// failed apply is retried instead of being swallowed by the next
	// byte-comparison (which would find the file already up to date).
	cfgDirty  bool
	planDirty bool

	lastRejoin time.Time

	// mu guards cur for the whole of every start/stop/reload, so the supervisor
	// goroutine cannot start a second sing-box while sync is reloading one.
	mu  sync.Mutex
	cur *child
}

// child is one sing-box process and a channel that closes once it has been
// reaped. The channel is what makes a stop synchronous: SIGKILL only queues
// the kill, and the tun device stays open until the kernel has finished tearing
// the process down.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (a *Agent) Run(ctx context.Context) error {
	a.cfgPath = filepath.Join(a.DataDir, "singbox.json")
	a.planPath = filepath.Join(a.DataDir, "relay-plan.json")
	if err := os.MkdirAll(a.DataDir, 0o700); err != nil {
		return err
	}
	if err := a.load(); err != nil {
		return err
	}
	defer a.stopSingBox()
	defer a.stopRelay()

	// A failed first sync is not fatal if we still have the config from last
	// time. Two situations need this:
	//
	//   - the head is simply down while this node restarts. Exiting here threw
	//     away a working data plane for no reason.
	//   - KNOT_HEAD points at the head's MESH address. Then it is unreachable
	//     by construction until sing-box is up, so requiring the head first is
	//     a deadlock: head needs mesh, mesh needs config, config needs head.
	//
	// The ticker below picks up the real config as soon as the head answers.
	if err := a.sync(ctx); err != nil {
		if _, statErr := os.Stat(a.cfgPath); statErr != nil {
			return fmt.Errorf("initial sync (and no cached config): %w", err)
		}
		fmt.Fprintf(os.Stderr, "knot: initial sync failed (%v)\nknot: starting from cached config\n", err)
		if b, rerr := os.ReadFile(a.planPath); rerr == nil {
			var p Plan
			if json.Unmarshal(b, &p) == nil {
				if err := a.applyPlan(p); err != nil {
					fmt.Fprintf(os.Stderr, "knot: cached relay plan failed: %v\n", err)
				}
			}
		}
		if err := a.reloadSingBox(ctx); err != nil {
			return fmt.Errorf("initial sync failed and cached config is unusable: %w", err)
		}
	}

	// Supervise the child independently of the head, because the loop below
	// cannot: sync() returns at its first line when the head is unreachable, so
	// it never gets as far as looking at sing-box. On a relay that is a trap
	// rather than an inconvenience -- the head is reached THROUGH this very
	// process, so a sing-box that dies takes down the only channel that could
	// have noticed. One lost tun race then means a permanent outage.
	go a.superviseSingBox(ctx)

	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := a.sync(ctx); err != nil {
				// A head outage must not take the data plane down: sing-box
				// keeps running with the config it already has.
				fmt.Fprintf(os.Stderr, "knot: sync failed (keeping current config): %v\n", err)
			}
		}
	}
}

// superviseSingBox restarts sing-box whenever it is not running.
//
// Polling rather than watching the exit channel: a start that fails produces no
// child and therefore no future exit to wake up on, which would park this
// goroutine forever exactly when it is needed most.
func (a *Agent) superviseSingBox(ctx context.Context) {
	const maxWait = 30 * time.Second
	wait := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if a.singBoxAlive() {
			wait = time.Second
			continue
		}
		if _, err := os.Stat(a.cfgPath); err != nil {
			continue // no config yet; nothing to run
		}
		fmt.Fprintln(os.Stderr, "knot: sing-box is not running, restarting it")
		if err := a.startSingBox(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "knot: sing-box restart failed (retry in %s): %v\n", wait, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if wait < maxWait {
				wait *= 2
			}
			continue
		}
		wait = time.Second
	}
}

// load reads the persisted identity, joining the head if this is a first run.
func (a *Agent) load() error {
	p := filepath.Join(a.DataDir, "identity.json")
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &a.id); err == nil && a.id.NodeID != "" {
			// KNOT_HEAD wins over the address recorded at join time. Without
			// this, changing the env var does nothing at all -- sync() reads
			// the persisted copy -- and moving the head (say, from its public
			// address to its mesh address) silently has no effect.
			if a.Head != "" && a.Head != a.id.Head {
				fmt.Fprintf(os.Stderr, "knot: head address changed %s -> %s\n", a.id.Head, a.Head)
				a.id.Head = a.Head
				if nb, err := json.MarshalIndent(a.id, "", "  "); err == nil {
					os.WriteFile(p, nb, 0o600)
				}
			}
			return nil
		}
	}
	if a.Token == "" {
		return fmt.Errorf("no identity on disk and no KNOT_TOKEN given")
	}
	body, _ := json.Marshal(map[string]string{
		"token": a.Token, "name": a.Name, "endpoint": a.Endpoint,
	})
	resp, err := a.client().Post(a.Head+"/api/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("join rejected: %s", e["error"])
	}
	if err := json.NewDecoder(resp.Body).Decode(&a.id); err != nil {
		return err
	}
	a.id.Head, a.id.Name = a.Head, a.Name
	b, _ := json.MarshalIndent(a.id, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

type configResp struct {
	SingBox json.RawMessage `json:"singbox"`
	Hosts   string          `json:"hosts"`
	Relay   Plan            `json:"relay"`
}

// sync pulls the current config and applies the three parts of it -- sing-box
// config, /etc/hosts block, relay plan -- independently of each other.
//
// The head hashes all three into one ETag, so a 200 says only "something
// changed". Acting on that wholesale is what made routine operations
// expensive: adding a node changes every other node's hosts block and every
// relay's peer-key map, and that used to tear down every leaf session and bounce
// sing-box. Comparing the parts byte by byte means a change only costs what it
// actually touches.
func (a *Agent) sync(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/config?id=%s&key=%s", a.id.Head, a.id.NodeID, a.id.Key)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if a.etag != "" {
		req.Header.Set("If-None-Match", a.etag)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil // superviseSingBox keeps the child alive; nothing to apply
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		// 403 too: a head from before these codes were split answers both
		// "unknown node" and "cannot build config" with it.
		return a.reenroll(ctx, errorBody(resp))
	case resp.StatusCode != 200:
		return fmt.Errorf("config: HTTP %d: %s", resp.StatusCode, errorBody(resp))
	}
	var cr configResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return err
	}

	changed, err := writeIfChanged(a.cfgPath, cr.SingBox)
	if err != nil {
		return err
	}
	a.cfgDirty = a.cfgDirty || changed

	// Cache the relay plan next to the sing-box config. Without it a node that
	// cold-starts while the head is unreachable comes up half working: its own
	// outbound traffic flows, but it holds no relay session, so nothing can be
	// pushed back down to it and it is silently unreachable.
	planJSON, err := json.Marshal(cr.Relay)
	if err != nil {
		return err
	}
	changed, err = writeIfChanged(a.planPath, planJSON)
	if err != nil {
		return err
	}
	a.planDirty = a.planDirty || changed

	// Hosts is only ever a file; sing-box does not read it (there is no dns
	// block on purpose), so this never costs a reload.
	if err := writeHosts(cr.Hosts); err != nil {
		fmt.Fprintf(os.Stderr, "knot: hosts update failed: %v\n", err)
	}

	// Relay first: sing-box will start forwarding peer traffic into the SOCKS
	// listener as soon as it comes up, so that listener has to exist already.
	if a.planDirty || a.relay == nil {
		if err := a.applyPlan(cr.Relay); err != nil {
			fmt.Fprintf(os.Stderr, "knot: relay plan failed: %v\n", err)
		} else {
			a.planDirty = false
		}
	}
	if a.cfgDirty || !a.singBoxAlive() {
		if err := a.reloadSingBox(ctx); err != nil {
			return err
		}
		a.cfgDirty = false
	}
	// Remember the ETag only once everything landed. Not storing it is what
	// makes the next poll answer 200 instead of 304, and that is the only thing
	// that retries an apply which failed.
	if !a.cfgDirty && !a.planDirty {
		a.etag = resp.Header.Get("ETag")
	}
	return nil
}

// reenroll handles the head saying it does not know this identity, which
// happens when the node was deleted from the panel or the head's state file was
// replaced.
//
// This used to be terminal in a way that was hard to see: load() returns as
// soon as identity.json has a node ID, so KNOT_TOKEN was never consulted again.
// The node polled forever, the relay rejected its Hello because its ID was not
// in PeerKeys, and the relay's Reality inbound logged "unknown UUID" for it --
// three unrelated-looking symptoms of one dead credential.
func (a *Agent) reenroll(ctx context.Context, reason string) error {
	if a.Token == "" {
		return fmt.Errorf("head rejected our identity (%s) and no KNOT_TOKEN is set to re-join with", reason)
	}
	// Re-joining uses a token and, if the name is gone from the head, allocates
	// a new VIP. Do not do that on a loop: if the head still rejects a freshly
	// issued identity, something else is wrong and hammering it hides that.
	if !a.lastRejoin.IsZero() && time.Since(a.lastRejoin) < 5*time.Minute {
		return fmt.Errorf("head still rejects our identity (%s) %s after re-joining",
			reason, time.Since(a.lastRejoin).Truncate(time.Second))
	}
	fmt.Fprintf(os.Stderr, "knot: head rejected our identity (%s); re-joining with KNOT_TOKEN\n", reason)
	if err := os.Remove(filepath.Join(a.DataDir, "identity.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.lastRejoin = time.Now()
	a.id, a.etag = Identity{}, ""
	if err := a.load(); err != nil {
		return fmt.Errorf("re-join: %w", err)
	}
	fmt.Fprintf(os.Stderr, "knot: re-joined as %s (vip %s)\n", a.id.NodeID, a.id.VIP)
	// Pull the config for the new identity now rather than waiting 30s: on a
	// cold start this is the first sync, so nothing has been applied yet. This
	// recurses at most once -- a second rejection hits the lastRejoin guard
	// above and comes back as an error.
	return a.sync(ctx)
}

// writeIfChanged writes b only when it differs from what is already there, and
// reports whether it did.
func writeIfChanged(path string, b []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	return true, os.WriteFile(path, b, 0o600)
}

// errorBody pulls the head's {"error": ...} message out of a failed response.
// Reporting the status code alone is what made a mesh-wide config failure
// ("config: HTTP 403") look like a credential problem.
func errorBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return "no message"
}

// reloadSingBox brings the running sing-box in line with the config on disk.
//
// SIGHUP rather than kill-and-restart. sing-box's run loop answers it by
// closing the old instance and only then building a new one from the file, all
// in one process (cmd/sing-box/cmd_run.go), so the tun device is provably
// released before it is reopened. Killing and immediately re-execing raced:
// SIGKILL is asynchronous, so the new process reached ioctl(TUNSETIFF, "knot0")
// while the old one still held it and died at startup with "device or resource
// busy" -- taking the relay's :443, and with it the head behind its Reality
// fallback, down with it.
//
// Live connections are still dropped: a reload closes every inbound and
// outbound. This buys correctness, not seamlessness.
func (a *Agent) reloadSingBox(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.aliveLocked(); c != nil {
		// Check before signalling. sing-box checks too, but on a failed reload
		// it logs one line to its own stderr and silently keeps the old config,
		// so without this knot would report success while nothing changed.
		if err := checkConfig(a.SingBox, a.cfgPath); err != nil {
			return err
		}
		if err := c.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			return fmt.Errorf("reload sing-box: %w", err)
		}
		return nil
	}
	return a.startLocked(ctx)
}

func (a *Agent) startSingBox(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startLocked(ctx)
}

func (a *Agent) startLocked(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Validate before swapping: a bad config would otherwise leave the node
	// with no data plane at all.
	if err := checkConfig(a.SingBox, a.cfgPath); err != nil {
		return err
	}
	a.stopLocked()

	cmd := exec.Command(a.SingBox, "run", "-c", a.cfgPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait() // reap, so a crashed child does not become a zombie
		close(c.done)
	}()
	a.cur = c
	return nil
}

func checkConfig(bin, path string) error {
	out, err := exec.Command(bin, "check", "-c", path).CombinedOutput()
	if err != nil {
		// Include err itself: when the binary is missing or unrunnable there
		// is no output at all, and a bare empty message is impossible to debug.
		return fmt.Errorf("config rejected by sing-box: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *Agent) stopSingBox() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
}

// stopLocked kills the child and waits for it to be reaped. The wait is the
// whole point -- see reloadSingBox.
func (a *Agent) stopLocked() {
	c := a.cur
	a.cur = nil
	if c == nil {
		return
	}
	c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "knot: sing-box did not exit within 10s of SIGKILL")
	}
}

func (a *Agent) singBoxAlive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.aliveLocked() != nil
}

// aliveLocked returns the current child, or nil if there is none or it has
// exited. Reading the done channel rather than cmd.ProcessState: that field is
// written by the reaping goroutine, so touching it here is a data race.
func (a *Agent) aliveLocked() *child {
	if a.cur == nil {
		return nil
	}
	select {
	case <-a.cur.done:
		return nil
	default:
		return a.cur
	}
}

// writeHosts maintains a knot-owned block in /etc/hosts so peers are
// reachable as <name>.knot. Everything outside the markers is preserved.
func writeHosts(block string) error {
	const begin = "# >>> knot >>>"
	const end = "# <<< knot <<<"
	cur, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	s := string(cur)
	if i := strings.Index(s, begin); i >= 0 {
		if j := strings.Index(s[i:], end); j >= 0 {
			s = s[:i] + s[i+j+len(end)+1:]
		}
	}
	s = strings.TrimRight(s, "\n") + "\n" + begin + "\n" + block + end + "\n"
	return os.WriteFile("/etc/hosts", []byte(s), 0o644)
}

func (a *Agent) client() *http.Client {
	return &http.Client{Timeout: 20 * time.Second}
}
