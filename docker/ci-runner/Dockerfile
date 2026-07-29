# Test-runner image for the hermetic CI stack.
#
# Baked rather than apk-adding tools at container start, so the tool install is
# paid once into Docker's layer cache instead of on every `make ci`.
#
# The Go module and build caches are NOT baked in — they are mounted as named
# volumes by docker-compose.ci.yml, which is what keeps a warm run fast while
# still letting `make ci-clean` discard them.
FROM golang:1.25-alpine

# git: the go toolchain shells out to it for module operations.
# curl: readiness probing from scripts/ci-runner.sh — notably for Jetstream,
#   whose upstream image ships no HTTP client and therefore cannot healthcheck
#   itself (docker-compose.dev.yml disables its healthcheck outright).
# bash: scripts/ci-runner.sh uses non-POSIX constructs.
# ca-certificates: the allowlisted opt-in tests that reach real hosts.
# grep: GNU grep, for --line-buffered, which the BusyBox build does not support.
#   Only the cosmetic progress display needs it, but silently losing progress
#   output is the kind of papercut that gets rediscovered every few months.
RUN apk add --no-cache git curl bash ca-certificates grep

# GOFLAGS=-mod=readonly makes an out-of-date go.sum a hard failure in CI rather
# than something the toolchain silently repairs in a mounted checkout.
ENV GOFLAGS=-mod=readonly \
    GOCACHE=/go-build-cache \
    GOMODCACHE=/go/pkg/mod \
    CGO_ENABLED=0

WORKDIR /src

ENTRYPOINT ["/bin/bash", "/src/scripts/ci-runner.sh"]
