# Pre-baked PLC directory image, used by both the hermetic CI stack and the
# dev stack (docker-compose.dev.yml).
#
# Baking the install into an image moves its cost into Docker's layer cache:
# it is paid again only when PLC_REF changes. CI wipes its volumes every run,
# so installing at container start would add minutes to every gate.
FROM node:22.23.4-alpine3.24

# Pinned to a SHA rather than tracking main, for two reasons: a build is
# reproducible, and an upstream PLC commit can never break it on its own.
# Upstream main has since moved from yarn to pnpm; this commit still uses yarn.
#
# 2ed82a5 = "fix: apply backpressure to /export/stream ws producer (#150)",
# 2026-07-02. Bump deliberately; rebuild with:
#   docker compose -f docker-compose.ci.yml build plc-directory
#   docker compose -f docker-compose.dev.yml build plc-directory
ARG PLC_REF=2ed82a5ccf1b424aa5e2f6c5b461dc0ee133278b

# git to fetch, python3/make/g++ to build the native deps behind better-sqlite3
# et al. Kept in the final image rather than split into a builder stage: the
# built node_modules contain compiled artifacts that a slim runtime stage would
# have to copy wholesale anyway, so the split buys ~nothing and adds a way for
# the image to be subtly incomplete.
RUN apk add --no-cache git python3 make g++

WORKDIR /app

# GitHub honours fetching an arbitrary SHA (uploadpack.allowAnySHA1InWant), so
# the happy path downloads exactly one commit. The fallback covers mirrors and
# proxies that refuse SHA-in-want.
RUN git init -q . \
    && git remote add origin https://github.com/did-method-plc/did-method-plc.git \
    && (git fetch -q --depth 1 origin "${PLC_REF}" || git fetch -q origin) \
    && git checkout -q "${PLC_REF}"

RUN yarn install --frozen-lockfile && yarn build

# packages/server is the only thing either stack runs; the rest of the monorepo is built
# because the server imports the sibling packages.
WORKDIR /app/packages/server

# PLC's own default port. docker-compose.ci.yml overrides this to 3002, because
# every CI service shares one network namespace and therefore has to listen on
# the port the rest of the stack already expects to find it on — the same 3002
# the dev stack publishes. See the header of docker-compose.ci.yml.
ENV PORT=3000
EXPOSE 3000

CMD ["yarn", "start"]
