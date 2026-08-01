# Bidirectional relay

[中文](./DESIGN-relay.zh-CN.md)

> **Implemented.** This records the reasoning; the code is in
> `internal/relay/` and `internal/node/relayd.go`.

## The problem

The data plane is sing-box, and **sing-box's VLESS/Reality is strictly
one-directional**: clients dial servers, full stop.

```
leaf → relay            ✓  the leaf dials
relay → leaf            ✗  nothing to dial; a leaf has no endpoint
leaf A → relay → leaf B ✗  stuck on the second hop
```

The obvious workaround is to give every leaf an endpoint too — turning every
node into a relay. **That means every machine exposes an inbound tunnel port**,
which defeats the reason for using Reality in the first place. If your IP pool
is small, each exposed port is a liability.

## The goal

A leaf is **outbound-only** and listens on nothing. Any two leaves can still
reach each other through a relay.

```
leaf A ──Reality──> relay <──Reality── leaf B
        <═════════         ═════════>
        bidirectional multiplexing over the same connection
```

## The key decision: don't implement Reality

The first instinct is to have knot speak Reality itself —
`github.com/metacubex/utls` exposes both sides as a Go library, so knot could
import it and skip sing-box for this.

**We did the opposite: the control session rides inside the Reality tunnel
sing-box has already built.**

```
leaf knot ──dial relayVIP:9997──> sing-box ──Reality──> relay sing-box ──> relay knot
                                                        ↑ persistent, bidirectional yamux
```

The leaf dials the relay's *mesh* address. Its own TUN captures that, routes it
through the existing Reality outbound, and the relay's sing-box hands over
plaintext. So `internal/relay/` contains **no TLS code at all** — no second
Reality implementation to keep in sync with the first, no second set of
fingerprints to get wrong.

## Protocol

Deliberately tiny, because yamux already does framing, ordering and flow
control.

### Session setup

1. The leaf dials the relay's mesh address and sends `HELLO{nodeID, key}`
2. The relay compares `sha256(key)` against the hashes the head handed it, and
   registers the connection in a `nodeID → session` table
3. Both sides run **yamux** over it; either end can open streams
4. The leaf keeps this session up, reconnecting with backoff capped at 60s

The head sends relays a `nodeID → sha256(key)` map rather than the keys, so a
readable relay state file does not leak every node's credential.

**A leaf homes on every relay it can dial**, not just the ones some route names
today. A relay can only push traffic down a session that already exists — a
backup relay is useless if the destination was never homed there. The cost is
one idle TCP connection per relay.

### Forwarding

Leaf A wants `10.88.0.5:5672` on leaf B:

1. sing-box on A routes that address to knot's local SOCKS listener
2. knot opens a stream on its session and writes `OPEN{dstNodeID, dstAddr}`
3. The relay looks up B's session, opens a stream on **that** session, and
   passes the same header through unchanged
4. B dials `dstAddr` locally and answers
5. The relay splices the two streams and never parses the payload

**A relay that is itself the source uses its own registry directly** instead of
bouncing off another relay.

### Two details that matter more than they look

**Answer the SOCKS request only after the path is known.** Replying "succeeded"
and *then* discovering the peer is unreachable turns every routing mistake into
"the TCP connect worked but no bytes ever arrived" — a symptom that points
nowhere near the cause. It also makes `nc -z` lie: a probe that only completes
the handshake reports a peer as up when nothing can get through.

**Failures carry a reason.** `OPEN` is answered with OK or a failure string, so
the caller can immediately try its next path instead of waiting out a timeout,
and the log says which of the two ends refused.

### Health and failover

- yamux keepalive at 15s (the 30s default leaves half-open sessions around long
  enough to matter on a flaky cross-border link)
- A reconnecting node replaces its old session, and the stale one is closed so
  its streams are freed instead of timing out
- Before using a relay, knot checks the session is alive — a dead primary costs
  nothing rather than a dial timeout per connection
- Which relay an `OPEN` uses comes from the head's routing policy; the ordering
  and failover happen on the leaf

## Limits

**Streams only, no datagrams.** UDP to a relayed peer cannot work, so it is
explicitly dropped at the routing layer rather than handed to a SOCKS outbound
that would reject every UDP-ASSOCIATE and fill the log with errors.

**Single hop.** `A → relay1 → relay2 → B` is not implemented. A relay that does
not have `dstNodeID` in its registry fails the `OPEN` rather than re-routing.
The protocol has room for it — the relay already routes by node ID, not address
— but nothing needed it yet.

## Where this leaves sing-box

sing-box is now doing TUN, Reality transport, and routing. Since the relay path
no longer depends on its outbound selection logic, replacing it with
`github.com/sagernet/sing-tun` plus a Reality library would remove the child
process entirely.

**Not worth it yet.** The CLI and config schema are a stable interface;
its Go API is not.
