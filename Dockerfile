# Build-mode rebuilds shell out to `docker compose ... build/up`, so the image
# ships the Docker CLI and its compose/buildx plugins. They come from the
# official CLI image (pinned to the 28.x line) and are statically linked Go
# binaries (`file` reports "statically linked"; `ldd`: "not a dynamic
# executable"), so they run in this scratch-based image without a libc.
FROM docker:28-cli AS dockercli

# Directory skeleton for the scratch stage. scratch ships no filesystem at all
# -- not even /tmp -- and compose/buildx hard-require a writable temp dir:
# buildx materializes a compose `dockerfile_inline` into a temp directory
# (os.MkdirTemp in buildx build/opt.go), and compose places its bake
# metadata file under os.TempDir(). Without /tmp, every rebuild of a
# dockerfile_inline service fails with "mkdir /tmp/dockerfileNNN: no such
# file or directory" (surfaced by the updater only as "exit status 1" before
# rebuild errors carried the compose output tail). /root backs $HOME so the
# CLI has a place for ~/.docker without creating it at container runtime.
RUN mkdir -p /skel/tmp /skel/root && chmod 1777 /skel/tmp && chmod 700 /skel/root

FROM scratch

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/docker-updater"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="Automatic Docker container updater service"

# /tmp (mode 1777) and /root (mode 0700) from the skeleton above.
COPY --from=dockercli /skel/ /
COPY --from=gcr.io/distroless/static-debian12 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
# /usr/local/libexec/docker/cli-plugins is on the CLI's default plugin search
# path, so `docker compose` / `docker buildx` resolve without configuration.
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --chmod=755 build/docker-updater_linux_amd64 /docker-updater

# scratch defines no PATH, HOME, or TMPDIR: PATH lets the updater exec `docker`
# for build-mode rebuilds; HOME points the CLI at the /root shipped above for
# its config dir (~/.docker); TMPDIR pins temp usage to the /tmp shipped above
# (belt and braces -- Go's os.TempDir defaults to /tmp anyway).
ENV PATH=/usr/local/bin
ENV HOME=/root
ENV TMPDIR=/tmp

# Web dashboard / JSON API (DOCKER_UPDATER_DASHBOARD_ADDR, default :8080).
EXPOSE 8080

STOPSIGNAL SIGTERM

ENTRYPOINT ["/docker-updater"]
