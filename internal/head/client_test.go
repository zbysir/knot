package head

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zbysir/knot/internal/model"
)

func addRoleToken(t *testing.T, s *Server, tok, role string, ttl time.Duration) string {
	t.Helper()
	err := s.store.Write(func(st *model.State) error {
		st.Tokens = append(st.Tokens, &model.JoinToken{
			Token: tok, Reusable: true, Expires: time.Now().Add(time.Hour),
			Role: role, ClientTTL: ttl,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func join(t *testing.T, s *Server, body string) (int, joinResp) {
	t.Helper()
	w := post(s.Handler(), "/api/join", body, "10.0.0.1")
	var out joinResp
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestClientJoinTakesNoAddress(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "clienttoken", model.RoleClient, 0)

	code, out := join(t, s, `{"token":"clienttoken","name":"laptop"}`)
	if code != 200 {
		t.Fatalf("join failed: %d", code)
	}
	if out.Role != model.RoleClient {
		t.Errorf("role = %q, want client", out.Role)
	}
	if out.VIP != "" {
		t.Errorf("a client was given the mesh address %q; it now appears in every node's routing table", out.VIP)
	}
	if out.Key == "" {
		t.Error("no key issued")
	}
	s.store.Read(func(st *model.State) {
		n := st.NodeByName("laptop")
		if n == nil || !n.IsClient() {
			t.Fatalf("client record not stored: %+v", n)
		}
		if n.UUID != "" {
			t.Errorf("a client got its own Reality uuid (%q); it should present the shared door", n.UUID)
		}
	})
}

// TestClientTokenCannotEnrolARelay: the credential an operator carries on a
// laptop must not be able to add a machine to the mesh.
func TestClientTokenCannotEnrolARelay(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "clienttoken", model.RoleClient, 0)
	code, _ := join(t, s, `{"token":"clienttoken","name":"evil","endpoint":"evil.example.com:443"}`)
	if code == 200 {
		t.Fatal("a client token enrolled a relay")
	}
}

func TestClientExpiryComesFromTheToken(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "shortlived", model.RoleClient, 48*time.Hour)
	_, out := join(t, s, `{"token":"shortlived","name":"laptop"}`)
	if out.Expires.IsZero() {
		t.Fatal("no expiry set from a token that has a TTL")
	}
	if d := time.Until(out.Expires); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("expiry is %s away, want ~48h", d)
	}
}

func TestRoleAndNameCollisionsAreRefused(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "nodetoken", model.RoleNode, 0)
	addRoleToken(t, s, "clienttoken", model.RoleClient, 0)

	if code, _ := join(t, s, `{"token":"nodetoken","name":"shared"}`); code != 200 {
		t.Fatalf("node join failed: %d", code)
	}
	if code, _ := join(t, s, `{"token":"clienttoken","name":"shared"}`); code == 200 {
		t.Error("a client took over a mesh node's name")
	}
	if code, _ := join(t, s, `{"token":"clienttoken","name":"laptop"}`); code != 200 {
		t.Fatal("client join failed")
	}
	if code, _ := join(t, s, `{"token":"nodetoken","name":"laptop"}`); code == 200 {
		t.Error("a mesh node took over a client's name")
	}
}

// TestRevokedClientLeavesThePlanButNothingElse is the point of the whole
// design: the relay's sing-box config must be byte-identical before and after a
// client comes and goes, because a difference means a restart.
func TestRevokedClientLeavesThePlanButNothingElse(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "nodetoken", model.RoleNode, 0)
	addRoleToken(t, s, "clienttoken", model.RoleClient, 0)

	_, relay := join(t, s, `{"token":"nodetoken","name":"hk","endpoint":"hk.example.com:443"}`)
	h := s.Handler()

	_, before := getConfig(h, relay.NodeID, relay.Key)

	_, client := join(t, s, `{"token":"clienttoken","name":"laptop"}`)
	_, withClient := getConfig(h, relay.NodeID, relay.Key)

	var cfgBefore, cfgWith struct {
		SingBox json.RawMessage `json:"singbox"`
		Relay   relayPlan       `json:"relay"`
	}
	json.Unmarshal([]byte(before), &cfgBefore)
	json.Unmarshal([]byte(withClient), &cfgWith)

	if string(cfgBefore.SingBox) != string(cfgWith.SingBox) {
		t.Error("adding a client changed the relay's sing-box config -- that is a restart and a mesh-wide tunnel drop")
	}
	if _, ok := cfgWith.Relay.ClientKeys[client.NodeID]; !ok {
		t.Fatalf("the client is not in the relay's ClientKeys: %+v", cfgWith.Relay.ClientKeys)
	}
	if _, ok := cfgWith.Relay.PeerKeys[client.NodeID]; ok {
		t.Error("the client is in PeerKeys, which would make it addressable by other nodes")
	}
	for _, p := range cfgWith.Relay.Peers {
		if p.NodeID == client.NodeID {
			t.Error("the client appears as a routable peer")
		}
	}

	// Now revoke it.
	s.store.Write(func(st *model.State) error {
		for i, n := range st.Nodes {
			if n.ID == client.NodeID {
				st.Nodes = append(st.Nodes[:i], st.Nodes[i+1:]...)
				break
			}
		}
		return nil
	})
	_, after := getConfig(h, relay.NodeID, relay.Key)
	var cfgAfter struct {
		SingBox json.RawMessage `json:"singbox"`
		Relay   relayPlan       `json:"relay"`
	}
	json.Unmarshal([]byte(after), &cfgAfter)
	if string(cfgAfter.SingBox) != string(cfgBefore.SingBox) {
		t.Error("revoking a client changed the relay's sing-box config")
	}
	if _, ok := cfgAfter.Relay.ClientKeys[client.NodeID]; ok {
		t.Error("a revoked client is still in ClientKeys")
	}
}

func TestExpiredClientIsDroppedFromThePlanAndRefusedConfig(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "nodetoken", model.RoleNode, 0)
	addRoleToken(t, s, "clienttoken", model.RoleClient, 0)
	_, relay := join(t, s, `{"token":"nodetoken","name":"hk","endpoint":"hk.example.com:443"}`)
	_, client := join(t, s, `{"token":"clienttoken","name":"laptop"}`)

	s.store.Write(func(st *model.State) error {
		st.NodeByID(client.NodeID).Expires = time.Now().Add(-time.Minute)
		return nil
	})

	_, body := getConfig(s.Handler(), relay.NodeID, relay.Key)
	var cfg struct {
		Relay relayPlan `json:"relay"`
	}
	json.Unmarshal([]byte(body), &cfg)
	if _, ok := cfg.Relay.ClientKeys[client.NodeID]; ok {
		t.Error("an expired client is still authorised on the relay")
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET",
		"/api/client?id="+client.NodeID+"&key="+client.Key, nil))
	if w.Code != 401 {
		t.Errorf("expired client got its bundle: HTTP %d", w.Code)
	}
}

func TestClientBundle(t *testing.T) {
	s := newTestServer(t)
	addRoleToken(t, s, "nodetoken", model.RoleNode, 0)
	addRoleToken(t, s, "clienttoken", model.RoleClient, 0)
	_, relay := join(t, s, `{"token":"nodetoken","name":"hk","endpoint":"hk.example.com:443"}`)
	join(t, s, `{"token":"nodetoken","name":"pg"}`)
	_, client := join(t, s, `{"token":"clienttoken","name":"laptop"}`)

	get := func(id, key string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/client?id="+id+"&key="+key, nil))
		return w
	}

	w := get(client.NodeID, client.Key)
	if w.Code != 200 {
		t.Fatalf("bundle: HTTP %d %s", w.Code, w.Body)
	}
	var b clientBundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.DoorUUID == "" {
		t.Error("no door uuid in the bundle")
	}
	if len(b.Relays) != 1 || b.Relays[0].Name != "hk" || b.Relays[0].VIP == "" || b.Relays[0].KnotPort == 0 {
		t.Fatalf("relay list is unusable: %+v", b.Relays)
	}
	if b.Relays[0].PublicKey == "" || b.Relays[0].ShortID == "" {
		t.Error("the bundle carries no Reality material, so the client cannot dial")
	}
	names := map[string]bool{}
	for _, n := range b.Nodes {
		names[n.Name] = true
	}
	if !names["hk"] || !names["pg"] {
		t.Errorf("mesh nodes missing from the bundle: %+v", b.Nodes)
	}
	if names["laptop"] {
		t.Error("the client is listed as a forward target for itself")
	}

	// Private material must never be in there.
	if body := w.Body.String(); strings.Contains(body, "reality_private") ||
		strings.Contains(body, "private_key") || strings.Contains(body, relayPrivateOf(t, s)) {
		t.Error("the bundle leaks a relay's Reality private key")
	}

	// A mesh node must not be able to use the client endpoint, and vice versa.
	if w := get(relay.NodeID, relay.Key); w.Code != 401 {
		t.Errorf("a mesh node fetched a client bundle: HTTP %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET",
		"/api/config?id="+client.NodeID+"&key="+client.Key, nil))
	if w.Code == 200 {
		t.Error("a client fetched a node's sing-box config")
	}
}

// relayPrivateOf returns the relay's actual private key, so the leak check
// looks for the value and not only for the field name it happens to have.
func relayPrivateOf(t *testing.T, s *Server) string {
	t.Helper()
	var priv string
	s.store.Read(func(st *model.State) {
		if n := st.NodeByName("hk"); n != nil {
			priv = n.RealityPrivate
		}
	})
	if priv == "" {
		t.Fatal("test relay has no private key to check against")
	}
	return priv
}
