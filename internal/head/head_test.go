package head

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zbysir/knot/internal/model"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := model.NewStore(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPassword(store, "correct horse"); err != nil {
		t.Fatal(err)
	}
	return New(store)
}

func post(h http.Handler, path, body, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func addToken(t *testing.T, s *Server) string {
	t.Helper()
	const tok = "0123456789abcdef"
	err := s.store.Write(func(st *model.State) error {
		st.Tokens = append(st.Tokens, &model.JoinToken{
			Token: tok, Reusable: true, Expires: time.Now().Add(time.Hour),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func getConfig(h http.Handler, id, key string) (int, string) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/config?id="+id+"&key="+key, nil))
	return w.Code, w.Body.String()
}

// TestConfigSeparatesAuthFromGenerationFailure is a regression test for two very
// different failures answered with one status code.
//
// handleConfig used to assign both "this id/key is not ours" and "sb.Generate
// returned an error" to the same err variable and report 403 for either. One bad
// node record therefore deauthorized the entire mesh as far as the logs were
// concerned -- every node printed "config: HTTP 403" and nothing said why.
func TestConfigSeparatesAuthFromGenerationFailure(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	tok := addToken(t, s)
	w := post(h, "/api/join", `{"token":"`+tok+`","name":"leaf"}`, "10.0.0.1")
	if w.Code != 200 {
		t.Fatalf("join: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var j joinResp
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}

	if code, body := getConfig(h, j.NodeID, j.Key); code != 200 {
		t.Fatalf("own config: got %d, want 200: %s", code, body)
	}
	if code, body := getConfig(h, j.NodeID, "wrongkey"); code != 401 {
		t.Errorf("wrong key: got %d, want 401: %s", code, body)
	}
	if code, body := getConfig(h, "deadbeef", j.Key); code != 401 {
		t.Errorf("unknown id: got %d, want 401: %s", code, body)
	}

	// A relay whose endpoint has no port -- what an older version happily
	// stored, and what every other node then has to build an outbound for.
	err := s.store.Write(func(st *model.State) error {
		st.Nodes = append(st.Nodes, &model.Node{
			ID: "badrelay", Name: "bad", VIP: "10.88.0.9",
			IsRelay: true, Endpoint: "1.2.3.4", Key: "k",
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := getConfig(h, j.NodeID, j.Key)
	if code != 500 {
		t.Fatalf("unbuildable config: got %d, want 500 -- a server-side failure must not look like a bad credential: %s", code, body)
	}
	if !strings.Contains(body, "cannot build config") {
		t.Errorf("body does not say what went wrong: %s", body)
	}
}

// TestJoinRejectsMalformedEndpoint keeps the above from happening in the first
// place. checkHostPort guarded the panel but not the join API, so one node with
// a typo in KNOT_ENDPOINT broke config generation for the whole mesh.
func TestJoinRejectsMalformedEndpoint(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	tok := addToken(t, s)

	w := post(h, "/api/join", `{"token":"`+tok+`","name":"r","endpoint":"1.2.3.4"}`, "10.0.0.1")
	if w.Code != 400 {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
	}
	var n int
	s.store.Read(func(st *model.State) { n = len(st.Nodes) })
	if n != 0 {
		t.Fatalf("%d nodes enrolled despite the rejection", n)
	}

	// The same name with a valid endpoint is fine.
	if w := post(h, "/api/join", `{"token":"`+tok+`","name":"r","endpoint":"1.2.3.4:443"}`, "10.0.0.1"); w.Code != 200 {
		t.Fatalf("valid endpoint: got %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestConcurrentLoginAndAuth is a regression test for an unguarded map: login
// writes s.sessions while every authenticated request reads it. In Go that is
// not a torn value, it is a hard runtime panic -- so on a reachable panel it
// was a remote crash. Run with -race.
func TestConcurrentLoginAndAuth(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			post(h, "/api/login", `{"password":"correct horse"}`, "10.0.0.1")
		}()
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/api/state", nil)
			r.AddCookie(&http.Cookie{Name: "knot_session", Value: "whatever"})
			h.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
}

func TestLoginThrottle(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// The first few wrong guesses are free, then the client is told to wait.
	var got429 bool
	for i := 0; i < 8; i++ {
		w := post(h, "/api/login", `{"password":"nope"}`, "10.0.0.2")
		if w.Code == 429 {
			got429 = true
			break
		}
		if w.Code != 401 {
			t.Fatalf("attempt %d: got %d, want 401", i, w.Code)
		}
	}
	if !got429 {
		t.Fatal("never throttled: unlimited password guessing")
	}

	// A different client must not inherit the penalty.
	if w := post(h, "/api/login", `{"password":"correct horse"}`, "10.0.0.3"); w.Code != 200 {
		t.Fatalf("other client got %d, want 200 -- throttle is global, not per-client", w.Code)
	}
}

func TestLoginSucceedsAndAuthorizes(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	w := post(h, "/api/login", `{"password":"correct horse"}`, "10.0.0.4")
	if w.Code != 200 {
		t.Fatalf("login: got %d, want 200", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	if !cookies[0].HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}

	r := httptest.NewRequest("GET", "/api/state", nil)
	r.AddCookie(cookies[0])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("authed request: got %d, want 200", rec.Code)
	}

	// And without the cookie it must not be.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthed request: got %d, want 401", rec.Code)
	}
}
