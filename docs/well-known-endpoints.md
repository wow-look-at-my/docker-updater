# The `/.well-known/docker-updater/` update-check contract

The standard way a container tells docker-updater whether it accepts an update, and whether it came back healthy. It is a self-describing HTTP contract that needs **no labels**. A container that serves the endpoints is discovered automatically.

## Endpoints

Both answer `GET` and carry their meaning in the **status code**. Both must be cheap enough to call on every check cycle. The default cycle is 5 minutes.

| Path | Asked | 2xx | Other |
|---|---|---|---|
| `/.well-known/docker-updater/health` | after an update, until it passes or the timeout expires (default 60s) | the new container is up and serving | keep polling; on timeout the update is rolled back |
| `/.well-known/docker-updater/pre-update` | once, immediately before an update is applied | go ahead | hold the update; retried next cycle, reported as `skipped` |

`404` and `501` mean **not implemented**:

- on `health`, that is the whole container being unconfigured. docker-updater warns and falls back to Docker's `HEALTHCHECK` status. A container without a `HEALTHCHECK` falls back to nothing but the grace period. The warning says which, because those are very different guarantees.
- on `pre-update`, it is a normal, supported choice. The endpoint is optional. A container that serves only `health` is fully conformant, and updates are never held back for it.

`pre-update` is the place to say "not right now". Reasons include a running migration, a long request in flight, and a draining queue. It must not block. Answer immediately with what is true at that moment.

Neither endpoint needs a body. Anything returned is ignored, so a one-line handler is a complete implementation.

## Discovery

docker-updater builds the base URL from the container's own Docker state, so nothing is configured twice:

- **Address** — the container's network IP. Discovery takes the networks in name order, so a multi-network container resolves to the same endpoint every cycle. The probe dials that IP directly, and it [attaches itself](#attaching-to-the-network) to the network first when it is not on it already. A container with no IP of its own cannot be discovered, so give it an absolute `docker-updater.health-check.url` instead. The `host`, `none` and `container:` network modes are the cases with no IP.
- **Port** — the container's single exposed TCP port. With several exposed ports there is no way to guess which one speaks HTTP. Discovery stops and warns until `docker-updater.well-known.port` names one.

A container implements the contract when `health` answers with anything other than 404 or 501. A `503` from a container that is currently unhealthy still counts. That is a health problem, not a configuration one.

### Attaching to the network

An IP on a Docker network is only routable from that network. So docker-updater connects **itself** to each network it is not already on, before it probes the containers there. It holds the Docker socket to stop, create and start containers, so the connect costs no extra access. An operator never runs `docker network connect` for a stack.

The join needs docker-updater's own container ID, the same one self-update uses. That ID is auto-detected from `/proc/self/mountinfo`, or set explicitly with `DOCKER_UPDATER_CONTAINER_ID`. Without it, attaching is off, said once at startup. Checks then reach only containers on a network it already shares.

Networks are joined once per check cycle and never disconnected. An endpoint left behind by a departed container is harmless. Dropping a network something else attached breaks reachability docker-updater did not create. A join that fails is reported as a per-container warning. The warning names the network and the error, because that is why the probe cannot connect.

**It does not route between the networks it joins.** Membership of two networks is not a bridge between them. `ip_forward` is a per-namespace sysctl that starts at `0` in a container. Nothing in another container routes through docker-updater. Docker's own inter-network isolation rules are untouched. What each join does add is the other direction. The dashboard and webhook ports become reachable from that network, because docker-updater gets an address and a DNS name on it.

### The address after an update

An update replaces the container, and Docker gives the replacement its own IP. So the address discovery resolved belongs to a container that no longer exists. Docker can since have recycled that IP onto an unrelated container. Before it polls `health`, docker-updater re-resolves the address from the container it just started and rebuilds the URL. The port and path are unchanged, because a recreated container keeps the port it serves on.

If the new container has no IP of its own, the gate fails and the update rolls back. It never falls back to the pre-update address. That either burns the whole health budget on a dead IP, or passes the gate on another service's health.

This applies to every URL docker-updater builds itself: the standard endpoint, and the `:port/path` short form of `docker-updater.health-check.url`. An absolute `docker-updater.health-check.url` is polled exactly as written. It names something other than the container's own IP on purpose, such as a service name or a stable host. So its host is never rewritten.

## Labels

| Label | Effect |
|---|---|
| `docker-updater.well-known.port` | Which exposed port serves the endpoints. Required when the container exposes more than one TCP port |
| `docker-updater.well-known` = `false` | Skip discovery entirely and stop warning. For containers that legitimately serve no HTTP — a database, a queue worker |

## Warnings

Warnings are shown per row on the dashboard, in amber, under the container name. Each one is logged once per container, and re-logged only when it changes. They never block an update. They describe configuration, not failure.

- **Missing** — `does not serve /.well-known/docker-updater/health (probed http://…); post-update liveness falls back to Docker HEALTHCHECK.` Implement the endpoint, or opt out with `docker-updater.well-known=false`. The container answered, so this is the only warning that says anything about its code.
- **Unreachable** — `cannot reach http://…/.well-known/docker-updater/health (dial tcp …: connect: connection refused); … docker-updater must be attached to a network the container is on …`. No answer arrived, so nothing is known about whether the endpoint exists. docker-updater attaches itself to the network, so the usual cause is the port. Check that `docker-updater.well-known.port` names one the container serves on. An **Unattached** warning in the same cycle explains it instead.
- **Unattached** — `cannot attach docker-updater to this container's network <id>: <error>.` The join failed, so nothing is routable to that container this cycle. It is retried on the next one.
- **Undiscoverable** — `no standard update endpoints: container exposes 3 TCP ports (80, 443, 9000); set docker-updater.well-known.port to pick one.`
- **Nonstandard** — `nonstandard update checks: docker-updater.health-check.url overrides the standard /.well-known/docker-updater/ endpoints.` The container works exactly as before. The warning marks it as not yet migrated.

The Missing warning names the gate the container ACTUALLY gets, which is not the same sentence for every container. One with a Docker `HEALTHCHECK` really does fall back to it. One without gets `post-update liveness is UNVERIFIED beyond the container staying up briefly (this container has no Docker HEALTHCHECK to fall back to)`. The reason is that `waitHealthy` waits for a health status only when there is one, and otherwise degrades to the grace period. Printing the `HEALTHCHECK` sentence for that container tells an operator an update was verified when nothing verified it.

## Compatibility with the label-configured checks

The original opt-in labels still work and always win. Those labels are `docker-updater.pre-check.url` and `.command`, plus `docker-updater.health-check.url` and `.command`. Discovery fills in only what no label set, per check. A container can carry an explicit `health-check.command` and still use the standard `pre-update` endpoint. Any of those labels marks the container nonstandard.

To migrate: implement the two endpoints, delete the labels, and the warning goes away on the next cycle.

`docker-updater.well-known: "false"` does not silence this one. It answers a different question: are there standard endpoints to look for here. A nonstandard check is a choice you are told about for as long as it is in effect. An image you do not build keeps the warning permanently, which is accurate. Its checks are nonstandard permanently.

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

[RFC 8615](https://www.rfc-editor.org/rfc/rfc8615) reserves the prefix for exactly this: a path an automated client requests without prior arrangement. It also keeps the contract out of an application's own namespace, so adding it cannot collide with an existing route.
