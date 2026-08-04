// knot -- a small mesh with a Reality data plane.
//
//	knot head                 run the control plane + web panel
//	knot node                 run the agent (joins with a token, then syncs)
//	knot passwd <password>    set the panel password
//
// Everything is configured through environment variables so a node is
// literally `docker run` + one token.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zbysir/knot/internal/head"
	"github.com/zbysir/knot/internal/model"
	"github.com/zbysir/knot/internal/node"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "head":
		err = runHead()
	case "node":
		err = runNode()
	case "passwd":
		err = runPasswd()
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "knot:", err)
		os.Exit(1)
	}
}

const usage = `knot -- mesh networking with a Reality data plane

  knot head              control plane + web panel
      KNOT_DATA          state directory                  (default /var/lib/knot)
      KNOT_LISTEN        listen address                   (default :8080)
      KNOT_PASSWORD      panel password, applied at boot
      KNOT_TLS_CERT      optional TLS cert path
      KNOT_TLS_KEY       optional TLS key path

  knot node              mesh agent
      KNOT_HEAD          head URL, e.g. https://head:8080  (required)
      KNOT_TOKEN         join token, first run only
      KNOT_NAME          node name                         (default hostname)
      KNOT_ENDPOINT      host:port peers dial -- set this to make it a relay
      KNOT_DATA          state directory                   (default /var/lib/knot)
      KNOT_SINGBOX       sing-box binary path              (default sing-box)
      KNOT_POLL          config poll interval              (default 2s, min 1s)

  knot passwd <password> set the panel password (only while head is stopped;
                         prefer KNOT_PASSWORD on the head instead)
`

func runHead() error {
	store, err := model.NewStore(dataPath("state.json"))
	if err != nil {
		return err
	}
	// Set the panel password at boot. `knot passwd` only works while the head
	// is stopped -- the running server holds state in memory and would
	// overwrite whatever the CLI wrote to the file.
	if pw := os.Getenv("KNOT_PASSWORD"); pw != "" {
		if err := head.SetPassword(store, pw); err != nil {
			return err
		}
	}
	srv := head.New(store)
	addr := env("KNOT_LISTEN", ":8080")

	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	cert, key := os.Getenv("KNOT_TLS_CERT"), os.Getenv("KNOT_TLS_KEY")

	go func() {
		var err error
		if cert != "" && key != "" {
			hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			fmt.Printf("knot head listening on https://%s\n", addr)
			err = hs.ListenAndServeTLS(cert, key)
		} else {
			fmt.Printf("knot head listening on http://%s\n", addr)
			fmt.Println("knot: no TLS configured -- put this behind a reverse proxy before exposing it")
			err = hs.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "knot:", err)
			os.Exit(1)
		}
	}()

	waitSignal()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(ctx)
}

func runNode() error {
	headURL := os.Getenv("KNOT_HEAD")
	if headURL == "" {
		return fmt.Errorf("KNOT_HEAD is required")
	}
	name := os.Getenv("KNOT_NAME")
	if name == "" {
		h, _ := os.Hostname()
		name = h
	}
	a := &node.Agent{
		Head:     strings.TrimRight(headURL, "/"),
		Token:    os.Getenv("KNOT_TOKEN"),
		Name:     name,
		Endpoint: os.Getenv("KNOT_ENDPOINT"),
		DataDir:  env("KNOT_DATA", "/var/lib/knot"),
		SingBox:  env("KNOT_SINGBOX", "sing-box"),
	}
	if v := os.Getenv("KNOT_POLL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("KNOT_POLL %q: %w", v, err)
		}
		a.Poll = d
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { waitSignal(); cancel() }()
	return a.Run(ctx)
}

func runPasswd() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: knot passwd <password>")
	}
	store, err := model.NewStore(dataPath("state.json"))
	if err != nil {
		return err
	}
	if err := head.SetPassword(store, os.Args[2]); err != nil {
		return err
	}
	fmt.Println("panel password set")
	return nil
}

func dataPath(name string) string {
	return env("KNOT_DATA", "/var/lib/knot") + "/" + name
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func waitSignal() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
}
