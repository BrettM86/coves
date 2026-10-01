#!/usr/bin/env bash
# pg-backup.sh — nightly logical backup of the production AppView Postgres.
#
# Run as root by coves-pg-backup.timer (scripts/systemd/) from the installed
# copy at /usr/local/sbin/coves-pg-backup, never from the /opt/coves checkout:
# deploys `git pull` that checkout, and an untracked or edited script there
# blocks the pull. docs/PRODUCTION_BACKUPS.md has the install steps.
#
# Custom format (-Fc) so a restore can be selective. The dump is streamed out
# of the container and written by the host under umask 077, so it is root-only
# from its first byte: it holds every hosted community's sealed PDS password
# and every aggregator's sealed OAuth session.
#
# NOT covered: ENCRYPTION_KEY and the rest of /opt/coves/.env. Without
# ENCRYPTION_KEY the credential columns in this dump are unreadable ciphertext.

set -euo pipefail
umask 077

BACKUP_DIR="${BACKUP_DIR:-/opt/coves/backups}"
CONTAINER="${CONTAINER:-coves-prod-postgres}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DUMP="${BACKUP_DIR}/coves-${STAMP}.dump"

log() { echo "[$(date -u +%FT%TZ)] $*"; }

# A failed run keeps its partial under one fixed name, so the timer's retries
# overwrite it instead of piling up a full-size partial per attempt. The
# partial only exists between pg_dump starting and the final mv, so its
# presence here means this run failed.
FAILED_PARTIAL="${BACKUP_DIR}/coves-last-failed.dump.partial"
keep_failed_partial() {
    local status=$?
    if [[ -e "${DUMP}.partial" ]]; then
        mv -f "${DUMP}.partial" "${FAILED_PARTIAL}"
        log "ERROR: run failed; its partial is kept as ${FAILED_PARTIAL}" >&2
    fi
    exit "${status}"
}
trap keep_failed_partial EXIT
# bash skips the EXIT trap on a signal it has no trap for; systemd stops and
# timeouts send SIGTERM, so turn it into an exit the EXIT trap sees.
trap 'exit 143' TERM
trap 'exit 130' INT

log "backup starting: ${DUMP}"

# The user and database names are not hardcoded in this script or its unit
# file: they come from the container's environment.
docker exec "${CONTAINER}" sh -c \
    'pg_dump -Fc --no-owner --no-acl -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
    > "${DUMP}.partial"

# Verify the archive BEFORE it gets the real name, with a full read: --list
# only reads the table of contents, so a truncated dump would pass it.
docker exec -i "${CONTAINER}" pg_restore -f /dev/null < "${DUMP}.partial"
mv "${DUMP}.partial" "${DUMP}"
log "backup verified: ${DUMP} ($(du -h "${DUMP}" | cut -f1))"

# Retention ages out completed dumps only. The failed-run partial is never
# deleted, and is warned about once it is older than a day.
find "${BACKUP_DIR}" -maxdepth 1 -name 'coves-*.dump' -mtime "+${RETENTION_DAYS}" -delete
find "${BACKUP_DIR}" -maxdepth 1 -name 'coves-*.dump.partial' -mmin +1440 -print | while read -r stale; do
    log "WARNING: stale partial from a failed run: ${stale}" >&2
done

log "backup done"
