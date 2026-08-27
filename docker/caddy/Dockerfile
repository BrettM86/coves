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
# bump the plugin tag, then `docker compose -f docker-compose.prod.yml build caddy`
# and `up -d caddy`. Certs/ACME state live in the caddy-data volume and
# survive the recreate.

FROM caddy:2.11.4-builder@sha256:4bdeabce8e79d36b23d1cba7d20598cec2c1117ace960d8ca06071f945e8fc9b AS builder

RUN xcaddy build \
    --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11.4@sha256:df7f1c2fb114453b951de51a98efc010db1655a92c2e86be6706714e2417a78d

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
