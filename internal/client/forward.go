package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Forward is one local port wired to one address, dialled from one machine.
//
// Node is the machine that does the dialling -- any node in the mesh, relay or
// leaf. Host is resolved and connected BY THAT MACHINE, never here. That is
// what makes this useful for the job it exists for: a production database
// usually answers on a private address or an internal DNS name that a laptop
// cannot resolve and must not be able to reach. We only ever say the name.
//
// The two cases an operator thinks in terms of are the same thing underneath:
// "the database on pg" is Node=pg Host=127.0.0.1, and "the managed database in
// hk's VPC" is Node=hk Host=rds.internal.
type Forward struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Local int    `json:"local"`
	Node  string `json:"node"` // which machine dials Host:Port
	Host  string `json:"host"`
	Port  int    `json:"port"`
	// Exit is the field Node replaced. Read only, to migrate a config written
	// by the first version of this app; see migrateForwards.
	Exit    string `json:"exit,omitempty"`
	Enabled bool   `json:"enabled"`
}

// migrateForwards moves a pre-Node config over.
//
// The old shape named an exit relay and a target address that relay would
// reach, which is exactly what the new shape means by Node and Host -- so this
// is a rename, not a reinterpretation, and the forwards keep working.
func migrateForwards(list []Forward) []Forward {
	for i, f := range list {
		if f.Node == "" && f.Exit != "" {
			list[i].Node = f.Exit
		}
		list[i].Exit = ""
	}
	return list
}

func (f Forward) Validate() error {
	if f.Local < 1 || f.Local > 65535 {
		return fmt.Errorf("本地端口要在 1-65535 之间")
	}
	if f.Local < 1024 {
		// Not a hard error anywhere but macOS, but it is always a mistake here:
		// the whole appeal of this app is that it needs no privileges.
		return fmt.Errorf("本地端口请用 1024 以上，低端口需要 root")
	}
	if f.Node == "" {
		return fmt.Errorf("请选择目标节点")
	}
	if f.Host == "" {
		return fmt.Errorf("请填写目标地址")
	}
	if f.Port < 1 || f.Port > 65535 {
		return fmt.Errorf("目标端口要在 1-65535 之间")
	}
	return nil
}

// Stat is what the panel shows for one forward.
type Stat struct {
	Listening bool   `json:"listening"`
	Active    int64  `json:"active"`
	Total     int64  `json:"total"`
	Up        int64  `json:"up"`
	Down      int64  `json:"down"`
	LastErr   string `json:"last_err,omitempty"`
}

type netConn = net.Conn

// dialFunc opens a connection to host:port as dialled from node.
type dialFunc func(ctx context.Context, node, host string, port int) (netConn, error)

// Manager owns the local listeners.
//
// Opening and closing them is the entire cost of editing a forward -- sing-box
// never learns that anything changed, so the other forwards' connections, and
// the Reality tunnel underneath them, are untouched.
type Manager struct {
	dial dialFunc
	logf func(string, ...any)

	mu   sync.Mutex
	open map[string]*fwdListener
}

func NewManager(dial dialFunc, logf func(string, ...any)) *Manager {
	return &Manager{dial: dial, logf: logf, open: map[string]*fwdListener{}}
}

type fwdListener struct {
	ln     net.Listener
	cancel context.CancelFunc

	mu   sync.Mutex
	spec Forward

	active, total, up, down atomic.Int64
	lastErr                 atomic.Value // string
}

// close stops the listener and its goroutine. Safe on a placeholder, which has
// neither.
func (l *fwdListener) close() {
	if l.cancel != nil {
		l.cancel()
	}
	if l.ln != nil {
		l.ln.Close()
	}
}

func (l *fwdListener) get() Forward {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec
}

func (l *fwdListener) set(f Forward) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spec = f
}

func (l *fwdListener) setErr(err error) {
	if err == nil {
		l.lastErr.Store("")
		return
	}
	l.lastErr.Store(err.Error())
}

func (l *fwdListener) errStr() string {
	s, _ := l.lastErr.Load().(string)
	return s
}

// Apply brings the open listeners in line with list.
//
// A forward whose target changed keeps its socket: only the local port and the
// enabled flag describe the socket itself, and re-binding for a target edit
// would drop connections for no reason. New connections pick up the new target
// because the handler reads the spec per connection.
func (m *Manager) Apply(list []Forward) {
	want := map[string]Forward{}
	for _, f := range list {
		if f.Enabled {
			want[f.ID] = f
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, l := range m.open {
		f, ok := want[id]
		// A placeholder for a bind that failed has no socket and no goroutine.
		// Drop it whatever happens: if the forward is gone we are done, and if
		// it is still wanted we want another attempt -- a port that was busy a
		// minute ago is usually free now, and without this retry the row stays
		// red until somebody edits it.
		if l.ln == nil {
			delete(m.open, id)
			continue
		}
		if !ok || f.Local != l.get().Local {
			l.close()
			delete(m.open, id)
			continue
		}
		l.set(f)
	}
	for id, f := range want {
		if _, ok := m.open[id]; ok {
			continue
		}
		l, err := m.listen(f)
		if err != nil {
			m.logf("转发 %s 监听 127.0.0.1:%d 失败: %v", f.name(), f.Local, err)
			// Remember the failure against the forward itself so the panel can
			// show it. A bare log line is invisible to someone staring at a
			// row that simply never turns green.
			fl := &fwdListener{spec: f}
			fl.setErr(err)
			m.open[id] = fl
			continue
		}
		m.open[id] = l
	}
}

func (f Forward) name() string {
	if f.Label != "" {
		return f.Label
	}
	return fmt.Sprintf("%d -> %s 上的 %s:%d", f.Local, f.Node, f.Host, f.Port)
}

func (m *Manager) listen(f Forward) (*fwdListener, error) {
	// Loopback only. Binding 0.0.0.0 would turn a debugging tunnel into a way
	// for anyone on the café wifi to reach the production database.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.Local))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &fwdListener{ln: ln, cancel: cancel, spec: f}
	l.setErr(nil)
	go m.serve(ctx, l)
	m.logf("转发已开: 127.0.0.1:%d -> %s 上的 %s:%d", f.Local, f.Node, f.Host, f.Port)
	return l, nil
}

func (m *Manager) serve(ctx context.Context, l *fwdListener) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go m.handle(ctx, l, c)
	}
}

func (m *Manager) handle(ctx context.Context, l *fwdListener, c net.Conn) {
	defer c.Close()
	f := l.get()
	l.active.Add(1)
	l.total.Add(1)
	defer l.active.Add(-1)

	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	remote, err := m.dial(dialCtx, f.Node, f.Host, f.Port)
	if err != nil {
		l.setErr(err)
		m.logf("转发 %s: %v", f.name(), err)
		return
	}
	defer remote.Close()
	l.setErr(nil)

	// Copy both ways and leave as soon as either side is done. Closing the
	// other half rather than waiting for it is what stops a half-open
	// connection from pinning a goroutine and a stream for the tunnel's idle
	// timeout.
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(remote, c)
		l.up.Add(n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(c, remote)
		l.down.Add(n)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, l := range m.open {
		l.close()
		delete(m.open, id)
	}
}

func (m *Manager) Stats() map[string]Stat {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Stat, len(m.open))
	for id, l := range m.open {
		out[id] = Stat{
			Listening: l.ln != nil,
			Active:    l.active.Load(),
			Total:     l.total.Load(),
			Up:        l.up.Load(),
			Down:      l.down.Load(),
			LastErr:   l.errStr(),
		}
	}
	return out
}
