#!/usr/bin/env bash
# Provisions the template database that tests/testkit clones per test, and
# sweeps clones orphaned by processes that died before their cleanup ran.
#
# WHY THIS RUNS BEFORE `go test` RATHER THAN INSIDE IT
#
# `go test ./...` builds one binary per package and runs several concurrently.
# If each of those processes provisioned the template itself, N processes would
# race to drop-and-recreate the same database — a built-in flake, and the
# reason this is a harness responsibility rather than a TestMain one. Doing it
# here means the template is already correct by the time any test binary
# starts.
#
# testkit still has a fallback (an advisory-lock-guarded verify-or-provision) so
# that an ad-hoc `go test ./internal/foo` with no Makefile in the loop works. It
# is a fallback, not the plan.
#
# IDEMPOTENT. Creates the template if absent, rebuilds it if the migration set
# changed since it was stamped, and does nothing otherwise. Safe to call before
# every test run, which is what `make test` and scripts/ci-runner.sh do.
#
# The work itself is Go (tests/testkit/cmd/testdbprepare) because the CI runner
# image ships neither psql nor the goose CLI, and because sharing testkit's own
# provisioning code is what keeps the script and the fallback from drifting
# apart.
#
# Coordinates come from the environment — POSTGRES_TEST_HOST/PORT/USER/
# PASSWORD/DB, POSTGRES_TEST_TEMPLATE — with defaults matching
# docker-compose.dev.yml's postgres-test as published on the host (localhost:5434)
# and .env.ci inside the hermetic stack, which share a network namespace. So the
# same invocation works from a developer's shell and from the CI runner.
#
# Flags are passed straight through:
#   --force            rebuild even if the stamp matches
#   --sweep-age 30m    change the orphan age cutoff (0 disables sweeping)
#   --wait 90s         how long to wait for Postgres to accept connections
#   --print-flags      print ONLY the safe `go test` concurrency flags and exit
#                      ("-p N -parallel M"), for $(...) splicing by the Makefile
#                      and the CI runner
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO_ROOT"

# --print-flags is consumed by a shell substitution, so it must emit the flags
# and nothing else.
if [[ ${1:-} != "--print-flags" ]]; then
    echo "▶ Preparing the test template database"
fi

exec go run ./tests/testkit/cmd/testdbprepare "$@"
