package sb

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zbysir/knot/internal/model"
)

// meshWithClient is a relay, a leaf, and an operator client -- the client
// having no VIP, no uuid and no Reality material, as the head issues it.
func meshWithClient() (*model.State, *model.Node) {
	st, relay := relayState()
	st.ClientUUID = "11111111-2222-4333-8444-555555555555"
	st.Nodes = append(st.Nodes,
		&model.Node{ID: "n2", Name: "pg", VIP: "10.88.0.2", UUID: "uuid-pg"},
		&model.Node{ID: "c1", Name: "laptop", Role: model.RoleClient, Key: "k"},
	)
	return st, relay
}

func routeOf(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var cfg struct {
		Route struct {
			Rules []map[string]any `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Route.Rules
}

func usersOf(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var cfg struct {
		Inbounds []struct {
			Tag   string           `json:"tag"`
			Users []map[string]any `json:"users"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, in := range cfg.Inbounds {
		if in.Tag == "reality-in" {
			return in.Users
		}
	}
	t.Fatal("no reality-in inbound")
	return nil
}

// TestClientIsNotAUserAndNotAnAddress is the load-bearing property: if a client
// ever shows up in the Reality user list, issuing one restarts sing-box on
// every relay and drops the whole mesh's tunnels.
func TestClientIsNotAUserAndNotAnAddress(t *testing.T) {
	st, relay := meshWithClient()
	raw, err := Generate(st, relay)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	names := map[string]string{}
	for _, u := range usersOf(t, raw) {
		names[u["name"].(string)], _ = u["uuid"].(string)
	}
	if _, ok := names["laptop"]; ok {
		t.Error("the client is listed as its own Reality user; issuing one now restarts every relay")
	}
	if names[ClientDoorUser] != st.ClientUUID {
		t.Errorf("the shared door is missing or wrong: %v", names)
	}
	for _, want := range []string{"hk", "pg"} {
		if _, ok := names[want]; !ok {
			t.Errorf("mesh node %s lost its Reality user", want)
		}
	}

	if strings.Contains(string(raw), "laptop") {
		t.Errorf("the client's name appears in a node's config:\n%s", raw)
	}
	if h := Hosts(st); strings.Contains(h, "laptop") {
		t.Errorf("the client is in the hosts block: %q", h)
	}
}

// TestDoorRulesComeFirstAndDenyByDefault. Order is the security property: the
// destination rules below match on address alone, so a client asking for
// another node's mesh address would be forwarded by one of them.
func TestDoorRulesComeFirstAndDenyByDefault(t *testing.T) {
	st, relay := meshWithClient()
	raw, err := Generate(st, relay)
	if err != nil {
		t.Fatal(err)
	}
	rules := routeOf(t, raw)
	if len(rules) < 2 {
		t.Fatalf("want at least the two door rules, got %d", len(rules))
	}

	allow, deny := rules[0], rules[1]
	if u, _ := allow["auth_user"].([]any); len(u) != 1 || u[0] != ClientDoorUser {
		t.Fatalf("first rule is not the client allow rule: %v", allow)
	}
	if cidr, _ := allow["ip_cidr"].([]any); len(cidr) != 1 || cidr[0] != relay.VIP+"/32" {
		t.Errorf("the allow rule does not pin the relay's own address: %v", allow)
	}
	if p, _ := allow["port"].([]any); len(p) != 1 || int(p[0].(float64)) != RelayPort {
		t.Errorf("the allow rule does not pin knot's port: %v", allow)
	}
	if allow["outbound"] != "direct" {
		t.Errorf("the allow rule should go direct: %v", allow)
	}

	if u, _ := deny["auth_user"].([]any); len(u) != 1 || u[0] != ClientDoorUser {
		t.Fatalf("second rule is not the client deny rule: %v", deny)
	}
	if deny["action"] != "reject" {
		t.Fatalf("without a catch-all reject the shared door is an open proxy: %v", deny)
	}

	// And no later rule may reintroduce the client user.
	for i, r := range rules[2:] {
		if _, ok := r["auth_user"]; ok {
			t.Errorf("rule %d also matches on auth_user, after the deny: %v", i+2, r)
		}
	}
}

// TestNoDoorWithoutAUUID: a head that predates clients must not emit a user
// with an empty uuid, which sing-box would accept and nobody could explain.
func TestNoDoorWithoutAUUID(t *testing.T) {
	st, relay := meshWithClient()
	st.ClientUUID = ""
	raw, err := Generate(st, relay)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range usersOf(t, raw) {
		if u["name"] == ClientDoorUser {
			t.Fatalf("emitted the door with no uuid: %v", u)
		}
	}
	for _, r := range routeOf(t, raw) {
		if _, ok := r["auth_user"]; ok {
			t.Fatalf("emitted a door rule with no door: %v", r)
		}
	}
}

// TestHalfWrittenNodeDoesNotBreakTheMesh: one record without a VIP used to
// produce `"ip_cidr": ["/32"]` and fail generation for everybody.
func TestHalfWrittenNodeDoesNotBreakTheMesh(t *testing.T) {
	st, relay := meshWithClient()
	st.Nodes = append(st.Nodes, &model.Node{ID: "broken", Name: "halfway"})
	raw, err := Generate(st, relay)
	if err != nil {
		t.Fatalf("one bad record broke config generation for the whole mesh: %v", err)
	}
	if strings.Contains(string(raw), `"/32"`) {
		t.Errorf("generated a rule with no address:\n%s", raw)
	}
	if strings.Contains(Hosts(st), "halfway") {
		t.Error("a node with no VIP reached the hosts block")
	}
}

// TestGenerateRefusesAClient guards the wiring: a client has no sing-box config
// the head could produce.
func TestGenerateRefusesAClient(t *testing.T) {
	st, _ := meshWithClient()
	client := st.NodeByName("laptop")
	if _, err := Generate(st, client); err == nil {
		t.Fatal("Generate accepted a client")
	}
}
