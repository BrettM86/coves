# PRD: Content classification and community drift signals

**Status:** Draft, 2026-11-10, revised 2026-11-11 after review. Nothing here is built. D1–D7 were decided by Bretton on 2026-11-10 and D8–D19 on 2026-11-12; nothing is open.

**Scope:** Coves AppView. Phase 1 alerts on NSFW flag changes without a model. Phase 2 scores every post, comment and community with a hosted classifier and stores the result, with no moderation action; its only alerts are configured score alerts (D17). Phase 3 (acting on scores) is described only far enough to keep phases 1 and 2 compatible with it; it gets its own approval.

**Related:** [Moderation](MODERATION.md), [CSAM response](PRD_CSAM_RESPONSE.md), [Media](MEDIA.md), Tidepool `docs/PRD_MODERATION_GAPS.md` (T4, T5).

## 1. Summary

As Tidepool follows whole Lemmy instances, communities arrive that nobody reviewed, and a reviewed community can change its rules later. Coves has no signal for either today. The policy this PRD serves (D1): explicit sexual imagery is allowed only when labelled NSFW.

| Phase | What it adds | Acts on content |
|---|---|---|
| 1 | An alert when a community is indexed as NSFW or its NSFW flag changes, and a weekly per-community digest of NSFW-flagged post share | No |
| 2 | A classification queue, a worker that calls Cloudflare's Clef models, stored scores for posts, comments and community text, admin read and verdict endpoints | No |
| 3 (not in this build) | Automatic `nsfw` label on unlabelled explicit content, admin reports for the other categories, classifier-based community alerts | Yes |

## 2. What exists today

- **Community flag.** Tidepool writes `contentWarnings: ["nsfw"]` on a bridged community's profile record when the Lemmy actor is `sensitive` (`internal/materialize/actors.go` in Tidepool). The community consumer stores it (`communities.content_warnings`) and overwrites it on update with no history. Native communities can set it too.
- **Post and comment flags.** Tidepool writes an `nsfw` self-label on a post or comment whose Lemmy object is `sensitive`. Both are stored in `content_labels`.
- **Instance NSFW label.** `labelContent` accepts post URIs only (`validateLabelRequest`). Comments carry self-labels and image embeds, but an admin cannot label a comment NSFW, and the web client does not blur comment images (`Comment.svelte`: "Comment images get no NSFW blur").
- **Alerts.** Admin reports reach Telegram through `adminreports.Notifier` and its `MessageSender` (`internal/notify/telegram`).
- **Images.** `ImageProxyService.GetImageResolvingPDS` resolves the owner DID, checks the blob is referenced and not blocked, verifies the CID, and renders a preset.
- **CSAM hashing.** The Cloudflare CSAM Scanning Tool is enabled on the `coves.social` zone (confirmed by Bretton 2026-11-10). It scans images served through `img.coves.social`. Nothing hashes at upload or ingest.

## 3. Decisions

- **D1. Policy.** Explicit sexual imagery is allowed only when labelled NSFW. This is the end state phase 3 enforces.
- **D2. Automatic actions.** Only the `nsfw` label and containment on a known-CSAM hash match may run without a person. Everything else is a report. Phases 1 and 2 take no action at all.
- **D3. Third-party classifier.** Public content may be sent to Cloudflare Workers AI.
- **D4. Attribution.** Automatic decisions are attributed to a new system actor, the coves.social labeler, distinct from any admin DID in the modlog. Phase 3 defines it; phases 1 and 2 store only which model and policy version produced a score.
- **D5. NCMEC ESP registration waits.** Upload and ingest hashing (PhotoDNA or HMA) are out of this PRD.
- **D6. Community text is classified.** Name, display name and description are classified on create and update, and so are avatars and banners (D16).
- **D7. Order.** Build phases 1 and 2, run them, tune policy and thresholds on the stored results, and only then approve phase 3.
- **D8. In the AppView, toggleable (was Q1).** The worker runs in the AppView process and is switched on and off with `CLASSIFIER_ENABLED`, with no other change needed. The worker touches only the queue tables, the image service and the classifier client, so moving it to its own binary later stays a wiring change. A separate service that consumes Jetstream itself would repeat identity resolution, bridge trust and removal state.
- **D9. Cloudflare first, provider kept behind an interface (was Q2).** The build uses Cloudflare Workers AI (R3). The classifier client is a Go interface that takes the content and the policy's questions and returns per-question probabilities, so a self-hosted model (open-weights Clef-flash, a ROOST Model Community model) can be added later as a second implementation without touching the queue, results or endpoints. E2E tests call the real Cloudflare API (§ 6).
- **D10. Policy (was Q3).** The R5 question list stands. Explicit and suggestive-but-not-explicit are asked as two separate questions in the first policy version, and tuning compares thresholds on explicit alone against the two combined.
- **D11. Comment NSFW labels come with phase 3 (was Q4).** P1 is approved, and is built when phase 3 is approved, because phases 1 and 2 take no action and P1 spans the web and mobile repos.
- **D12. Cloudflare account (was Q5).** The token belongs to the account that holds the `coves.social` zone. Development uses a one-week token Bretton creates for the build; production gets a long-lived token at deploy.
- **D13. No back-fill (was Q6).** Existing content is not back-filled; Coves has no known NSFW content today. Phase 2 runs on new and updated content as it is indexed, and that live traffic is the test set for tuning. Re-enqueue (R6) stays for catch-up after the classifier was off and for re-scoring after a policy edit.
- **D14. Instance-level drift is caught by the classifier only (P2 declined).** Tidepool does not poll Lemmy site settings: an instance can keep its stated rules while its communities break them, so the classifier's per-community rollups are the signal that matters.
- **D15. Offline model comparison is the final chunk (P3 approved).** The D9 interface keeps the provider swappable, and chunk 6 runs the verdict set through ROOST Model Community models on home-pc.
- **D16. Community avatars and banners are classified (P5 approved).**
- **D17. Configurable score alerts in phase 2 (P6 approved, widened).** Alerts are a configured list of question and threshold pairs, defaulting to the minors question only. Bretton will tune which questions alert from the phase 2 results, separately from what phase 3 acts on automatically.
- **D18. No admin review screen for now (P4 declined).** Tuning uses the R6 endpoints directly.
- **D19. Comment NSFW labels approved (P1).** Built with phase 3 (D11): `labelContent` and `retractContentLabel` accept comment URIs, `commentView.moderation.contentLabels` carries the instance label, and web and mobile blur comment images on a self-label or instance label. It closes the gap where an admin's only tool for an explicit comment image is removal.

## 4. Requirements

### R1. Community NSFW signals (phase 1)

- The community consumer records an event when a community is first indexed with `nsfw` in `contentWarnings`, and when an update adds or removes it. The event holds the community DID, the origin host (null for a native community), the old and new values, the time, and its delivery state: `delivered_at`, attempt count and last error code. It is written in the indexing transaction.
- After commit, each event sends one Telegram message through `MessageSender` naming the community handle, the origin host or "native", and the change. A failed send is retried by a sweep with backoff up to a configured attempt limit; an event past the limit stays undelivered and is counted in a metric.
- A weekly job computes, per community with at least a configured minimum of posts in the last 7 days, the share carrying an `nsfw` self-label, against the same share over the prior 28 days. Communities whose share rose by more than a configured amount are sent in one digest, ordered by the size of the rise and cut to fit Telegram's 4,096-character limit, ending with a count of the communities left out. The numbers are computed from `posts`, with no new per-post state.
- Messages are sent when Telegram is configured and `COMMUNITY_SIGNAL_ALERTS_ENABLED=true`. Events are recorded either way.

### R2. Classification queue

- A job is identified by subject URI (a community: its DID), record CID and evaluation, where the evaluation is the policy version plus the model IDs. A redelivered event with the same identity adds nothing. A new evaluation of the same CID, requested through R6, is a new job, and earlier results are kept.
- Jobs are inserted in the indexing transaction when a post, comment or community is created, and when an update changes its text or embed (a community: name, display name, description, avatar or banner).
- Before every outbound request, including retries and each image batch, the worker checks two things:
  - **Current version.** The subject's indexed CID still equals the job's CID. A superseded job is closed without a request; the newer version has its own job.
  - **Effective visibility.** The subject is not deleted or instance-removed, has no `illegal-content` block, is not hidden by a community removal, an account ban or a PDS account-status restriction on its author, and is not beneath a removed post or comment. Its community is not removed. A subject failing any of these is closed without a request.
- Each job has a status, attempt count, last error code and next-attempt time. Failures retry with backoff up to a configured attempt limit, then stop and are counted.
- The worker runs in the AppView process (D8), started only when the classifier is configured. With no configuration no jobs are inserted, so a self-hosted server pays nothing. Content indexed while the classifier is off is never queued: enabling it, or re-enabling it after a period off, needs an R6 re-enqueue over that period, and `PRODUCTION_DEPLOY.md` says so.
- A configured daily request cap stops the worker for the day and logs once; jobs wait. The cap counts requests, not subjects.

### R3. Classifier client

- One provider in this build, behind the D9 interface: Cloudflare Workers AI, the System One request (`state`, `questions`, `images`), `clef-flash` for text-only requests and `clef` for requests with images. Both model IDs are configuration.
- Configuration: `CLASSIFIER_ENABLED`, the Cloudflare account ID, an API token limited to Workers AI, model IDs, the daily cap, the worker interval and concurrency. The token is never logged.
- Text is truncated to a configured character limit before sending, and the result records that it was truncated.
- A request carries the subject's public content and nothing else: no DID, handle, URI or IP address is added as metadata. Identifiers that appear inside the content itself, such as links and mentions in a body or the provenance line on a bridged community's description, are sent as they appear, under D3.

### R4. What is sent

| Subject | Text | Images |
|---|---|---|
| Post | Title, body, external link URL and title | Gallery images and the link thumbnail |
| Comment | Body | Image embeds |
| Community | Name, display name, description | Avatar and banner |

- Images are read through `ImageProxyService.GetImageResolvingPDS` at the `content_preview` preset with the configured identity resolver, so DID resolution, reference checks, media blocks and CID verification apply as they do for a viewer. A blocked or unfetchable image is recorded as skipped, and the text is still classified. A link thumbnail that is not a blob is skipped.
- Animated GIFs send the first frame. Video is not sent.
- A request carries at most 4 images. A subject with more is sent as several requests, each with the text, and the subject's score per question is the maximum across them.

### R5. Policy and results

- The policy is a versioned file in the repo: a list of questions, each with a key, a type, instructions and, optionally, whether it is asked only in requests that carry an image. The first version (D10) asks: explicit sexual content; suggestive but not explicit sexual content; sexual content involving or sexualizing minors; graphic violence; hate or harassment; spam or scam.
- A result row belongs to one job and stores the subject URI and CID, subject kind, community DID, the author DID (the account that wrote it, which for a legacy post is not the repo that holds it), model ID, policy version, each question's probabilities, the number of images sent and skipped, and whether text was truncated. It stores no content. Phase 2 sends no alert on any result.
- **Score alerts (D17).** After a result is stored, each configured (question, threshold) pair it meets sends one Telegram message through `MessageSender` carrying the result ID, the question key and the subject kind, and nothing else. The default configuration alerts on the minors question only. On a minors alert the admin follows the existing `illegal-content` removal and the [CSAM response](PRD_CSAM_RESPONSE.md) steps; NCMEC reporting still waits on D5. Alert delivery is retried and recorded like R1 events.
- Results are treated like content snapshots under account erasure: erasing an account deletes the results and queued jobs whose author DID is that account, under the same erasure coordination the snapshots use, and the worker writes no result for an erased author. Deletes and removals of the subject keep its results.

### R6. Admin endpoints

Admin-only, under `social.coves.moderation`, held back from lexicon publishing with the rest of the unreleased moderation set:

- List results, filtered by question, minimum probability, subject kind, community, model, policy version and time, newest first, with the subject's current view and whether its current CID is the scored CID.
- Record a verdict on a result: for each question, whether the category is present in the content, judged by the admin independently of the score. A verdict is accepted only while the subject's current CID equals the scored CID; otherwise the result is unreviewable.
- Precision and recall per question at a given threshold, for one model and policy version: precision is the share of verdicted results at or above the threshold where the category is present; recall is the share of verdicted results with the category present that score at or above it. Unreviewable results are counted separately and excluded.
- Per-community summary: for the last 7 days, the count of scored posts and comments and the share above a given probability on a given question, alongside the R1 flag events.
- Re-enqueue: all communities, or posts and comments in a time range, under the current evaluation. This is the catch-up after the classifier was off (R2) and the way to re-score after a policy edit; there is no initial back-fill (D13). It respects the daily cap.

### R7. Operations

- Metrics: jobs queued, sent, failed, closed as superseded and closed as not visible; requests and input tokens per model per day; worker lag; R1 events undelivered.
- Logs carry job IDs and fixed error codes, never content, DIDs, handles or probabilities tied to a subject.
- `PRODUCTION_DEPLOY.md` gains the configuration, the Cloudflare token scope, and the re-enqueue step for enabling the classifier.

## 5. Chunks

| # | Scope | Needs |
|---|---|---|
| 1 | R1 community flag events, delivery sweep and weekly digest | none |
| 2 | R2 queue and worker, R3 client, R5 results, text only, for posts, comments and communities | none |
| 3 | R4 images | 2 |
| 4 | R6 endpoints, verdicts and re-enqueue | 2 |
| 5 | R7 metrics, D17 score alerts and deploy documentation | 2 |
| 6 | Offline comparison (D15): a script outside the AppView, run on home-pc, that reads verdicted results through the R6 endpoints, runs the same content through Shieldstral-1.0-3B, PolicyLM-1.7B and CoPE-B, and reports precision and recall per question beside Clef's | 4, and enough verdicts from live traffic |

## 6. Testing

- **Chunk 1.** E2E against the local stack: a community record created with `contentWarnings`, updated to add it and to remove it, through the real PDS and Jetstream, asserting the event rows, and one real Telegram delivery per change to a test chat through the real `internal/notify/telegram` adapter. The test bot token and chat ID come from `.env.dev`; when they are absent the test fails, it does not skip. Retry and digest-size behaviour are integration tests against the real Postgres.
- **Chunks 2–4.** E2E calls the real Cloudflare API with the development token from `.env.dev`, and fails when the token is absent (D9). Every request goes through a recording `http.RoundTripper` wrapped around the real client, which forwards to the provider unchanged and records the subject of each request in the test. The tests assert from that record that a removed, superseded or otherwise hidden subject is never sent, and that no request carries identity metadata. Worker behaviour that does not depend on the model (identity, retries, the cap, truncation, verdict acceptance) is tested against the real Postgres queue.

## 7. Proposals outside the approved scope

None open. P1–P6 were decided on 2026-11-12 and are recorded as D14–D19. P7 (pushing decisions to other operators) was dropped the same day: phase 3's labels are published on the instance labeler's stream once `LABEL_SIGNING_KEY` is set, and consuming them is up to other operators.

## 8. Open questions

None. Q1–Q6 were answered on 2026-11-12 and are recorded as D8–D13.


## Implementation notes

### Chunks landed

- 01 Community NSFW flag events, delivery sweep and weekly digest — 1312a55d
- 02 Classifier queue, worker and Cloudflare client for post text — 5ad573bf
- 03 Comment and community text — 81c6dbd4
- 04 Images through the image proxy — 2af37fb8
- 05 Admin results listing, verdicts and precision — 0ee4b778
- 06 Community summary and re-enqueue — 8751d84e
- 07 Score alerts, metrics and deploy documentation — 7aba5d61

PRD chunk 6, the offline comparison (D15), is not built.

### Decisions made during the build

- No Telegram live test (Bretton). Telegram delivery for flag events and score alerts is covered by integration tests with a recording sender.
- Community `name` stays immutable (identity column and `/c/<name>` routing key). A profile edit that changes only the name adds no job; the classified name is the stored one.
- The daily request cap is one cap across models, counting text and image requests together. `ReserveRequest` takes an advisory lock on the UTC date; per-model usage rows remain for metrics.
- The results listing withholds content for author-deleted subjects and subjects with a hidden-reason removal, following the existing `ListActions` rule.
- The re-enqueue steps in `PRODUCTION_DEPLOY.md` ("Enabling or re-enabling the content classifier") apply to re-enabling only; a first enable has no back-fill (D13).
- `.env.ci` enables the classifier with dummy Cloudflare values. Outbound requests fail at the blocked CI network and no hermetic assertion depends on them.

### Pre-merge fixes

- Re-enqueue revives failed jobs under the same evaluation: queued again and due now, with attempts and error cleared; the report counts them as `revived`. Closed jobs are left alone.
- A job's final failure with no answered batch deletes its stored batch rows in the same statement and logs the count as `batches_discarded`. With answered batches it stores a partial result instead (below).
- Deferral of a job awaiting admission backs off exponentially: migration 099 adds `deferral_count`, each deferral waits `CLASSIFIER_PENDING_DEFERRAL` doubled per earlier deferral up to 6 hours and costs no attempt, and the count resets to 0 when the job is re-armed or revived.
- `CLASSIFIER_DAILY_REQUEST_CAP`, `CLASSIFIER_RETRY_BASE_DELAY` and `CLASSIFIER_PENDING_DEFERRAL` must be greater than 0 when the classifier is enabled.
- Request failures log `http_status`.

### Approved additions (Bretton, 2026-11-14)

- **Partial result on a final failure.** A job that fails at the attempt limit with answered batches stores a result from them in the same statement as the failure (`StorePartialResult`): the per-question maximum, the images never sent counted skipped, the model as for a full result, and its score alerts raised as `StoreResult` raises them. The subject was checked before the failed request, as for any stored answer, and the write is conditioned on the job being non-terminal, so erasure rules are unchanged. The job's status stays `failed`, with its attempt count and `last_error_code`, and `classification_results.partial` (migration 100) is true; `listClassificationResults` shows `partial`. Status `failed` was kept because nothing assumed a result implies `classified`; the one interaction, re-enqueue reviving a failed job that holds a result (`job_id` is unique), is handled by replacing the result in place when the revived job completes: same ID so verdicts stay, `partial` false, `created_at` reset, and alerts already raised kept (`ON CONFLICT DO NOTHING`). Logged with `partial_result_stored`. A job with no answered batch stores nothing, as before.
- **Partial results are verdictable** and count toward precision and recall like any result; the list view flags them.
- **Pause on a refused Cloudflare token.** The client maps 401 and 403 to `provider_auth`. The worker spends no attempt and records no failure, gives back every claim of the run, sends no further request that run (a concurrent job checks the pause before reserving its request), logs once per pause at Error with `code=provider_auth` and `http_status`, and claims nothing until the pause ends: 5 minutes, doubled per refusal after a pause up to 1 hour, reset to 5 minutes by the next accepted request. The pause is in memory (`ProviderAuthPause`), shared by the worker and the metrics; `getClassificationMetrics` answers `authPausedUntil` while it holds. A restart clears it.

### Notes for maintainers

- Migrations: 094 `community_nsfw_flag_events` and `community_signal_digest_marker`; 095 `classification_jobs`, `classification_results` and `classification_usage`; 096 `classification_job_batches`; 097 `classification_verdicts`; 098 `classification_score_alerts`; 099 `classification_jobs.deferral_count`; 100 `classification_results.partial`.
- Worker order per job: missing subject closes as not visible; a CID mismatch closes as superseded; a post awaiting admission (and comments under it) is deferred; any other hidden state (removal, deletion, erased author, refused admission, deleted community) closes as not visible; only then is a request reserved and sent.
- Claims, metrics and worker lag are scoped to the current evaluation (model and policy version); jobs under an older evaluation are neither claimed nor counted.
- A post becoming accepted re-arms the post's and its comments' non-terminal jobs (`rearmClassificationJobs`): an acceptance that admits it, or the author event that converges a `pending_reacceptance` row on the acceptance already recorded.
- Multi-batch image jobs store per-batch progress and continue across runs and across the daily cap boundary. Image fetches are bounded so the request still fits in the run; cut-off and refused images count as skipped.
- Flag-event and score-alert delivery claim one row at a time under a lease (`FOR UPDATE SKIP LOCKED`); outcome writes are conditioned on the lease and use a bounded context that survives cancellation, so a delivered alert is not resent. A score alert whose last attempt fails is logged at Error.
- Erasure: user erasure deletes the author's classification results and jobs in the same transaction as the moderation snapshots. Enqueue, re-enqueue and verdict writes take the shared erasure advisory lock for the author first; community results use the community DID as author, so user erasure leaves them.
- Logs carry job IDs and fixed error codes (for example `classification_image_fetch_failed`, `daily_cap_reached`, `provider_auth`, `partial_result_stored`, `classification_review_list_failed`, `classification_enqueue_failed`, `classification_summary_failed`), never content, DIDs, CIDs or probabilities tied to a subject.
- Policy v3 (`policy/v3.json`) marks `sexual_minors` `requiresImage`: it is asked only in requests that carry an image. A text-only request (a post or comment without images, a community without a fetched avatar or banner, or a batch whose images all failed to fetch) is asked the other five questions, so a text-only result has no `sexual_minors` key and raises no `sexual_minors` score alert. A later batch with a fetched image is still asked it. The question's instructions still name text harms such as grooming or solicitation, but under v3 text alone is not scored for them: the text-only scores were false positives on news reports and discussion (2026-11 lemmy.world backfill). The community summary for `sexual_minors` counts only subjects whose latest result has the key, so a subject whose newest version is text-only falls back to an older result that scored it. The other questions' instructions are unchanged from v2; results scored under v1 or v2 keep their version. Jobs still queued, retrying or deferred under v2 are not claimed after the switch; re-enqueue their range to score them under v3.
- Every new configuration key is forwarded explicitly in `docker-compose.prod.yml` and listed in `.env.dev.example` and `.env.prod.example`. The score-alert default is `sexual_minors:0.3`.

### Deferred findings

- Flag-event and score-alert sends have no per-send timeout of their own; a `TELEGRAM_TIMEOUT_SECONDS` above the 5-minute lease could double-send.
- The results listing reads the deleted and withheld flags and the subject's content in separate snapshots, so a subject hidden between the two reads can be returned with its content once.
- Re-enqueue ranges use `indexed_at`, the Jetstream event time; an event delivered late while the classifier was off can fall before the range start. The deploy doc asks for an overlapping start; a processing-time column would close it (about a day).
- Transient image fetch errors (proxy busy, PDS timeout, resolver outage) are a permanent skip for that result, not a retryable failure (about 30 lines and a test).
- Metrics report undelivered score alerts as one count mixing pending and exhausted rows; a separate exhausted count would tell "Telegram down, retrying" from "alerts dropped".
- `rearmClassificationJobs` also makes a claimed or backing-off job due at once, and `Defer` is not conditioned on the claim lease, so an acceptance racing a worker can cost a duplicate request or a 10-minute delay.

### Live checks

Real Cloudflare live tests for chunks 2–4 (`tests/live/classifier_cloudflare_test.go`: post text, comment and community text, images in batches) passed on 2026-11-12 against the integration branch. There is no Telegram live check, by decision.

### Lexicons

The new admin endpoints (`listClassificationResults`, `recordClassificationVerdict`, `getClassificationPrecision`, `enqueueClassification`, `getCommunityClassificationSummary`, `getClassificationMetrics`) are on the held-back list in `docs/LEXICON_PUBLISHING.md` and need the next lexicon publish.
