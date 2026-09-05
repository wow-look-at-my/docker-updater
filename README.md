# docker-updater

docker-updater keeps running containers on their newest build. It watches the containers you mark, and it replaces one when a newer version exists.

## Update modes

- **image** -- the container runs a registry image. The updater pulls the tag and compares digests.
- **git** -- the container tracks a git ref. The updater reads that ref from the remote over smart HTTP.
- **build** -- Docker Compose builds the image on this host. The updater rebuilds when the base image moves.

## What a replacement does

The updater creates the new container from the old one's configuration, with the new image. It first clears the values the old image supplied, so the new image supplies its own. A rolling update starts the replacement beside the old container. The service alias moves only after the replacement reports healthy. A replacement that fails its health check is removed. The previous image comes back.

## Configuration

Put the label `docker-updater.enable=true` on each container to monitor. The server reads the rest of its configuration from environment variables. Run the binary with no arguments to start the update loop and the dashboard.

The dashboard is read only. It lists every container on this host, its update state, and any error from the last cycle. Put it behind a reverse proxy with access control.

## Update-check endpoints

A container can answer `/.well-known/docker-updater/health` and `/.well-known/docker-updater/pre-update`. The updater finds the port from the image's own `EXPOSE`. Depth: `docs/well-known-endpoints.md`.

## Webhooks

An inbound GitHub webhook starts a check as soon as a package publishes, rather than at the next interval. Outbound notifications report each update to a generic endpoint or to Discord.
