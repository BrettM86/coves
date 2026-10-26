# PRD: CSAM Removal Response

**Status:** Draft. Nothing in this document is implemented. D1–D8 decided 2026-10-19; D1, D6 and D8 revised and D9–D10 decided 2026-10-25 after review. The preservation period awaits counsel.

**Date:** 2026-10-19, revised 2026-10-25

**Scope:** Coves AppView. When an admin confirms a removal is apparent CSAM, Coves contains the content at the Coves PDS, preserves it outside the database, records a case, and prepares the NCMEC report for an admin to submit.

**Related:** [Moderation](MODERATION.md), [Moderation gaps](PRD_MODERATION_GAPS.md) (G1, G5, D6, D7), [Media](MEDIA.md), [Production backups](PRODUCTION_BACKUPS.md), [Lexicon publishing](LEXICON_PUBLISHING.md), the removed `PRD_CSAM_SCANNING.md` Workstream 4 (`git show 5e419b8^:docs/PRD_CSAM_SCANNING.md`)

## 1. Summary

An `illegal-content` removal today blocks the image CIDs for every owner, purges the image proxy disk cache and the Cloudflare edge, and stores no snapshot. The record and blobs stay on the PDS that holds them: `com.atproto.sync.getBlob` still serves the blobs, and relays still sync the record. Nothing is preserved and nothing prepares a report.

| Step | Today | After this PRD |
|---|---|---|
| Contain | AppView, disk cache and edge only | Plus PDS takedown of the record, its blobs and the author's account, for repos and authors on the Coves PDS |
| Preserve | Nothing | Encrypted bundle in object storage, when the operator configures it |
| Index | Nothing | Case row in Postgres, metadata only |
| Report | Nothing | Report package an admin submits through the NCMEC web form, then records the CyberTipline report ID |
| Delete | Nothing | Unchanged: manual, after preservation is confirmed (§ 5) |

## 2. Decisions

- **D1 (2026-10-19, revised 2026-10-25). A separate CSAM confirmation starts the pipeline.** `illegal-content` covers more than CSAM, and a false NCMEC report or a year of retained user data must not follow from a non-CSAM removal. The admin sets a private flag on the removal; the public reason stays `illegal-content`. The flag can be set on the first removal or later, by calling `removeContent` again on a subject that already has an active `illegal-content` removal (R1). There is no separate confirm endpoint.
- **D2 (2026-10-19). Steps 1–3 are automatic; step 4 is prepared automatically and submitted by an admin.** No NCMEC API integration (§ 5).
- **D3 (2026-10-19). Confirmed CSAM takes the author's account down at the Coves PDS.** Recorded in PRD_MODERATION_GAPS as D7. Instance account bans in the AppView stay G5 (gaps D6).
- **D4 (2026-10-19). No new storage infrastructure.** Preservation writes under a dedicated prefix in an existing S3 bucket, never in Postgres or the PDS.
- **D5 (2026-10-19). Preservation is operator configuration.** Self-hosted Coves servers may have no object storage. Preservation is on when its environment variables are set, like `CLOUDFLARE_CACHE_PURGE_*`, and the other steps run either way.
- **D6 (2026-10-19, revised 2026-10-25). Restore lifts only the takedowns this case applied.** A CSAM-confirmed removal stays restorable, because restore is the only way to undo a mistaken confirmation. Restore marks the case retracted and lifts a record, blob or account takedown only if this case applied it and no other open case references the same subject. A takedown that was already in place when the case started, including one an operator applied by hand, is never lifted. Any filed report stays, since it cannot be undone.
- **D7 (2026-10-19). Bundles are encrypted to an offline public key.** The AppView holds only the public key, so neither it nor anyone holding the bucket credential can read a bundle. The private key stays with the operator, off the server. Scheme in R4.
- **D8 (2026-10-19, revised 2026-10-25). Production preserves under a prefix in the PDS blobstore bucket.** The bucket is the one the production PDS's blobstore variable names; this public document does not name it. Use an access key limited to the prefix if OVH supports it. Remaining risk: the PDS's own key can still reach the prefix. D7 stops it reading a bundle, but not deleting or overwriting one. Coves writes each bundle once under a new key and refuses to overwrite.
- **D9 (2026-10-25). A retracted case's bundle is flagged for manual deletion.** Retraction moves the case out of the open list and marks its bundle `delete-pending`. The operator deletes it by hand and records that in the case. Coves never deletes preserved objects itself (§ 5).
- **D10 (2026-10-25). Report fields are stored on the case except the email.** The case row stores the author's handle at confirmation, each blob's size, and when Coves first learned of the content. The email is fetched live from the Coves PDS when an admin opens the case. That works because the account is taken down, not deleted. An author on another PDS has no email in the package.

## 3. Requirements

### R1. Confirmation

- `removeContent` gains an admin-only boolean, `apparentCsam`. With any reason other than `illegal-content` it returns the existing `UnsupportedReason` error.
- `apparentCsam` is part of the idempotency fingerprint.
- **Upgrade.** When the subject already has an active `illegal-content` removal, a call with `apparentCsam` set records the confirmation on that removal and opens the case. The content is never restored in between. It returns `OutcomeApplied` with the existing action. Without the flag, or with a case already open for that action, it stays `OutcomeUnchanged`.
- The flag and the confirming admin are stored on the action and shown only in `listAdminActions` and the case endpoints. `listActions` is unchanged, because the subject is already withheld for `illegal-content`.
- The case is created in the same transaction as the removal or upgrade, so a committed confirmation always has its case. The steps run after commit.
- `removeContent` is in the pending moderation release (LEXICON_PUBLISHING.md § Published and held back). The new field ships in that release. The three case endpoints (R5) join the moderation release's publish command and that section's list, as `listAdminActions` did.

### R2. Case and durable steps

- `moderation_csam_cases` holds:
  - the case ID and action ID;
  - the subject URI, and the reviewed CID (R4);
  - the repo DID that holds the subject, and the author DID;
  - for each of them, whether it is hosted on the Coves PDS;
  - the author's handle at confirmation;
  - per blob: CID, SHA-256, MD5 and size;
  - when Coves first learned of the content: the earliest of the confirmation time and any `csam` admin report on the subject;
  - per step and per takedown: status, attempt count, last error code, and whether the takedown was already in place before the case (D6);
  - the preservation object key and bundle status;
  - the CyberTipline report ID, the preservation deadline, and the case status (`open`, `reported`, `retracted`).
- It never holds content bytes, record text, or the account's email.
- Each step is idempotent and retried by a sweep, like the CDN purge targets. Before every containment attempt the sweep re-reads the case status, and it never applies a takedown to a retracted case.
- **Alerts.** Coves sends a case-ID-only message through the Telegram `MessageSender` directly, not through the report notifier, which formats a `Report`. It sends one when a case opens, because the reporting clock starts when we learn of the content. It sends another when any step reaches 5 failed attempts. When Telegram is not configured, startup logs a warning, and `listCsamCases` is the only signal. Delivery stays best-effort.
- **Log hygiene.** Logs, alerts and stored error codes for a case contain the case ID and fixed error codes only. They never contain DIDs, handles, emails, URIs or hashes.

### R3. Contain

- "Hosted here" means that resolving the DID with the configured identity resolver gives a PDS endpoint equal to `PDS_URL`. It is checked separately for the repo DID and the author DID. `PDS_ADMIN_PASSWORD` and every admin call go only to `PDS_URL`, never to a resolved endpoint.
- **Record and blobs.** When the repo is hosted here, call `com.atproto.admin.updateSubjectStatus` with `takedown` for the record and for each blob. These target the repo that holds them, which for a legacy post is the community repo.
- **Account.** When the author is hosted here and is not a community account, take the author's account down. A community account is never taken down. The account takedown blocks login, stops the PDS serving the repo and its blobs, and emits a `#account` `takendown` event to the network. Coves itself ignores that event until G5, so the author's other posts stay visible in Coves.
- **Partial containment.** The record, each blob and the account are separate takedowns, each attempted even if another one failed. A case is contained when every takedown that applies has succeeded.
- **Already taken down.** A subject that is already taken down records `already-taken-down`. That counts as contained, and the takedown is kept on restore (D6).
- **Not hosted here.** Content on any other PDS is recorded as not hosted here, and the existing every-owner block and edge purge remain its containment. The report package names the hosting PDS so its operator can be contacted. A Tidepool bridge PDS subject is also recorded as `tidepool-withhold-pending-G1`.

### R4. Preserve

**Read, before containment**

- The record: `com.atproto.repo.getRecord` with the CID the admin reviewed (`reviewedSubject`, otherwise the indexed CID at confirmation). If the PDS returns a different CID, preserve what it returned and record the mismatch.
- The blobs: `getBlob` for every CID the removal blocked.
- The account: `com.atproto.admin.getAccountInfo`, when the author is hosted here. Keep only an explicit list of fields: `did`, `handle`, `email`, `emailConfirmedAt`, `indexedAt`, `deactivatedAt`. Invite codes and `invitedBy` name other accounts and are dropped.
- An author-deleted record records `record-gone`. Its blobs are still read, and containment still runs.

**Limits**

- Each read has a 30-second timeout, and all reads for a case together get 60 seconds.
- A bundle may hold at most 256 MiB. A blob that would exceed this is left out, and the case lists it.
- At most 4 cases read at once.
- A timeout, an oversized blob or a read failure marks preservation `incomplete` and proceeds to containment. A slow PDS never delays the takedown past the read budget.

**Write, before containment**

- When preservation is configured, Coves builds the bundle in memory: a tar of `manifest.json` (case metadata and hashes), `record.json`, `account.json` and `blobs/{cid}`. It encrypts the bundle with age (X25519) to `CSAM_PRESERVATION_AGE_RECIPIENT`. The library is `filippo.io/age`, a new dependency.
- It writes the bundle to `{prefix}/{case ID}.tar.age` with a conditional write that refuses to overwrite. Containment starts only after that write succeeds or fails. The bytes are never held only in memory across a takedown.
- If the S3 write fails, the encrypted bundle goes to `CSAM_PRESERVATION_SPOOL_DIR` on a persistent volume, and the sweep uploads it from there. It is encrypted, so the spool holds nothing readable. If the spool write also fails, the case is marked `manual-recovery`. The build ships a runbook section for copying the blob bytes out of the PDS blobstore after a takedown, verified against the local PDS.
- The operator decrypts offline with `age -d -i <identity file>`.
- When preservation is not configured, the case records `not-configured`. The bytes remain under the PDS takedown, and the case endpoint tells the admin the operator is responsible for preservation.

**Configuration**

- `CSAM_PRESERVATION_S3_BUCKET`, `_PREFIX`, `_ENDPOINT`, `_REGION`, `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY`, `_AGE_RECIPIENT` and `_SPOOL_DIR`. Either all are set or none; a partial set fails startup.
- Each new variable goes in `ClearEnvForTest`.

### R5. Report package

- `getCsamCase` and `listCsamCases` (admin-only) return what the CyberTipline web form asks for:
  - when Coves first learned of the content;
  - the subject URL and the hosting PDS;
  - the author's DID and their handle at confirmation;
  - the email, fetched live (D10);
  - file hashes and sizes;
  - the preservation status and location.
- `recordCsamReport` stores the CyberTipline report ID the admin receives, and records the operator's manual deletion of a `delete-pending` bundle (D9).
- `listCsamCases` lists open cases: confirmed and not yet reported. A reported or retracted case leaves the list. A retracted case with a `delete-pending` bundle stays visible under a separate filter until its deletion is recorded.
- No endpoint returns content bytes.

## 4. Testing

The test author writes these per requirement before production code.

- **R1:** `apparentCsam` with each non-illegal reason returns `UnsupportedReason`. An upgrade of an active illegal-content removal returns `OutcomeApplied` and opens one case with no restore in between. An idempotent replay with a different `apparentCsam` conflicts. The case commits with the action and rolls back with it.
- **R2:**
  - The sweep retries each step and alerts at 5 failures, following the CDN purge tests.
  - The opening alert carries only the case ID.
  - A retracted case is never contained by a later retry.
  - Captured logs and error codes contain no DID, handle, URI or hash.
- **R3 (T2, real local PDS):**
  - The record, blob and account takedowns, after which `getBlob` and login are refused.
  - A legacy post takes down the community repo's record and blobs but never the community account.
  - Partial failure still attempts the account takedown.
  - An account already taken down is recorded and survives restore.
  - A foreign-PDS subject never receives the admin password.
- **R4 (MinIO in `docker-compose.ci.yml`):**
  - The bundle round trip, decrypting with the test identity.
  - The write refuses to overwrite.
  - An S3 failure falls back to the spool, and the sweep uploads from it.
  - A read timeout or oversized blob marks `incomplete` and still contains.
  - The account file holds only the allowlisted fields.
  - A CID mismatch is recorded.
- **R5:** The package fields are present, and the email is live from the PDS. Report ID recording removes the case from the open list. A retraction marks the bundle `delete-pending`.
- **D6:** Two cases sharing an account or image: restoring one keeps the shared takedown. Restore never lifts a pre-existing takedown.

## 5. Out of scope

- A separate preservation bucket, Object Lock retention and a key-management service.
- NCMEC CyberTipline API integration.
- Automatic deletion of PDS bytes or preserved bundles.
- Tidepool bridged media: depends on G1's withhold and delete endpoint.
- Hiding the account's other content in Coves: G5.
- Hash matching at upload (CSAM phase 2).
- IP addresses. The reference PDS does not expose them. Coves logs `client_ip` on XRPC requests (`internal/api/xrpc/decode.go`), but the report package does not collect them.

## 6. Open questions

- **Preservation period.** One year under the REPORT Act's amendment to 18 U.S.C. §2258A(h), to be confirmed by counsel. It sets the case's preservation deadline only; nothing deletes automatically.
- **A subject already removed under another reason.** A removal's reason cannot be corrected (MODERATION.md), so CSAM found on content removed as spam would need a restore and re-removal, which puts the content back up in between. Recommendation: also allow the R1 upgrade on an active removal of any reason. It would record a new `illegal-content` removal that supersedes the old one without restoring it. This needs your decision before `/tdd`.
