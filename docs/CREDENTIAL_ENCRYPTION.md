# Credential encryption at rest

Stored credentials — community PDS passwords and session tokens, aggregator
OAuth access/refresh tokens and DPoP private keys — are sealed by the AppView
process with AES-256-GCM before they reach PostgreSQL. The key comes from the
`ENCRYPTION_KEY` environment variable and is never stored in the database, so
a database read or a `pg_dump` backup yields ciphertext the reader cannot open.

This replaced the scheme from migrations 006, 007 and 025, where pgcrypto's
`pgp_sym_encrypt` used a key stored in the `encryption_keys` table of the same
database. Anyone with a database read or a backup file had the key beside the
ciphertext. Migration 046 dropped that table.

## Mechanism

- Package `internal/crypto/credentialcipher`. Wire format is one version byte
  (`credentialcipher.Version`, `0x01`), a 12-byte random nonce, the
  ciphertext, and the 16-byte GCM tag. pgcrypto output starts with `0xC3`, so
  the two formats never collide on the first byte.
- Every value is bound to an authenticated context string of the form
  `<table>.<column>:<did>` (see `internal/db/postgres/credential_context.go`),
  so a ciphertext copied to another row or column fails to decrypt. The version
  byte is part of the authenticated data as well. The context strings are
  persisted as authenticated data, so renaming a table or column requires a
  re-encryption pass.
- Repositories take the cipher as a constructor argument and encrypt before the
  parameterized write and decrypt after the scan. A value that fails to open
  makes the read return an error wrapping `credentialcipher.ErrInvalidCiphertext`
  and naming the DID; nothing is skipped or returned empty. An unknown version
  byte additionally wraps `ErrUnsupportedVersion`, and a `0xC3` first byte is
  reported as pgcrypto legacy ciphertext so the operator knows the startup
  conversion has not run on that database.
- Empty-credential semantics are unchanged: `Create` and `SetAPIKey` store NULL
  for an empty value; `UpdateCredentials` and `UpdateOAuthTokens` always write
  ciphertext. A zero-length bytea is read as an absent credential, the same as
  NULL.

## Configuration

`ENCRYPTION_KEY` is standard base64 of exactly 32 random bytes:

```bash
openssl rand -base64 32
```

- Production (`IS_DEV_ENV=false`): required. Unset, the documented
  `CHANGE_ME_...` placeholder, non-base64, or a wrong decoded length all fail
  startup with a message naming the variable.
- Dev and CI: `.env.dev`, `.env.dev.example` and `.env.ci` pin a key. If it is
  unset in dev the server generates a random key per boot and logs a warning;
  every credential stored under a generated key is unreadable after the next
  restart, so keep it pinned. `rematerialize-posts` refuses to run under a
  generated key for the same reason.
- A set but malformed key fails in dev as well; it is never silently replaced.
- There is no key rotation. Rotating means decrypting every row under the old
  key and re-sealing under the new one, which nothing implements today.

## Legacy conversion and migration 046

The server converts rows still sealed by pgcrypto at startup, before goose runs:

1. `postgres.ReencryptLegacyCredentials` runs on the migration connection. It
   handles each table only once all three of that table's credential columns
   exist (communities from 007, aggregators from 025), so a database older than
   025 gets a second boot instead of a crash: migration 046 refuses the first
   boot, the operator restarts, and the pass converts what 025 sealed.
2. When the `encryption_keys` table is absent (fresh database, or already
   converted) the pass is a no-op, unless a legacy value still exists in a
   credential column. That value can no longer be decrypted by anyone, so the
   pass fails startup and names the table and count; the recovery is a pre-046
   backup, or NULLing the column and re-provisioning.
3. Otherwise, in one transaction with the affected rows locked, it decrypts
   each legacy value with `pgp_sym_decrypt` (the legacy key is read by the
   database inside the same statement and never enters the Go process), seals
   it with the application cipher, and rewrites the row. Values already in the
   new format and NULLs are left untouched, so the pass is idempotent. A
   zero-length value is rewritten to NULL. Any value that fails to decrypt
   aborts the whole pass with an error naming the table and DID; nothing is
   committed.
4. It refuses to convert under a dev-generated key while legacy rows remain,
   because those rows would be stranded at the next restart.
5. Migration 046 then raises unless every non-NULL credential column starts
   with the version byte, and only then drops `encryption_keys`. The pgcrypto
   extension stays installed because the conversion pass needs it.

The conversion commits before goose runs. If 046 then fails for an unrelated
reason, the rows are already in the new format and a pre-cutover binary cannot
read them; roll forward by fixing the migration and booting again, which is
self-healing because the pass is idempotent.

Migration 046's Down is schema-only. It recreates `encryption_keys` with a
fresh random key while the credential columns keep their AES-256-GCM values,
so a pre-046 binary still cannot read any stored credential. There is no
reverse conversion. To actually roll back, restore a pre-cutover backup or
NULL the credential columns and re-provision the affected communities and
aggregators.

## Threat model and accepted tradeoffs

- Protected: read access to the database, and backup files, no longer yield
  credentials or the key that opens them.
- Not protected: a compromise of the AppView host or its environment, which
  holds the key.
- Accepted: the authenticated context has no freshness component. Someone who
  can write to the database, or who restores a stale backup over live data, can
  put back an older ciphertext for the same row and column and it will open.
  Binding a timestamp into the context would catch this at the cost of a
  re-encryption on every row update; the threat model above does not include a
  database-write attacker, so the simpler scheme was kept.
- Assumed: a single AppView instance replaced on deploy. Migration 046's guard
  and the conversion pass are not safe against an older binary writing pgcrypto
  ciphertext concurrently.

## Production cutover

1. Generate a key and add `ENCRYPTION_KEY=<value>` to the production env file.
   Check the file for an existing pin first; pins override compose defaults.
   `docker-compose.prod.yml` already passes the variable to the AppView.
2. Deploy the new image. On boot the server converts the existing rows, logs
   `legacy database credentials re-encrypted with the application cipher` with
   the counts, and migration 046 drops the key table. Startup fails loudly
   instead of dropping the key if anything is left unconverted.
3. Take a fresh backup and confirm it no longer contains the table. Backups are
   plain SQL, gzipped:

   ```bash
   zcat <dump>.sql.gz | grep -c encryption_keys   # expect 0
   ```

4. Older backups still hold the old key beside the old ciphertext. Treat them
   as containing plaintext credentials: purge them once a post-cutover backup is
   verified (the backup script prunes after 30 days on its own), and rotate the
   community PDS passwords and aggregator OAuth sessions if any old dump may
   have left the host.

Losing `ENCRYPTION_KEY` after the cutover makes every stored credential
unrecoverable. Keep it with the other production secrets.

## Tests

- T0: `internal/crypto/credentialcipher/cipher_test.go` (key validation,
  framing, tamper, wrong-key/context and version rejection),
  `internal/config/encryption_key_config_test.go`.
- T1: `internal/db/postgres/credential_cipher_acceptance_test.go` (the
  end-to-end contract), `credential_cipher_repo_test.go` (per-method
  behavior, tamper handling, legacy-value classification),
  `credential_reencrypt_migration_test.go` (migration 046 guard and rollback,
  the conversion pass including the pre-025 schema, zero-length values and the
  dropped-table case, and the handoff between them), plus the pre-existing
  `community_repo_credentials_test.go`, `aggregator_repo_credentials_test.go`
  and `internal/core/communities/service_credentials_test.go`, which now run
  against the application cipher.
