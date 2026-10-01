#!/usr/bin/env bash
# pds-backup.sh — nightly consistent copy of the production PDS data directory.
#
# The PDS is the source of truth for every account it hosts (communities,
# native users, aggregators): their repos live in the per-actor store.sqlite
# files and their signing keys in the per-actor `key` files. The AppView can be
# rebuilt from repos; these cannot be rebuilt from anything. Blobs are NOT in
# this directory — PDS_BLOBSTORE_S3_BUCKET puts them in object storage — so
# the archive stays small.
#
# Copying the live directory with cp or tar is not a backup: the databases run
# in WAL mode while the PDS writes to them. Each database is copied through
# SQLite's online backup API (sqlite3 .backup), which yields a consistent
# snapshot without stopping the PDS, and every copy must pass
# `PRAGMA integrity_check` before it is archived. Everything else (actor keys,
# the blocks directory) is copied as-is.
#
# Run as root (the volume is root-owned) by coves-pds-backup.timer from the
# installed copy at /usr/local/sbin/coves-pds-backup. Needs the host sqlite3
# package. docs/PRODUCTION_BACKUPS.md has the install and restore steps.

set -euo pipefail
umask 077

PDS_DATA="${PDS_DATA:-/var/lib/docker/volumes/coves-prod-pds-data/_data}"
BACKUP_DIR="${BACKUP_DIR:-/opt/coves/backups}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
ARCHIVE="${BACKUP_DIR}/pds-${STAMP}.tar.gz"

log() { echo "[$(date -u +%FT%TZ)] $*"; }

# An empty or wrong PDS_DATA would otherwise archive nothing and report success.
if [[ ! -f "${PDS_DATA}/account.sqlite" ]]; then
    log "ERROR: ${PDS_DATA}/account.sqlite not found; refusing to write an empty backup" >&2
    exit 1
fi

# WORK holds the NUL-delimited manifests next to STAGE so they stay out of the
# archive, which is built from STAGE alone.
#
# A failed run keeps its archive partial under one fixed name, so the timer's
# retries overwrite it instead of piling up a partial per attempt. The partial
# only exists between tar starting and the final mv, so its presence here
# means this run failed.
WORK="$(mktemp -d "${BACKUP_DIR}/.pds-stage.XXXXXX")"
FAILED_PARTIAL="${BACKUP_DIR}/pds-last-failed.tar.gz.partial"
on_exit() {
    local status=$?
    rm -rf "${WORK}"
    if [[ -e "${ARCHIVE}.partial" ]]; then
        mv -f "${ARCHIVE}.partial" "${FAILED_PARTIAL}"
        log "ERROR: run failed; its partial is kept as ${FAILED_PARTIAL}" >&2
    fi
    exit "${status}"
}
trap on_exit EXIT
# bash skips the EXIT trap on a signal it has no trap for; systemd stops and
# timeouts send SIGTERM, so turn it into an exit the EXIT trap sees.
trap 'exit 143' TERM
trap 'exit 130' INT
STAGE="${WORK}/data"
mkdir "${STAGE}"

log "backup starting: ${ARCHIVE}"
cd "${PDS_DATA}"

databases=0
backup_db() {
    local db="$1" tables check
    mkdir -p "${STAGE}/$(dirname "${db}")"
    # The PDS holds write locks briefly; wait for them instead of failing on SQLITE_BUSY.
    # The CLI's .backup restarts the copy whenever the source is written
    # mid-copy, so on a large, busy database (sequencer.sqlite) a slow or
    # never-finishing run points here; `VACUUM INTO` is the alternative.
    sqlite3 -readonly -cmd '.timeout 30000' "${db}" ".backup '${STAGE}/${db}'"
    # integrity_check reports "ok" for an empty file, so a copy with no schema
    # would otherwise pass as a valid backup.
    if [[ ! -s "${STAGE}/${db}" ]]; then
        log "ERROR: the copy of ${db} is empty" >&2
        exit 1
    fi
    tables="$(sqlite3 "${STAGE}/${db}" 'SELECT count(*) FROM sqlite_master;')"
    if (( tables == 0 )); then
        log "ERROR: the copy of ${db} has no schema" >&2
        exit 1
    fi
    check="$(sqlite3 "${STAGE}/${db}" 'PRAGMA integrity_check;')"
    if [[ "${check}" != "ok" ]]; then
        log "ERROR: integrity_check failed for the copy of ${db}: ${check}" >&2
        exit 1
    fi
    databases=$((databases + 1))
}

# Each database is a separate snapshot, so the set is only consistent if the
# order is right. Stopping the PDS would make it exact but costs nightly
# downtime; instead account.sqlite is copied first, then the sequencer, then
# did_cache and the actor stores, then the key files, and the account list is
# checked against the staged stores and keys below. An account created mid-run
# only adds a store with no account row (harmless on restore); an account
# deleted mid-run leaves a row with no store or key, which fails the run and
# the next night retries.
backup_db ./account.sqlite
backup_db ./sequencer.sqlite

find . -type f -name '*.sqlite' ! -path ./account.sqlite ! -path ./sequencer.sqlite -print0 > "${WORK}/databases"
while IFS= read -r -d '' db; do
    backup_db "${db}"
done < "${WORK}/databases"

# The -wal and -shm files belong to the live databases; each .backup copy
# above is already self-contained.
find . -type f ! -name '*.sqlite' ! -name '*.sqlite-wal' ! -name '*.sqlite-shm' -print0 > "${WORK}/files"
files=0
while IFS= read -r -d '' file; do
    mkdir -p "${STAGE}/$(dirname "${file}")"
    cp -p "${file}" "${STAGE}/${file}"
    files=$((files + 1))
done < "${WORK}/files"

# An account without its repo or signing key cannot be restored.
sqlite3 "${STAGE}/account.sqlite" 'SELECT did FROM account;' > "${WORK}/dids"
missing=0
shopt -s nullglob
while IFS= read -r did; do
    stores=("${STAGE}"/actors/*/"${did}"/store.sqlite)
    keys=("${STAGE}"/actors/*/"${did}"/key)
    if (( ${#stores[@]} == 0 || ${#keys[@]} == 0 )); then
        log "ERROR: account ${did} has no staged store.sqlite or key" >&2
        missing=$((missing + 1))
    fi
done < "${WORK}/dids"
shopt -u nullglob
if (( missing > 0 )); then
    log "ERROR: ${missing} account(s) incomplete; not writing an archive" >&2
    exit 1
fi

tar -C "${STAGE}" -czf "${ARCHIVE}.partial" .
tar -tzf "${ARCHIVE}.partial" > /dev/null
mv "${ARCHIVE}.partial" "${ARCHIVE}"
log "backup verified: ${ARCHIVE} (${databases} databases, ${files} other files, $(du -h "${ARCHIVE}" | cut -f1))"

find "${BACKUP_DIR}" -maxdepth 1 -name 'pds-*.tar.gz' -mtime "+${RETENTION_DAYS}" -delete
# Matches the failed-run partial, which is never deleted.
find "${BACKUP_DIR}" -maxdepth 1 -name 'pds-*.tar.gz.partial' -mmin +1440 -print | while read -r stale; do
    log "WARNING: stale partial from a failed run: ${stale}" >&2
done
# A SIGKILL or power loss skips the EXIT trap (SIGTERM and SIGINT are
# trapped) and leaves plaintext database copies and signing keys behind.
find "${BACKUP_DIR}" -maxdepth 1 -type d -name '.pds-stage.*' -mmin +1440 -print | while read -r stale; do
    log "WARNING: stale staging directory from a killed run (contains signing keys): ${stale}" >&2
done

log "backup done"
