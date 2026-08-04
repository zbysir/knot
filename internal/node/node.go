// Package node is the agent that runs on every machine in the mesh.
//
// It joins the head once with a token, then polls for its sing-box config and
// keeps a sing-box child process in sync with it. Everything it needs to
// persist lives in one small identity file, so re-creating the container is
// harmless: it re-joins under the same name and gets the same VIP back.
package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	SingBox  string        // path to the sing-box binary
	Insecure bool          // skip TLS verification against the head
	Poll     time.Duration // config poll interval; defaults to DefaultPoll

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

// DefaultPoll is how often a node asks the head for its config.
//
// Two seconds, not the 30 it used to be. Everything that follows a change to the
// mesh is gated on this: a relay cannot accept a node that joined after its last
// poll, so with a 30s interval a new node spent up to half a minute being
// rejected by the relay with nothing in either log to say why. The cost is one
// conditional GET per node per 2s, answered by a 304 that touches no disk.
const DefaultPoll = 2 * time.Second

// MinPoll keeps a mistaken KNOT_POLL from turning every node into a spin loop
// against the head.
const MinPoll = time.Second

func (a *Agent) poll() time.Duration {
	switch {
	case a.Poll <= 0:
		return DefaultPoll
	case a.Poll < MinPoll:
		return MinPoll
	default:
		return a.Poll
	}
}

// logTime matches the date and time sing-box prints, so the two halves of a
// container's log line up.
const logTime = "2006-01-02 15:04:05"

// logf writes one timestamped knot line to stderr.
//
// Timestamped because these lines are read interleaved with sing-box's, which
// are -- and an undated "relay: session ended" next to a dated sing-box error is
// impossible to correlate with the thing that caused it. Reading one relay
// outage backwards was enough.
func logf(format string, v ...any) { logTo(os.Stderr, format, v...) }

func logTo(w io.Writer, format string, v ...any) {
	fmt.Fprintf(w, "%s knot: %s\n", time.Now().Format(logTime), fmt.Sprintf(format, v...))
}

// ansi matches the colour escapes sing-box emits even when its output is a pipe.
// Unreadable in `docker logs`, and they make every line grep-hostile.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// realityProbe matches the one sing-box message that says nothing about this
// node: Reality's answer to a connection that is not an authenticated client.
var realityProbe = regexp.MustCompile(
	`process connection from (\S+):\d+: TLS handshake: REALITY: processed invalid connection`)

// probeReportEvery is how often the suppressed probes are summarised.
const probeReportEvery = 5 * time.Minute

// pipeSingBoxLog copies sing-box's output to ours, stripping colour and
// collapsing Reality's rejection notices into a periodic summary.
//
// A relay's :443 is scanned continuously -- 53 distinct addresses in six minutes
// on ours -- and sing-box reports every one of those at ERROR level. It is not
// an error and not about us, and the volume is what let a real FATAL sit
// unnoticed in the log for hours.
//
// Counted rather than dropped, because the identical message is what a node
// whose cached config holds a stale Reality public key would get. Many addresses
// once each is the internet; one address over and over is a node of yours, and
// the summary names it.
func pipeSingBoxLog(r io.Reader, out io.Writer, every time.Duration) {
	lines := make(chan string, 512)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // sing-box prints long lines
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	probes := map[string]int{}
	since := time.Now()
	report := func() {
		if len(probes) > 0 {
			total, worst, worstN := 0, "", 0
			for addr, n := range probes {
				total += n
				if n > worstN || (n == worstN && addr < worst) {
					worst, worstN = addr, n
				}
			}
			logTo(out, "reality: rejected %d unauthenticated handshakes in %s from %d addresses (most: %s x%d)",
				total, time.Since(since).Truncate(time.Second), len(probes), worst, worstN)
			probes = map[string]int{}
		}
		since = time.Now()
	}

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				report() // sing-box exited; do not lose the tail
				return
			}
			line = ansi.ReplaceAllString(line, "")
			if m := realityProbe.FindStringSubmatch(line); m != nil {
				probes[m[1]]++
				continue
			}
			fmt.Fprintln(out, line)
		case <-t.C:
			report()
		}
	}
}

func (a *Agent) Run(ctx context.Context) error {
	logf("node %q -> %s", a.Name, a.Head)
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
		logf("initial sync failed (%v)", err)
		logf("starting from cached config")
		if b, rerr := os.ReadFile(a.planPath); rerr == nil {
			var p Plan
			if json.Unmarshal(b, &p) == nil {
				if err := a.applyPlan(p); err != nil {
					logf("cached relay plan failed: %v", err)
				}
			}
		}
		if err := a.restartSingBox(ctx); err != nil {
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

	t := time.NewTicker(a.poll())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := a.sync(ctx); err != nil {
				// A head outage must not take the data plane down: sing-box
				// keeps running with the config it already has.
				logf("sync failed (keeping current config): %v", err)
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
		logf("sing-box is not running, restarting it")
		if err := a.startSingBox(ctx); err != nil {
			logf("sing-box restart failed (retry in %s): %v", wait, err)
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
	p := a.identityPath()
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &a.id); err == nil && a.id.NodeID != "" {
			// KNOT_HEAD wins over the address recorded at join time. Without
			// this, changing the env var does nothing at all -- sync() reads
			// the persisted copy -- and moving the head (say, from its public
			// address to its mesh address) silently has no effect.
			if a.Head != "" && a.Head != a.id.Head {
				logf("head address changed %s -> %s", a.id.Head, a.Head)
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
	id, err := a.join()
	if err != nil {
		return err
	}
	return a.saveIdentity(id)
}

func (a *Agent) identityPath() string { return filepath.Join(a.DataDir, "identity.json") }

// headURL is where the head is, in priority order.
//
// KNOT_HEAD first and the identity second, never the other way round: the
// identity can be empty or half-written, and deriving the URL from it produced
// requests to `/api/config?id=&key=` -- "unsupported protocol scheme" -- with no
// way back.
func (a *Agent) headURL() string {
	if a.Head != "" {
		return a.Head
	}
	return a.id.Head
}

// join enrols with the head and returns the identity it hands back, touching
// neither the Agent nor the disk. The caller decides whether to keep it, which
// is what lets a re-join that fails leave a working node exactly as it was.
func (a *Agent) join() (Identity, error) {
	body, _ := json.Marshal(map[string]string{
		"token": a.Token, "name": a.Name, "endpoint": a.Endpoint,
	})
	resp, err := a.client().Post(a.headURL()+"/api/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("join: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Identity{}, fmt.Errorf("join rejected: %s", errorBody(resp))
	}
	var id Identity
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		return Identity{}, err
	}
	if id.NodeID == "" {
		return Identity{}, fmt.Errorf("join returned no node id")
	}
	id.Head, id.Name = a.headURL(), a.Name
	return id, nil
}

// saveIdentity persists an identity and adopts it, in that order.
func (a *Agent) saveIdentity(id Identity) error {
	b, _ := json.MarshalIndent(id, "", "  ")
	if err := os.WriteFile(a.identityPath(), b, 0o600); err != nil {
		return err
	}
	a.id = id
	return nil
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
	url := fmt.Sprintf("%s/api/config?id=%s&key=%s", a.headURL(), a.id.NodeID, a.id.Key)
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
		logf("hosts update failed: %v", err)
	}

	// Relay first: sing-box will start forwarding peer traffic into the SOCKS
	// listener as soon as it comes up, so that listener has to exist already.
	if a.planDirty || a.relay == nil {
		if err := a.applyPlan(cr.Relay); err != nil {
			logf("relay plan failed: %v", err)
		} else {
			a.planDirty = false
		}
	}
	if a.cfgDirty || !a.singBoxAlive() {
		if err := a.restartSingBox(ctx); err != nil {
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

// reenroll handles the head saying it does not know this identity, which happens
// when the node was deleted from the panel or the head's state file was replaced.
//
// This used to be terminal in a way that was hard to see: load() returns as soon
// as identity.json has a node ID, so KNOT_TOKEN was never consulted again. The
// node polled forever, the relay rejected its Hello because its ID was not in
// PeerKeys, and the relay's Reality inbound logged "unknown UUID" for it -- three
// unrelated-looking symptoms of one dead credential.
//
// Nothing is thrown away until the head has handed over a new identity. The first
// version of this deleted identity.json and zeroed the in-memory copy up front,
// so a re-join that failed -- a consumed one-shot token, the usual case -- left
// the node with no identity AND no head URL, and every later poll went to
// `/api/config?id=&key=` with "unsupported protocol scheme". Permanently: there
// is no path back from that without re-creating the container.
func (a *Agent) reenroll(ctx context.Context, reason string) error {
	if a.Token == "" {
		return fmt.Errorf("head rejected our identity (%s) and no KNOT_TOKEN is set to re-join with", reason)
	}
	// Re-joining spends a token and, if the name is gone from the head, takes a
	// new VIP. Do not do that on a loop: if the head still rejects us, something
	// else is wrong and hammering it hides that.
	if !a.lastRejoin.IsZero() && time.Since(a.lastRejoin) < 5*time.Minute {
		return fmt.Errorf("head rejected our identity (%s); last re-join attempt was %s ago",
			reason, time.Since(a.lastRejoin).Truncate(time.Second))
	}
	logf("head rejected our identity (%s); re-joining with KNOT_TOKEN", reason)
	a.lastRejoin = time.Now()
	id, err := a.join()
	if err != nil {
		// Identity, config and data plane are all untouched; we keep polling and
		// keep saying so until someone issues a usable token.
		return fmt.Errorf("re-join: %w", err)
	}
	if err := a.saveIdentity(id); err != nil {
		return err
	}
	a.etag = ""
	logf("re-joined as %s (vip %s)", a.id.NodeID, a.id.VIP)
	// Pull the config for the new identity now rather than waiting for the next
	// tick: on a cold start this is the first sync, so nothing has been applied
	// yet. This recurses at most once -- a second rejection hits the guard above.
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

// restartSingBox brings sing-box in line with the config on disk by replacing
// the process. startLocked stops the old child and waits for it to be reaped
// first, which is what keeps the two of them off the same tun device.
//
// NOT SIGHUP, which sing-box does support: its run loop closes the instance and
// rebuilds it from the config file without leaving the process
// (cmd/sing-box/cmd_run.go). We shipped that and watched it fail in production.
// instance.Close() does not fully release the tun inbound, the rebuild died on
// ioctl(TUNSETIFF, "knot0") with "device or resource busy", and because the
// SIGHUP branch discards the Close error it went ahead with the rebuild anyway
// and then exited -- so a config change killed sing-box instead of reloading it.
// The supervisor recovered in 68ms, but a deliberate replacement is better than
// a crash plus a rescue.
//
// Live connections are dropped either way; nothing here can avoid that.
func (a *Agent) restartSingBox(ctx context.Context) error {
	return a.startSingBox(ctx)
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
	// Both streams through one pipe so nothing bypasses the filter. An os.Pipe
	// rather than cmd.StderrPipe(): that one is closed by cmd.Wait(), which runs
	// in its own goroutine here and would race the reader.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return err
	}
	// Drop our copy of the write end: the child holds its own, so the reader now
	// sees EOF exactly when the child exits.
	pw.Close()
	go func() {
		defer pr.Close()
		pipeSingBoxLog(pr, os.Stderr, probeReportEvery)
	}()

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

// stopLocked ends the child and waits for it to be reaped.
//
// The wait is the whole point. Signals are asynchronous, and the tun device
// stays open until the kernel has finished tearing the process down -- start the
// replacement before that and it dies on ioctl(TUNSETIFF, "knot0") with "device
// or resource busy". On a relay that means losing :443, and with it the head
// behind the Reality fallback.
//
// SIGTERM first so sing-box closes its own inbounds; SIGKILL only if it will not
// go.
func (a *Agent) stopLocked() {
	c := a.cur
	a.cur = nil
	if c == nil {
		return
	}
	c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
		return
	case <-time.After(5 * time.Second):
	}
	logf("sing-box ignored SIGTERM, killing it")
	c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		logf("sing-box did not exit within 10s of SIGKILL")
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
