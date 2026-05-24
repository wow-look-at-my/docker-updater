FROM scratch

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/docker-updater"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="Automatic Docker container updater service"

COPY --from=gcr.io/distroless/static-debian12 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --chmod=755 build/docker-updater_linux_amd64 /docker-updater

STOPSIGNAL SIGTERM

ENTRYPOINT ["/docker-updater"]
