# PRD: CSAM Removal Response

**Status:** Draft. Nothing in this document is implemented. D1–D8 decided 2026-10-19. The preservation period awaits counsel.

**Date:** 2026-10-19

**Scope:** Coves AppView. When an admin confirms a removal is apparent CSAM, Coves contains the content at the Coves PDS, preserves it outside the database, records a case, and prepares the NCMEC report for an admin to submit.

**Related:** [Moderation](MODERATION.md), [Moderation gaps](PRD_MODERATION_GAPS.md) (G1, G5, Q3), [Media](MEDIA.md), [Production backups](PRODUCTION_BACKUPS.md), the removed `PRD_CSAM_SCANNING.md` Workstream 4 (`git show 5e419b8^:docs/PRD_CSAM_SCANNING.md`)

## 1. Summary

An `illegal-content` removal today blocks the image CIDs for every owner, purges the image proxy disk cache and the Cloudflare edge, and stores no snapshot. The record and blobs stay on the author's PDS, where `com.atproto.sync.getBlob` still serves them and relays still sync them. Nothing is preserved and nothing prepares a report.

| Step | Today | After this PRD |
|---|---|---|
| Contain | AppView, disk cache and edge only | Plus PDS takedown of the record, its blobs and the account, for accounts on the Coves PDS |
| Preserve | Nothing | Encrypted bundle in object storage, when the operator configures it |
| Index | Nothing | Case row in Postgres, metadata only |
| Report | Nothing | Report package an admin submits through the NCMEC web form, then records the CyberTipline report ID |
| Delete | Nothing | Unchanged: manual, after preservation is confirmed (§ 5) |

## 2. Decisions

- **D1 (2026-10-19). A separate CSAM confirmation starts the pipeline.** `illegal-content` covers more than CSAM, and a false NCMEC report or a year of retained user data must not follow from a non-CSAM removal. The admin sets a private flag on the removal; the public reason stays `illegal-content`.
- **D2 (2026-10-19). Steps 1–3 are automatic; step 4 is prepared automatically and submitted by an admin.** No NCMEC API integration (§ 5).
- **D3 (2026-10-19). Confirmed CSAM takes the account down at the Coves PDS.** This answers PRD_MODERATION_GAPS Q3 for this case only. Instance account takedown in the AppView stays G5.
- **D4 (2026-10-19). No new storage infrastructure.** Preservation writes under a dedicated prefix in an existing S3 bucket, never in Postgres or the PDS.
- **D5 (2026-10-19). Preservation is operator configuration.** Self-hosted Coves servers may have no object storage. Preservation is on when its environment variables are set, like `CLOUDFLARE_CACHE_PURGE_*`, and the other steps run either way.
- **D6 (2026-10-19). Restore lifts the PDS takedowns.** Restoring a CSAM-confirmed removal lifts the record, blob and account takedowns and marks the case retracted. The preserved bundle and any filed report stay, since neither can be undone.
- **D7 (2026-10-19). Bundles are encrypted to an offline public key.** The AppView holds only the public key, so neither it nor anyone holding the bucket credential can read a bundle. The private key stays with the operator, off the server.
- **D8 (2026-10-19). Production preserves under a prefix in the PDS blobstore bucket.** The only production bucket is `pumped-wigner`. Use an access key limited to the prefix if OVH supports it; the PDS's own key can still reach the prefix, which D7 makes harmless.

## 3. Requirements

### R1. Confirmation

- `removeContent` gains an admin-only boolean, `apparentCsam`, valid only with reason `illegal-content`; any other reason with it set is InvalidRequest. `moderation.removeContent` is unpublished, so this is a lexicon change without a publish step.
- The flag is stored on the action and shown only in `listAdminActions` and the case endpoints. `listActions` is unchanged: the subject is already withheld for `illegal-content`.
- The case is created in the removal transaction, so a committed removal always has its case. Steps run after commit.

### R2. Case and durable steps

- `moderation_csam_cases` holds the case ID, action ID, subject URI, author DID, hosting PDS, each blob's owner DID, CID, SHA-256 and MD5, a status per step, attempt counts and last error codes, the preservation object key, the CyberTipline report ID and the preservation deadline. It never holds content bytes, record text or the account's email.
- Each step is idempotent and retried by a sweep, like the CDN purge targets. A step that keeps failing raises an admin alert through the existing Telegram notifier, with the case ID only.
- Opening a case sends the same alert, because the reporting clock starts when we learn of the content.

### R3. Contain

- When the author's PDS is the configured Coves PDS (`PDS_URL` with `PDS_ADMIN_PASSWORD`), call `com.atproto.admin.updateSubjectStatus` with `takedown` for the record, each blob and the account.
- The account takedown blocks login, stops the PDS serving the repo and its blobs, and emits a `#account` `takendown` event to the network. Coves itself ignores that event until G5, so the author's other posts stay visible in Coves.
- Content on any other PDS, including the Tidepool bridge PDS, is recorded as not hosted here. The existing every-owner block and edge purge remain its containment. The report package names the hosting PDS so its operator can be contacted.

### R4. Preserve

- Before containment, read the record (`com.atproto.repo.getRecord`), the blobs (`getBlob`) and the account details (`com.atproto.admin.getAccountInfo`) into memory. A taken-down blob is no longer served, so reading first avoids depending on the PDS's internal quarantine layout. A read failure does not delay containment; it marks preservation failed for manual recovery from the PDS.
- When configured, write one bundle per case (record JSON, blobs, account details, case metadata) to `{prefix}/{case ID}`, encrypted to the operator public key (D7). Nothing in Coves deletes preserved objects.
- When not configured, the case records `not-configured`. The bytes remain under the PDS takedown, and the case endpoint tells the admin that the operator is responsible for preservation.
- Configuration: `CSAM_PRESERVATION_S3_BUCKET`, `_PREFIX`, `_ENDPOINT`, `_REGION`, `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY`, plus the public key. All set or none; a partial set fails startup. Each new variable goes in `ClearEnvForTest`.

### R5. Report package

- `getCsamCase` and `listCsamCases` (admin-only) return what the CyberTipline web form asks for: when Coves learned of the content, the subject URL, the account's DID, handle and email, the hosting PDS, file hashes and sizes, and the preservation location.
- `recordCsamReport` stores the CyberTipline report ID the admin receives. A case with no report ID stays in the open list.
- No content bytes are returned by any endpoint.

## 4. Testing

- T2 against the real local PDS: takedown of the record, blob and account; `getBlob` and login refused afterwards; restore lifts every takedown (D6).
- Preservation needs real S3 in CI: add a MinIO service to `docker-compose.ci.yml` and test the bundle round trip, including decryption with the test private key.
- Retry and alert tests on the sweep, following the CDN purge tests.

## 5. Out of scope

- A separate preservation bucket, Object Lock retention and key-management service.
- NCMEC CyberTipline API integration.
- Automatic deletion of PDS bytes or preserved bundles.
- Tidepool bridged media: depends on G1's withhold and delete endpoint.
- Hiding the account's other content in Coves: G5.
- Hash matching at upload (CSAM phase 2).
- IP addresses: the reference PDS does not expose them, and Coves does not log them.

## 6. Open questions

- **Preservation period.** One year under the REPORT Act's amendment to 18 U.S.C. §2258A(h), to be confirmed by counsel. It sets the case's preservation deadline only; nothing deletes automatically.
