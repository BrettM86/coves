package jetstream

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// PostEventConsumer consumes post-related events from Jetstream
// Handles CREATE, UPDATE, and DELETE operations for social.coves.community.post
type PostEventConsumer struct {
	postRepo      posts.Repository
	communityRepo communities.Repository
	userService   users.UserService
	db            *sql.DB // Direct DB access for atomic count reconciliation
	// bridgeTrust gates whether a post's community repo may assert bridgedStats.
	// nil means default-deny (bridgedStats are ignored for every post).
	bridgeTrust *BridgeTrust
	// identityResolver is used only when relay scheduling delivers a post
	// before its author's profile. The identity is admitted only when its PDS
	// passes bridgeTrust.
	identityResolver identity.Resolver
}

// PostEventConsumerOption configures optional PostEventConsumer behaviour.
type PostEventConsumerOption func(*PostEventConsumer)

// WithPostBridgeTrust installs the provenance gate that decides which community repos
// may assert bridgedStats on their posts. Without it, bridgedStats are default-denied.
func WithPostBridgeTrust(bt *BridgeTrust) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.bridgeTrust = bt }
}

// WithPostIdentityResolver enables safe discovery of bridged authors whose
// profile-repo event has not reached the AppView yet.
func WithPostIdentityResolver(resolver identity.Resolver) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.identityResolver = resolver }
}

// NewPostEventConsumer creates a new Jetstream consumer for post events
func NewPostEventConsumer(
	postRepo posts.Repository,
	communityRepo communities.Repository,
	userService users.UserService,
	db *sql.DB,
	opts ...PostEventConsumerOption,
) *PostEventConsumer {
	c := &PostEventConsumer{
		postRepo:      postRepo,
		communityRepo: communityRepo,
		userService:   userService,
		db:            db,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RevGated reports whether this consumer applies the per-record rev gate; always true
// for posts (gating is hardwired via c.db). main.go checks this at boot to refuse
// multi-feed operation with an ungated consumer.
func (c *PostEventConsumer) RevGated() bool { return true }

// HandleEvent processes a Jetstream event for post records
// Handles CREATE, UPDATE, and DELETE operations
func (c *PostEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for post records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Handle post record operations
	if commit.Collection == "social.coves.community.post" {
		switch commit.Operation {
		case "create":
			return c.createPost(ctx, event.Did, commit, event.TimeUS)
		case "update":
			return c.updatePost(ctx, event.Did, commit, event.TimeUS)
		case "delete":
			return c.deletePost(ctx, event.Did, commit)
		}
	}

	// Silently ignore other operations and other collections
	return nil
}

// eventTime converts a Jetstream time_us wall-clock timestamp to a time.Time.
// ok is false when the event carries no timestamp (TimeUS == 0 — synthetic or
// legacy test events), in which case recency guards are bypassed and updates
// apply unconditionally (backward compatible).
func eventTime(timeUS int64) (time.Time, bool) {
	if timeUS <= 0 {
		return time.Time{}, false
	}
	return time.UnixMicro(timeUS).UTC(), true
}

// indexedAtForEvent returns the row watermark recorded for an applied event: the
// Jetstream event time when available, falling back to wall clock for synthetic
// events (TimeUS == 0). Keeping the watermark in Jetstream's clock domain (event
// time compared against event time, never against AppView wall clock) is what
// makes the stale-redrive recency guard exact: Jetstream time_us is monotonic
// per stream, so a live in-order update always carries a time_us strictly
// greater than the watermark left by the previous applied event, while a
// DeadLetterRedriver replay of an OLDER failed update carries a smaller one and
// is skipped instead of silently reverting newer content.
func indexedAtForEvent(timeUS int64) time.Time {
	if t, ok := eventTime(timeUS); ok {
		return t
	}
	return time.Now()
}

// createPost indexes a new post from the firehose
func (c *PostEventConsumer) createPost(ctx context.Context, repoDID string, commit *CommitEvent, timeUS int64) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: post create event missing record data", ErrPermanentEvent)
	}

	// Parse the post record
	postRecord, err := parsePostRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse post record: %w", err)
	}

	// SECURITY: Validate this is a legitimate post event. Returns the community row so
	// we can check bridgedStats provenance against its resolved PDS host.
	community, err := c.validatePostEvent(ctx, repoDID, postRecord)
	if err != nil {
		logPostValidationRejection("post create", err)
		return err
	}

	// Build AT-URI for this post
	// Format: at://community_did/social.coves.community.post/rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", repoDID, commit.RKey)

	// Parse timestamp from record
	createdAt, err := time.Parse(time.RFC3339, postRecord.CreatedAt)
	if err != nil {
		// Fallback to current time if parsing fails
		log.Printf("Warning: Failed to parse createdAt timestamp, using current time: %v", err)
		createdAt = time.Now()
	}

	// SECURITY: Clamp future timestamps to now. created_at drives the "new" sort
	// and the hot-rank age, so a record asserting a future date (hostile or
	// clock-skewed federated repo) could otherwise pin itself to the top of
	// feeds until wall-clock catches up.
	if now := time.Now(); createdAt.After(now) {
		log.Printf("Warning: post %s has future createdAt %s, clamping to now", uri, postRecord.CreatedAt)
		createdAt = now
	}

	// Build post entity
	post := &posts.Post{
		URI:          uri,
		CID:          commit.CID,
		RKey:         commit.RKey,
		AuthorDID:    postRecord.Author,
		CommunityDID: postRecord.Community,
		Title:        postRecord.Title,
		Content:      postRecord.Content,
		CreatedAt:    createdAt,
		IndexedAt:    indexedAtForEvent(timeUS), // recency-guard watermark (see indexedAtForEvent)
		// Native stats remain at 0 (no native votes yet); bridged stats applied below.
		UpvoteCount:   0,
		DownvoteCount: 0,
		Score:         0,
		CommentCount:  0,
	}

	// Apply bridge-asserted origin-platform vote aggregates if the record carries them,
	// the community repo is a trusted bridge (provenance gate, default-deny), and the
	// aggregate passes input hygiene. At create there are no native votes, so the
	// inclusive score is simply the bridged delta. bridgedStats is optional (absent for
	// natively-authored posts). The counts + asOf are applied atomically: an unparseable
	// or out-of-hygiene aggregate is ignored WHOLE, never leaving counts with a NULL
	// asOf (which would defeat the update-path regression guard).
	if postRecord.BridgedStats != nil {
		if c.bridgeTrust.TrustsPDS(community.PDSURL) {
			if up, down, asOf, ok := validatedBridgedStats(postRecord.BridgedStats, uri); ok {
				post.BridgedUpvoteCount = up
				post.BridgedDownvoteCount = down
				post.BridgedStatsAsOf = &asOf
				post.Score = up - down
			}
		} else {
			log.Printf("debug: ignoring bridgedStats on post %s from untrusted repo %s (not a trusted bridge PDS)", uri, repoDID)
		}
	}

	// Serialize JSON fields (facets, embed, labels)
	// Return error if any non-empty field fails to serialize (prevents silent data loss)
	if postRecord.Facets != nil {
		facetsJSON, marshalErr := json.Marshal(postRecord.Facets)
		if marshalErr != nil {
			return fmt.Errorf("failed to serialize facets: %w", marshalErr)
		}
		facetsStr := string(facetsJSON)
		post.ContentFacets = &facetsStr
	}

	if postRecord.Embed != nil {
		embedJSON, marshalErr := json.Marshal(postRecord.Embed)
		if marshalErr != nil {
			return fmt.Errorf("failed to serialize embed: %w", marshalErr)
		}
		embedStr := string(embedJSON)
		post.Embed = &embedStr
	}

	if postRecord.Labels != nil {
		labelsJSON, marshalErr := json.Marshal(postRecord.Labels)
		if marshalErr != nil {
			return fmt.Errorf("failed to serialize labels: %w", marshalErr)
		}
		labelsStr := string(labelsJSON)
		post.ContentLabels = &labelsStr
	}

	// Atomically: Rev-gate + Index post + Reconcile comment count for out-of-order arrivals
	if err := c.indexPostAndReconcileCounts(ctx, post, commit.Rev); err != nil {
		return fmt.Errorf("failed to index post and reconcile counts: %w", err)
	}

	log.Printf("✓ Indexed post: %s (author: %s, community: %s, rkey: %s)",
		uri, post.AuthorDID, post.CommunityDID, commit.RKey)
	return nil
}

// deletePost handles post deletion events from Jetstream
// Soft-deletes the post in AppView database by setting deleted_at timestamp
func (c *PostEventConsumer) deletePost(ctx context.Context, repoDID string, commit *CommitEvent) error {
	// Build AT-URI for this post
	// Format: at://community_did/social.coves.community.post/rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", repoDID, commit.RKey)

	// REV GATE + soft delete in one transaction (the repo's SoftDelete is not
	// transaction-aware, and the delete's rev must be recorded atomically with
	// the tombstone: it is what rejects a stale cross-feed copy of the CREATE
	// arriving later and resurrecting the post). The gate row is advanced even
	// when the post was never indexed, so the late create of an already-deleted
	// record is rejected too.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	won, err := tryAdvanceRecordRev(ctx, tx, uri, commit.Rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(ConsumerPosts, "delete", uri, commit.Rev)
		return nil
	}

	// Same statement as postRepo.SoftDelete, inlined for transactionality.
	// Idempotent: zero rows (already deleted or never indexed) is success.
	if _, err := tx.ExecContext(ctx,
		`UPDATE posts SET deleted_at = NOW() WHERE uri = $1 AND deleted_at IS NULL`, uri,
	); err != nil {
		return fmt.Errorf("failed to soft delete post: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit post delete transaction: %w", err)
	}

	log.Printf("✓ Deleted post: %s (community: %s, rkey: %s)", uri, repoDID, commit.RKey)
	return nil
}

// updatePost handles post record update events from Jetstream.
//
// Posts previously ignored updates; the bridge now edits post records (content and
// especially the refreshed bridgedStats aggregate) via debounced record updates, so
// we must fold those into the index. Every branch below is idempotent, which matters
// because the connector logs-and-drops on error WITHOUT tracking a cursor (it live-
// tails Jetstream): a returned error is NOT retried or replayed, so we only return an
// error for genuinely transient infra faults and otherwise skip benign no-ops cleanly.
// The accepted consequence for stats: if a bridgedStats update errors out, the folded
// counts stay stale until the bridge next edits the record (which it does on every
// stats refresh), so the desync is self-healing rather than permanent.
//
// Security mirrors createPost (repoDID must equal record.community; community and
// author must exist). We additionally reject reassignment: an update may not move a
// post to a different community or author. Reassignment, a missing stored row, and a
// soft-deleted stored row are all skipped (logged, no error).
func (c *PostEventConsumer) updatePost(ctx context.Context, repoDID string, commit *CommitEvent, timeUS int64) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: post update event missing record data", ErrPermanentEvent)
	}

	postRecord, err := parsePostRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse post record: %w", err)
	}

	// SECURITY: identical validation to create (repo == community, community/author exist).
	community, err := c.validatePostEvent(ctx, repoDID, postRecord)
	if err != nil {
		logPostValidationRejection("post update", err)
		return err
	}

	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", repoDID, commit.RKey)

	// Fetch the stored row so we can enforce immutability and run the asOf regression guard.
	var (
		storedID           int64
		storedCommunityDID string
		storedAuthorDID    string
		storedDeletedAt    *time.Time
		storedAsOf         *time.Time
		storedIndexedAt    time.Time
	)
	err = c.db.QueryRowContext(ctx,
		`SELECT id, community_did, author_did, deleted_at, bridged_stats_as_of, indexed_at FROM posts WHERE uri = $1`,
		uri,
	).Scan(&storedID, &storedCommunityDID, &storedAuthorDID, &storedDeletedAt, &storedAsOf, &storedIndexedAt)
	if err == sql.ErrNoRows {
		// Not indexed yet (out-of-order delivery). Jetstream will replay CREATE; skip.
		log.Printf("Update event for non-indexed post: %s (will be indexed on CREATE)", uri)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to load stored post for update: %w", err)
	}

	// Skip soft-deleted rows: a deleted post should not be resurrected by an edit.
	if storedDeletedAt != nil {
		log.Printf("Update event for soft-deleted post: %s (skipping)", uri)
		return nil
	}

	// RECENCY GUARD: a redriven (DeadLetterRedriver) or rewound update can arrive
	// AFTER a newer update was already indexed. indexed_at is the watermark of the
	// last applied event for this row (event time, see indexedAtForEvent); an event
	// whose time_us is not strictly newer must be skipped, or a stale replay would
	// silently revert newer content. Skipping is SUCCESS (the newer state wins) —
	// returning an error would re-dead-letter an event that must never be applied.
	// This Go pre-check exists for clean logging; the UPDATE below repeats the
	// comparison atomically so a concurrent newer write between this read and the
	// write still cannot be clobbered.
	if evTime, ok := eventTime(timeUS); ok && !storedIndexedAt.Before(evTime) {
		log.Printf("INFO: skipping stale post update for %s (event time %s <= last indexed %s; newer state already applied)",
			uri, evTime.Format(time.RFC3339Nano), storedIndexedAt.Format(time.RFC3339Nano))
		return nil
	}

	// SECURITY: community and author are immutable. Reassignment is rejected (skipped).
	if storedCommunityDID != postRecord.Community || storedAuthorDID != postRecord.Author {
		log.Printf("🚨 SECURITY: Rejecting post update - community/author reassignment is not allowed: %s (stored community=%s author=%s; incoming community=%s author=%s)",
			uri, storedCommunityDID, storedAuthorDID, postRecord.Community, postRecord.Author)
		return nil
	}

	// Serialize optional JSON content fields (return on failure to avoid silent data loss).
	var facetsJSON, embedJSON, labelsJSON sql.NullString
	if postRecord.Facets != nil {
		b, marshalErr := json.Marshal(postRecord.Facets)
		if marshalErr != nil {
			return fmt.Errorf("failed to serialize facets: %w", marshalErr)
		}
		facetsJSON.String, facetsJSON.Valid = string(b), true
	}
	if postRecord.Embed != nil {
		b, marshalErr := json.Marshal(postRecord.Embed)
		if marshalErr != nil {
			return fmt.Errorf("failed to serialize embed: %w", marshalErr)
		}
		embedJSON.String, embedJSON.Valid = string(b), true
	}
	if postRecord.Labels != nil {
		b, marshalErr := json.Marshal(postRecord.Labels)
		if marshalErr != nil {
			return fmt.Errorf("failed to serialize labels: %w", marshalErr)
		}
		labelsJSON.String, labelsJSON.Valid = string(b), true
	}

	// Decide the candidate bridged aggregate to hand to the atomic UPDATE. It is applied
	// only when the record carried bridgedStats AND the community repo is a trusted
	// bridge (provenance gate, default-deny) AND the aggregate passes input hygiene
	// (non-negative, within the magnitude cap) AND its asOf parses. Otherwise we pass a
	// NULL incoming asOf, which makes the SQL guard below leave the stored bridged
	// columns untouched. The actual newer-or-equal regression comparison is done
	// ATOMICALLY inside the UPDATE (see the CASE expressions) rather than read-here /
	// write-later, so it cannot race a concurrent write; storedAsOf is read only to log
	// the strictly-older case.
	var (
		incomingUp, incomingDown int
		incomingAsOf             *time.Time
	)
	if postRecord.BridgedStats != nil {
		if c.bridgeTrust.TrustsPDS(community.PDSURL) {
			if up, down, asOf, ok := validatedBridgedStats(postRecord.BridgedStats, uri); ok {
				incomingUp, incomingDown, incomingAsOf = up, down, &asOf
				// Best-effort log only (the write is authoritative and atomic): a
				// strictly-older asOf is dropped by the SQL guard. Kept at debug because
				// the bridge re-sends the same asOf on every content edit, so this is
				// noise, not an anomaly.
				if storedAsOf != nil && asOf.Before(*storedAsOf) {
					log.Printf("debug: ignoring strictly-older bridgedStats for %s (incoming asOf %s < stored %s)",
						uri, asOf.Format(time.RFC3339), storedAsOf.Format(time.RFC3339))
				}
			}
		} else {
			log.Printf("debug: ignoring bridgedStats on post %s from untrusted repo %s (not a trusted bridge PDS)", uri, repoDID)
		}
	}

	// Single atomic UPDATE. edited_at is bumped only when content actually changed (so a
	// debounced stats-only refresh does not mark the post edited). The bridged columns
	// and the inclusive score move together via a shared applies-guard: apply the
	// incoming counts only when an incoming asOf is present and is newer-or-equal to the
	// stored one (NULL stored => first application). score is always recomputed from the
	// LIVE native counts plus whichever bridged counts win, so concurrent native votes
	// are never clobbered. $10 is the incoming asOf (NULL => no bridged change); the
	// stored asOf is read directly from the row, keeping the compare atomic.
	//
	// $11 is the event's Jetstream time_us. The WHERE clause repeats the recency
	// guard atomically (indexed_at must be strictly older than the event, unless the
	// event carries no timestamp), and indexed_at is advanced to the event time on
	// every applied update so the NEXT stale replay is blocked too.
	updateQuery := `
		UPDATE posts
		SET
			cid = $2,
			title = $3,
			content = $4,
			content_facets = $5,
			embed = $6,
			content_labels = $7,
			edited_at = CASE
				WHEN title IS DISTINCT FROM $3 OR content IS DISTINCT FROM $4
				  OR content_facets IS DISTINCT FROM $5 OR embed IS DISTINCT FROM $6
				  OR content_labels IS DISTINCT FROM $7
				THEN NOW() ELSE edited_at END,
			indexed_at = CASE
				WHEN $11::bigint > 0 THEN to_timestamp($11::bigint / 1000000.0)
				ELSE indexed_at END,
			bridged_upvote_count = CASE
				WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
				THEN $8 ELSE bridged_upvote_count END,
			bridged_downvote_count = CASE
				WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
				THEN $9 ELSE bridged_downvote_count END,
			bridged_stats_as_of = CASE
				WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
				THEN $10 ELSE bridged_stats_as_of END,
			score = upvote_count - downvote_count
				+ CASE
					WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
					THEN $8 ELSE bridged_upvote_count END
				- CASE
					WHEN $10::timestamptz IS NOT NULL AND (bridged_stats_as_of IS NULL OR $10 >= bridged_stats_as_of)
					THEN $9 ELSE bridged_downvote_count END
		WHERE id = $1 AND deleted_at IS NULL
		  AND ($11::bigint <= 0 OR indexed_at < to_timestamp($11::bigint / 1000000.0))
	`
	// REV GATE + UPDATE in one transaction. The gate (strictly-newer rev wins)
	// is the cross-feed ordering guard: the time_us recency guard in the WHERE
	// clause below cannot reject a stale copy delivered by ANOTHER feed, because
	// each feed stamps its own emission time — a pre-edit update replayed by the
	// lagging bsky feed carries a NEWER time_us than the edit it would regress.
	// Only rev, assigned by the repo itself, orders events across feeds.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	won, err := tryAdvanceRecordRev(ctx, tx, uri, commit.Rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(ConsumerPosts, "update", uri, commit.Rev)
		return nil
	}

	result, err := tx.ExecContext(ctx, updateQuery,
		storedID, commit.CID, postRecord.Title, postRecord.Content,
		facetsJSON, embedJSON, labelsJSON,
		incomingUp, incomingDown, incomingAsOf,
		timeUS,
	)
	if err != nil {
		return fmt.Errorf("failed to update post: %w", err)
	}

	// A post can be soft-deleted — or overtaken by a concurrent NEWER update (recency
	// guard) — between the load above and this UPDATE; the WHERE guards then match no
	// rows. Report that as a skip instead of falsely logging a successful update
	// (mirrors vote_consumer's RowsAffected check). Both cases are success: the row's
	// current state supersedes this event.
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check post update result: %w", err)
	}
	if rowsAffected == 0 {
		// The deferred rollback also reverts the gate advance — conservative: a
		// replay re-evaluates against whatever state superseded this event.
		log.Printf("Update event for post that was deleted or superseded by a newer update between load and write: %s (skipping)", uri)
		return nil
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit post update transaction: %w", err)
	}

	if incomingAsOf != nil {
		log.Printf("✓ Updated post: %s (bridgedStats candidate applied if newer-or-equal: up=%d down=%d)", uri, incomingUp, incomingDown)
	} else {
		log.Printf("✓ Updated post: %s", uri)
	}
	return nil
}

// parseBridgedAsOf parses a bridgedStats.asOf timestamp, logging (and returning the
// error) on failure so callers can decide to skip applying the aggregate.
func parseBridgedAsOf(asOf, uri string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, asOf)
	if err != nil {
		log.Printf("Warning: failed to parse bridgedStats.asOf %q for %s: %v", asOf, uri, err)
		return time.Time{}, err
	}
	return t, nil
}

// indexPostAndReconcileCounts atomically indexes a post and reconciles comment counts
// This fixes the race condition where comments arrive before their parent post
func (c *PostEventConsumer) indexPostAndReconcileCounts(ctx context.Context, post *posts.Post, rev string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 0. REV GATE: apply this create only if its rev is strictly newer than the
	// last applied event for this record — rejects duplicate replays (equal rev)
	// and stale cross-feed copies of a create arriving after the post's delete
	// (which would resurrect it). Runs first, inside the transaction, so gate
	// and writes commit or roll back together.
	won, err := tryAdvanceRecordRev(ctx, tx, post.URI, rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(ConsumerPosts, "create", post.URI, rev)
		return nil
	}

	// 1. Insert the post (idempotent with RETURNING clause)
	var facetsJSON, embedJSON, labelsJSON sql.NullString

	if post.ContentFacets != nil {
		facetsJSON.String = *post.ContentFacets
		facetsJSON.Valid = true
	}

	if post.Embed != nil {
		embedJSON.String = *post.Embed
		embedJSON.Valid = true
	}

	if post.ContentLabels != nil {
		labelsJSON.String = *post.ContentLabels
		labelsJSON.Valid = true
	}

	insertQuery := `
		INSERT INTO posts (
			uri, cid, rkey, author_did, community_did,
			title, content, content_facets, embed, content_labels,
			created_at, indexed_at,
			bridged_upvote_count, bridged_downvote_count, bridged_stats_as_of, score
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12,
			$13, $14, $15, $16
		)
		ON CONFLICT (uri) DO NOTHING
		RETURNING id
	`

	var postID int64
	insertErr := tx.QueryRowContext(
		ctx, insertQuery,
		post.URI, post.CID, post.RKey, post.AuthorDID, post.CommunityDID,
		post.Title, post.Content, facetsJSON, embedJSON, labelsJSON,
		post.CreatedAt, post.IndexedAt,
		post.BridgedUpvoteCount, post.BridgedDownvoteCount, post.BridgedStatsAsOf, post.Score,
	).Scan(&postID)

	// If no rows returned, post already exists (idempotent - OK for Jetstream replays)
	if insertErr == sql.ErrNoRows {
		// KNOWN LIMITATION (accepted): a genuine RE-CREATE of the same rkey while
		// the row is still ACTIVE also lands here and is treated as an idempotent
		// duplicate, dropping the new content. Reaching that state requires the
		// exact sequence: create A applied → delete dead-lettered (failed, never
		// applied) → re-create B (same rkey, strictly newer rev) arrives while
		// the row is still active. B's content is never applied, the gate
		// advances to B's rev, and the redriven delete A is then gate-rejected —
		// the row survives with A's content. This needs a dead-lettered delete
		// AND an rkey reuse inside the redrive window; rare enough to document
		// rather than plumb a full content upsert through the create path
		// (comments implement the in-place re-create because their resurrection
		// machinery already exists; see comment_consumer.go).
		log.Printf("Post already indexed: %s (idempotent)", post.URI)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	if insertErr != nil {
		return fmt.Errorf("failed to insert post: %w", insertErr)
	}

	// 2. Reconcile comment_count for this newly inserted post
	// In case any comments arrived out-of-order before this post was indexed
	// This is the CRITICAL FIX for the race condition identified in the PR review
	// NOTE: Uses root_uri to count ALL comments in thread (including nested replies)
	// NOTE: Counts include deleted comments since they're shown as "[deleted]" placeholders
	//
	// IMPORTANT: This reconciliation logic and the increment logic in CommentEventConsumer
	// must stay in sync. Both use the same counting semantics:
	// - Count ALL comments (including deleted) since deleted comments appear as "[deleted]" placeholders
	// - This ensures comment_count matches the actual visible thread structure
	// If you modify one, you must review and potentially modify the other.
	// See: comment_consumer.go indexCommentAndUpdateCounts()
	reconcileQuery := `
		UPDATE posts
		SET comment_count = (
			SELECT COUNT(*)
			FROM comments c
			WHERE c.root_uri = $1
		)
		WHERE id = $2
	`
	_, reconcileErr := tx.ExecContext(ctx, reconcileQuery, post.URI, postID)
	if reconcileErr != nil {
		// Reconciliation failure is a critical error - it means comment_count will be incorrect
		// This could cause data inconsistency where the displayed count doesn't match reality
		// Roll back the transaction to maintain consistency
		return fmt.Errorf("failed to reconcile comment_count for %s: %w", post.URI, reconcileErr)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// errValidationInfra marks a post-validation failure caused by an infrastructure fault
// (e.g. a DB error while checking that the community or author exists) rather than a
// policy rejection. The two are logged differently: policy rejections are security
// events (🚨), infra faults are plain operational errors that must NOT masquerade as
// an attack in the logs.
var errValidationInfra = errors.New("validation infrastructure error")

// logPostValidationRejection logs a validatePostEvent failure, distinguishing genuine
// policy rejections (security-relevant) from infrastructure faults (operational). See
// errValidationInfra.
func logPostValidationRejection(op string, err error) {
	if errors.Is(err, errValidationInfra) {
		log.Printf("Error: %s could not be validated (infrastructure fault, not a rejection): %v", op, err)
		return
	}
	log.Printf("🚨 SECURITY: Rejecting %s: %v", op, err)
}

// validatePostEvent performs security validation on post events and, on success,
// returns the community row (whose resolved PDS host drives the bridgedStats
// provenance gate). This prevents malicious actors from indexing fake posts.
func (c *PostEventConsumer) validatePostEvent(ctx context.Context, repoDID string, post *PostRecordFromJetstream) (*communities.Community, error) {
	// CRITICAL SECURITY CHECK:
	// Posts MUST come from community repositories, not user repositories
	// This prevents users from creating posts that appear to be from communities they don't control
	//
	// Example attack prevented:
	//   - User creates post in their own repo (at://user_did/social.coves.community.post/xyz)
	//   - Claims it's for community X (community field = community_did)
	//   - Without this check, fake post would be indexed
	//
	// With this check:
	//   - We verify event.Did (repo owner) == post.community (claimed community)
	//   - Reject if mismatch
	if repoDID != post.Community {
		// PERMANENT: a spoofed repo can never become valid — retrying or redriving
		// this event would reject it identically every time.
		return nil, fmt.Errorf("%w: repository DID (%s) doesn't match community DID (%s) - posts must come from community repos",
			ErrPermanentEvent, repoDID, post.Community)
	}

	// CRITICAL: Verify community exists in AppView.
	// Posts MUST reference valid communities (enforced by FK constraint). If the
	// community isn't indexed yet we reject; because the connector does not track a
	// cursor, the post is only re-indexed if the record is re-emitted (which the bridge
	// does on edits) rather than automatically replayed.
	community, err := c.communityRepo.GetByDID(ctx, post.Community)
	if err != nil {
		if communities.IsNotFound(err) {
			// Policy rejection - community must be indexed before posts.
			// Deliberately NOT ErrPermanentEvent: this is an ORDERING failure (the
			// community's create event may not have been indexed yet) — the redrive
			// will succeed once the community arrives.
			return nil, fmt.Errorf("community not found: %s - cannot index post before community", post.Community)
		}
		// Infrastructure fault (DB error): not an attack. Tag it so the caller logs it
		// as an operational error, not a 🚨 rejection.
		return nil, fmt.Errorf("%w: failed to verify community exists: %v", errValidationInfra, err)
	}

	// CRITICAL: Verify author exists in AppView.
	// Every post MUST have a valid author (enforced by FK constraint). Even though posts
	// live in community repos, they belong to specific authors.
	_, err = c.userService.GetUserByDID(ctx, post.Author)
	if err != nil {
		// Use proper error type checking with errors.Is()
		if errors.Is(err, users.ErrUserNotFound) {
			// BigSky preserves order within a repo, not across repos. A post in
			// a community repo can therefore arrive before actor.profile in the
			// author's repo. Resolve and minimally index only identities hosted
			// by an explicitly trusted bridge; unknown native users remain
			// rejected by default.
			if c.identityResolver != nil {
				resolved, resolveErr := c.identityResolver.Resolve(ctx, post.Author)
				if resolveErr != nil {
					return nil, fmt.Errorf("%w: resolve missing post author %s: %v", errValidationInfra, post.Author, resolveErr)
				}
				if resolved != nil && resolved.DID == post.Author &&
					c.bridgeTrust.TrustsPDS(resolved.PDSURL) {
					if indexErr := c.userService.IndexUser(ctx, resolved.DID, resolved.Handle, resolved.PDSURL); indexErr != nil {
						return nil, fmt.Errorf("%w: index trusted bridge author %s: %v", errValidationInfra, post.Author, indexErr)
					}
					return community, nil
				}
			}
			return nil, fmt.Errorf("author not found: %s - cannot index untrusted post author", post.Author)
		}
		// Infrastructure fault (DB error): not an attack.
		return nil, fmt.Errorf("%w: failed to verify author exists: %v", errValidationInfra, err)
	}

	return community, nil
}

// PostRecordFromJetstream represents a post record as received from Jetstream
// Matches the structure written to PDS via social.coves.community.post
type PostRecordFromJetstream struct {
	OriginalAuthor interface{}                `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}                `json:"federatedFrom,omitempty"`
	Location       interface{}                `json:"location,omitempty"`
	Title          *string                    `json:"title,omitempty"`
	Content        *string                    `json:"content,omitempty"`
	Embed          map[string]interface{}     `json:"embed,omitempty"`
	Labels         *posts.SelfLabels          `json:"labels,omitempty"`
	BridgedStats   *BridgedStatsFromJetstream `json:"bridgedStats,omitempty"`
	Type           string                     `json:"$type"`
	Community      string                     `json:"community"`
	Author         string                     `json:"author"`
	CreatedAt      string                     `json:"createdAt"`
	Facets         []interface{}              `json:"facets,omitempty"`
}

// BridgedStatsFromJetstream is the bridge-asserted aggregate of origin-platform
// votes carried on federated/bridged post and comment records (social.coves
// community.post / community.comment #bridgedStats). A nil pointer means the
// record carried no bridgedStats, which callers treat as "leave stored counts
// alone" rather than "reset to zero".
type BridgedStatsFromJetstream struct {
	Upvotes   int    `json:"upvotes"`
	Downvotes int    `json:"downvotes"`
	AsOf      string `json:"asOf"`
}

// parsePostRecord converts a raw Jetstream record map to a PostRecordFromJetstream
func parsePostRecord(record map[string]interface{}) (*PostRecordFromJetstream, error) {
	// Marshal to JSON and back to ensure proper type conversion
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var post PostRecordFromJetstream
	if err := json.Unmarshal(recordJSON, &post); err != nil {
		// PERMANENT: the record's shape doesn't match the lexicon (wrong field
		// types); replaying the identical bytes can never parse differently.
		return nil, fmt.Errorf("%w: failed to unmarshal post record: %v", ErrPermanentEvent, err)
	}

	// Validate required fields. PERMANENT: a record missing required fields is
	// structurally invalid forever — retries and redrives cannot fix it.
	if post.Community == "" {
		return nil, fmt.Errorf("%w: post record missing community field", ErrPermanentEvent)
	}
	if post.Author == "" {
		return nil, fmt.Errorf("%w: post record missing author field", ErrPermanentEvent)
	}
	if post.CreatedAt == "" {
		return nil, fmt.Errorf("%w: post record missing createdAt field", ErrPermanentEvent)
	}

	return &post, nil
}
