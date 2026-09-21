package client

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed panel.html
var assets embed.FS

// serveUI starts the local panel and returns its listener.
//
// Loopback only, and two guards on top of that. A page on any website the
// operator happens to have open can issue requests to 127.0.0.1, so:
//
//   - every mutation requires Content-Type: application/json, which forces a
//     CORS preflight that a cross-origin page cannot satisfy. A plain HTML form
//     post, which needs no preflight, is therefore refused.
//   - the Host header must be the address we bound. Without that check, a
//     domain resolving to 127.0.0.1 (DNS rebinding) reaches us as same-origin.
//
// Neither is theatre: what is behind this panel is a live path into production.
func (a *Agent) serveUI(ctx context.Context) (net.Listener, error) {
	host, _, err := net.SplitHostPort(a.UIAddr)
	if err != nil {
		return nil, fmt.Errorf("面板地址 %q: %w", a.UIAddr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("面板只能监听回环地址，收到 %q", host)
	}
	ln, err := net.Listen("tcp", a.UIAddr)
	if err != nil {
		// Double-clicking the icon of an app that is already running is the
		// normal way people re-open a window. Without this the second launch
		// dies on "address already in use" -- invisibly, because a bundle has
		// no terminal -- and the operator concludes the app is broken.
		if alreadyRunning(a.UIAddr) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("面板监听 %s: %w", a.UIAddr, err)
	}
	// Record the RESOLVED address separately instead of writing it back to the
	// exported field. UIAddr may be ":0" or any port the caller left to the
	// kernel, and overwriting a field the caller owns is a data race with
	// whoever set it.
	a.mu.Lock()
	a.uiAddr = ln.Addr().String()
	a.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := assets.ReadFile("panel.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", a.guard(a.handleState))
	mux.HandleFunc("POST /api/join", a.guard(a.handleJoin(ctx)))
	mux.HandleFunc("POST /api/forget", a.guard(a.handleForget))
	mux.HandleFunc("POST /api/reconnect", a.guard(a.handleReconnect(ctx)))
	mux.HandleFunc("POST /api/disconnect", a.guard(a.handleDisconnect))
	mux.HandleFunc("POST /api/forward", a.guard(a.handleForward))
	mux.HandleFunc("POST /api/forward/delete", a.guard(a.handleForwardDelete))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	return ln, nil
}

// ErrAlreadyRunning says another copy of the app owns the panel address.
var ErrAlreadyRunning = errors.New("knot 客户端已经在运行")

func alreadyRunning(addr string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/api/state")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// panelAddr is the address the panel actually bound.
func (a *Agent) panelAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uiAddr
}

// PanelURL is where the operator's browser should go. Empty until the panel is
// listening.
func (a *Agent) PanelURL() string {
	if addr := a.panelAddr(); addr != "" {
		return "http://" + addr
	}
	return ""
}

func (a *Agent) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Host != a.panelAddr() {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.Method == "POST" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}

// ------------------------------------------------------------------- views

type uiRelay struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	SNI      string `json:"sni"`
	Up       bool   `json:"up"` // is the relay session established
}

type uiForward struct {
	Forward
	Stat Stat `json:"stat"`
}

type uiState struct {
	Joined   bool         `json:"joined"`
	Head     string       `json:"head"`
	Name     string       `json:"name"`
	Status   string       `json:"status"`
	Error    string       `json:"error,omitempty"`
	SingBox  bool         `json:"singbox"`
	Relays   []uiRelay    `json:"relays"`
	Nodes    []BundleNode `json:"nodes"`
	Forwards []uiForward  `json:"forwards"`
	Logs     []string     `json:"logs"`
}

func (a *Agent) handleState(w http.ResponseWriter, r *http.Request) {
	stats := a.fwd.Stats()
	alive := a.singBoxAlive()

	a.mu.Lock()
	rc := a.rc
	out := uiState{
		Joined:  a.st.joined(),
		Head:    a.st.Head,
		Name:    a.st.Name,
		Status:  a.status,
		Error:   a.lastErr,
		SingBox: alive,
	}
	// "已连接" means sing-box started, which is not the same as being able to
	// reach anything: a revoked credential leaves every session closed while
	// the process runs happily. Report what the sessions actually say, so the
	// panel does not claim a tunnel that is not there.
	if out.Status == "已连接" {
		up := 0
		for _, r := range a.bundle.Relays {
			if rc != nil && rc.Up(r.Name) {
				up++
			}
		}
		if up == 0 {
			out.Status = "隧道未建立"
		}
	}
	// Relays are projected, never marshalled directly: the Relay struct carries
	// the shared door uuid and the relays' Reality material, and none of that
	// has any business being in a JSON body any local process can fetch.
	for _, r := range a.bundle.Relays {
		out.Relays = append(out.Relays, uiRelay{
			Name:     r.Name,
			Endpoint: r.Endpoint,
			SNI:      r.ServerName,
			Up:       rc != nil && rc.Up(r.Name),
		})
	}
	out.Nodes = append(out.Nodes, a.bundle.Nodes...)
	for _, f := range a.st.Forwards {
		out.Forwards = append(out.Forwards, uiForward{Forward: f, Stat: stats[f.ID]})
	}
	a.mu.Unlock()
	out.Logs = a.log.lines()
	writeJSON(w, out)
}

// ----------------------------------------------------------------- actions

func (a *Agent) handleJoin(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Head     string `json:"head"`
			Token    string `json:"token"`
			Name     string `json:"name"`
			Insecure bool   `json:"insecure"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, 400, "请求格式不对")
			return
		}
		if err := a.Join(ctx, req.Head, req.Token, req.Name, req.Insecure); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (a *Agent) handleForget(w http.ResponseWriter, r *http.Request) {
	a.Forget()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Agent) handleReconnect(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.Reconnect(ctx); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (a *Agent) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	a.Disconnect()
	writeJSON(w, map[string]any{"ok": true})
}

// handleForward creates or updates one forward.
func (a *Agent) handleForward(w http.ResponseWriter, r *http.Request) {
	var f Forward
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		httpErr(w, 400, "请求格式不对")
		return
	}
	f.Host = strings.TrimSpace(f.Host)
	f.Label = strings.TrimSpace(f.Label)
	if err := f.Validate(); err != nil {
		httpErr(w, 400, err.Error())
		return
	}

	a.mu.Lock()
	// Two forwards on one local port cannot both work, and the second one's
	// bind error is a confusing way to find that out.
	for _, o := range a.st.Forwards {
		if o.ID != f.ID && o.Local == f.Local && o.Enabled && f.Enabled {
			a.mu.Unlock()
			httpErr(w, 400, fmt.Sprintf("本地端口 %d 已经被「%s」占用", f.Local, o.name()))
			return
		}
	}
	found := false
	for i, o := range a.st.Forwards {
		if o.ID == f.ID && f.ID != "" {
			a.st.Forwards[i] = f
			found = true
			break
		}
	}
	if !found {
		f.ID = randHex(6)
		a.st.Forwards = append(a.st.Forwards, f)
	}
	err := a.saveLocked()
	a.mu.Unlock()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	a.applyForwards()
	writeJSON(w, map[string]any{"ok": true, "id": f.ID})
}

func (a *Agent) handleForwardDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	out := a.st.Forwards[:0]
	for _, f := range a.st.Forwards {
		if f.ID != req.ID {
			out = append(out, f)
		}
	}
	a.st.Forwards = out
	err := a.saveLocked()
	a.mu.Unlock()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	a.applyForwards()
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
