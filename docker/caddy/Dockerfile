# Production Caddy with the Cloudflare DNS plugin, built from the official
# Docker Official Image rather than pulled from a third-party registry.
#
# Why: this container terminates TLS for every host we serve and holds both
# Cloudflare DNS tokens. A mutable third-party tag (the previous
# ghcr.io/slothcroissant/caddy-cloudflaredns:latest) meant anyone who could
# push that tag could ship a Caddy that intercepts all traffic. Building it
# here narrows trust to the official caddy image (pinned by digest) plus the
# caddy-dns/cloudflare Go module (pinned to a tag; Go's sumdb verifies it).
#
# Upgrading: bump the caddy version + BOTH digests together (resolve with
# `docker buildx imagetools inspect caddy:<ver>` and `caddy:<ver>-builder`),
# bump the plugin tag if a newer one exists and the CADDY_VERSION default in
# docker-compose.prod.yml, then `docker compose -f docker-compose.prod.yml build caddy`
# and `up -d caddy`. Certs/ACME state live in the caddy-data volume and
# survive the recreate.

FROM caddy:2.11.7-builder@sha256:a2535ba3f1878ec781a58cf2d6edfecc47633d22e2a92447fa8ca14b3a6bfaff AS builder

RUN xcaddy build \
    --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11.7@sha256:685ae3d7c34d9b5f8caeddc5d14051db7bbd88ff1f5a2fa0c16bd8ca328fb7af

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
