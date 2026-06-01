# docker-updater

Automatic Docker container updater service. Monitors running containers and updates them when newer versions are available.

## Features

- **Image-based updates**: Detects new image digests from container registries (watchtower-style)
- **Git-based updates**: Monitors git remote refs via smart HTTP protocol to detect new commits
- **Pre-update checks**: HTTP or exec-based checks to verify containers are ready before updating
- **Web dashboard**: Built-in status page showing every container (monitored or not), uptime, last pull, and whether a newer image is upstream
- **Webhook notifications**: Supports generic, Discord, and Slack webhooks
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
| `DOCKER_UPDATER_WEBHOOK_URL` | | Webhook endpoint URL |
| `DOCKER_UPDATER_WEBHOOK_TYPE` | `generic` | `generic`, `discord`, or `slack` |
| `DOCKER_UPDATER_DRY_RUN` | `false` | Monitor only, don't update |
| `DOCKER_UPDATER_CONFIG` | | Path to Docker `config.json` for private registry auth |
| `DOCKER_UPDATER_DASHBOARD_ADDR` | `:8080` | Listen address for the web dashboard; set empty to disable |

## Dashboard

docker-updater serves a read-only web dashboard (default `:8080`) that lists
**every** container on the host -- both the ones it auto-updates and the ones it
doesn't -- so you can see your fleet at a glance:

![docker-updater dashboard](docs/dashboard.png)

- **Auto-update status**: whether a container is monitored, and in `image` or `git` mode
- **State & uptime**: running/stopped, healthcheck status, and how long it has been up
- **Last checked / last pulled**: when docker-updater last polled the registry and pulled the image
- **Upstream**: whether a newer image digest (or git commit) is available, or the last error

The page auto-refreshes every few seconds. The same data is available as JSON at
`/api/containers`, and `/healthz` returns `200 ok` for external liveness probes.

With `--network host` (recommended), the dashboard is reachable on the host's
port directly, e.g. `http://<host>:8080`. Without host networking, publish the
port (`-p 8080:8080`). Set `DOCKER_UPDATER_DASHBOARD_ADDR` to a different address
(e.g. `:9000`) to move it, or to an empty string to disable it entirely.

Note that "last pulled" and "last checked" only apply to monitored containers;
the tool never polls the registry for containers it isn't watching.

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
```

### Healthcheck Requirement

All containers monitored by docker-updater **must** define a `HEALTHCHECK` in their Dockerfile. After starting the replacement container, docker-updater waits up to 60 seconds for it to report healthy. If the container does not reach a healthy state, docker-updater stops it and reports the failure via webhook notifications.

This applies to both standard recreate and rolling update modes.

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
2. docker-updater waits for Docker's HEALTHCHECK to report healthy (up to 60s)
3. Old container receives SIGTERM and drains existing connections
4. Old container is removed, new container is renamed to the original name

Requirements:
- The container must have a `HEALTHCHECK` in its Dockerfile
- A reverse proxy must route traffic via Docker DNS (network alias), not published ports
- The container must not publish host ports (the proxy owns port bindings)

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

  my-app:
    image: myapp:latest
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
      interval: 10s
      timeout: 5s
      retries: 3
    labels:
      docker-updater.enable: "true"
      docker-updater.pre-check.url: ":8080/ready-to-update"
```
