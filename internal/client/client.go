// Package client is the ops-workstation side of knot.
//
// It is NOT a mesh member. It holds no mesh address, runs no tun device, and is
// not reachable by anything: it joins the head with a client token, obtains a
// credential, and opens plain local TCP listeners that it carries to a chosen
// machine over the relay protocol.
//
// Three layers, and the split is what makes the thing behave:
//
//   - sing-box does transport ONLY. Its config is one fixed-destination
//     listener per relay, generated at connect time and never touched again.
//   - the relay session (internal/relay, the same code leaves use) carries
//     every forward inside one multiplexed connection per relay, and is where
//     this client proves who it is.
//   - the forwards themselves are ours. Adding or removing one costs a socket,
//     not a sing-box restart -- which matters here in a way it does not on a
//     server: an operator edits forwards all day, and a restart would drop
//     every other forward's live connections.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zbysir/knot/internal/relay"
)

// State is everything the app persists. One file, because all of it changes
// together from the UI and none of it is worth a second format.
type State struct {
	Head     string `json:"head"`
	Insecure bool   `json:"insecure"`

	NodeID string `json:"node_id"`
	Key    string `json:"key"`
	Name   string `json:"name"`

	Forwards []Forward `json:"forwards"`
}

func (s *State) joined() bool { return s.NodeID != "" && s.Key != "" && s.Head != "" }

// Poll is how often the app re-reads its bundle from the head.
//
// Far slower than a node's 10s: nothing here is on anyone's data path, and the
// only thing a poll can discover is that a relay's address or Reality material
// changed -- a manual operation somebody performs once in a while. It is also
// how a revoked credential finds out, but the relay does not wait for us: it
// closes the session the moment its own poll tells it.
const Poll = 30 * time.Second

// applyRetry is how often a connect that never got off the ground is tried
// again.
const applyRetry = 15 * time.Second

type Agent struct {
	DataDir string // default ~/.knot
	SingBox string // optional path override; otherwise discovered
	UIAddr  string // where the local panel listens
	OpenUI  bool   // open a browser once the panel is up
	// LogTo receives the same lines the panel shows. Defaults to stderr; a
	// test or an embedder can silence it without losing the panel's log or the
	// file in DataDir.
	LogTo io.Writer

	log *ring
	// stop ends Run. The panel's quit button is the only way out of a bundled
	// .app, which has no terminal to Ctrl-C and no dock icon to right-click.
	stop context.CancelFunc

	// mu guards everything below. Every mutation arrives either from a UI
	// request goroutine or from the poll loop, so none of it is single
	// threaded.
	mu     sync.Mutex
	st     State
	bundle Bundle
	etag   string
	ports  map[string]int // relay name -> the local port that reaches its knot port
	cfg    []byte         // the sing-box config currently on disk
	child  *child
	fwd    *Manager
	// rc holds one relay session per relay. Replaced wholesale on reconnect.
	rc         *relay.Client
	sessCancel context.CancelFunc
	// sessFor is the identity rc was built with. Re-joining issues a NEW key
	// while leaving the relay list untouched, so the sing-box config comes out
	// byte-identical and nothing else here notices -- the sessions would keep
	// presenting the revoked credential for ever, and the relay would keep
	// refusing it. Found exactly that way.
	sessFor string

	uiAddr    string // the address the panel actually bound
	status    string
	lastErr   string
	lastApply time.Time
	// paused is set by Disconnect. Without it the supervisor notices sing-box
	// is not running two seconds later and starts it again, so the button
	// appears to do nothing.
	paused bool

	// procMu guards the whole of every start/stop, so the supervisor cannot
	// start a second sing-box while a sync is replacing one -- two of them
	// would fight over the same loopback ports and the loser dies with a bind
	// error that looks like a config fault.
	procMu sync.Mutex
}

// child is one sing-box process and a channel closed once it has been reaped.
// Same shape as the node agent's, and for the same reason: a stop has to be
// synchronous or the replacement races the corpse for the listen ports.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func New() *Agent {
	return &Agent{
		DataDir: defaultDataDir(),
		UIAddr:  "127.0.0.1:8765",
		OpenUI:  true,
		log:     newRing(400),
		ports:   map[string]int{},
		status:  "未连接",
	}
}

func defaultDataDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ".knot"
	}
	return filepath.Join(h, ".knot")
}

func (a *Agent) statePath() string  { return filepath.Join(a.DataDir, "state.json") }
func (a *Agent) bundlePath() string { return filepath.Join(a.DataDir, "bundle.json") }
func (a *Agent) cfgPath() string    { return filepath.Join(a.DataDir, "singbox.json") }

// Run brings the app up and blocks until ctx is done.
//
// The panel starts FIRST and unconditionally. Before the first join there is no
// identity, no bundle and no tunnel -- but the whole point of the app is that
// the operator pastes the head address and token into a window, so that window
// has to exist before any of the rest can.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.stop = cancel
	if err := os.MkdirAll(a.DataDir, 0o700); err != nil {
		return err
	}
	a.loadState()
	a.fwd = NewManager(a.dialForward, a.logf)
	defer a.fwd.CloseAll()
	defer a.stopSessions()
	defer a.stopSingBox()

	ui, err := a.serveUI(ctx)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			// Hand the window to the copy that is already up and get out of
			// its way, without touching its state directory.
			if a.OpenUI {
				exec.Command("open", "http://"+a.UIAddr).Start()
			}
			fmt.Fprintf(os.Stderr, "knot: 已经有一个客户端在 %s 运行，打开它的面板\n", a.UIAddr)
			return nil
		}
		return err
	}
	defer ui.Close()
	a.logf("面板 %s", a.PanelURL())
	if a.OpenUI {
		exec.Command("open", a.PanelURL()).Start()
	}

	if a.joined() {
		// A failure here is not fatal: the panel is up, it will say what went
		// wrong, and the loop below retries. Exiting would leave the operator
		// with a dead icon and no way to see why.
		if err := a.connect(ctx); err != nil {
			a.setErr("连接失败: %v", err)
		}
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	last := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if !a.joined() || a.isPaused() {
				continue
			}
			a.superviseOnce(ctx)
			if time.Since(last) >= Poll {
				last = time.Now()
				if err := a.sync(ctx); err != nil {
					a.logf("同步失败（保持当前配置）: %v", err)
				}
			}
		}
	}
}

func (a *Agent) isPaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused
}

func (a *Agent) joined() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.joined()
}

// ------------------------------------------------------------------- state

func (a *Agent) loadState() {
	b, err := os.ReadFile(a.statePath())
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	json.Unmarshal(b, &a.st)
	a.st.Forwards = migrateForwards(a.st.Forwards)
}

// saveLocked persists under the lock the caller already holds.
func (a *Agent) saveLocked() error {
	b, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath())
}

// ------------------------------------------------------------------- join

// Join enrols with the head and brings the tunnel up.
//
// The token decides what this becomes; we only check the answer. A node token
// would hand the laptop a mesh address and put it in every other node's hosts
// file and routing table -- working, but exactly the thing this app exists not
// to do -- so that is refused here rather than discovered later.
func (a *Agent) Join(ctx context.Context, head, token, name string, insecure bool) error {
	head = strings.TrimRight(strings.TrimSpace(head), "/")
	if head == "" {
		return fmt.Errorf("请填写 head 地址")
	}
	if !strings.HasPrefix(head, "http://") && !strings.HasPrefix(head, "https://") {
		head = "https://" + head
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("请填写接入令牌")
	}
	if name == "" {
		h, _ := os.Hostname()
		name = h
	}

	body, _ := json.Marshal(map[string]string{"token": strings.TrimSpace(token), "name": name})
	resp, err := httpClient(insecure).Post(head+"/api/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("连接 head: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("head 拒绝接入: %s", errorBody(resp))
	}
	var id struct {
		NodeID  string    `json:"node_id"`
		Key     string    `json:"key"`
		Role    string    `json:"role"`
		Expires time.Time `json:"expires"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		return err
	}
	if id.NodeID == "" {
		return fmt.Errorf("head 没有返回身份")
	}
	if id.Role != "client" {
		return fmt.Errorf("这是一个节点令牌，不是客户端令牌 —— 用它接入会把这台机器加进网络。" +
			"请在 head 面板里生成「客户端」令牌")
	}

	a.mu.Lock()
	a.st.Head, a.st.Insecure = head, insecure
	a.st.NodeID, a.st.Key, a.st.Name = id.NodeID, id.Key, name
	a.etag = ""
	err = a.saveLocked()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if id.Expires.IsZero() {
		a.logf("已接入 %s", head)
	} else {
		a.logf("已接入 %s，凭证有效期至 %s", head, id.Expires.Format("2006-01-02 15:04"))
	}
	// The identity is the part that consumes a token and cannot be redone
	// cheaply, so once it is saved the join has succeeded -- even if the tunnel
	// then fails to come up. Returning the connect error here instead made the
	// panel show "接入失败" over a join that had in fact worked, and the obvious
	// next move (press it again) spends a second token.
	if err := a.connect(ctx); err != nil {
		a.setErr("已接入，但连接失败: %v", err)
	}
	return nil
}

// Forget drops the identity and everything derived from it. Forwards are kept:
// they describe what the operator wants to reach, not who they are, and
// re-joining to find them gone is a small cruelty.
func (a *Agent) Forget() {
	a.Disconnect()
	a.mu.Lock()
	a.st.Head, a.st.NodeID, a.st.Key, a.st.Name = "", "", "", ""
	a.etag = ""
	a.bundle = Bundle{}
	a.saveLocked()
	a.mu.Unlock()
	os.Remove(a.bundlePath())
	a.setStatus("未连接")
	a.logf("已注销身份")
}

// ---------------------------------------------------------------- lifecycle

func (a *Agent) connect(ctx context.Context) error {
	a.mu.Lock()
	a.paused = false
	a.mu.Unlock()
	a.setStatus("连接中")
	if _, err := a.refreshBundle(ctx); err != nil {
		// Fall back to the cached bundle: a head that is briefly unreachable
		// should not stop an operator from reaching a database.
		if !a.loadBundleCache() {
			a.setStatus("未连接")
			a.setErr("%v", err)
			return err
		}
		a.logf("head 不可达，使用上次缓存的网络信息: %v", err)
	}
	if err := a.applyBundle(ctx); err != nil {
		a.setStatus("未连接")
		a.setErr("%v", err)
		return err
	}
	return nil
}

// Disconnect stops the tunnel and every forward, keeping the identity.
func (a *Agent) Disconnect() {
	a.mu.Lock()
	a.paused = true
	a.mu.Unlock()
	a.fwd.CloseAll()
	a.stopSessions()
	a.stopSingBox()
	a.setStatus("已断开")
	a.logf("已断开")
}

// Reconnect is what the panel's button calls: full stop, full start.
func (a *Agent) Reconnect(ctx context.Context) error {
	a.fwd.CloseAll()
	a.stopSessions()
	a.stopSingBox()
	a.mu.Lock()
	a.etag, a.cfg = "", nil
	a.mu.Unlock()
	return a.connect(ctx)
}

// sync pulls the bundle and applies it if it changed.
func (a *Agent) sync(ctx context.Context) error {
	changed, err := a.refreshBundle(ctx)
	if err != nil || !changed {
		return err
	}
	return a.applyBundle(ctx)
}

// refreshBundle pulls from the head and reports whether anything differs from
// what we already hold.
//
// Kept separate from applying it, because the two fail for unrelated reasons
// and the operator needs to be told which: "head 不可达" and "sing-box 没装"
// have nothing to do with each other, and folding them into one error reported
// a missing binary as a network problem.
func (a *Agent) refreshBundle(ctx context.Context) (bool, error) {
	a.mu.Lock()
	st, etag := a.st, a.etag
	a.mu.Unlock()
	if !st.joined() {
		return false, fmt.Errorf("尚未接入")
	}

	url := fmt.Sprintf("%s/api/client?id=%s&key=%s", st.Head, st.NodeID, st.Key)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := httpClient(st.Insecure).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return false, nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		// Revoked, expired, or the head's state file was replaced. Unlike a
		// node we hold no token to re-join with, so say it plainly and let the
		// operator paste a new one.
		a.setErr("head 不再接受这个身份（%s），请重新接入", errorBody(resp))
		return false, fmt.Errorf("凭证已失效")
	case resp.StatusCode == 404:
		return false, fmt.Errorf("这个 head 还不支持客户端（没有 /api/client），先升级 head")
	case resp.StatusCode != 200:
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errorBody(resp))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, err
	}
	bundle, err := ParseBundle(raw)
	if err != nil {
		return false, err
	}

	a.mu.Lock()
	a.bundle = bundle
	a.etag = resp.Header.Get("ETag")
	a.mu.Unlock()
	os.WriteFile(a.bundlePath(), raw, 0o600)
	return true, nil
}

func (a *Agent) loadBundleCache() bool {
	b, err := os.ReadFile(a.bundlePath())
	if err != nil {
		return false
	}
	bundle, err := ParseBundle(b)
	if err != nil {
		return false
	}
	a.mu.Lock()
	a.bundle = bundle
	a.mu.Unlock()
	return true
}

// applyBundle regenerates the sing-box config, restarts sing-box only if the
// config actually changed, then re-establishes sessions and forwards.
func (a *Agent) applyBundle(ctx context.Context) error {
	a.mu.Lock()
	bundle := a.bundle
	// Give every relay its own loopback port. Ports are only re-probed when a
	// relay appears or disappears; a stable mesh keeps the same ports across
	// polls and therefore produces a byte-identical config.
	ports := map[string]int{}
	for _, r := range bundle.Relays {
		if p, ok := a.ports[r.Name]; ok {
			ports[r.Name] = p
			continue
		}
		p, err := freePort()
		if err != nil {
			a.mu.Unlock()
			return err
		}
		ports[r.Name] = p
	}
	a.ports = ports
	a.mu.Unlock()

	cfg, err := GenerateConfig(bundle, ports)
	if err != nil {
		return err
	}

	a.mu.Lock()
	same := bytes.Equal(a.cfg, cfg)
	a.mu.Unlock()
	a.mu.Lock()
	staleIdentity := a.sessFor != identityOf(a.st)
	a.mu.Unlock()

	if !same || !a.singBoxAlive() {
		if err := os.WriteFile(a.cfgPath(), cfg, 0o600); err != nil {
			return err
		}
		if err := a.startSingBox(ctx); err != nil {
			a.setStatus("sing-box 启动失败")
			return err
		}
		a.mu.Lock()
		a.cfg = cfg
		a.mu.Unlock()
		// sing-box owns the loopback ports the sessions dial, so sessions
		// established against the old process are gone with it.
		a.startSessions(ctx)
	} else if !a.sessionsRunning() || staleIdentity {
		a.startSessions(ctx)
	}
	a.setStatus("已连接")
	a.clearErr()
	a.applyForwards()
	return nil
}

// applyForwards hands the manager the current list. It is idempotent: a forward
// that is already open and unchanged is left alone, so calling this on every
// poll costs nothing.
func (a *Agent) applyForwards() {
	a.mu.Lock()
	list := append([]Forward(nil), a.st.Forwards...)
	paused := a.paused
	a.mu.Unlock()
	if paused {
		// Editing forwards while disconnected is allowed -- that is the point
		// of keeping them -- but opening their sockets is not. A green row over
		// a tunnel that is down only produces a connection that hangs and then
		// fails, which is a worse answer than a closed port.
		a.fwd.Apply(nil)
		return
	}
	a.fwd.Apply(list)
}

// ---------------------------------------------------------------- sessions

// startSessions opens one relay session per relay and keeps them up.
//
// This is where the client proves who it is. The Reality credential the
// sing-box config carries is shared by every client and reaches nothing but the
// port on the far side of this dial; the identity that matters is the node id
// and key sent in the relay protocol's Hello, which the relay checks against a
// list it reloads without restarting anything. That is what makes issuing and
// revoking a laptop free.
func (a *Agent) startSessions(ctx context.Context) {
	a.stopSessions()

	a.mu.Lock()
	st, bundle := a.st, a.bundle
	a.mu.Unlock()

	sctx, cancel := context.WithCancel(ctx)
	rc := &relay.Client{
		NodeID: st.NodeID,
		Key:    st.Key,
		Logf:   a.logf,
		// We are a client: nothing may be pushed down at us. The relay does not
		// register us anywhere it could be pushed FROM, so this is the second
		// lock on the same door.
		NoInbound: true,
		NameOf:    func(k string) string { return k },
		Dial: func(ctx context.Context, relayName string) (net.Conn, error) {
			a.mu.Lock()
			port, ok := a.ports[relayName]
			a.mu.Unlock()
			if !ok {
				return nil, fmt.Errorf("中继 %s 没有本地端口", relayName)
			}
			// A plain loopback dial. sing-box's listener on this port has a
			// fixed destination -- the relay's knot port -- so there is no
			// address to negotiate and nothing here has to know about TLS.
			var d net.Dialer
			return d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
		},
	}
	for _, r := range bundle.Relays {
		go rc.Maintain(sctx, r.Name)
	}

	a.mu.Lock()
	a.rc, a.sessCancel = rc, cancel
	a.sessFor = identityOf(st)
	a.mu.Unlock()
}

// identityOf fingerprints the credential the sessions authenticate with.
func identityOf(st State) string { return st.NodeID + "\x00" + st.Key }

func (a *Agent) stopSessions() {
	a.mu.Lock()
	cancel := a.sessCancel
	a.rc, a.sessCancel, a.sessFor = nil, nil, ""
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *Agent) sessionsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rc != nil
}

// dialForward opens one forwarded connection: ask a relay to reach host:port
// from the machine the forward names.
//
// Resolving the node here rather than when the forward was created means a node
// that has gone away produces an error the operator can read, instead of a
// forward silently pinned to something that no longer exists.
func (a *Agent) dialForward(ctx context.Context, node, host string, port int) (netConn, error) {
	a.mu.Lock()
	rc, bundle := a.rc, a.bundle
	a.mu.Unlock()
	if rc == nil {
		return nil, fmt.Errorf("隧道未运行")
	}
	dst, ok := bundle.Node(node)
	if !ok {
		return nil, fmt.Errorf("节点 %q 不在这个网络里", node)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var lastErr error
	try := func(relayName string) (netConn, bool) {
		if !rc.Up(relayName) {
			lastErr = fmt.Errorf("到中继 %s 的会话未建立", relayName)
			return nil, false
		}
		c, err := rc.Open(relayName, dst.ID, addr)
		if err != nil {
			lastErr = err
			return nil, false
		}
		return c, true
	}

	// If the target machine is itself a relay we hold a session to, ask it
	// directly -- there is no reason to bounce our own traffic off a third
	// machine.
	if _, isRelay := bundle.Relay(node); isRelay {
		if c, ok := try(node); ok {
			return c, nil
		}
	}
	for _, r := range bundle.Relays {
		if r.Name == node {
			continue // already tried above
		}
		if c, ok := try(r.Name); ok {
			return c, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的中继")
	}
	return nil, fmt.Errorf("到 %s 的 %s: %w", node, addr, lastErr)
}

// --------------------------------------------------------------- sing-box

// superviseOnce keeps the data plane in the state the operator asked for.
func (a *Agent) superviseOnce(ctx context.Context) {
	a.mu.Lock()
	haveCfg := a.cfg != nil
	haveBundle := len(a.bundle.Relays) > 0
	retryDue := time.Since(a.lastApply) > applyRetry
	a.mu.Unlock()

	if !haveCfg {
		// Nothing was ever applied. Overwhelmingly the reason is that sing-box
		// was not installed when we first tried, so retrying here is what makes
		// `brew install sing-box` sufficient on its own -- without it the app
		// sits there with a red banner until somebody guesses that the 重连
		// button is now required.
		if !haveBundle || !retryDue {
			return
		}
		a.mu.Lock()
		a.lastApply = time.Now()
		a.mu.Unlock()
		if err := a.applyBundle(ctx); err != nil {
			a.setErr("%v", err)
		}
		return
	}
	if a.singBoxAlive() {
		return
	}
	a.logf("sing-box 不在运行，重启")
	if err := a.startSingBox(ctx); err != nil {
		a.setErr("sing-box 重启失败: %v", err)
		return
	}
	a.startSessions(ctx)
}

func (a *Agent) startSingBox(ctx context.Context) error {
	a.procMu.Lock()
	defer a.procMu.Unlock()
	return a.startLocked(ctx)
}

func (a *Agent) startLocked(ctx context.Context) error {
	bin, err := FindSingBox(a.SingBox)
	if err != nil {
		return err
	}
	// Validate before swapping. A config sing-box rejects would otherwise leave
	// the operator with no tunnel and a process that exited too fast to read.
	if out, err := exec.Command(bin, "check", "-c", a.cfgPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("sing-box 不接受这份配置: %v: %s", err, strings.TrimSpace(string(out)))
	}
	a.stopLocked()

	cmd := exec.Command(bin, "run", "-c", a.cfgPath())
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
	pw.Close() // the child holds its own end; we now see EOF exactly when it exits
	go func() {
		defer pr.Close()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			a.logf("sing-box: %s", ansi.ReplaceAllString(sc.Text(), ""))
		}
	}()

	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(c.done)
	}()
	a.mu.Lock()
	a.child = c
	a.mu.Unlock()
	a.logf("sing-box 已启动 (%s)", bin)
	return nil
}

func (a *Agent) stopSingBox() {
	a.procMu.Lock()
	defer a.procMu.Unlock()
	a.stopLocked()
}

func (a *Agent) stopLocked() {
	a.mu.Lock()
	c := a.child
	a.child = nil
	a.mu.Unlock()
	if c == nil {
		return
	}
	c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
		return
	case <-time.After(5 * time.Second):
	}
	c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
}

func (a *Agent) singBoxAlive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.child == nil {
		return false
	}
	select {
	case <-a.child.done:
		return false
	default:
		return true
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ------------------------------------------------------------------ helpers

func (a *Agent) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

func (a *Agent) setErr(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	a.mu.Lock()
	a.lastErr = msg
	a.mu.Unlock()
	a.logf("%s", msg)
}

func (a *Agent) clearErr() {
	a.mu.Lock()
	a.lastErr = ""
	a.mu.Unlock()
}

func (a *Agent) logf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	a.log.add(time.Now().Format("15:04:05") + " " + msg)
	w := a.LogTo
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintln(w, msg)
	// Also to a file. Launched from a .app there is no stderr to read, and the
	// in-memory ring dies with the process -- which is exactly the case where
	// somebody needs to know why it would not start.
	a.appendLog(time.Now().Format("2006-01-02 15:04:05") + " " + msg)
}

// logCap keeps the file from growing without bound on a laptop that never
// restarts the app. Truncating rather than rotating: this is a debugging aid,
// not an audit trail.
const logCap = 1 << 20

func (a *Agent) appendLog(line string) {
	path := filepath.Join(a.DataDir, "knot.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > logCap {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func httpClient(insecure bool) *http.Client {
	c := &http.Client{Timeout: 20 * time.Second}
	if insecure {
		c.Transport = insecureTransport()
	}
	return c
}

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
	return "无错误信息"
}

// ring keeps the last n log lines for the panel. Bounded on purpose: this
// process can run for days on a laptop that never restarts it.
type ring struct {
	mu  sync.Mutex
	n   int
	buf []string
}

func newRing(n int) *ring { return &ring{n: n} }

func (r *ring) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, s)
	if len(r.buf) > r.n {
		r.buf = r.buf[len(r.buf)-r.n:]
	}
}

func (r *ring) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.buf...)
}
