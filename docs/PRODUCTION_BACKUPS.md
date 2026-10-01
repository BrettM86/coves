# Production backups

What is backed up on the production host, how it is scheduled, and how to
restore it. Tidepool's own database backup is documented in tidepool's
`DEPLOY.md` ("Backup and restore"); it runs on the same host on the same
pattern.

## What runs

| Unit | Time (UTC) | Script (installed copy) | Output in `/opt/coves/backups` |
|---|---|---|---|
| `tidepool-pg-backup.timer` | 02:17 | `/usr/local/sbin/tidepool-pg-backup` ← `/opt/tidepool/scripts/pg-backup.sh` | (writes to `/opt/tidepool/backups/tidepool-*.dump`) |
| `coves-pg-backup.timer` | 02:47 | `/usr/local/sbin/coves-pg-backup` ← `scripts/pg-backup.sh` | `coves-<stamp>.dump` |
| `coves-pds-backup.timer` | 03:07 | `/usr/local/sbin/coves-pds-backup` ← `scripts/pds-backup.sh` | `pds-<stamp>.tar.gz` |

- **AppView Postgres**: `pg_dump -Fc`, verified with a full read
  (`pg_restore -f /dev/null`) before it gets its final name.
- **PDS data directory**: every SQLite database copied with the online backup
  API and checked with `PRAGMA integrity_check`, plus the per-actor signing
  `key` files. This is the source of truth for every hosted community and
  native account. Blobs are in object storage (`PDS_BLOBSTORE_S3_BUCKET`), not
  in this archive. The run fails if any account in `account.sqlite` lacks its
  store or signing key in the snapshot.
- Retention is 14 days (`RETENTION_DAYS`).
- A run that fails while the dump or archive is being written keeps only the
  most recent failed partial, under a fixed name that each failed retry
  overwrites: `coves-last-failed.dump.partial` or
  `pds-last-failed.tar.gz.partial`. It is never deleted and is warned about
  once it is older than a day. A PDS run that fails earlier leaves nothing
  behind: check `systemctl --failed` and the journal. A `.pds-stage.*` staging
  directory left by a killed PDS run is warned about once it is older than a
  day.
- A failed run is retried every 5 minutes, up to 4 starts in 2 hours, then
  left failed until the next night. A run missed while the host was down
  fires at boot.
- Files are root-only (`umask 077`); the directory is mode 700.

Output goes to the journal: `journalctl -u coves-pg-backup.service`. A failed
run shows in `systemctl --failed`. Nothing alerts on failure yet.

## What is NOT backed up here

- **Secrets in `.env` files**: `/opt/coves/.env` (`ENCRYPTION_KEY`, OAuth keys,
  the PDS PLC rotation key and admin password, S3 keys), `/opt/tidepool/.env`
  (`BRIDGE_KEK`), and the aggregators' `.env` files. A database dump without
  `ENCRYPTION_KEY` restores community and aggregator credentials as unreadable
  ciphertext. Keep these in a password manager, not on this host.
- **Off-host copies**: the dumps sit on the same disk array as the data they
  protect. They cover a dropped database or a bad migration, not loss of the
  host.
- **The S3 blob bucket**: one copy, in object storage only.

## Install or update (on the production host)

The scripts are installed outside `/opt/coves`, because deploys `git pull`
that checkout and a locally changed or untracked file blocks the pull. After a
change to either script, re-run the `install` lines. Tidepool's script is
installed the same way, as `/usr/local/sbin/tidepool-pg-backup`; its steps are
in tidepool's `DEPLOY.md`.

```bash
sudo apt-get install -y sqlite3
sudo chmod 700 /opt/coves/backups
cd /opt/coves
sudo install -m 755 scripts/pg-backup.sh  /usr/local/sbin/coves-pg-backup
sudo install -m 755 scripts/pds-backup.sh /usr/local/sbin/coves-pds-backup
sudo cp scripts/systemd/coves-{pg,pds}-backup.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now coves-pg-backup.timer coves-pds-backup.timer
sudo systemctl start coves-pg-backup.service coves-pds-backup.service   # first backups now
systemctl list-timers 'coves-*' 'tidepool-*'
```

## Restore

### AppView Postgres

Restore into a freshly created database, not over the live one:
`pg_restore --clean` only drops objects that are in the archive, so tables or
columns added by a later migration survive and goose then fails on them.
`--exit-on-error --single-transaction` makes any error abort the whole restore
instead of being skipped.

Set `DUMP` on the second line, then run the block. It stops at the first
failed step: the dump must exist and pass a full read before the AppView is
stopped or the database dropped, and the AppView is started only after the
restore succeeds (on startup it applies migrations, so it must not boot
against an empty database). The script is passed with `-c`, not on stdin,
because `pg_restore` reads the dump from stdin. The block is one
single-quoted string, so it must not contain a single quote.

```bash
sudo bash -euo pipefail -c '
DUMP=/opt/coves/backups/coves-<stamp>.dump
POSTGRES=coves-prod-postgres
cd /opt/coves

DB_USER="$(docker exec "$POSTGRES" printenv POSTGRES_USER)"
DB_NAME="$(docker exec "$POSTGRES" printenv POSTGRES_DB)"

test -s "$DUMP"
docker exec -i "$POSTGRES" pg_restore -f /dev/null < "$DUMP"

docker compose -f docker-compose.prod.yml stop appview
docker exec "$POSTGRES" dropdb -U "$DB_USER" "$DB_NAME"
docker exec "$POSTGRES" createdb -U "$DB_USER" "$DB_NAME"
docker exec -i "$POSTGRES" pg_restore --exit-on-error --single-transaction \
  --no-owner --no-acl -U "$DB_USER" -d "$DB_NAME" < "$DUMP"
docker compose -f docker-compose.prod.yml start appview
'
```

The AppView resumes Jetstream from the cursors in the dump. Everything indexed
after the dump was taken is lost, apart from what Jetstream can still replay.

To check a dump without touching production, restore it into a throwaway
container:

```bash
DUMP=/opt/coves/backups/coves-<stamp>.dump    # root-only: read it with sudo
sudo docker run -d --name coves-restore-drill --network none \
  -e POSTGRES_PASSWORD=drill postgres:15
# TCP, not the socket: during first-boot init the socket answers for a
# temporary server that is about to restart.
until sudo docker exec coves-restore-drill pg_isready -h 127.0.0.1 -U postgres; do sleep 1; done
sudo cat "$DUMP" | sudo docker exec -i coves-restore-drill pg_restore --exit-on-error --no-owner --no-acl -U postgres -d postgres
sudo docker exec coves-restore-drill psql -U postgres -At \
  -c 'SELECT count(*) FROM posts' -c 'SELECT count(*) FROM communities'
# -v: the image's anonymous data volume holds a full copy of production data.
sudo docker rm -f -v coves-restore-drill
```

### PDS

This procedure has not been rehearsed.

First record the live sequencer's high-water mark (skip this if the live
database is the thing that is broken):

```bash
sudo sqlite3 -readonly /var/lib/docker/volumes/coves-prod-pds-data/_data/sequencer.sqlite \
  'SELECT max(seq) FROM repo_seq'
```

Set `ARCHIVE` on the second line, then run the block. It stops at the first
failed step: the PDS must be stopped and the archive must extract with an
`account.sqlite` before the live data is touched, and the live data is moved
aside, not deleted. As above, the script is passed with `-c`, not on stdin,
so no command inside it can swallow the rest of the script.

```bash
sudo bash -euo pipefail -c '
ARCHIVE=/opt/coves/backups/pds-<stamp>.tar.gz
VOLUME=/var/lib/docker/volumes/coves-prod-pds-data
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

cd /opt/coves
docker compose -f docker-compose.prod.yml stop pds
test "$(docker inspect -f "{{.State.Running}}" coves-prod-pds)" = false

NEW="$(mktemp -d "${VOLUME}/_data.restore-${STAMP}.XXXXXX")"
tar -C "$NEW" -xzf "$ARCHIVE"
test -f "${NEW}/account.sqlite"
# mktemp creates the directory mode 700; match the live directory.
chown --reference="${VOLUME}/_data" "$NEW"
chmod --reference="${VOLUME}/_data" "$NEW"

mv "${VOLUME}/_data" "${VOLUME}/_data.before-restore-${STAMP}"
mv "$NEW" "${VOLUME}/_data"
docker compose -f docker-compose.prod.yml start pds
'
```

Anything the PDS accepted after the archive was taken is lost, and the
restored PDS rewinds in two ways that relays and Jetstream notice:

- Its sequencer restarts below the cursor they already hold for this PDS, so
  new events can be skipped until `seq` passes the high-water mark recorded
  above.
- Every repo's `rev` goes back, so relays may treat those repos as out of
  sync.

After the PDS is up, ask each relay that crawls it to recrawl
(`com.atproto.sync.requestCrawl` with this PDS's hostname).
