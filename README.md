# docker-updater

Automatic Docker container updater service. Monitors running containers and updates them when newer versions are available.

## Features

- **Image-based updates**: Detects new image digests from container registries (watchtower-style)
- **Git-based updates**: Monitors git remote refs via smart HTTP protocol to detect new commits
- **Pre-update checks**: HTTP or exec-based checks to verify containers are ready before updating
- **Post-update health checks**: HTTP or exec-based checks to verify new containers are healthy without requiring `curl` or any binary inside the image
- **Web dashboard**: Built-in status page showing every container (monitored or not), uptime, restart count, last pull, and whether a newer image is upstream
- **Webhook notifications**: Supports generic, Discord, and Slack webhooks
- **GitHub webhook trigger**: An authenticated inbound endpoint that lets GitHub kick off a check the moment a ghcr image is pushed, instead of waiting for the next interval
- **Dry-run mode**: Monitor for updates without applying them
- **Scratch image**: No external dependencies at runtime

## Usage

```bash
docker run -d \
  --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/wow-look-at-my/docker-updater
```

Host networking is recommended so docker-updater can reach containers' pre-check endpoints directly via their bridge IPs, without joining every container's network.

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKER_UPDATER_INTERVAL` | `5m` | Check interval (Go duration) |
| `DOCKER_UPDATER_LABEL` | `docker-updater.enable` | Container opt-in label |
| `DOCKER_UPDATER_WEBHOOK_URL` | | Outbound notification webhook endpoint URL |
| `DOCKER_UPDATER_WEBHOOK_TYPE` | `generic` | `generic`, `discord`, or `slack` |
| `DOCKER_UPDATER_DRY_RUN` | `false` | Monitor only, don't update |
| `DOCKER_UPDATER_CONFIG` | | Path to Docker `config.json` for private registry auth |
| `DOCKER_UPDATER_DASHBOARD_ADDR` | `:8080` | Listen address for the web dashboard; set empty to disable |
| `DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR` | | Listen address for the inbound GitHub webhook (e.g. `:9000`); empty disables it |
| `DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET` | | Shared secret for verifying GitHub webhook signatures; **required** when the webhook is enabled |
| `DOCKER_UPDATER_GITHUB_WEBHOOK_PACKAGES` | | Comma-separated allowlist of package names (or `namespace/name`) that may trigger a check; empty means any package. Most useful with an org-level webhook |

## Dashboard

docker-updater serves a read-only web dashboard (default `:8080`) that lists
**every** container on the host -- both the ones it auto-updates and the ones it
doesn't -- so you can see your fleet at a glance:

![docker-updater dashboard](docs/dashboard.png)

- **Auto-update status**: whether a container is monitored, and in `image` or `git` mode
- **State & uptime**: running/stopped, healthcheck status, and how long it has been up
- **Restarts**: Docker's restart count -- how many times the daemon's restart policy has restarted the container since it was last (re)created. Because docker-updater creates a fresh container on every pull, this reads as "restarts since the last pull" for monitored containers, and a non-zero value flags a crash-looping container
- **Last checked / last pulled**: *last checked* is when docker-updater last polled the registry; *last pulled* is when it last actually downloaded a newer image. An up-to-date check that downloads nothing does **not** advance "last pulled", so the column reflects genuine image changes rather than resetting every cycle
- **Upstream**: whether a newer image digest (or git commit) is available, or the last error

The page auto-refreshes every few seconds. The same data is available as JSON at
`/api/containers`, and `/healthz` returns `200 ok` for external liveness probes.

With `--network host` (recommended), the dashboard is reachable on the host's
port directly, e.g. `http://<host>:8080`. Without host networking, publish the
port (`-p 8080:8080`). Set `DOCKER_UPDATER_DASHBOARD_ADDR` to a different address
(e.g. `:9000`) to move it, or to an empty string to disable it entirely.

Note that "last pulled" and "last checked" only apply to monitored containers;
the tool never polls the registry for containers it isn't watching.

## GitHub Webhook Trigger

Normally docker-updater polls the registry every `DOCKER_UPDATER_INTERVAL`. With
a short interval that means a lot of needless polling; with a long interval a
freshly pushed image can sit unnoticed for a while. The inbound GitHub webhook
closes that gap: when GitHub Packages publishes or updates a package (a ghcr
image), it POSTs to docker-updater, which runs a check **immediately** instead
of waiting out the rest of the interval.

This endpoint is meant to be reachable from the public internet (GitHub has to
reach it), so it is locked down:

- **Authenticated.** Every request must carry a valid GitHub `X-Hub-Signature-256`
  HMAC-SHA256 signature of the raw body, computed with your shared secret and
  checked in constant time. Requests without a valid signature get `401` and are
  ignored.
- **Fail closed.** The listener refuses to start without
  `DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET`; docker-updater will exit with a config
  error rather than expose an unauthenticated trigger.
- **Isolated.** It runs on its own listen address, separate from the dashboard,
  so you can expose this port publicly while keeping the dashboard internal.
- **Hardened.** POST-only, the body is size-capped (1 MiB), and unrelated events
  are acknowledged but ignored. The `ping` GitHub sends on creation is answered
  so the delivery shows healthy.

A burst of deliveries (for example the several events a multi-arch push emits)
coalesces into at most one pending check, and a webhook-triggered check realigns
the interval timer so the next scheduled poll is a full interval later.

### Enabling it

Set both variables and publish the port:

```yaml
environment:
  DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR: ":9000"
  DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET: "use-a-long-random-string"
```

With `--network host` the endpoint is reachable on the host's port directly
(e.g. `http://<host>:9000`); otherwise publish it (`-p 9000:9000`). Because the
request carries a secret, terminate TLS in front of it (a reverse proxy such as
nginx, Traefik, or Caddy) so deliveries travel over HTTPS. The webhook accepts
any request path, so route whatever public path you like to it.

### Configuring the webhook on GitHub

In the repository (or organization) **Settings > Webhooks > Add webhook**:

- **Payload URL**: your public URL, e.g. `https://updater.example.com/`
- **Content type**: `application/json`
- **Secret**: the same value as `DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET`
- **Which events**: choose "Let me select individual events" and tick
  **Packages** (only)

GitHub sends a `ping` on save; a `200 pong` confirms the secret and URL are
correct. From then on, each package publish/update triggers an immediate check.

A valid delivery triggers a full check of all monitored containers (the same
work a normal interval tick does), so you don't need to map the package to a
specific container -- whichever containers have a newer image get updated.

### Repository vs organization webhooks

The webhook can be added on a single repository or on the whole organization
(**Org Settings > Webhooks**). The authentication is identical either way -- the
same secret and `X-Hub-Signature-256` signature -- so no extra configuration is
needed for an org-level hook.

The one behavioral difference: an **organization** webhook fires for *every*
package event in the org, not just the image you have in mind. Each delivery
runs a full check, which is harmless (checks are idempotent and bursts coalesce
into at most one pending check) but can be more frequent than necessary if the
org publishes many unrelated packages.

To scope an org-level hook to just the images you care about, set
`DOCKER_UPDATER_GITHUB_WEBHOOK_PACKAGES` to a comma-separated allowlist of
package names. Only matching deliveries trigger a check; the rest are
acknowledged and ignored. Each entry matches either the bare package name or its
`namespace/name` form (case-insensitive):

```yaml
environment:
  # ghcr.io/wow-look-at-my/buildhost and .../docker-updater
  DOCKER_UPDATER_GITHUB_WEBHOOK_PACKAGES: "buildhost,wow-look-at-my/docker-updater"
```

Leave it empty (the default) to react to any package -- the right choice for a
single-repository webhook.

## Container Labels

Add these labels to containers you want to monitor:

```yaml
labels:
  docker-updater.enable: "true"
  # Optional: update mode (default: image)
  docker-updater.mode: "image"  # or "git"
  # Git mode only:
  docker-updater.git-repo: "https://github.com/user/repo"
  docker-updater.git-ref: "refs/heads/main"
  # Post-update health check (HTTP or exec; falls back to Docker HEALTHCHECK):
  docker-updater.health-check.url: ":8080/health"
  docker-updater.health-check.command: "curl -sf http://localhost:8080/health"
  docker-updater.health-check.timeout: "60s"
  # Pre-update readiness check:
  docker-updater.pre-check.url: ":8080/ready-to-update"
  docker-updater.pre-check.command: "/check-ready.sh"
  docker-updater.pre-check.timeout: "30s"
  # Zero-downtime rolling update:
  docker-updater.rolling: "true"
```

### Post-Update Health Checks

After starting the replacement container, docker-updater verifies it is healthy before completing the update. There are three ways to configure this:

1. **HTTP health check** (`docker-updater.health-check.url`): docker-updater polls the URL via HTTP GET from its own process -- no `curl` or other binary inside the container is required. This is the fix for images that ship without a shell or HTTP client (e.g. when `ollama` dropped `curl`, its in-container `HEALTHCHECK` could no longer run).
2. **Exec health check** (`docker-updater.health-check.command`): docker-updater runs a command inside the container via `docker exec` (requires a shell). Exit code 0 means healthy.
3. **Docker HEALTHCHECK fallback**: if neither label is set, docker-updater waits for Docker's built-in `HEALTHCHECK` to report `healthy`. In this mode the image **must** define a `HEALTHCHECK`, and an `unhealthy` result rolls the update back immediately. The wait deadline is derived from the container's own `HEALTHCHECK` config (`start-period + retries × interval + probe timeout`, with a 30s floor and a 5-minute fallback when no config is present), so slow-starting containers only need the right `HEALTHCHECK` parameters.

For the HTTP and exec checks, docker-updater polls every 2 seconds until the check passes or `docker-updater.health-check.timeout` (default 60s) elapses. If the health check fails, docker-updater stops the new container and reports the failure via webhook notifications. This applies to both standard recreate and rolling update modes.

```yaml
labels:
  docker-updater.enable: "true"
  # HTTP check from docker-updater's own process (no curl needed in the image):
  docker-updater.health-check.url: ":8080/health"
  docker-updater.health-check.timeout: "60s"  # optional, default 60s
```

```yaml
labels:
  docker-updater.enable: "true"
  # Exec check inside the container:
  docker-updater.health-check.command: "curl -sf http://localhost:8080/health"
  docker-updater.health-check.timeout: "60s"  # optional, default 60s
```

URLs starting with `:` (port prefix) are resolved using the container's bridge IP, the same way pre-check URLs are (see [Pre-Update Checks](#pre-update-checks)). If both `health-check.url` and `health-check.command` are set, the HTTP check takes precedence.

### Image Mode (default)

Polls the container registry for new image digests. When a newer digest is found, the container is recreated with the updated image.

The registry reference to poll is taken from the reference the container was created with (its `Config.Image`, e.g. `ghcr.io/you/app:latest`), falling back to the running image's `RepoDigests` to recover the repository. This is deliberately independent of whether the running image still carries a repo tag: a long-running image can lose its tags (so `docker ps` shows a bare `sha256:...` image ID), and the updater keeps polling the correct tag regardless. If no registry repository can be resolved for a container (e.g. a locally-built image that was never pulled or pushed), that container is logged and skipped rather than stalling the loop.

### Git Mode

Checks a git remote for new commits on the tracked ref using the smart HTTP protocol (no git binary required). When a new commit is detected, the container is recreated with a fresh image pull.

### Pre-Update Checks

Before applying an update, docker-updater can verify that the container is ready to be updated. This prevents updates during critical operations like database migrations or active request processing.

The check type is inferred from which label is set -- no separate type label needed.

#### HTTP Check

Set `docker-updater.pre-check.url` to send an HTTP GET before updating. The container is only updated if the response status is 2xx.

```yaml
labels:
  docker-updater.enable: "true"
  docker-updater.pre-check.url: ":8080/ready-to-update"
  docker-updater.pre-check.timeout: "10s"  # optional, default 30s
```

URLs starting with `:` (port prefix) are resolved using the container's bridge IP at runtime. docker-updater inspects the container, finds its IP, and constructs the full URL (e.g., `http://172.17.0.5:8080/ready-to-update`). This requires docker-updater to run with `--network host`.

Full URLs (e.g., `http://myapp:8080/ready`) are used as-is and require docker-updater to share a network with the target container.

#### Exec Check

Set `docker-updater.pre-check.command` to run a command inside the container via `docker exec`. The container is only updated if the command exits with code 0. The command is run via `sh -c`, so the container must have a shell.

```yaml
labels:
  docker-updater.enable: "true"
  docker-updater.pre-check.command: "/check-ready.sh"
  docker-updater.pre-check.timeout: "15s"  # optional, default 30s
```

If both `url` and `command` are set, the HTTP check takes precedence.

If a pre-check fails, the update is skipped for that cycle and retried on the next interval. Skipped updates are reported in webhook notifications.

### Rolling Updates (Zero-Downtime)

Set `docker-updater.rolling: "true"` to start the new container before stopping the old one. This eliminates downtime when a reverse proxy (e.g., nginx) routes traffic by DNS or health.

```yaml
labels:
  docker-updater.enable: "true"
  docker-updater.rolling: "true"
```

The rolling update flow:
1. New container is created with a temporary name, same networks and aliases
2. docker-updater waits for the health check to pass (HTTP, exec, or Docker HEALTHCHECK)
3. Old container receives SIGTERM and drains existing connections
4. Old container is removed, new container is renamed to the original name

Requirements:
- A reverse proxy must route traffic via Docker DNS (network alias), not published ports
- The container must not publish host ports (the proxy owns port bindings)
- If using the Docker HEALTHCHECK fallback, the image must define a `HEALTHCHECK`

Pre-checks are skipped for rolling updates -- the old container drains naturally via graceful shutdown.

## Docker Compose Example

```yaml
services:
  docker-updater:
    image: ghcr.io/wow-look-at-my/docker-updater
    network_mode: host  # dashboard reachable at http://<host>:8080
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ~/.docker/config.json:/config.json:ro
    environment:
      DOCKER_UPDATER_INTERVAL: "10m"
      DOCKER_UPDATER_WEBHOOK_URL: "https://discord.com/api/webhooks/..."
      DOCKER_UPDATER_WEBHOOK_TYPE: "discord"
      DOCKER_UPDATER_CONFIG: "/config.json"
      DOCKER_UPDATER_DASHBOARD_ADDR: ":8080"
      # Optional: let GitHub trigger a check the instant a ghcr image is pushed.
      DOCKER_UPDATER_GITHUB_WEBHOOK_ADDR: ":9000"
      DOCKER_UPDATER_GITHUB_WEBHOOK_SECRET: "use-a-long-random-string"

  my-app:
    image: myapp:latest
    labels:
      docker-updater.enable: "true"
      # HTTP health check from docker-updater's process -- no curl needed in the image:
      docker-updater.health-check.url: ":8080/health"
      docker-updater.health-check.timeout: "60s"
      docker-updater.pre-check.url: ":8080/ready-to-update"
```
