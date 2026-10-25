# docker-updater

docker-updater keeps running containers on their newest build. It watches the containers you mark, and it replaces one when a newer version exists.

## Update modes

- **image** -- the container runs a registry image. The updater pulls the tag and compares digests.
- **git** -- the container tracks a git ref. The updater reads that ref from the remote over smart HTTP.
- **build** -- Docker Compose builds the image on this host. The updater rebuilds when the base image moves.

## What a replacement does

The updater creates the new container from the one's configuration, with the new image. It first clears the values the image supplied, so the new image supplies its own. A rolling update starts the replacement beside the container. The service alias moves only after the replacement reports healthy. A replacement that fails its health check is removed. The image comes back.

A compose service in image mode is recreated through `docker compose up -d --no-deps`. As a result, the compose file applies. Each check also compares every compose container with its file. The dashboard marks a container that drifted from its file as out of sync and lists the differences. Its `recreate` button queues a recreate from the file, which then waits for the same gates as an update. A failed check of the running image does not hold it back.

The recreate can move a container off an image no registry has. Depth: `docs/compose-drift.md`.

An image can ask for `--security-opt` values with the label `docker-updater.security-opt` (space-separated). The replacement gets the missing ones, and they persist. Only a container that binds the Docker socket gets them: it is already root on the host.

## Configuration

Put the label `docker-updater.enable=true` on each container to monitor. The server reads the rest of its configuration from environment variables. Run the binary with no arguments to start the update loop and the dashboard.

The dashboard lists every container on this host, its update state, and any error from the last cycle. A held-back update has a "force update" button: after a confirmation, it applies the update past the pre-check and dry-run mode. Put the dashboard behind a reverse proxy with access control.

A container can carry the label and still be impossible to check. It runs a bare image ID, or image mode finds an image that is in no registry. The dashboard gives the reason and counts the container as an error. Such a container is never up to date, because nothing looked.

## Update-check endpoints

A container can answer `/.well-known/docker-updater/health` and `/.well-known/docker-updater/pre-update`. The updater finds the port from the image's own `EXPOSE`. Depth: `docs/well-known-endpoints.md`.

## Webhooks

An inbound GitHub webhook starts a check as soon as a package publishes, rather than at the next interval. Outbound notifications report each update to a generic endpoint or to Discord.
