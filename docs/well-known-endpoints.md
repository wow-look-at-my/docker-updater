# The `/.well-known/docker-updater/` update-check contract

The standard way a container tells docker-updater whether it may be updated and
whether it came back healthy. It is HTTP, self-describing, and requires **no
labels**: a container that serves the endpoints is discovered automatically.

## Endpoints

Both answer `GET`, carry their meaning in the **status code**, and should be
cheap enough to call on every check cycle (default: every 5 minutes).

| Path | Asked | 2xx | Other |
|---|---|---|---|
| `/.well-known/docker-updater/health` | after an update, until it passes or the timeout expires (default 60s) | the new container is up and serving | keep polling; on timeout the update is rolled back |
| `/.well-known/docker-updater/pre-update` | once, immediately before an update is applied | go ahead | hold the update; retried next cycle, reported as `skipped` |

`404` and `501` mean **not implemented**:

- on `health`, that is the whole container being unconfigured — docker-updater
  warns and falls back to Docker's `HEALTHCHECK` status, or, for a container
  that has no `HEALTHCHECK` either, to nothing but the grace period. The
  warning says which, because those are very different guarantees.
- on `pre-update`, it is a normal, supported choice. The endpoint is optional;
  a container that only serves `health` is fully conformant and updates are
  never held back for it.

`pre-update` is the place to say "not right now": a migration is running, a
long request is in flight, a queue is draining. It must not block — answer
immediately with what is true at that moment.

Neither endpoint needs a body. Anything returned is ignored, so a one-line
handler is a complete implementation.

## Discovery

docker-updater builds the base URL from the container's own Docker state, so
nothing is configured twice:

- **Address** — the container's first network IP. docker-updater dials it
  directly, so it has to be attached to a network the container is on. A
  container with no IP of its own (`host`, `none`, or `container:` network
  mode) cannot be discovered; give it an absolute
  `docker-updater.health-check.url` instead.
- **Port** — the container's single exposed TCP port. With several exposed
  ports there is no way to guess which one speaks HTTP, so discovery stops and
  warns until `docker-updater.well-known.port` names one.

A container is considered to implement the contract when `health` answers with
anything other than 404/501. A `503` from a container that is currently
unhealthy still counts: that is a health problem, not a configuration one.

### The address after an update

An update replaces the container, and Docker gives the replacement its own IP —
so the address discovery resolved belongs to a container that no longer exists,
and may since have been recycled onto an unrelated one. Before polling `health`,
docker-updater re-resolves the address from the container it just started and
rebuilds the URL; the port and path are unchanged, because a recreated container
keeps the port it serves on.

If the new container has no IP of its own, the gate fails and the update rolls
back. It never falls back to the pre-update address: that either burns the whole
health budget on a dead IP or passes the gate on another service's health.

This applies to every URL docker-updater builds itself — the standard endpoint,
and the `:port/path` short form of `docker-updater.health-check.url`. An
absolute `docker-updater.health-check.url` is polled exactly as written: it names
something other than the container's own IP on purpose (a service name, a stable
host), so its host is never rewritten.

## Labels

| Label | Effect |
|---|---|
| `docker-updater.well-known.port` | Which exposed port serves the endpoints. Required when the container exposes more than one TCP port |
| `docker-updater.well-known` = `false` | Skip discovery entirely and stop warning. For containers that legitimately serve no HTTP — a database, a queue worker |

## Warnings

Warnings are shown per row on the dashboard (amber, under the container name)
and logged once per container, re-logged only when they change. They never
block an update — they describe configuration, not failure.

- **Missing** — `does not serve /.well-known/docker-updater/health (probed
  http://…); post-update liveness falls back to Docker HEALTHCHECK.` Implement
  the endpoint, or opt out with `docker-updater.well-known=false`. The container
  answered: this is the only warning that says anything about its code.

Both warnings name the gate the container will ACTUALLY get, which is not the
same sentence for every container. One with a Docker `HEALTHCHECK` really does
fall back to it. One WITHOUT gets `post-update liveness is UNVERIFIED beyond
the container staying up briefly (this container has no Docker HEALTHCHECK to
fall back to)` — because `waitHealthy` only waits for a health status when
there is one, and otherwise degrades to the grace period. Printing the
`HEALTHCHECK` sentence for that container would tell an operator an update was
verified when nothing verified it.
- **Unreachable** — `cannot reach http://…/.well-known/docker-updater/health
  (dial tcp …: connect: connection refused); … docker-updater must be attached
  to a network the container is on …`. No answer arrived, so nothing has been
  learned about whether the endpoint exists. Check that docker-updater is
  attached to one of the container's networks (`docker network connect <net>
  docker-updater`) and that `docker-updater.well-known.port` names a port the
  container serves on.
- **Undiscoverable** — `no standard update endpoints: container exposes 3 TCP
  ports (80, 443, 9000); set docker-updater.well-known.port to pick one.`
- **Nonstandard** — `nonstandard update checks: docker-updater.health-check.url
  overrides the standard /.well-known/docker-updater/ endpoints.` The container
  works exactly as before; the warning marks it as not yet migrated.

## Compatibility with the label-configured checks

The original opt-in labels — `docker-updater.pre-check.url` / `.command` and
`docker-updater.health-check.url` / `.command` — still work and always win.
Discovery fills in only what no label set, per check: a container can carry an
explicit `health-check.command` and still use the standard `pre-update`
endpoint. Any of those labels marks the container nonstandard.

To migrate: implement the two endpoints, delete the labels, and the warning
goes away on the next cycle.

`docker-updater.well-known: "false"` does not silence this one. It answers a
different question -- "are there standard endpoints to look for here" -- and
choosing a nonstandard check is a choice you are told about for as long as it
is in effect. An image you do not build keeps the warning permanently, which is
accurate: its checks are nonstandard permanently.

## Implementing it

Go:

```go
mux.HandleFunc("/.well-known/docker-updater/health", func(w http.ResponseWriter, _ *http.Request) {
    if !ready() {
        w.WriteHeader(http.StatusServiceUnavailable)
        return
    }
    w.WriteHeader(http.StatusOK)
})

mux.HandleFunc("/.well-known/docker-updater/pre-update", func(w http.ResponseWriter, _ *http.Request) {
    if migrationRunning() {
        w.WriteHeader(http.StatusServiceUnavailable) // hold; asked again next cycle
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

Express:

```js
app.get('/.well-known/docker-updater/health', (_, res) => res.sendStatus(ready() ? 200 : 503));
app.get('/.well-known/docker-updater/pre-update', (_, res) => res.sendStatus(busy() ? 503 : 200));
```

## Why `/.well-known/`

[RFC 8615](https://www.rfc-editor.org/rfc/rfc8615) reserves the prefix for
exactly this: a path an automated client may request without prior arrangement.
It also keeps the contract out of an application's own namespace, so adding it
cannot collide with an existing route.
