// Package model holds the shared types and the head's storage.
package model

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Roles a node record can have. The zero value is a mesh member, so every
// record written before clients existed keeps its meaning.
const (
	RoleNode   = ""
	RoleClient = "client"
)

// Node is one machine in the mesh -- or, when Role is RoleClient, one operator
// workstation that only dials INTO it.
type Node struct {
	ID   string `json:"id"`   // stable, generated at join
	Name string `json:"name"` // human name, also the MagicDNS-ish label
	VIP  string `json:"vip"`  // 10.88.0.x, assigned by head; empty for clients

	// Role separates the two things a record can be.
	//
	// A client is not a member of the address space: no VIP, absent from every
	// other node's hosts file, routing table and relay plan, and -- the part
	// that matters operationally -- absent from the relays' Reality user list.
	// That list lives in the sing-box config, so putting clients in it would
	// mean a config change, and therefore a sing-box restart on every relay,
	// every time a laptop credential is issued or revoked. Clients authenticate
	// through the shared door instead (see State.ClientUUID), which is constant,
	// and are then checked individually by knot's own relay protocol against a
	// list the relay hot-reloads.
	Role string `json:"role,omitempty"`
	// Expires bounds a client credential. Zero means never. Nodes ignore it:
	// a server that stops working at midnight is not a feature.
	Expires time.Time `json:"expires,omitempty"`

	// Relay fields. A node is dialable by others only if it has a public
	// endpoint -- that means it terminates a Reality inbound.
	IsRelay  bool   `json:"is_relay"`
	Endpoint string `json:"endpoint"` // host:port others dial, e.g. hk.example.com:443

	// Reality server-side material. Only meaningful when IsRelay.
	// PrivateKey never leaves the head except to its owning node.
	RealityPrivate string `json:"reality_private,omitempty"`
	RealityPublic  string `json:"reality_public,omitempty"`
	ShortID        string `json:"short_id,omitempty"`
	// ServerName is the SNI this node impersonates. Fallback is where
	// non-tunnel traffic is forwarded -- point it at a real local site so
	// active probing sees a genuine server.
	ServerName string `json:"server_name,omitempty"`
	Fallback   string `json:"fallback,omitempty"` // host:port, e.g. 127.0.0.1:8443
	// FallbackProxyProtocol prefixes forwarded connections with a PROXY
	// protocol header (1 or 2) so the fallback can recover the visitor's
	// address instead of seeing this relay. Off unless the fallback is
	// configured to expect it -- sending a header to a server that does not
	// breaks that server.
	FallbackProxyProtocol int `json:"fallback_proxy_protocol,omitempty"`

	// UUID authenticates this node when it dials others.
	UUID string `json:"uuid"`

	Key string `json:"key"` // node's API credential, issued at join
	// LastSeen is runtime only -- see Store.WriteVolatile. Stamped on every
	// config poll and deliberately never written to disk: persisting it made the
	// whole state file a write-per-poll, and persisting it *sometimes* (whenever
	// an unrelated change happened to flush) was worse -- the file then carried a
	// last-seen time frozen at some arbitrary moment, which reads as authoritative
	// and is not. The panel renders the zero value as "-" until the first poll.
	LastSeen time.Time `json:"-"`
	Created  time.Time `json:"created"`
}

// IsClient reports whether this record is an operator client rather than a
// mesh member.
func (n *Node) IsClient() bool { return n.Role == RoleClient }

// Expired reports whether a client credential has run out. Always false for
// mesh nodes.
func (n *Node) Expired() bool {
	return n.IsClient() && !n.Expires.IsZero() && time.Now().After(n.Expires)
}

// Usable reports whether this record may appear in generated config. A node
// without a VIP cannot: every consumer of the node list builds an address from
// it, and a blank one produces rules like `"ip_cidr": ["/32"]` that sing-box
// rejects -- which fails config generation for the WHOLE mesh, not just for the
// node with the bad record.
func (n *Node) Usable() bool { return !n.IsClient() && n.VIP != "" }

// Path is one way to reach a destination: either straight to the peer, or
// bounced off a relay.
type Path struct {
	Kind    string `json:"kind"`               // "direct" | "relay"
	RelayID string `json:"relay_id,omitempty"` // set when Kind=="relay"
}

// Route says how From should reach To. Via is ordered: the first entry is
// primary, the rest are fallbacks. An empty Via means "use the default",
// which is direct-if-possible then any relay.
type Route struct {
	From string `json:"from"` // node ID, or "*" for every node
	To   string `json:"to"`   // node ID
	Via  []Path `json:"via"`
}

// JoinToken is a one-shot or reusable credential a node presents to enroll.
type JoinToken struct {
	Token    string    `json:"token"`
	Reusable bool      `json:"reusable"`
	Expires  time.Time `json:"expires"`
	Used     int       `json:"used"`

	// Role is what this token may create. A client token cannot enrol a relay,
	// so the credential an operator carries around on a laptop cannot be used
	// to add a machine to the mesh.
	Role string `json:"role,omitempty"`
	// ClientTTL bounds the credentials this token issues, when it issues client
	// ones. Zero means they do not expire.
	ClientTTL time.Duration `json:"client_ttl,omitempty"`
}

// State is the whole head database. Small enough to keep in one JSON file --
// this tops out at tens of nodes, not thousands.
type State struct {
	Nodes  []*Node      `json:"nodes"`
	Routes []Route      `json:"routes"`
	Tokens []*JoinToken `json:"tokens"`

	// ClientUUID is the ONE Reality identity every operator client presents.
	//
	// Shared on purpose. It is a door, not a key: the relays' generated config
	// lets this user reach exactly one destination -- the relay's own knot
	// port -- and rejects everything else, so holding it grants nothing except
	// the right to knock. The individual credential is checked behind that
	// door, where changing it costs no restart.
	//
	// Generated once and then constant, which is the entire point: a value that
	// changed per client would put us back to restarting every relay whenever
	// somebody gets a laptop.
	ClientUUID string `json:"client_uuid,omitempty"`

	// Defaults applied to newly joined relays.
	DefaultServerName string `json:"default_server_name"`
	DefaultFallback   string `json:"default_fallback"`
	MeshCIDR          string `json:"mesh_cidr"` // e.g. 10.88.0.0/24

	PasswordHash string `json:"password_hash"` // panel login
}

// Store is a mutex-guarded State persisted to a JSON file. Every mutation
// writes the whole file -- fine at this scale, and it means the on-disk
// format is always readable and hand-editable.
type Store struct {
	mu   sync.RWMutex
	path string
	st   *State
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, st: &State{
		MeshCIDR: "10.88.0.0/24",
	}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, s.st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.st.MeshCIDR == "" {
		s.st.MeshCIDR = "10.88.0.0/24"
	}
	return s, nil
}

// Read runs fn against a read-locked snapshot.
func (s *Store) Read(fn func(*State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.st)
}

// Write runs fn against the state and persists the result. If fn returns an
// error nothing is written.
func (s *Store) Write(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(s.st); err != nil {
		return err
	}
	return s.flush()
}

// WriteVolatile mutates the state WITHOUT persisting it.
//
// For fields that change on every request and are not worth a file rewrite --
// LastSeen, which every node stamps at its poll interval. Persisting that made
// the whole state file a write-per-poll: at a 2s interval and a handful of nodes
// it is a rewrite every 500ms, for a value nothing depends on.
//
// The cost is that LastSeen resets when the head restarts. Nodes stamp it again
// within one poll interval, which is fresher than anything a flush would have
// preserved.
func (s *Store) WriteVolatile(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.st)
}

func (s *Store) flush() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// NodeByID returns the node or nil. Caller must hold a lock via Read/Write.
func (st *State) NodeByID(id string) *Node {
	for _, n := range st.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func (st *State) NodeByName(name string) *Node {
	for _, n := range st.Nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// Relays returns every node that can accept inbound connections.
func (st *State) Relays() []*Node {
	var out []*Node
	for _, n := range st.Nodes {
		if n.IsRelay && n.Endpoint != "" && n.Usable() {
			out = append(out, n)
		}
	}
	return out
}

// MeshNodes returns the records that belong in generated config: mesh members
// with an address. Clients and half-written records are left out.
func (st *State) MeshNodes() []*Node {
	var out []*Node
	for _, n := range st.Nodes {
		if n.Usable() {
			out = append(out, n)
		}
	}
	return out
}

// Clients returns the operator clients, expired ones included -- the panel has
// to show those, and only the code that hands out access filters them.
func (st *State) Clients() []*Node {
	var out []*Node
	for _, n := range st.Nodes {
		if n.IsClient() {
			out = append(out, n)
		}
	}
	return out
}

// RouteFor resolves the effective path list for from->to, honouring the most
// specific rule: an exact From match beats a "*" wildcard.
func (st *State) RouteFor(from, to string) []Path {
	var wildcard []Path
	for _, r := range st.Routes {
		if r.To != to {
			continue
		}
		if r.From == from {
			return r.Via
		}
		if r.From == "*" {
			wildcard = r.Via
		}
	}
	if wildcard != nil {
		return wildcard
	}
	return st.defaultPaths(from, to)
}

// defaultPaths is the policy when nothing is configured: dial the peer
// directly if it is dialable, then fall back to every relay in turn.
func (st *State) defaultPaths(from, to string) []Path {
	dst := st.NodeByID(to)
	if dst == nil {
		return nil
	}
	var out []Path
	if dst.IsRelay && dst.Endpoint != "" {
		out = append(out, Path{Kind: "direct"})
	}
	for _, r := range st.Relays() {
		if r.ID == to || r.ID == from {
			continue
		}
		out = append(out, Path{Kind: "relay", RelayID: r.ID})
	}
	return out
}
