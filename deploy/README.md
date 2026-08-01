# Deployment

Three ways, all equivalent at runtime. Pick by what you already run.

| | File | Notes |
|---|---|---|
| `docker run` | [in the main README](../README.md#getting-started) | Fewest moving parts |
| docker compose | [docker-compose.yml](./docker-compose.yml) | |
| Docker Swarm | [swarm-stack.yml](./swarm-stack.yml) | **Two non-obvious substitutions — read below** |

## What a node needs, and why

Whatever you use, a `knot node` needs exactly three things. Each one fails
differently when it is missing, and none of the errors name the real cause:

| Requirement | Missing it gives you |
|---|---|
| `/dev/net/tun` | `open: No such file or directory` — looks like a missing binary |
| `CAP_NET_ADMIN` | `ioctl(TUNSETIFF): Operation not permitted` |
| host network namespace | **no error at all** — the interface is created, and the host cannot see it |

That third one is the trap. A TUN interface belongs to the network namespace it
was created in. In the container's own namespace it comes up fine, gets its
address, and is completely useless: nothing on the host can route into it.

`--privileged` is not needed. The two specific grants are enough.

## Swarm: two things that are not the same as compose

Both were verified on a live swarm, not assumed.

**1. `devices:` is silently dropped.** `docker stack deploy` accepts the key,
prints no warning, and the container simply has no `/dev/net/tun`. Bind-mount
the device node instead:

```yaml
    volumes:
      - /dev/net/tun:/dev/net/tun     # NOT devices:
```

**2. `network_mode: host` is ignored.** Declare the predefined `host` network
as external and attach to it:

```yaml
networks:
  hostnet:
    name: host
    external: true
```

With both in place a swarm task really does land in the host namespace — same
`/proc/self/ns/net` inode as PID 1 — and the interface it creates shows up on
the host.

**Node naming.** `KNOT_NAME: "{{.Node.Hostname}}"` is expanded by swarm per
task, so a `mode: global` service gives every machine its own node name.

**Relays do not fit a global service.** Each relay needs its own
`KNOT_ENDPOINT`, and one service definition cannot vary it per host. Run relays
as a separate service with a placement constraint, or just `docker run` them.

## The head

The head has no special requirements — it is an ordinary HTTP service and is
never on the data path. Put it wherever is convenient.

**Do not expose it without TLS.** Anyone who reaches it can mint join tokens
and read every node's Reality private key. Either front it with a reverse proxy
or set `KNOT_TLS_CERT` / `KNOT_TLS_KEY`.

The password is set with `KNOT_PASSWORD` at boot. Note that an environment
variable is readable via `docker inspect`, and under swarm it also goes into
the raft log — so treat the panel password as low-value and rely on TLS plus
the login throttle, not on the password being secret from anyone with docker
access to the host.

## Upgrading

Relays first, then leaves.

Nodes poll config every 30s and compare by ETag, so an unchanged config never
restarts sing-box — a restart would drop every live connection. New config is
validated with `sing-box check` before it is swapped in, so a bad config cannot
take a node's data plane away.
