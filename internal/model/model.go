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

// Node is one machine in the mesh.
type Node struct {
	ID   string `json:"id"`   // stable, generated at join
	Name string `json:"name"` // human name, also the MagicDNS-ish label
	VIP  string `json:"vip"`  // 10.88.0.x, assigned by head

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

	// UUID authenticates this node when it dials others.
	UUID string `json:"uuid"`

	Key      string    `json:"key"`       // node's API credential, issued at join
	LastSeen time.Time `json:"last_seen"` // updated on every config poll
	Created  time.Time `json:"created"`
}

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
}

// State is the whole head database. Small enough to keep in one JSON file --
// this tops out at tens of nodes, not thousands.
type State struct {
	Nodes  []*Node      `json:"nodes"`
	Routes []Route      `json:"routes"`
	Tokens []*JoinToken `json:"tokens"`

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
		if n.IsRelay && n.Endpoint != "" {
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
