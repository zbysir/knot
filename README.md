# knot

[中文](./README.zh-CN.md)

[![ci](https://github.com/zbysir/knot/actions/workflows/ci.yml/badge.svg)](https://github.com/zbysir/knot/actions/workflows/ci.yml)
[![docker](https://img.shields.io/docker/v/bysir/knot?logo=docker&label=docker%20hub)](https://hub.docker.com/r/bysir/knot)

A small mesh network whose data plane is **VLESS + Reality**. Every node gets a
fixed private IP; joining is one `docker run` and a token.

Your services talk to `10.88.0.2:5672` and neither end knows a tunnel exists.
From the outside, each machine looks like it is running an ordinary HTTPS site
— because it is.

```
                     ┌──────────────────────┐
                     │  relay               │
                     │  knot head  :8080    │  control plane, off the data path
                     │  knot node  :443     │  Reality; non-tunnel traffic
                     └──────┬───────┬───────┘  falls back to a real site
                    Reality │       │ Reality
              ┌─────────────┘       └─────────────┐
              │                                   │
     ┌────────┴────────┐                 ┌────────┴────────┐
     │  leaf A         │                 │  leaf B         │
     │  no open ports  │                 │  no open ports  │
     └─────────────────┘                 └─────────────────┘
```

## Why this combination

Three requirements at once leave a narrow choice:

1. **A private network across borders** — fixed internal IPs, no per-application
   proxy configuration, no protocol changes
2. **Redundancy** — when the path between two machines dies, traffic must still
   get through, and must not be pinned to one relay
3. **Traffic that cannot be identified** — a small IP pool means every blocked
   address hurts; slow is fine, being fingerprinted as a VPN is not

Requirement 3 rules out WireGuard and Tailscale (their payloads carry cleartext
protocol markers) and plain VMess. Requirement 1 rules out point-to-point
tunnels — running xray by itself does not give you a network. What is left is
**Reality for transport, TUN for the network**.

### Why not just xray?

A fair question, and an almost backwards one: **Reality is xray's invention.**
sing-box's implementation is a port. So knot already runs xray's protocol — the
question is only which implementation carries it.

| | xray-core | sing-box |
|---|---|---|
| VLESS + Reality | ✅ **the original** | ✅ port |
| XTLS-Vision | ✅ **the original** | ✅ |
| Fallback | ✅ **richer** — can branch on SNI / ALPN / path | ✅ enough for us |
| Routing rules | ✅ | ✅ |
| Health-checked failover | ✅ `observatory` + `balancer` | ✅ `urltest` |
| **TUN** | ❌ **none** | ✅ built in |

The last row decides it. xray-core has no TUN inbound; the ecosystem bolts on
`tun2socks` or `hev-socks5-tunnel`, and GUI clients ship their own TUN layer
around it.

**Without TUN this is a proxy, not a network.** Applications have to be able to
open `10.88.0.2:5672` directly — RabbitMQ clients and database drivers have no
proxy setting to configure. Something must own the mesh CIDR and hand packets
to the tunnel, and with xray that is a *second* child process.

So switching would buy a more mature Reality implementation and a more flexible
fallback, at the cost of one more moving part. Not worth it.

> This reflects xray-core as of writing. Verify against current releases before
> acting on it.

The third option is to drop the child process entirely —
[`sing-tun`](https://github.com/sagernet/sing-tun) for the interface plus a
Reality library such as [`utls`](https://github.com/metacubex/utls). That is
tracked in [DESIGN-relay.md](./DESIGN-relay.md) and deliberately deferred: the
sing-box CLI and config schema are a stable interface, its Go API is not.

Running sing-box out-of-process has a second, unplanned benefit — it is
GPL-3.0, and calling a separate binary keeps that off knot itself.

## Components

| Component | Role |
|---|---|
| `knot head` | Control plane: node registry, routing policy, tokens, web panel. **Never on the data path** |
| `knot node` | Per-machine agent: joins with a token, syncs config, supervises sing-box |
| sing-box | The data plane: Reality in/out, TUN, and health-checked relay failover |

sing-box runs as a **child process** in the same container rather than being
imported as a library. Its Go API changes between releases; the CLI and config
schema are the stable interface. Upgrading is a version bump in the Dockerfile.

### What knot adds on top of sing-box

sing-box is the engine. knot is what turns N machines into one managed network.
**It is not a fork** — the container ships the upstream binary, pinned by
version, with no patches.

**1. A config compiler** (`internal/sb`, ~300 lines). sing-box wants one
hand-written JSON file per machine, and those files are coupled to each other:

- a relay's `users` list must contain **every** node's UUID — miss one and that
  node's handshake fails with `processed invalid connection`, which points
  nowhere near the cause
- every leaf needs an outbound plus route rules for each relay it may dial
- multiple relays have to be wrapped in a `urltest` group
- route rules must **not** filter on `inbound`, or relay-forwarded traffic falls
  through to `final: direct` and the relay tries to dial the peer on itself
- UDP toward a relayed peer has to be explicitly blocked, or every datagram
  becomes a `socks5: request rejected, code=7` in the log

Each of those is a bug we hit. They are all encoded in the generator now, so
adding a machine touches no config at all.

**2. Identity and key lifecycle** (`internal/head`). Reality keypairs, shortIDs,
UUIDs and node keys are generated centrally and pushed to whoever needs them.
Including one detail you only find by reading sing-box's source: **the x25519
private key must be clamped**. sing-box uses the stored bytes as-is, so an
unclamped key derives a public key that does not match the one you handed out.

**3. Bidirectional relaying** (`internal/relay` + `node/relayd`, ~700 lines).
**This is the part sing-box does not have at all.** VLESS is one-directional, so
a relay cannot dial back to a leaf and `leaf A → relay → leaf B` is impossible.
knot layers yamux inside the Reality tunnel sing-box already built, letting a
leaf reuse its own outbound connection in both directions. That is a protocol,
not a config trick — and it is what lets a leaf keep zero open ports.

**4. Agent lifecycle** (`internal/node`). Config is polled with an ETag and
validated by `sing-box check` before it is swapped in, so a bad config can never
take a node's data plane away. The three parts of a response — sing-box config,
`/etc/hosts` block, relay plan — are compared and applied independently, so a
change costs only what it actually touches: a node joining updates every relay's
peer map without disturbing a single session. A sing-box config that really did
change replaces the child process, waiting for the old one to be reaped first,
and the child is supervised independently of the head. Peer names are kept in
`/etc/hosts`.

Plus the web panel: node table, routing matrix, tokens.

## Leaves never open a port

sing-box's VLESS/Reality is strictly one-directional: clients dial servers, and
that is it. Taken literally, a relay cannot dial back to a leaf, so
`leaf A → relay → leaf B` does not work.

Giving every leaf a public endpoint would fix that — and would also mean every
machine exposes an inbound tunnel port, which defeats the whole point.

So a leaf **reuses the connection it already dialled out**, multiplexing it in
both directions. This is what DERP does:

```
leaf A ──Reality──> relay <──Reality── leaf B
        <═════════         ═════════>
        bidirectional multiplexing over the same connection
```

Each leaf "homes" on every relay it can reach and keeps one idle session there.
When the relay needs to push traffic to a leaf, it opens a new stream over that
existing session. **A leaf listens on `127.0.0.1:9998` and nothing else.**

The relay's own `:9997` binds **only to its mesh address** (e.g.
`10.88.0.1:9997`), so it is unreachable from the internet — the single
externally open port is 443. Session auth compares a hash of the head-issued
node key, not "any non-empty string".

Multiplexing is yamux, running inside the Reality tunnel sing-box already
established — which is why there is not a single line of TLS code in
`internal/relay/`. The reasoning is in [DESIGN-relay.md](./DESIGN-relay.md).

## Two limits you must know

**1. `ping` does not work.** This is a *proxy mesh*, not a real L3 VPN. TCP is
completely normal (HTTP, databases, message queues are all fine), but VLESS
carries streams, not raw IP packets. Verify connectivity with `nc` or `curl`.

**2. Only TCP survives a relay hop.** A relay session carries streams, not
datagrams, so UDP to a relayed peer is dropped. Direct UDP is fine. In practice
the only thing that hits this is another overlay: `tailscaled` will advertise
knot's TUN address as a WireGuard endpoint and probe it every 5 seconds; it
fails over on its own.

## Getting started

### 1. Control plane

```bash
docker run -d --name knot-head --restart=always \
  -p 8080:8080 \
  -e KNOT_PASSWORD=<panel password> \
  -v knot-head:/var/lib/knot \
  bysir/knot:latest head
```

Open `http://<head>:8080` and log in.

> Set the password with `KNOT_PASSWORD`, **not** `knot passwd`. The subcommand
> writes the state file directly and the running server would overwrite it —
> `knot passwd` is only for when the head is stopped.

> The panel and the node-join API share this port. **Put it behind a reverse
> proxy with TLS before exposing it**, or let it terminate TLS itself with
> `KNOT_TLS_CERT` / `KNOT_TLS_KEY`. Anyone who reaches it can mint join tokens
> and read every node's Reality private key.

Or use [deploy/docker-compose.yml](./deploy/docker-compose.yml) /
[deploy/swarm-stack.yml](./deploy/swarm-stack.yml) — see
[deploy/README.md](./deploy/README.md), which also covers the two swarm
substitutions that fail silently.

### 2. Add nodes

Generate a token in the panel, then on each machine:

```bash
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=https://head.example.com \
  -e KNOT_TOKEN=<TOKEN> \
  -e KNOT_NAME=$(hostname) \
  bysir/knot:latest node
```

`KNOT_TOKEN` is only needed on the first run; after that the node authenticates
with its own key.

**To make a machine dialable by others (i.e. a relay), add an endpoint:**

```bash
  -e KNOT_ENDPOINT=relay.example.com:443
```

The head generates its Reality keypair and shortId and hands the public half to
whoever needs to dial it.

### 3. Routing

Leave it alone and you get the default policy: **direct if dialable, otherwise
any relay.** That is usually right.

To be explicit, the panel's routing matrix has one cell per ordered pair, and
each cell takes an **ordered** list of candidate paths:

- earlier entries win
- with more than one, health checks pick the first live path
- empty cell = default policy

> An **empty path list** is not "use the default" — it means *no path exists*
> and will cut that pair off. "Use the default" is having no rule at all.

### Pointing nodes at the head over the mesh

Once a node has joined, it does not need the head's public address any more —
it can reach it through the mesh. That lets you close the head's public port
entirely.

```bash
# 1. Join normally, using an address the new node can actually reach.
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=http://<public-addr>:8080 \
  -e KNOT_TOKEN=<TOKEN> \
  bysir/knot:latest node

# 2. Once it is up and has a mesh address, re-create it pointing at the head's
#    MESH address. KNOT_TOKEN is no longer needed.
docker rm -f knot-node
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=http://10.88.0.1:8080 \
  bysir/knot:latest node
```

`KNOT_HEAD` overrides the address recorded at join time, so step 2 is just an
env change.

The startup then looks like this, and every line of it is expected:

```
knot: head address changed http://<public-addr>:8080 -> http://10.88.0.1:8080
knot: initial sync failed (... context deadline exceeded)   <- mesh is not up yet
knot: starting from cached config                            <- so use last known
knot: relay: session to 10.88.0.1:9997 up                    <- mesh is up now
                                                             <- next poll succeeds
```

**A node has to have joined at least once.** With no cached config there is
nothing to bootstrap the mesh from, and pointing a brand-new node at a mesh
address is a deadlock: the head needs the mesh, the mesh needs config, the
config needs the head.

Since only relays are reachable by every node, **the head belongs on a relay** —
its reachability requirement is identical, so co-locating costs nothing.

## Sharing 443 with an existing site

Reality **cannot** sit behind nginx or traefik — it has to own the TLS handshake
to do its job. But you can invert it: Reality takes 443 and forwards everything
that is not a tunnel connection to your existing web server.

```
:443 → Reality
        ├── authenticated handshake → tunnel
        └── everything else         → forwarded to the node's fallback address
```

In the panel, set the node's **fallback** to your real local site (e.g. the
`127.0.0.1:8443` traefik moved to) and its **SNI** to that site's domain. Active
probing of your 443 then finds a **genuine, working site with a valid
certificate** — because it is one. That is far more convincing than borrowing
`www.microsoft.com`.

### ⚠️ The fallback site's certificate chain must be small

Reality relays the fallback's handshake through a fixed-size buffer. A large
chain overruns it, and the failure looks nothing like the cause: **the
connection dies *after* Reality auth succeeds**, reported as a generic
`REALITY: processed invalid connection`. You need `log.level: trace` to see it.

Measure before you commit:

```bash
openssl s_client -connect HOST:443 -servername HOST -tls1_3 </dev/null |
  awk '/BEGIN CERT/,/END CERT/' | wc -c
```

| Site | Bytes | Works |
|---|---|---|
| a typical single-domain site | ~1800 | ✅ |
| `dl.google.com` | 2732 | ✅ |
| `www.microsoft.com` | 8273 | ❌ |

## Environment variables

### `knot head`

| Variable | Meaning |
|---|---|
| `KNOT_LISTEN` | Listen address, default `:8080` |
| `KNOT_PASSWORD` | Panel password, applied at boot |
| `KNOT_TLS_CERT` / `KNOT_TLS_KEY` | Terminate TLS directly |
| `KNOT_DATA` | State directory, default `/var/lib/knot` |

### `knot node`

| Variable | Meaning |
|---|---|
| `KNOT_HEAD` | Head URL — **required** |
| `KNOT_TOKEN` | Join token, first run only |
| `KNOT_NAME` | Node name, defaults to hostname |
| `KNOT_ENDPOINT` | Public `host:port` — **setting this makes it a relay** |
| `KNOT_DATA` | State directory, default `/var/lib/knot` |
| `KNOT_SINGBOX` | sing-box binary path, default `sing-box` |

## Runtime behaviour

- Nodes poll config every 30s; the sing-box config, the `/etc/hosts` block and
  the relay plan are compared byte by byte and applied independently, so
  **adding a node disturbs neither sing-box nor any relay session** on the nodes
  that were already there
- A sing-box config that did change replaces the process, and the stop **waits
  for the old child to be reaped** before the new one starts. Signals are
  asynchronous and the tun device stays open until the kernel has finished with
  the process, so starting the replacement early killed it with
  `TUNSETIFF: device or resource busy`. Not SIGHUP: sing-box supports it, but
  `instance.Close()` does not fully release a tun inbound, so the in-process
  rebuild hits the same error and the SIGHUP path discards the close error and
  exits — a config change that kills sing-box rather than reloading it
- sing-box is supervised independently of the head, so a child that dies is
  restarted even while the head is unreachable — on a relay the head is reached
  *through* that child, so nothing else could have noticed
- New config is validated with `sing-box check` before it is swapped in, so a
  bad config can never leave a node without a data plane
- If the head says it does not know this node (401), the agent re-joins with
  `KNOT_TOKEN` instead of polling a dead credential forever
- Relay sessions reconnect automatically with backoff capped at 60s — a relay
  that was down for an hour is picked back up within a minute
- Before forwarding, knot checks whether the target relay's session is actually
  alive, so a dead primary costs nothing instead of a dial timeout per
  connection
- **The head going down does not stop forwarding**: nodes keep running the
  config they already hold, they just stop receiving changes
- Node names are written to a marked block in `/etc/hosts`; both `<name>.knot`
  and the bare name resolve

> Name resolution is host-only. **Containers have their own `/etc/hosts`** and
> will not see it — inside a container, use the mesh IP.

## Development

```bash
go build ./...      # go.mod declares 1.24.7; Go 1.21+ switches toolchain automatically
go vet ./...
docker build -t knot .
```

State is a single JSON file (`$KNOT_DATA/state.json`) — readable and editable by
hand.

Cross-compiling for a small server is worth it; building sing-box from source
needs more RAM than a 2 GB box has:

```bash
docker buildx build --platform linux/amd64 -t <registry>/knot:latest --push .
```

## License

[MIT](./LICENSE)

knot builds on [sing-box](https://github.com/SagerNet/sing-box) (data plane)
and [yamux](https://github.com/hashicorp/yamux) (relay multiplexing).
