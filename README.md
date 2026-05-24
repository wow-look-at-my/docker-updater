# docker-updater

Automatic Docker container updater service. Monitors running containers and updates them when newer versions are available.

## Features

- **Image-based updates**: Detects new image digests from container registries (watchtower-style)
- **Git-based updates**: Monitors git remote refs via smart HTTP protocol to detect new commits
- **Webhook notifications**: Supports generic, Discord, and Slack webhooks
- **Dry-run mode**: Monitor for updates without applying them
- **Scratch image**: No external dependencies at runtime

## Usage

```bash
docker run -d \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/wow-look-at-my/docker-updater
```

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKER_UPDATER_INTERVAL` | `5m` | Check interval (Go duration) |
| `DOCKER_UPDATER_LABEL` | `docker-updater.enable` | Container opt-in label |
| `DOCKER_UPDATER_WEBHOOK_URL` | | Webhook endpoint URL |
| `DOCKER_UPDATER_WEBHOOK_TYPE` | `generic` | `generic`, `discord`, or `slack` |
| `DOCKER_UPDATER_DRY_RUN` | `false` | Monitor only, don't update |

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
  # Optional: pre-update check (see below)
  docker-updater.pre-check: "http"  # or "exec"
```

### Image Mode (default)

Polls the container registry for new image digests. When a newer digest is found, the container is recreated with the updated image.

### Git Mode

Checks a git remote for new commits on the tracked ref using the smart HTTP protocol (no git binary required). When a new commit is detected, the container is recreated with a fresh image pull.

### Pre-Update Checks

Before applying an update, docker-updater can verify that the container is ready to be updated. This prevents updates during critical operations like database migrations or active request processing.

Two check types are supported:

#### HTTP Check

Sends an HTTP GET request to a URL. The container is only updated if the response status is 2xx. This is the recommended approach -- it works with any container regardless of whether it has a shell, and is especially useful for bare metal containers where `docker exec` is cumbersome.

```yaml
labels:
  docker-updater.enable: "true"
  docker-updater.pre-check: "http"
  docker-updater.pre-check.url: "http://myapp:8080/ready-to-update"
  docker-updater.pre-check.timeout: "10s"  # optional, default 30s
```

#### Exec Check

Runs a command inside the container via `docker exec`. The container is only updated if the command exits with code 0.

```yaml
labels:
  docker-updater.enable: "true"
  docker-updater.pre-check: "exec"
  docker-updater.pre-check.command: "/check-ready.sh"
  docker-updater.pre-check.timeout: "15s"  # optional, default 30s
```

If a pre-check fails, the update is skipped for that cycle and retried on the next interval. Skipped updates are reported in webhook notifications.

## Docker Compose Example

```yaml
services:
  docker-updater:
    image: ghcr.io/wow-look-at-my/docker-updater
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      DOCKER_UPDATER_INTERVAL: "10m"
      DOCKER_UPDATER_WEBHOOK_URL: "https://discord.com/api/webhooks/..."
      DOCKER_UPDATER_WEBHOOK_TYPE: "discord"

  my-app:
    image: nginx:latest
    labels:
      docker-updater.enable: "true"
      docker-updater.pre-check: "http"
      docker-updater.pre-check.url: "http://my-app:80/health"
```
