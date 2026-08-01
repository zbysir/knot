// Package node is the agent that runs on every machine in the mesh.
//
// It joins the head once with a token, then polls for its sing-box config and
// keeps a sing-box child process in sync with it. Everything it needs to
// persist lives in one small identity file, so re-creating the container is
// harmless: it re-joins under the same name and gets the same VIP back.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Identity struct {
	NodeID string `json:"node_id"`
	Key    string `json:"key"`
	VIP    string `json:"vip"`
	Head   string `json:"head"`
	Name   string `json:"name"`
}

type Agent struct {
	Head     string // https://head.example.com
	Token    string
	Name     string
	Endpoint string // set on relays: the host:port peers dial
	DataDir  string
	SingBox  string // path to the sing-box binary
	Insecure bool   // skip TLS verification against the head

	id       Identity
	etag     string
	cmd      *exec.Cmd
	cfgPath  string
	planPath string
	relay    *relayd
}

func (a *Agent) Run(ctx context.Context) error {
	a.cfgPath = filepath.Join(a.DataDir, "singbox.json")
	a.planPath = filepath.Join(a.DataDir, "relay-plan.json")
	if err := os.MkdirAll(a.DataDir, 0o700); err != nil {
		return err
	}
	if err := a.load(); err != nil {
		return err
	}
	defer a.stopSingBox()
	defer a.stopRelay()

	// A failed first sync is not fatal if we still have the config from last
	// time. Two situations need this:
	//
	//   - the head is simply down while this node restarts. Exiting here threw
	//     away a working data plane for no reason.
	//   - KNOT_HEAD points at the head's MESH address. Then it is unreachable
	//     by construction until sing-box is up, so requiring the head first is
	//     a deadlock: head needs mesh, mesh needs config, config needs head.
	//
	// The ticker below picks up the real config as soon as the head answers.
	if err := a.sync(ctx); err != nil {
		if _, statErr := os.Stat(a.cfgPath); statErr != nil {
			return fmt.Errorf("initial sync (and no cached config): %w", err)
		}
		fmt.Fprintf(os.Stderr, "knot: initial sync failed (%v)\nknot: starting from cached config\n", err)
		if b, rerr := os.ReadFile(a.planPath); rerr == nil {
			var p Plan
			if json.Unmarshal(b, &p) == nil {
				if err := a.startRelay(p); err != nil {
					fmt.Fprintf(os.Stderr, "knot: cached relay plan failed: %v\n", err)
				}
			}
		}
		if err := a.restartSingBox(); err != nil {
			return fmt.Errorf("initial sync failed and cached config is unusable: %w", err)
		}
	}

	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := a.sync(ctx); err != nil {
				// A head outage must not take the data plane down: sing-box
				// keeps running with the config it already has.
				fmt.Fprintf(os.Stderr, "knot: sync failed (keeping current config): %v\n", err)
			}
		}
	}
}

// load reads the persisted identity, joining the head if this is a first run.
func (a *Agent) load() error {
	p := filepath.Join(a.DataDir, "identity.json")
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &a.id); err == nil && a.id.NodeID != "" {
			// KNOT_HEAD wins over the address recorded at join time. Without
			// this, changing the env var does nothing at all -- sync() reads
			// the persisted copy -- and moving the head (say, from its public
			// address to its mesh address) silently has no effect.
			if a.Head != "" && a.Head != a.id.Head {
				fmt.Fprintf(os.Stderr, "knot: head address changed %s -> %s\n", a.id.Head, a.Head)
				a.id.Head = a.Head
				if nb, err := json.MarshalIndent(a.id, "", "  "); err == nil {
					os.WriteFile(p, nb, 0o600)
				}
			}
			return nil
		}
	}
	if a.Token == "" {
		return fmt.Errorf("no identity on disk and no KNOT_TOKEN given")
	}
	body, _ := json.Marshal(map[string]string{
		"token": a.Token, "name": a.Name, "endpoint": a.Endpoint,
	})
	resp, err := a.client().Post(a.Head+"/api/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("join rejected: %s", e["error"])
	}
	if err := json.NewDecoder(resp.Body).Decode(&a.id); err != nil {
		return err
	}
	a.id.Head, a.id.Name = a.Head, a.Name
	b, _ := json.MarshalIndent(a.id, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

type configResp struct {
	SingBox json.RawMessage `json:"singbox"`
	Hosts   string          `json:"hosts"`
	Relay   Plan            `json:"relay"`
}

// sync pulls the current config. A 304 means nothing changed and we leave the
// running process alone -- restarting sing-box drops every live connection,
// so we only do it when the config actually differs.
func (a *Agent) sync(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/config?id=%s&key=%s", a.id.Head, a.id.NodeID, a.id.Key)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if a.etag != "" {
		req.Header.Set("If-None-Match", a.etag)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return a.ensureRunning()
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("config: HTTP %d", resp.StatusCode)
	}
	var cr configResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return err
	}
	if err := os.WriteFile(a.cfgPath, cr.SingBox, 0o600); err != nil {
		return err
	}
	if err := writeHosts(cr.Hosts); err != nil {
		fmt.Fprintf(os.Stderr, "knot: hosts update failed: %v\n", err)
	}
	a.etag = resp.Header.Get("ETag")
	// Cache the relay plan next to the sing-box config. Without it a node that
	// cold-starts while the head is unreachable comes up half working: its own
	// outbound traffic flows, but it holds no relay session, so nothing can be
	// pushed back down to it and it is silently unreachable.
	if b, err := json.Marshal(cr.Relay); err == nil {
		os.WriteFile(a.planPath, b, 0o600)
	}
	// Relay first: sing-box will start forwarding peer traffic into the SOCKS
	// listener as soon as it comes up, so that listener has to exist already.
	if err := a.startRelay(cr.Relay); err != nil {
		fmt.Fprintf(os.Stderr, "knot: relay start failed: %v\n", err)
	}
	return a.restartSingBox()
}

func (a *Agent) ensureRunning() error {
	if a.cmd != nil && a.cmd.Process != nil {
		if a.cmd.ProcessState == nil || !a.cmd.ProcessState.Exited() {
			return nil
		}
	}
	return a.restartSingBox()
}

func (a *Agent) restartSingBox() error {
	// Validate before swapping: a bad config would otherwise leave the node
	// with no data plane at all.
	out, err := exec.Command(a.SingBox, "check", "-c", a.cfgPath).CombinedOutput()
	if err != nil {
		// Include err itself: when the binary is missing or unrunnable there
		// is no output at all, and a bare empty message is impossible to debug.
		return fmt.Errorf("config rejected by sing-box: %v: %s", err, strings.TrimSpace(string(out)))
	}
	a.stopSingBox()

	cmd := exec.Command(a.SingBox, "run", "-c", a.cfgPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	a.cmd = cmd
	go cmd.Wait() // reap, so a crashed child does not become a zombie
	return nil
}

func (a *Agent) stopSingBox() {
	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Kill()
		a.cmd = nil
	}
}

// writeHosts maintains a knot-owned block in /etc/hosts so peers are
// reachable as <name>.knot. Everything outside the markers is preserved.
func writeHosts(block string) error {
	const begin = "# >>> knot >>>"
	const end = "# <<< knot <<<"
	cur, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	s := string(cur)
	if i := strings.Index(s, begin); i >= 0 {
		if j := strings.Index(s[i:], end); j >= 0 {
			s = s[:i] + s[i+j+len(end)+1:]
		}
	}
	s = strings.TrimRight(s, "\n") + "\n" + begin + "\n" + block + end + "\n"
	return os.WriteFile("/etc/hosts", []byte(s), 0o644)
}

func (a *Agent) client() *http.Client {
	return &http.Client{Timeout: 20 * time.Second}
}
