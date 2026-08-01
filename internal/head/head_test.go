package head

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
