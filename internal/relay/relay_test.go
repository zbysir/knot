package relay

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// helloServer accepts sessions, reads the Hello, and then either hangs up (the way
// a relay refuses a node that is not in its peer list) or holds the connection.
func helloServer(t *testing.T, hold bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if _, err := ReadHello(c); err != nil {
				c.Close()
				continue
			}
			if hold {
				held = append(held, c)
				continue
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func recorder() (*Client, func() string) {
	var mu sync.Mutex
	var lines []string
	c := &Client{
		NodeID: "n1",
		Key:    "k",
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		Logf: func(f string, v ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(f, v...))
		},
	}
	return c, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

// TestMaintainReportsWaitingNotFailure is the regression test for a log that
// described a broken node while nothing was wrong.
//
// A relay refuses every session from a node it has not picked up from the head
// yet, by closing right after the Hello. That produced "session to X up" --
// announced before any confirmation existed -- followed by "ended: EOF (retry in
// 1s)", per attempt, doubling. It reads exactly like a fault and it is a state
// that resolves itself on the relay's next poll.
func TestMaintainReportsWaitingNotFailure(t *testing.T) {
	t.Parallel()
	addr := helloServer(t, false)
	c, log := recorder()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c.Maintain(ctx, addr) // returns when ctx expires

	got := log()
	if strings.Contains(got, "established") {
		t.Errorf("announced a session the relay had refused:\n%s", got)
	}
	if n := strings.Count(got, "waiting for"); n != 1 {
		t.Errorf("logged the wait %d times, want exactly 1:\n%s", n, got)
	}
	if strings.Contains(got, "EOF") {
		t.Errorf("an expected wait was reported as a failure:\n%s", got)
	}
}

// TestMaintainAnnouncesAnAcceptedSession is the other half: a session that stays
// open has to be reported, or the wiring that delays the announcement would make
// every node silently never report one.
func TestMaintainAnnouncesAnAcceptedSession(t *testing.T) {
	t.Parallel()
	addr := helloServer(t, true)
	c, log := recorder()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Maintain(ctx, addr) }()

	deadline := time.Now().Add(acceptedAfter + 2*time.Second)
	for time.Now().Before(deadline) && !strings.Contains(log(), "established") {
		time.Sleep(20 * time.Millisecond)
	}
	// Cancelling has to be enough to stop it, with the session still open.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Maintain ignored a cancelled context while a session was up")
	}

	got := log()
	if n := strings.Count(got, "established"); n != 1 {
		t.Errorf("announced an accepted session %d times, want 1:\n%s", n, got)
	}
	if strings.Contains(got, "waiting for") {
		t.Errorf("a healthy session was reported as a wait:\n%s", got)
	}
}
