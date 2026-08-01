// Package head is the control plane: node registry, route policy, join
// tokens, and the web panel.
//
// It is not on the data path. If the head is down the mesh keeps forwarding
// with whatever config the nodes already hold -- they only lose the ability
// to pick up changes.
package head

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zbysir/knot/internal/model"
	"github.com/zbysir/knot/internal/sb"
)

//go:embed panel.html
var assets embed.FS

type Server struct {
	store *model.Store

	// mu guards sessions and loginFails. Both are touched from every request
	// goroutine; an unguarded map read racing a write is a hard runtime panic
	// in Go, not a corrupted value -- which on a reachable panel means anyone
	// can crash the control plane by logging in while another request is in
	// flight.
	mu         sync.Mutex
	sessions   map[string]time.Time // cookie -> expiry
	loginFails map[string]*failCount
}

// failCount throttles password guessing per client. Without it the panel is a
// single password with unlimited attempts.
type failCount struct {
	n    int
	next time.Time // no attempt accepted before this
}

func New(store *model.Store) *Server {
	return &Server{
		store:      store,
		sessions:   map[string]time.Time{},
		loginFails: map[string]*failCount{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- node-facing API (authenticated by join token / node key) ---
	mux.HandleFunc("POST /api/join", s.handleJoin)
	mux.HandleFunc("GET /api/config", s.handleConfig)

	// --- panel API (authenticated by session cookie) ---
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/state", s.auth(s.handleState))
	mux.HandleFunc("POST /api/token", s.auth(s.handleNewToken))
	mux.HandleFunc("POST /api/token/delete", s.auth(s.handleDeleteToken))
	mux.HandleFunc("POST /api/routes", s.auth(s.handleSetRoutes))
	mux.HandleFunc("POST /api/node/update", s.auth(s.handleUpdateNode))
	mux.HandleFunc("POST /api/node/delete", s.auth(s.handleDeleteNode))
	mux.HandleFunc("POST /api/settings", s.auth(s.handleSettings))

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := assets.ReadFile("panel.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	return mux
}

// ---------------------------------------------------------------- node API

type joinReq struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"` // optional; set => this node is a relay
}

type joinResp struct {
	NodeID string `json:"node_id"`
	Key    string `json:"key"`
	VIP    string `json:"vip"`
}

// handleJoin enrolls a node. The token is consumed unless it is reusable.
// Re-joining with the same name returns the existing identity, so a node can
// be recreated without churning its VIP.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "bad json")
		return
	}
	if req.Name == "" {
		httpErr(w, 400, "name required")
		return
	}
	var out joinResp
	err := s.store.Write(func(st *model.State) error {
		tok := findToken(st, req.Token)
		if tok == nil {
			return fmt.Errorf("invalid or expired token")
		}

		n := st.NodeByName(req.Name)
		if n == nil {
			vip, err := allocVIP(st)
			if err != nil {
				return err
			}
			n = &model.Node{
				ID:      randHex(8),
				Name:    req.Name,
				VIP:     vip,
				UUID:    uuidV4(),
				Key:     randHex(24),
				Created: time.Now(),
			}
			st.Nodes = append(st.Nodes, n)
		}
		// An endpoint means the node can accept inbound Reality connections.
		if req.Endpoint != "" {
			n.IsRelay = true
			n.Endpoint = req.Endpoint
			if n.RealityPrivate == "" {
				priv, pub, err := realityKeypair()
				if err != nil {
					return err
				}
				n.RealityPrivate, n.RealityPublic = priv, pub
				n.ShortID = randHex(4) // 8 hex chars; longer values are legal but not what
				// the xray/sing-box ecosystem actually exercises
			}
			if n.ServerName == "" {
				n.ServerName = orDefault(st.DefaultServerName, "dl.google.com")
			}
			if n.Fallback == "" {
				n.Fallback = orDefault(st.DefaultFallback, "127.0.0.1:8443")
			}
		}
		tok.Used++
		if !tok.Reusable {
			dropToken(st, tok.Token)
		}
		out = joinResp{NodeID: n.ID, Key: n.Key, VIP: n.VIP}
		return nil
	})
	if err != nil {
		httpErr(w, 403, err.Error())
		return
	}
	writeJSON(w, out)
}

// handleConfig returns the sing-box config plus the hosts block. Nodes poll
// this; the ETag lets them skip a restart when nothing changed.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	id, key := r.URL.Query().Get("id"), r.URL.Query().Get("key")
	var (
		cfg   []byte
		hosts string
		plan  relayPlan
		err   error
	)
	s.store.Write(func(st *model.State) error {
		n := st.NodeByID(id)
		if n == nil || subtle.ConstantTimeCompare([]byte(n.Key), []byte(key)) != 1 {
			err = fmt.Errorf("unauthorized")
			return err
		}
		n.LastSeen = time.Now()
		cfg, err = sb.Generate(st, n)
		hosts = sb.Hosts(st)
		plan = buildPlan(st, n)
		return nil
	})
	if err != nil {
		httpErr(w, 403, err.Error())
		return
	}
	planJSON, _ := json.Marshal(plan)
	sum := sha256.Sum256(append(append(cfg, hosts...), planJSON...))
	etag := hex.EncodeToString(sum[:8])
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, map[string]any{
		"singbox": json.RawMessage(cfg), "hosts": hosts, "relay": plan,
	})
}

type relayPlan struct {
	SelfID  string     `json:"self_id"`
	Key     string     `json:"key"`
	IsRelay bool       `json:"is_relay"`
	Listen  string     `json:"listen"`
	Socks   string     `json:"socks"`
	Uplinks []string   `json:"uplinks"`
	Peers   []planPeer `json:"peers"`
	// PeerKeys maps node ID -> sha256(key), for relays only. The relay checks
	// a joining node against this instead of accepting any non-empty key.
	// Hashes rather than the keys themselves so a readable relay state file
	// does not hand over every node's credential.
	PeerKeys map[string]string `json:"peer_keys,omitempty"`
}

type planPeer struct {
	NodeID string   `json:"node_id"`
	VIP    string   `json:"vip"`
	Relays []string `json:"relays"`
	Addrs  []string `json:"addrs"`
}

// buildPlan lists, for every peer this node cannot dial directly, the relays
// to try in order. Relays themselves are reached over the mesh, so the address
// is the relay's VIP -- sing-box turns that into a Reality connection.
func buildPlan(st *model.State, self *model.Node) relayPlan {
	p := relayPlan{
		SelfID:  self.ID,
		Key:     self.Key,
		IsRelay: self.IsRelay && self.Endpoint != "",
		Socks:   fmt.Sprintf("127.0.0.1:%d", sb.SocksPort),
	}
	if p.IsRelay {
		// Bind to the mesh address, never 0.0.0.0. This port speaks a plain
		// custom protocol -- it is only ever reached from inside a Reality
		// tunnel, and an extra open port with an unrecognisable protocol on it
		// is exactly what the camouflage exists to avoid.
		p.Listen = fmt.Sprintf("%s:%d", self.VIP, sb.RelayPort)
		p.PeerKeys = map[string]string{}
		for _, n := range st.Nodes {
			if n.ID != self.ID && n.Key != "" {
				p.PeerKeys[n.ID] = hashKey(n.Key)
			}
		}
	} else {
		// Home this node on every relay it can dial, not just the ones some
		// route names today. A relay can only push traffic down a session that
		// already exists, so a node that nobody talks to yet still has to be
		// reachable -- and an idle session is one TCP connection. This is also
		// what makes primary/backup work: the backup relay is useless if the
		// destination is not homed there when the primary dies.
		for _, r := range st.Relays() {
			if r.ID == self.ID || r.Endpoint == "" {
				continue
			}
			p.Uplinks = append(p.Uplinks, fmt.Sprintf("%s:%d", r.VIP, sb.RelayPort))
		}
	}
	for _, dst := range st.Nodes {
		if dst.ID == self.ID || (dst.IsRelay && dst.Endpoint != "") {
			continue // self, or directly dialable
		}
		pp := planPeer{NodeID: dst.ID, VIP: dst.VIP}
		for _, path := range st.RouteFor(self.ID, dst.ID) {
			if path.Kind != "relay" {
				continue
			}
			r := st.NodeByID(path.RelayID)
			if r == nil || !r.IsRelay || r.ID == self.ID {
				continue
			}
			pp.Relays = append(pp.Relays, r.ID)
			pp.Addrs = append(pp.Addrs, fmt.Sprintf("%s:%d", r.VIP, sb.RelayPort))
		}
		// A relay never lists itself as a hop, so its own peer list would come
		// out empty -- yet it is exactly the node already holding those leaves'
		// sessions. Keep the entry so it can serve them from its registry;
		// without this a relay cannot originate traffic to its own leaves.
		if len(pp.Addrs) > 0 || p.IsRelay {
			p.Peers = append(p.Peers, pp)
		}
	}
	return p
}

// --------------------------------------------------------------- panel API

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	client := clientIP(r)
	if wait, ok := s.throttled(client); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
		httpErr(w, 429, "too many attempts, retry in "+wait.Truncate(time.Second).String())
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	ok := false
	s.store.Read(func(st *model.State) {
		// Constant-time so a network observer cannot walk the hash byte by byte.
		ok = st.PasswordHash != "" &&
			subtle.ConstantTimeCompare([]byte(st.PasswordHash), []byte(hashPw(req.Password))) == 1
	})
	if !ok {
		s.loginFailed(client)
		httpErr(w, 401, "wrong password")
		return
	}
	s.loginOK(client)

	sid := randHex(24)
	s.mu.Lock()
	s.sessions[sid] = time.Now().Add(12 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "knot_session", Value: sid, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
		// Only when the request actually arrived over TLS -- setting it
		// unconditionally would break a plain-HTTP setup by making the browser
		// drop the cookie it was just handed.
		Secure: isTLS(r),
	})
	writeJSON(w, map[string]any{"ok": true})
}

// throttled reports whether client may attempt a login now, and if not, how
// long to wait.
func (s *Server) throttled(client string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.loginFails[client]
	if f == nil {
		return 0, true
	}
	if d := time.Until(f.next); d > 0 {
		return d, false
	}
	return 0, true
}

func (s *Server) loginFailed(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.loginFails[client]
	if f == nil {
		f = &failCount{}
		s.loginFails[client] = f
	}
	f.n++
	// First few attempts are free (fat fingers), then back off hard: 2s, 4s,
	// 8s ... capped at 5 minutes. Turns an online brute force into something
	// that takes geological time without ever locking the owner out for good.
	if f.n > 3 {
		d := time.Duration(1<<min(f.n-3, 8)) * time.Second
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		f.next = time.Now().Add(d)
	}
}

func (s *Server) loginOK(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.loginFails, client)
	// Opportunistically drop expired sessions; there is no other sweeper and
	// this map would otherwise only ever grow.
	now := time.Now()
	for id, exp := range s.sessions {
		if exp.Before(now) {
			delete(s.sessions, id)
		}
	}
}

// clientIP is the throttling key. X-Forwarded-For is honoured because the
// panel is meant to sit behind a reverse proxy -- which also means it can be
// spoofed when it is NOT behind one. That is acceptable here: the throttle is
// a brute-force speed bump, not an access control.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkHostPort rejects the half-edited values a text input produces --
// "1.2.3.4:" being the one that actually bit us. sing-box would have taken it
// and failed to start.
func checkHostPort(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("%q 不是 host:port", s)
	}
	if host == "" {
		return fmt.Errorf("%q 缺少主机", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q 的端口无效", s)
	}
	return nil
}

func isTLS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("knot_session")
		if err != nil {
			httpErr(w, 401, "login required")
			return
		}
		s.mu.Lock()
		exp, found := s.sessions[c.Value]
		s.mu.Unlock()
		if !found || exp.Before(time.Now()) {
			httpErr(w, 401, "login required")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var out any
	s.store.Read(func(st *model.State) {
		// Never ship private keys or node credentials to the browser.
		nodes := make([]map[string]any, 0, len(st.Nodes))
		for _, n := range st.Nodes {
			nodes = append(nodes, map[string]any{
				"id": n.ID, "name": n.Name, "vip": n.VIP,
				"is_relay": n.IsRelay, "endpoint": n.Endpoint,
				"server_name": n.ServerName, "fallback": n.Fallback,
				"last_seen": n.LastSeen, "created": n.Created,
			})
		}
		toks := make([]map[string]any, 0, len(st.Tokens))
		for _, t := range st.Tokens {
			toks = append(toks, map[string]any{
				"token": t.Token, "reusable": t.Reusable,
				"expires": t.Expires, "used": t.Used,
			})
		}
		out = map[string]any{
			"nodes": nodes, "routes": st.Routes, "tokens": toks,
			"mesh_cidr":           st.MeshCIDR,
			"default_server_name": st.DefaultServerName,
			"default_fallback":    st.DefaultFallback,
		}
	})
	writeJSON(w, out)
}

func (s *Server) handleNewToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reusable bool `json:"reusable"`
		Hours    int  `json:"hours"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Hours <= 0 {
		req.Hours = 24
	}
	t := &model.JoinToken{
		Token:    randHex(16),
		Reusable: req.Reusable,
		Expires:  time.Now().Add(time.Duration(req.Hours) * time.Hour),
	}
	s.store.Write(func(st *model.State) error {
		st.Tokens = append(st.Tokens, t)
		return nil
	})
	writeJSON(w, t)
}

// handleDeleteToken revokes an unused token. A token is a credential to join
// the mesh, so being able to see one is not much use without being able to
// take it back.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.store.Write(func(st *model.State) error {
		dropToken(st, req.Token)
		return nil
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSetRoutes(w http.ResponseWriter, r *http.Request) {
	var routes []model.Route
	if err := json.NewDecoder(r.Body).Decode(&routes); err != nil {
		httpErr(w, 400, "bad json")
		return
	}
	s.store.Write(func(st *model.State) error {
		st.Routes = routes
		return nil
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	// Pointers, not strings: the panel has to be able to CLEAR a field, and
	// with plain strings "" is indistinguishable from "not sent". That is how
	// a demoted relay kept showing a stale endpoint that nothing could erase.
	//   nil -> leave alone
	//   ""  -> clear
	var req struct {
		ID         string  `json:"id"`
		Endpoint   *string `json:"endpoint"`
		ServerName *string `json:"server_name"`
		Fallback   *string `json:"fallback"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	err := s.store.Write(func(st *model.State) error {
		n := st.NodeByID(req.ID)
		if n == nil {
			return fmt.Errorf("no such node")
		}
		if req.Endpoint != nil {
			v := strings.TrimSpace(*req.Endpoint)
			if v != "" {
				if err := checkHostPort(v); err != nil {
					return fmt.Errorf("对外端点 %w", err)
				}
			}
			n.Endpoint = v
		}
		if req.ServerName != nil {
			n.ServerName = strings.TrimSpace(*req.ServerName)
		}
		if req.Fallback != nil {
			v := strings.TrimSpace(*req.Fallback)
			if v != "" {
				if err := checkHostPort(v); err != nil {
					return fmt.Errorf("回落 %w", err)
				}
			}
			n.Fallback = v
		}

		// Role follows the endpoint, exactly as the panel describes it:
		// "a relay is a node with an endpoint". Keeping IsRelay as an
		// independent flag only created states nobody wanted -- a leaf still
		// showing relay settings, or a relay with no way in.
		n.IsRelay = n.Endpoint != ""
		if n.IsRelay {
			if n.RealityPrivate == "" {
				priv, pub, err := realityKeypair()
				if err != nil {
					return err
				}
				n.RealityPrivate, n.RealityPublic = priv, pub
				n.ShortID = randHex(4) // 8 hex chars; longer values are legal but not what
				// the xray/sing-box ecosystem actually exercises
			}
			if n.ServerName == "" {
				n.ServerName = orDefault(st.DefaultServerName, "dl.google.com")
			}
			if n.Fallback == "" {
				n.Fallback = orDefault(st.DefaultFallback, "127.0.0.1:8443")
			}
		} else {
			// Demoted to leaf: drop the relay-only settings so the panel does
			// not keep displaying an endpoint the node does not listen on.
			// The Reality keypair is kept -- reusing it on re-promotion avoids
			// pushing a new public key to every peer.
			n.ServerName, n.Fallback = "", ""
		}
		return nil
	})
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.store.Write(func(st *model.State) error {
		out := st.Nodes[:0]
		for _, n := range st.Nodes {
			if n.ID != req.ID {
				out = append(out, n)
			}
		}
		st.Nodes = out
		// Drop routes that referenced it, otherwise they silently do nothing.
		var rs []model.Route
		for _, rt := range st.Routes {
			if rt.To == req.ID || rt.From == req.ID {
				continue
			}
			rs = append(rs, rt)
		}
		st.Routes = rs
		return nil
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultServerName string `json:"default_server_name"`
		DefaultFallback   string `json:"default_fallback"`
		MeshCIDR          string `json:"mesh_cidr"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.store.Write(func(st *model.State) error {
		if req.DefaultServerName != "" {
			st.DefaultServerName = req.DefaultServerName
		}
		if req.DefaultFallback != "" {
			st.DefaultFallback = req.DefaultFallback
		}
		if req.MeshCIDR != "" {
			st.MeshCIDR = req.MeshCIDR
		}
		return nil
	})
	writeJSON(w, map[string]any{"ok": true})
}

// SetPassword is used by the CLI on first run.
func SetPassword(store *model.Store, pw string) error {
	return store.Write(func(st *model.State) error {
		st.PasswordHash = hashPw(pw)
		return nil
	})
}

// ----------------------------------------------------------------- helpers

// realityKeypair returns base64url-encoded x25519 keys in the form Reality
// expects.
//
// The private scalar MUST be clamped before we store it. sing-box uses the
// stored bytes as-is rather than clamping on load, so an unclamped private
// key derives a different public key server-side than the one we hand to
// clients -- and the only symptom is Reality reporting
// "processed invalid connection", which says nothing about keys at all.
// ecdh.GenerateKey does not clamp its stored bytes, hence the manual step.
func realityKeypair() (priv, pub string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	raw[0] &= 248
	raw[31] &= 127
	raw[31] |= 64

	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(k.Bytes()), enc.EncodeToString(k.PublicKey().Bytes()), nil
}

func allocVIP(st *model.State) (string, error) {
	_, ipnet, err := net.ParseCIDR(st.MeshCIDR)
	if err != nil {
		return "", fmt.Errorf("bad mesh_cidr %q: %w", st.MeshCIDR, err)
	}
	taken := map[string]bool{}
	for _, n := range st.Nodes {
		taken[n.VIP] = true
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("mesh_cidr must be IPv4")
	}
	// .1 upward; skip .0 and the broadcast address.
	for i := 1; i < 255; i++ {
		cand := net.IPv4(ip[0], ip[1], ip[2], byte(i)).String()
		if !ipnet.Contains(net.ParseIP(cand)) {
			continue
		}
		if !taken[cand] {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free address in %s", st.MeshCIDR)
}

func findToken(st *model.State, tok string) *model.JoinToken {
	for _, t := range st.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.Token), []byte(tok)) == 1 && time.Now().Before(t.Expires) {
			return t
		}
	}
	return nil
}

func dropToken(st *model.State, tok string) {
	out := st.Tokens[:0]
	for _, t := range st.Tokens {
		if t.Token != tok {
			out = append(out, t)
		}
	}
	st.Tokens = out
}

// hashKey is what a relay stores instead of a node's key. Keys are 128-bit
// random hex, so a plain hash is enough -- there is nothing to guess.
func hashKey(k string) string {
	sum := sha256.Sum256([]byte("knot-node\x00" + k))
	return hex.EncodeToString(sum[:])
}

func hashPw(pw string) string {
	// Salted with a fixed application string. The panel is not exposed to
	// untrusted networks and the store is root-only, so this is proportionate.
	sum := sha256.Sum256([]byte("knot-panel\x00" + pw))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func uuidV4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return strings.Join([]string{h[:8], h[8:12], h[12:16], h[16:20], h[20:]}, "-")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
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
