# knot

Mesh networking with a **VLESS + Reality** data plane. Every node gets a fixed
private IP; joining is one `docker run` and a token.

Your services talk to `10.88.0.2:5672` and neither end knows a tunnel exists.
From the outside, each machine looks like it is serving an ordinary HTTPS site
— because it is.

**Source, docs and issues: https://github.com/zbysir/knot**

## Two roles, one image

```bash
# control plane + web panel
docker run -d --name knot-head --restart=always \
  -p 8080:8080 -e KNOT_PASSWORD=<panel password> \
  -v knot-head:/var/lib/knot \
  bysir/knot:latest head

# agent, on every machine
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=https://head.example.com \
  -e KNOT_TOKEN=<TOKEN> \
  bysir/knot:latest node
```

Add `-e KNOT_ENDPOINT=host:port` to make a node a relay (that is the only
difference between a relay and a leaf).

## A node needs exactly three things

Each fails differently, and none of the errors name the real cause:

| Requirement | Missing it gives you |
|---|---|
| `--device /dev/net/tun` | `open: No such file or directory` — looks like a missing binary |
| `--cap-add NET_ADMIN` | `ioctl(TUNSETIFF): Operation not permitted` |
| `--network host` | **no error at all** — the TUN interface is created in the container's own namespace and the host cannot route into it |

`--privileged` is not needed.

## Tags

| Tag | What |
|---|---|
| `latest` | Tip of `main` |
| `1.2.3`, `1.2` | Release tags |
| `sha-abc1234` | Exact commit |

`linux/amd64` and `linux/arm64`.

## Two limits worth knowing before you deploy

**`ping` does not work.** This is a proxy mesh, not an L3 VPN — VLESS carries
streams, not raw IP packets. TCP is completely normal. Verify with `nc` or
`curl`.

**Only TCP survives a relay hop.** A relay session carries streams, not
datagrams, so UDP toward a relayed peer is dropped. Direct UDP is fine.

## What is inside

`knot` plus an unmodified upstream [sing-box](https://github.com/SagerNet/sing-box)
binary (pinned by version, built from source with explicit tags). sing-box runs
as a child process, not a linked library — its CLI and config schema are the
stable interface, its Go API is not.

knot itself is MIT. sing-box is GPL-3.0 and is invoked as a separate binary.
