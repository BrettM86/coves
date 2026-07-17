package jetstream

import (
	"Coves/internal/atproto/utils"
	"Coves/internal/core/comments"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Constants for comment validation and processing
const (
	// CommentCollection is the lexicon collection identifier for comments
	CommentCollection = "social.coves.community.comment"

	// ATProtoScheme is the URI scheme for atProto AT-URIs
	ATProtoScheme = "at://"

	// MaxCommentContentBytes is the maximum allowed size for comment content
	// Per lexicon: max 3000 graphemes, ~30000 bytes
	MaxCommentContentBytes = 30000
)

// CommentEventConsumer consumes comment-related events from Jetstream
// Handles CREATE, UPDATE, and DELETE operations for social.coves.community.comment
type CommentEventConsumer struct {
	commentRepo comments.Repository
	db          *sql.DB // Direct DB access for atomic count updates
	// bridgeTrust gates whether a comment's user repo may assert bridgedStats.
	// nil means default-deny (bridgedStats are ignored for every comment).
	bridgeTrust *BridgeTrust
}

// CommentEventConsumerOption configures optional CommentEventConsumer behaviour.
type CommentEventConsumerOption func(*CommentEventConsumer)

// WithCommentBridgeTrust installs the provenance gate that decides which user repos may
// assert bridgedStats on their comments. Without it, bridgedStats are default-denied.
func WithCommentBridgeTrust(bt *BridgeTrust) CommentEventConsumerOption {
	return func(c *CommentEventConsumer) { c.bridgeTrust = bt }
}

// NewCommentEventConsumer creates a new Jetstream consumer for comment events
func NewCommentEventConsumer(
	commentRepo comments.Repository,
	db *sql.DB,
	opts ...CommentEventConsumerOption,
) *CommentEventConsumer {
	c := &CommentEventConsumer{
		commentRepo: commentRepo,
		db:          db,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RevGated reports whether this consumer applies the per-record rev gate; always true
// for comments (gating is hardwired via c.db). main.go checks this at boot to refuse
// multi-feed operation with an ungated consumer.
func (c *CommentEventConsumer) RevGated() bool { return true }

// bridgeStatsAllowedForRepo reports whether the given comment repo (a user DID) is a
// trusted bridge, i.e. whether its records may assert bridgedStats. It resolves the
// repo's PDS host from the already-indexed users row (users.pds_url, populated from
// identity resolution at user creation) and checks it against the trust allowlist.
// Default-deny: any lookup failure — including the user not being indexed yet — means
// "not trusted", so bridgedStats are ignored. The accepted consequence: a bridged
// comment that arrives before its (bridge) author is indexed has its bridgedStats
// dropped at create; they are folded in on the next record edit once the author exists.
func (c *CommentEventConsumer) bridgeStatsAllowedForRepo(ctx context.Context, repoDID string) bool {
	if c.bridgeTrust == nil {
		return false
	}
	var pdsURL string
	err := c.db.QueryRowContext(ctx, `SELECT pds_url FROM users WHERE did = $1`, repoDID).Scan(&pdsURL)
	if err != nil {
		if err == sql.ErrNoRows {
			log.Printf("debug: ignoring bridgedStats from repo %s (user not indexed; provenance unverifiable)", repoDID)
		} else {
			log.Printf("Warning: bridgedStats provenance check failed for repo %s: %v", repoDID, err)
		}
		return false
	}
	return c.bridgeTrust.TrustsPDS(pdsURL)
}

// HandleEvent processes a Jetstream event for comment records
func (c *CommentEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for comment records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Handle comment record operations
	if commit.Collection == CommentCollection {
		switch commit.Operation {
		case "create":
			return c.createComment(ctx, event.Did, commit, event.TimeUS)
		case "update":
			return c.updateComment(ctx, event.Did, commit, event.TimeUS)
		case "delete":
			return c.deleteComment(ctx, event.Did, commit)
		}
	}

	// Silently ignore other operations and collections
	return nil
}

// createComment indexes a new comment from the firehose and updates parent counts
func (c *CommentEventConsumer) createComment(ctx context.Context, repoDID string, commit *CommitEvent, timeUS int64) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: comment create event missing record data", ErrPermanentEvent)
	}

	// Parse the comment record
	commentRecord, err := parseCommentRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse comment record: %w", err)
	}

	// SECURITY: Validate this is a legitimate comment event
	if err := c.validateCommentEvent(ctx, repoDID, commentRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting comment event: %v", err)
		return err
	}

	// Build AT-URI for this comment
	// Format: at://commenter_did/social.coves.community.comment/rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.comment/%s", repoDID, commit.RKey)

	// Parse timestamp from record
	createdAt, err := time.Parse(time.RFC3339, commentRecord.CreatedAt)
	if err != nil {
		log.Printf("Warning: Failed to parse createdAt timestamp, using current time: %v", err)
		createdAt = time.Now()
	}

	// Serialize optional JSON fields
	facetsJSON, embedJSON, labelsJSON, err := serializeOptionalFields(commentRecord)
	if err != nil {
		return fmt.Errorf("failed to serialize optional fields: %w", err)
	}

	// Build comment entity
	comment := &comments.Comment{
		URI:           uri,
		CID:           commit.CID,
		RKey:          commit.RKey,
		CommenterDID:  repoDID, // Comment comes from user's repository
		RootURI:       commentRecord.Reply.Root.URI,
		RootCID:       commentRecord.Reply.Root.CID,
		ParentURI:     commentRecord.Reply.Parent.URI,
		ParentCID:     commentRecord.Reply.Parent.CID,
		Content:       commentRecord.Content,
		ContentFacets: facetsJSON,
		Embed:         embedJSON,
		ContentLabels: labelsJSON,
		Langs:         commentRecord.Langs,
		CreatedAt:     createdAt,
		IndexedAt:     indexedAtForEvent(timeUS), // recency-guard watermark (see indexedAtForEvent in post_consumer.go)
	}

	// Apply bridge-asserted origin-platform vote aggregates if the record carries them,
	// the user repo is a trusted bridge (provenance gate, default-deny), and the
	// aggregate passes input hygiene. At create there are no native votes, so the
	// inclusive score is the bridged delta. bridgedStats is optional (absent for
	// natively-authored comments). Counts + asOf are applied atomically: an unparseable
	// or out-of-hygiene aggregate is ignored WHOLE, never leaving counts with a NULL
	// asOf (which would defeat the update-path regression guard).
	if commentRecord.BridgedStats != nil {
		if c.bridgeStatsAllowedForRepo(ctx, repoDID) {
			if up, down, asOf, ok := validatedBridgedStats(commentRecord.BridgedStats, uri); ok {
				comment.BridgedUpvoteCount = up
				comment.BridgedDownvoteCount = down
				comment.BridgedStatsAsOf = &asOf
				comment.Score = up - down
			}
		} else {
			log.Printf("debug: ignoring bridgedStats on comment %s from untrusted repo %s (not a trusted bridge PDS)", uri, repoDID)
		}
	}

	// Atomically: Rev-gate + Index comment + Update parent counts
	if err := c.indexCommentAndUpdateCounts(ctx, comment, commit.Rev); err != nil {
		return fmt.Errorf("failed to index comment and update counts: %w", err)
	}

	log.Printf("✓ Indexed comment: %s (on %s)", uri, comment.ParentURI)
	return nil
}

// updateComment updates an existing comment's content fields.
//
// Like updatePost, this is idempotent and error-return means log-and-drop (the
// connector tracks no cursor and live-tails Jetstream, so a returned error is NOT
// replayed): the folded bridged counts only self-heal on the bridge's next record
// edit. We therefore skip benign no-ops (missing row, soft-deleted row) cleanly and
// reserve errors for transient infra faults.
func (c *CommentEventConsumer) updateComment(ctx context.Context, repoDID string, commit *CommitEvent, timeUS int64) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: comment update event missing record data", ErrPermanentEvent)
	}

	// Parse the updated comment record
	commentRecord, err := parseCommentRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse comment record: %w", err)
	}

	// SECURITY: Validate this is a legitimate update
	if err := c.validateCommentEvent(ctx, repoDID, commentRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting comment update: %v", err)
		return err
	}

	// Build AT-URI for the comment being updated
	uri := fmt.Sprintf("at://%s/social.coves.community.comment/%s", repoDID, commit.RKey)

	// Load the raw stored columns needed to enforce threading immutability, skip
	// soft-deleted rows, and run the bridgedStats regression guard. This is a dedicated
	// consumer-only query (mirroring post_consumer's inline SELECT): it reads the RAW
	// native and bridged columns, deliberately NOT the folded display counts that
	// GetByURI now returns, so the guard reasons about the true separate aggregates.
	var (
		storedRootURI, storedRootCID     string
		storedParentURI, storedParentCID string
		storedDeletedAt                  *time.Time
		storedAsOf                       *time.Time
		storedIndexedAt                  time.Time
	)
	err = c.db.QueryRowContext(ctx,
		`SELECT root_uri, root_cid, parent_uri, parent_cid, deleted_at, bridged_stats_as_of, indexed_at
		 FROM comments WHERE uri = $1`, uri,
	).Scan(&storedRootURI, &storedRootCID, &storedParentURI, &storedParentCID, &storedDeletedAt, &storedAsOf, &storedIndexedAt)
	if err == sql.ErrNoRows {
		// Comment not indexed yet: its CREATE event will index it when it arrives.
		log.Printf("Update event for non-indexed comment: %s (will be indexed on CREATE)", uri)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to load stored comment for update: %w", err)
	}

	// RECENCY GUARD: a redriven (DeadLetterRedriver) or rewound update can arrive
	// AFTER a newer update was already indexed. indexed_at is the watermark of the
	// last applied event for this row (event time, see indexedAtForEvent in
	// post_consumer.go); an event whose time_us is not strictly newer is skipped,
	// or a stale replay would silently revert newer content. Skipping is SUCCESS
	// (the newer state wins) — an error would re-dead-letter an event that must
	// never be applied. The UPDATE below repeats this comparison atomically.
	if evTime, ok := eventTime(timeUS); ok && !storedIndexedAt.Before(evTime) {
		log.Printf("INFO: skipping stale comment update for %s (event time %s <= last indexed %s; newer state already applied)",
			uri, evTime.Format(time.RFC3339Nano), storedIndexedAt.Format(time.RFC3339Nano))
		return nil
	}

	// Skip soft-deleted rows. A moderator-removed (or author-deleted) comment must not be
	// resurrected by a debounced stats edit; the repo Update's deleted_at IS NULL guard
	// would otherwise match no rows and surface as a spurious ErrCommentNotFound failure
	// recurring on every stats refresh (mirrors post_consumer's soft-deleted skip).
	if storedDeletedAt != nil {
		log.Printf("Update event for soft-deleted comment: %s (skipping)", uri)
		return nil
	}

	// SECURITY: Threading references are IMMUTABLE after creation
	// Reject updates that attempt to change root/parent (prevents thread hijacking)
	if storedRootURI != commentRecord.Reply.Root.URI ||
		storedRootCID != commentRecord.Reply.Root.CID ||
		storedParentURI != commentRecord.Reply.Parent.URI ||
		storedParentCID != commentRecord.Reply.Parent.CID {
		log.Printf("🚨 SECURITY: Rejecting comment update - threading references are immutable: %s", uri)
		log.Printf("  Existing root: %s (CID: %s)", storedRootURI, storedRootCID)
		log.Printf("  Incoming root: %s (CID: %s)", commentRecord.Reply.Root.URI, commentRecord.Reply.Root.CID)
		log.Printf("  Existing parent: %s (CID: %s)", storedParentURI, storedParentCID)
		log.Printf("  Incoming parent: %s (CID: %s)", commentRecord.Reply.Parent.URI, commentRecord.Reply.Parent.CID)
		// PERMANENT: threading reassignment is a policy rejection that no retry or
		// redrive can ever make valid.
		return fmt.Errorf("%w: comment threading references cannot be changed after creation", ErrPermanentEvent)
	}

	// Serialize optional JSON fields
	facetsJSON, embedJSON, labelsJSON, err := serializeOptionalFields(commentRecord)
	if err != nil {
		return fmt.Errorf("failed to serialize optional fields: %w", err)
	}

	// Decide the candidate bridged aggregate handed to repo.Update. It is applied only
	// when the record carried bridgedStats AND the user repo is a trusted bridge
	// (provenance gate, default-deny) AND the aggregate passes input hygiene AND its asOf
	// parses. Otherwise we pass a nil incoming asOf, and the atomic SQL guard in
	// repo.Update leaves the stored bridged columns untouched. The newer-or-equal
	// regression comparison happens ATOMICALLY inside the UPDATE; storedAsOf is read here
	// only to log the strictly-older case.
	var (
		incomingUp, incomingDn int
		incomingAsOf           *time.Time
	)
	if commentRecord.BridgedStats != nil {
		if c.bridgeStatsAllowedForRepo(ctx, repoDID) {
			if up, down, asOf, ok := validatedBridgedStats(commentRecord.BridgedStats, uri); ok {
				incomingUp, incomingDn, incomingAsOf = up, down, &asOf
				if storedAsOf != nil && asOf.Before(*storedAsOf) {
					log.Printf("debug: ignoring strictly-older bridgedStats for %s (incoming asOf %s < stored %s)",
						uri, asOf.Format(time.RFC3339), storedAsOf.Format(time.RFC3339))
				}
			}
		} else {
			log.Printf("debug: ignoring bridgedStats on comment %s from untrusted repo %s (not a trusted bridge PDS)", uri, repoDID)
		}
	}

	// Single atomic UPDATE, issued inline (mirroring post_consumer) rather than via
	// commentRepo.Update because the consumer additionally needs the recency guard:
	// the WHERE clause re-checks that indexed_at is strictly older than the event's
	// time_us ($11) and the SET advances indexed_at to the event time on every
	// applied update, so a stale redriven update can never clobber a concurrently
	// applied newer one (the Go pre-check above only exists for clean logging).
	// The bridged columns and inclusive score keep repo.Update's semantics: apply the
	// incoming aggregate only when incomingAsOf ($10) is non-NULL and newer-or-equal
	// to the stored asOf, and always recompute score from LIVE native counts so
	// concurrent votes survive.
	updateQuery := `
		UPDATE comments
		SET
			cid = $2,
			content = $3,
			content_facets = $4,
			embed = $5,
			content_labels = $6,
			langs = $7,
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
		WHERE uri = $1 AND deleted_at IS NULL
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
		logSkippedStaleRev(ConsumerComments, "update", uri, commit.Rev)
		return nil
	}

	result, err := tx.ExecContext(ctx, updateQuery,
		uri, commit.CID, commentRecord.Content,
		facetsJSON, embedJSON, labelsJSON, pq.Array(commentRecord.Langs),
		incomingUp, incomingDn, incomingAsOf,
		timeUS,
	)
	if err != nil {
		return fmt.Errorf("failed to update comment: %w", err)
	}

	// The comment can be soft-deleted — or overtaken by a concurrent NEWER update
	// (recency guard) — between the load above and this UPDATE; the WHERE guards then
	// match no rows. Both cases are success: the row's current state supersedes this
	// event, so skip instead of erroring (an error would re-dead-letter the event).
	// The deferred rollback also reverts the gate advance, which is the conservative
	// choice: a replay re-evaluates against whatever state superseded this event.
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check comment update result: %w", err)
	}
	if rowsAffected == 0 {
		log.Printf("Update event for comment that was deleted or superseded by a newer update between load and write: %s (skipping)", uri)
		return nil
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit comment update transaction: %w", err)
	}

	if incomingAsOf != nil {
		log.Printf("✓ Updated comment: %s (bridgedStats candidate applied if newer-or-equal: up=%d down=%d)", uri, incomingUp, incomingDn)
	} else {
		log.Printf("✓ Updated comment: %s", uri)
	}
	return nil
}

// deleteComment soft-deletes a comment, blanking content to preserve thread
// structure while respecting user privacy: the row remains and is shown as
// "[deleted]" in thread views, so parent counts are intentionally NOT
// decremented.
//
// The rev-gate claim runs FIRST, inside the same transaction as the soft
// delete (mirrors deletePost). Claiming before touching the comments table
// closes the not-found tombstone race: a concurrent create of the same
// comment (another feed's copy, or a DeadLetterRedriver replay) serializes on
// the gate row lock, so it either commits before our delete (which then finds
// and soft-deletes the row) or blocks until our tombstone commits (its
// equal-or-older rev then loses the gate). The gate row is advanced — and
// committed — even when the comment was never indexed, so the create's late
// copy is rejected too.
func (c *CommentEventConsumer) deleteComment(ctx context.Context, repoDID string, commit *CommitEvent) error {
	// Build AT-URI for the comment being deleted
	uri := fmt.Sprintf("at://%s/social.coves.community.comment/%s", repoDID, commit.RKey)

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 0. REV GATE (see indexCommentAndUpdateCounts): skip duplicate replays and
	// stale cross-feed copies; the claimed row doubles as the tombstone that
	// rejects the create's later copies.
	won, err := tryAdvanceRecordRev(ctx, tx, uri, commit.Rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(ConsumerComments, "delete", uri, commit.Rev)
		return nil
	}

	// 1. Soft-delete the comment: blank content but preserve structure.
	// DELETE event from Jetstream = author deleted their own comment (the repo
	// owner IS the commenter), so deleted_by is the repo DID.
	// Use the repository's transaction-aware method for DRY.
	repoTx, ok := c.commentRepo.(comments.RepositoryTx)
	if !ok {
		return fmt.Errorf("comment repository does not support transactional operations")
	}

	rowsAffected, err := repoTx.SoftDeleteWithReasonTx(ctx, tx, uri, comments.DeletionReasonAuthor, repoDID)
	if err != nil {
		return fmt.Errorf("failed to delete comment: %w", err)
	}

	// Idempotent: zero rows means the comment was already deleted or never
	// indexed. Commit anyway — the gate advance is the tombstone that rejects
	// a stale cross-feed copy of the CREATE arriving later for a record that
	// no longer exists on the PDS.
	if rowsAffected == 0 {
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		log.Printf("Comment already deleted or not found: %s", uri)
		return nil
	}

	// NOTE: We intentionally do NOT decrement parent counts (comment_count/reply_count)
	// Deleted comments are shown as "[deleted]" placeholders to preserve thread structure,
	// so they should still count toward the displayed total.

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("✓ Deleted comment: %s", uri)
	return nil
}

// indexCommentAndUpdateCounts atomically indexes a comment and updates parent counts
func (c *CommentEventConsumer) indexCommentAndUpdateCounts(ctx context.Context, comment *comments.Comment, rev string) error {
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
	// last applied event for this record. This is what makes the resurrection
	// branch below SAFE: a stale cross-feed copy of the original create arriving
	// after the comment's delete carries an older rev than the tombstoned delete
	// and is rejected here, while a genuine re-creation of the rkey carries a
	// fresh, higher rev and passes through to the resurrection path. Runs first,
	// inside the transaction, so gate and writes commit or roll back together.
	won, err := tryAdvanceRecordRev(ctx, tx, comment.URI, rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(ConsumerComments, "create", comment.URI, rev)
		return nil
	}

	// 1. Check if comment exists and handle resurrection case
	// In atProto, deleted records' rkeys become available - users can recreate with same rkey
	// We must distinguish: idempotent replay (skip) vs resurrection (update + restore counts)
	var existingID int64
	var existingCID string
	var existingDeletedAt *time.Time
	var existingParentURI, existingRootURI string
	checkQuery := `SELECT id, cid, deleted_at, parent_uri, root_uri FROM comments WHERE uri = $1`
	checkErr := tx.QueryRowContext(ctx, checkQuery, comment.URI).Scan(&existingID, &existingCID, &existingDeletedAt, &existingParentURI, &existingRootURI)

	var commentID int64
	var isResurrectionWithSameParent bool // Track if we should skip parent count increment

	if checkErr == nil {
		// Comment exists
		if existingDeletedAt == nil {
			// Active row. Usually this is an idempotent replay of the same create
			// (same CID; equal revs are already rejected by the gate, and empty-rev
			// synthetic events land here too). But the gate can also admit a create
			// with a STRICTLY NEWER rev for an rkey whose row is still active:
			// create A applied → delete dead-lettered (failed, never applied) →
			// genuine re-create B of the same rkey. B carries new content and a new
			// CID; treating it as a duplicate would drop that content forever (the
			// gate has advanced to B's rev, so no replay can fix it). When the gate
			// won with a real rev, the incoming CID differs, and the threading refs
			// are UNCHANGED, apply B's record content in place — without touching
			// deletion metadata, reply counts, or native votes.
			if rev != "" && comment.CID != existingCID &&
				existingParentURI == comment.ParentURI && existingRootURI == comment.RootURI {
				log.Printf("Re-create of active comment with newer rev: %s (applying new content, CID %s -> %s)",
					comment.URI, existingCID, comment.CID)
				recreateQuery := `
					UPDATE comments
					SET
						cid = $1,
						root_cid = $2,
						parent_cid = $3,
						content = $4,
						content_facets = $5,
						embed = $6,
						content_labels = $7,
						langs = $8,
						created_at = $9,
						indexed_at = $10,
						bridged_upvote_count = $11,
						bridged_downvote_count = $12,
						bridged_stats_as_of = $13,
						-- Recompute the inclusive score from the SURVIVING native
						-- counts plus the incoming bridged values (see the resurrect
						-- path below for the migration 031 invariant).
						score = upvote_count + $11 - downvote_count - $12
					WHERE id = $14
				`
				if _, err = tx.ExecContext(ctx, recreateQuery,
					comment.CID, comment.RootCID, comment.ParentCID,
					comment.Content, comment.ContentFacets, comment.Embed, comment.ContentLabels,
					pq.Array(comment.Langs), comment.CreatedAt, comment.IndexedAt,
					comment.BridgedUpvoteCount, comment.BridgedDownvoteCount, comment.BridgedStatsAsOf,
					existingID,
				); err != nil {
					return fmt.Errorf("failed to apply re-created comment content: %w", err)
				}
				// Parent unchanged and the row was never decounted, so parent counts
				// are already correct — commit without the increment sections below.
				if commitErr := tx.Commit(); commitErr != nil {
					return fmt.Errorf("failed to commit transaction: %w", commitErr)
				}
				return nil
			}
			// KNOWN LIMITATION (accepted): the same dead-lettered-delete +
			// same-rkey-re-create sequence with a CHANGED parent/root lands here
			// and is skipped as a duplicate, keeping the OLD row. Applying it
			// would require decrementing the old parent's reply/comment counts
			// and incrementing the new ones for a row that was never decounted —
			// re-plumbing the count machinery for a case that additionally needs
			// a dead-lettered delete AND a cross-thread rkey reuse inside the
			// redrive window. Documented rather than fixed.
			log.Printf("Comment already indexed: %s (idempotent replay)", comment.URI)
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("failed to commit transaction: %w", commitErr)
			}
			return nil
		}

		// Comment was soft-deleted, now being recreated (resurrection)
		// This is a NEW record with same rkey - update ALL fields including threading refs
		// User may have deleted old comment and created a new one on a different parent/root
		// Clear deletion metadata to restore the comment
		log.Printf("Resurrecting previously deleted comment: %s", comment.URI)
		commentID = existingID

		// Check if parent is the same - if so, we should NOT increment parent counts
		// because deleteComment() no longer decrements counts (deleted = placeholder)
		// If parent is different, we need to increment the NEW parent's count
		isResurrectionWithSameParent = (existingParentURI == comment.ParentURI && existingRootURI == comment.RootURI)

		resurrectQuery := `
			UPDATE comments
			SET
				cid = $1,
				commenter_did = $2,
				root_uri = $3,
				root_cid = $4,
				parent_uri = $5,
				parent_cid = $6,
				content = $7,
				content_facets = $8,
				embed = $9,
				content_labels = $10,
				langs = $11,
				created_at = $12,
				indexed_at = $13,
				deleted_at = NULL,
				deletion_reason = NULL,
				deleted_by = NULL,
				reply_count = 0,
				bridged_upvote_count = $14,
				bridged_downvote_count = $15,
				bridged_stats_as_of = $16,
				-- Recompute the inclusive score in SQL from the SURVIVING native counts
				-- (upvote_count/downvote_count are intentionally not reset on resurrect)
				-- plus the incoming bridged values, upholding migration 031's invariant
				-- score = (up+bUp) - (down+bDown). Using comment.Score here would have
				-- written only the bridged delta and dropped the retained native votes.
				score = upvote_count + $14 - downvote_count - $15
			WHERE id = $17
		`

		_, err = tx.ExecContext(
			ctx, resurrectQuery,
			comment.CID,
			comment.CommenterDID,
			comment.RootURI,
			comment.RootCID,
			comment.ParentURI,
			comment.ParentCID,
			comment.Content,
			comment.ContentFacets,
			comment.Embed,
			comment.ContentLabels,
			pq.Array(comment.Langs),
			comment.CreatedAt,
			comment.IndexedAt,
			comment.BridgedUpvoteCount,
			comment.BridgedDownvoteCount,
			comment.BridgedStatsAsOf,
			commentID,
		)
		if err != nil {
			return fmt.Errorf("failed to resurrect comment: %w", err)
		}

	} else if checkErr == sql.ErrNoRows {
		// Comment doesn't exist - insert new comment
		// Use ON CONFLICT DO NOTHING to handle race conditions gracefully
		// (e.g., duplicate Jetstream events from reconnections/retries)
		insertQuery := `
			INSERT INTO comments (
				uri, cid, rkey, commenter_did,
				root_uri, root_cid, parent_uri, parent_cid,
				content, content_facets, embed, content_labels, langs,
				created_at, indexed_at,
				bridged_upvote_count, bridged_downvote_count, bridged_stats_as_of, score
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, $7, $8,
				$9, $10, $11, $12, $13,
				$14, $15,
				$16, $17, $18, $19
			)
			ON CONFLICT (uri) DO NOTHING
			RETURNING id
		`

		err = tx.QueryRowContext(
			ctx, insertQuery,
			comment.URI, comment.CID, comment.RKey, comment.CommenterDID,
			comment.RootURI, comment.RootCID, comment.ParentURI, comment.ParentCID,
			comment.Content, comment.ContentFacets, comment.Embed, comment.ContentLabels, pq.Array(comment.Langs),
			comment.CreatedAt, comment.IndexedAt,
			comment.BridgedUpvoteCount, comment.BridgedDownvoteCount, comment.BridgedStatsAsOf, comment.Score,
		).Scan(&commentID)
		if err == sql.ErrNoRows {
			// ON CONFLICT triggered - comment was inserted by concurrent process
			// This is an idempotent replay, skip gracefully
			log.Printf("Comment already indexed (concurrent insert): %s", comment.URI)
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("failed to commit transaction: %w", commitErr)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to insert comment: %w", err)
		}

	} else {
		// Unexpected error checking for existing comment
		return fmt.Errorf("failed to check for existing comment: %w", checkErr)
	}

	// 1.5. Reconcile reply_count for this newly inserted comment
	// In case any replies arrived out-of-order before this parent was indexed
	// NOTE: Counts include deleted comments since they're shown as "[deleted]" placeholders
	//
	// IMPORTANT: This reconciliation logic and the increment logic below (in parent count updates)
	// must stay in sync. Both use the same counting semantics:
	// - Count ALL comments (including deleted) since deleted comments appear as "[deleted]" placeholders
	// - This ensures reply_count matches the actual visible thread structure
	// If you modify one, you must review and potentially modify the other.
	reconcileQuery := `
		UPDATE comments
		SET reply_count = (
			SELECT COUNT(*)
			FROM comments c
			WHERE c.parent_uri = $1
		)
		WHERE id = $2
	`
	_, reconcileErr := tx.ExecContext(ctx, reconcileQuery, comment.URI, commentID)
	if reconcileErr != nil {
		// Reconciliation failure is a critical error - it means reply_count will be incorrect
		// This could cause data inconsistency where the displayed count doesn't match reality
		// Roll back the transaction to maintain consistency
		return fmt.Errorf("failed to reconcile reply_count for %s: %w", comment.URI, reconcileErr)
	}

	// 2. Update parent counts atomically
	// Parent could be a post (increment comment_count) or a comment (increment reply_count)
	// Parse collection from parent URI to determine target table
	//
	// SKIP if this is a resurrection with the same parent:
	// Since deleteComment() no longer decrements counts (deleted comments shown as "[deleted]" placeholders),
	// resurrecting a comment with the same parent should NOT increment the count again.
	// However, if the parent CHANGED (user recreated comment on different post/thread), we DO increment.
	//
	// NOTE: Post comment_count reconciliation IS implemented in PostEventConsumer.createPostAndUpdateCounts()
	// When a comment arrives before its parent post, the post update below returns 0 rows
	// and we log a warning. Later, when the post is indexed, the post consumer reconciles
	// comment_count by counting all pre-existing comments. This ensures accurate counts
	// despite out-of-order Jetstream event delivery.
	//
	// Test coverage: TestPostConsumer_CommentCountReconciliation in post_consumer_test.go
	if isResurrectionWithSameParent {
		log.Printf("Resurrection with same parent - skipping parent count increment for: %s", comment.URI)
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}
		return nil
	}

	collection := utils.ExtractCollectionFromURI(comment.ParentURI)

	switch collection {
	case "social.coves.community.post":
		// Top-level comment on post - increment posts.comment_count
		// NOTE: No deleted_at filter - we increment even for deleted parents to match reconciliation behavior
		updateQuery := `
			UPDATE posts
			SET comment_count = comment_count + 1
			WHERE uri = $1
		`
		result, err := tx.ExecContext(ctx, updateQuery, comment.ParentURI)
		if err != nil {
			return fmt.Errorf("failed to update post comment_count: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to check update result: %w", err)
		}
		if rowsAffected == 0 {
			log.Printf("Warning: Post not found: %s (comment indexed anyway)", comment.ParentURI)
		}

	case "social.coves.community.comment":
		// Nested reply to comment - update BOTH:
		// 1. Parent comment's reply_count (for thread structure)
		// 2. Root post's comment_count (for total thread count display)
		// NOTE: No deleted_at filter - we increment even for deleted parents to match reconciliation behavior

		// Update parent comment's reply_count
		replyQuery := `
			UPDATE comments
			SET reply_count = reply_count + 1
			WHERE uri = $1
		`
		result, err := tx.ExecContext(ctx, replyQuery, comment.ParentURI)
		if err != nil {
			return fmt.Errorf("failed to update parent reply_count: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to check reply update result: %w", err)
		}
		if rowsAffected == 0 {
			log.Printf("Warning: Parent comment not found: %s (comment indexed anyway)", comment.ParentURI)
		}

		// Also increment root post's comment_count for total thread count
		postQuery := `
			UPDATE posts
			SET comment_count = comment_count + 1
			WHERE uri = $1
		`
		result, err = tx.ExecContext(ctx, postQuery, comment.RootURI)
		if err != nil {
			return fmt.Errorf("failed to update root post comment_count: %w", err)
		}
		rowsAffected, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to check post update result: %w", err)
		}
		if rowsAffected == 0 {
			log.Printf("Warning: Root post not found: %s (comment indexed anyway)", comment.RootURI)
		}

	default:
		// Unknown or unsupported parent collection
		// Comment is still indexed, we just don't update parent counts
		log.Printf("Comment parent has unsupported collection: %s (comment indexed, parent count not updated)", collection)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// validateCommentEvent performs security validation on comment events
func (c *CommentEventConsumer) validateCommentEvent(ctx context.Context, repoDID string, comment *CommentRecordFromJetstream) error {
	// SECURITY: Comments MUST come from user repositories (repo owner = commenter DID)
	// The repository owner (repoDID) IS the commenter - comments are stored in user repos.
	//
	// We do NOT check if the user exists in AppView because:
	// 1. Comment events may arrive before user events in Jetstream (race condition)
	// 2. The comment came from the user's PDS repository (authenticated by PDS)
	// 3. The database FK constraint was removed to allow out-of-order indexing
	// 4. Orphaned comments (from never-indexed users) are harmless
	//
	// Security is maintained because:
	// - Comment must come from user's own PDS repository (verified by atProto)
	// - Fake DIDs will fail PDS authentication

	// All rejections below are PERMANENT (ErrPermanentEvent): they depend only on
	// the immutable event payload, so retries and redrives would fail identically.

	// Validate DID format (basic sanity check)
	if !strings.HasPrefix(repoDID, "did:") {
		return fmt.Errorf("%w: invalid commenter DID format: %s", ErrPermanentEvent, repoDID)
	}

	// Validate content is not empty (required per lexicon)
	if comment.Content == "" {
		return fmt.Errorf("%w: comment content is required", ErrPermanentEvent)
	}

	// Validate content length (defensive check - PDS should enforce this)
	// Per lexicon: max 3000 graphemes, ~30000 bytes
	// We check bytes as a simple defensive measure
	if len(comment.Content) > MaxCommentContentBytes {
		return fmt.Errorf("%w: comment content exceeds maximum length (%d bytes): got %d bytes", ErrPermanentEvent, MaxCommentContentBytes, len(comment.Content))
	}

	// Validate reply references exist
	if comment.Reply.Root.URI == "" || comment.Reply.Root.CID == "" {
		return fmt.Errorf("%w: invalid root reference: must have both URI and CID", ErrPermanentEvent)
	}

	if comment.Reply.Parent.URI == "" || comment.Reply.Parent.CID == "" {
		return fmt.Errorf("%w: invalid parent reference: must have both URI and CID", ErrPermanentEvent)
	}

	// Validate AT-URI structure for root and parent
	if err := validateATURI(comment.Reply.Root.URI); err != nil {
		return fmt.Errorf("%w: invalid root URI: %v", ErrPermanentEvent, err)
	}

	if err := validateATURI(comment.Reply.Parent.URI); err != nil {
		return fmt.Errorf("%w: invalid parent URI: %v", ErrPermanentEvent, err)
	}

	return nil
}

// validateATURI performs basic structure validation on AT-URIs
// Format: at://did:method:id/collection/rkey
// This is defensive validation - we trust PDS but catch obviously malformed URIs
func validateATURI(uri string) error {
	if !strings.HasPrefix(uri, ATProtoScheme) {
		return fmt.Errorf("must start with %s", ATProtoScheme)
	}

	// Remove at:// prefix and split by /
	withoutScheme := strings.TrimPrefix(uri, ATProtoScheme)
	parts := strings.Split(withoutScheme, "/")

	// Must have at least 3 parts: did, collection, rkey
	if len(parts) < 3 {
		return fmt.Errorf("invalid structure (expected at://did/collection/rkey)")
	}

	// First part should be a DID
	if !strings.HasPrefix(parts[0], "did:") {
		return fmt.Errorf("repository identifier must be a DID")
	}

	// Collection and rkey should not be empty
	if parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("collection and rkey cannot be empty")
	}

	return nil
}

// CommentRecordFromJetstream represents a comment record as received from Jetstream
// Matches social.coves.community.comment lexicon
type CommentRecordFromJetstream struct {
	Labels       interface{}                `json:"labels,omitempty"`
	Embed        map[string]interface{}     `json:"embed,omitempty"`
	BridgedStats *BridgedStatsFromJetstream `json:"bridgedStats,omitempty"`
	Reply        ReplyRefFromJetstream      `json:"reply"`
	Type         string                     `json:"$type"`
	Content      string                     `json:"content"`
	CreatedAt    string                     `json:"createdAt"`
	Facets       []interface{}              `json:"facets,omitempty"`
	Langs        []string                   `json:"langs,omitempty"`
}

// ReplyRefFromJetstream represents the threading structure
type ReplyRefFromJetstream struct {
	Root   StrongRefFromJetstream `json:"root"`
	Parent StrongRefFromJetstream `json:"parent"`
}

// parseCommentRecord parses a comment record from Jetstream event data
func parseCommentRecord(record map[string]interface{}) (*CommentRecordFromJetstream, error) {
	// Marshal to JSON and back for proper type conversion
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var comment CommentRecordFromJetstream
	if err := json.Unmarshal(recordJSON, &comment); err != nil {
		// PERMANENT: the record's shape doesn't match the lexicon; replaying the
		// identical bytes can never parse differently.
		return nil, fmt.Errorf("%w: failed to unmarshal comment record: %v", ErrPermanentEvent, err)
	}

	// Validate required fields. PERMANENT: structurally invalid forever.
	if comment.Content == "" {
		return nil, fmt.Errorf("%w: comment record missing content field", ErrPermanentEvent)
	}

	if comment.CreatedAt == "" {
		return nil, fmt.Errorf("%w: comment record missing createdAt field", ErrPermanentEvent)
	}

	return &comment, nil
}

// serializeOptionalFields serializes facets, embed, and labels from a comment record to JSON strings
// Returns nil pointers for empty/nil fields (DRY helper to avoid duplication)
// Returns an error if any non-empty field fails to serialize (prevents silent data loss)
func serializeOptionalFields(commentRecord *CommentRecordFromJetstream) (facetsJSON, embedJSON, labelsJSON *string, err error) {
	// Serialize facets if present
	if len(commentRecord.Facets) > 0 {
		facetsBytes, marshalErr := json.Marshal(commentRecord.Facets)
		if marshalErr != nil {
			return nil, nil, nil, fmt.Errorf("failed to serialize facets: %w", marshalErr)
		}
		facetsStr := string(facetsBytes)
		facetsJSON = &facetsStr
	}

	// Serialize embed if present
	if len(commentRecord.Embed) > 0 {
		embedBytes, marshalErr := json.Marshal(commentRecord.Embed)
		if marshalErr != nil {
			return nil, nil, nil, fmt.Errorf("failed to serialize embed: %w", marshalErr)
		}
		embedStr := string(embedBytes)
		embedJSON = &embedStr
	}

	// Serialize labels if present
	if commentRecord.Labels != nil {
		labelsBytes, marshalErr := json.Marshal(commentRecord.Labels)
		if marshalErr != nil {
			return nil, nil, nil, fmt.Errorf("failed to serialize labels: %w", marshalErr)
		}
		labelsStr := string(labelsBytes)
		labelsJSON = &labelsStr
	}

	return facetsJSON, embedJSON, labelsJSON, nil
}
