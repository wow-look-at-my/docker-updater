# Build-mode rebuilds shell out to `docker compose ... build/up`, so the image
# ships the Docker CLI and its compose/buildx plugins. They come from the
# official CLI image (pinned to the 28.x line) and are statically linked Go
# binaries (`file` reports "statically linked"; `ldd`: "not a dynamic
# executable"), so they run in this scratch-based image without a libc.
FROM docker:28-cli AS dockercli

FROM scratch

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/docker-updater"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="Automatic Docker container updater service"

COPY --from=gcr.io/distroless/static-debian12 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
# /usr/local/libexec/docker/cli-plugins is on the CLI's default plugin search
# path, so `docker compose` / `docker buildx` resolve without configuration.
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --chmod=755 build/docker-updater_linux_amd64 /docker-updater

# scratch defines no PATH or HOME: PATH lets the updater exec `docker` for
# build-mode rebuilds; HOME gives the CLI a writable config dir (~/.docker)
# it can create on demand.
ENV PATH=/usr/local/bin
ENV HOME=/root

# Web dashboard / JSON API (DOCKER_UPDATER_DASHBOARD_ADDR, default :8080).
EXPOSE 8080

STOPSIGNAL SIGTERM

ENTRYPOINT ["/docker-updater"]
