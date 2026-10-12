# Credential encryption

The AppView seals stored credentials with AES-256-GCM before they reach PostgreSQL: community PDS passwords, access tokens and refresh tokens, and aggregator OAuth access tokens, refresh tokens and DPoP private keys. The key comes from `ENCRYPTION_KEY` and is never stored in the database, so a database read or a `pg_dump` backup yields ciphertext nobody can open without the environment.

This replaced the pgcrypto scheme from migrations 006, 007 and 025, which kept its key in an `encryption_keys` table in the same database. Migration 046 dropped that table.

## Format

The cipher is `internal/crypto/credentialcipher`. A sealed value is one version byte (`credentialcipher.Version`, `0x01`), a 12-byte random nonce, the ciphertext and the 16-byte GCM tag. pgcrypto output starts with `0xC3`, so the two formats never collide on the first byte.

Every value is bound to an authenticated context string `<table>.<column>:<did>` (`internal/db/postgres/credential_context.go`), together with the version byte. A ciphertext copied to another row or column fails to open. Because the context is part of the authenticated data, renaming a credential table or column requires a re-encryption pass.

Repositories take the cipher as a constructor argument, encrypt before the write and decrypt after the scan. A value that fails to open makes the read return an error wrapping `credentialcipher.ErrInvalidCiphertext` and naming the DID; nothing is skipped or returned empty. An unknown version byte also wraps `ErrUnsupportedVersion`. A `0xC3` first byte is reported as pgcrypto legacy ciphertext, meaning the startup conversion has not run on that database.

`Create` and `SetAPIKey` store NULL for an empty credential. `UpdateCredentials` and `UpdateOAuthTokens` always write ciphertext. A zero-length bytea reads as absent, the same as NULL.

The community repository's `GetByDID` decrypts all three community credentials on every read, including reads that need only public fields. A wrong key therefore fails every path that loads a full community row, post admission among them, not only the paths that use the credentials.

## Key rules

`ENCRYPTION_KEY` is standard base64 of exactly 32 random bytes: `openssl rand -base64 32`.

- Production (`IS_DEV_ENV=false`): required. Unset, the documented `CHANGE_ME_` placeholder, non-base64 or a wrong decoded length all stop startup with a message naming the variable.
- Validation refuses only the `CHANGE_ME_` prefix. The keys pinned in `.env.dev`, `.env.dev.example` and `.env.ci` are public and would be accepted, so never copy them to production.
- Dev: if unset, the server generates a random key per boot and logs a warning. Every credential stored under it is unreadable after the next restart, so keep a key pinned. `rematerialize-posts` refuses to run under a generated key.
- A set but malformed key fails in dev as well. It is never silently replaced.
- There is no key rotation. Rotating would mean decrypting every row under the old key and resealing under the new one, and nothing implements that. Losing the key makes every stored credential unrecoverable. Production backups do not include `.env`, so keep the key with the other production secrets ([PRODUCTION_BACKUPS.md](PRODUCTION_BACKUPS.md)).

## Legacy conversion

`postgres.ReencryptLegacyCredentials` runs on every boot, on the migration connection, before goose. It handles a table only once all three of its credential columns exist. When `encryption_keys` is absent the pass is a no-op, unless a pgcrypto value still sits in a credential column: nobody can decrypt that any more, so startup fails naming the table and count. Otherwise it converts every legacy value in one transaction (the legacy key never enters the Go process), leaves new-format values and NULLs alone, and aborts without committing if any value fails to decrypt. It refuses to convert under a dev-generated key. Migration 046 then raises unless every non-NULL credential starts with the version byte, and only then drops `encryption_keys`. The pgcrypto extension stays installed because the pass needs it.

The pass commits before goose runs. If 046 then fails for another reason, the rows are already in the new format and an older binary cannot read them. Roll forward: fix the migration and boot again. The pass is idempotent.

## Rollback

Migration 046's Down is schema-only. It recreates `encryption_keys` with a fresh random key while the credential columns keep their AES-256-GCM values, so a pre-046 binary still cannot read any stored credential. There is no reverse conversion. To roll back, restore a pre-cutover backup, or NULL the credential columns and re-provision the affected communities and aggregators.

Backups taken before the cutover hold the old key beside the old ciphertext. Treat them as plaintext credentials, and rotate the community PDS passwords and aggregator OAuth sessions if one may have left the host.

## Threat model

- Protected: database read access and backup files no longer yield credentials or the key.
- Not protected: compromise of the AppView host or its environment, which holds the key.
- Accepted: the authenticated context has no freshness component. Someone who can write to the database, or who restores a stale backup over live data, can put back an older ciphertext for the same row and column and it will open. Binding a timestamp would catch this at the cost of resealing on every row update, and a database-write attacker is outside the threat model.
- Assumed: a single AppView instance replaced on deploy. Migration 046's guard and the conversion pass are not safe against an older binary writing pgcrypto ciphertext at the same time.
