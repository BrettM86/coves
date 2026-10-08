package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/communities"
	"Coves/internal/core/embeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/core/richtext"
	"Coves/internal/core/users"
)

// PostEventConsumer consumes author-owned posts and community decisions from
// Jetstream. The deprecated community-repo post collection is no longer
// ingested from the firehose; its existing rows stay served, are tombstoned
// directly by the API delete path, and still accumulate vote/comment counters.
type PostEventConsumer struct {
	postRepo      posts.Repository
	communityRepo communities.Repository
	userService   users.UserService
	db            *sql.DB // Direct DB access for atomic count reconciliation

	// nil keeps ingestion independent of moderation media reconciliation.
	mediaReconciler MediaReconciler
	// bridgeTrust gates whether a post's author repo may assert bridgedStats.
	// nil means default-deny (bridgedStats are ignored for every post).
	bridgeTrust *BridgeTrust
	// notifications writes mention notifications for newly indexed posts in
	// the same transaction as the post. nil disables post notifications.
	notifications notifications.Repository
	// identityResolver is used only when relay scheduling delivers a post
	// before its author's profile. The identity is admitted only when its PDS
	// passes bridgeTrust.
	identityResolver identity.Resolver

	// The collaborators author-owned post ingestion needs
	// (docs/PRD_AUTHOR_OWNED_POSTS.md §5.3-§5.6). All four are read by the
	// handlers in authorpost.go, and a nil one disables a capability rather
	// than degrading it — see each field.
	//
	// admissions holds the per-(community, post) decision state. nil means the
	// consumer is running in its pre-034 shape and ignores all three
	// author-owned collections rather than indexing them undecided.
	admissions posts.AdmissionRepository
	// deletedAccounts gates events from erased accounts. nil means no gate.
	deletedAccounts DeletedAccountLookup
	// postFetcher resolves an acceptance whose subject was never indexed. nil
	// means the dead-letter queue is the only convergence mechanism.
	postFetcher PostRecordFetcher
	// acceptanceCleanup withdraws a hosted community's acceptance when the
	// author tombstones the post. nil means no sweep runs.
	acceptanceCleanup AcceptanceDeleter
}

// PostEventConsumerOption configures optional PostEventConsumer behaviour.
type PostEventConsumerOption func(*PostEventConsumer)

// WithPostBridgeTrust installs the provenance gate that decides which author repos
// may assert bridgedStats on their posts. Without it, bridgedStats are default-denied.
func WithPostBridgeTrust(bt *BridgeTrust) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.bridgeTrust = bt }
}

// WithPostIdentityResolver enables safe discovery of bridged authors whose
// profile-repo event has not reached the AppView yet.
func WithPostIdentityResolver(resolver identity.Resolver) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.identityResolver = resolver }
}

// WithPostNotifications makes the consumer write eligible mentions when it
// indexes a new postv2 record.
func WithPostNotifications(repository notifications.Repository) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.notifications = repository }
}

// NotificationsWired reports whether the consumer writes post mentions.
func (c *PostEventConsumer) NotificationsWired() bool { return c.notifications != nil }

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

	switch commit.Collection {
	case posts.LegacyPostCollection:
		uri := fmt.Sprintf("at://%s/%s/%s", event.Did, commit.Collection, commit.RKey)
		log.Printf("dropping retired social.coves.community.post event %s: collection retired from ingestion", uri)
		return nil

	// The author-repo post: event.Did IS the author, and the community is a
	// claim the record makes (authorpost.go).
	case PostV2Collection:
		if !c.canRecordAdmissions(commit.Collection) {
			return nil
		}
		return c.handleAuthorPostEvent(ctx, event, commit)

	// The community's decision records: event.Did IS the community, and the
	// post is a subject the record names (authorpost.go).
	case posts.AcceptanceCollection, posts.RemovalCollection:
		if !c.canRecordAdmissions(commit.Collection) {
			return nil
		}
		return c.handleCommunityDecisionEvent(ctx, event, commit)
	}

	// Silently ignore other operations and other collections
	return nil
}

// eventTime converts a Jetstream time_us wall-clock timestamp to a time.Time.
// ok is false when the event carries no timestamp (TimeUS == 0), in which case
// recency guards are bypassed and updates apply unconditionally.
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

// tombstoneRecordIfRevWins soft-deletes the post at uri under the rev gate, and
// reports whether the deletion APPLIED.
//
// SOFT, never hard: the row is the rev gate's tombstone, the comment thread's
// parent, and what moderation still reads.
//
// When notifications are wired, take the author's erasure lock before touching
// the post row, then record its pre-withdrawal public visibility after the soft
// delete (including a zero-row delete). Notifications remain on author deletion.
// Both actions share the rev-gated transaction.
//
// The applied flag exists for the author-repo path's acceptance sweep, which
// must fire once per deletion rather than once per DELIVERY of it: the
// connector rewinds its cursor after every reconnect, so a tombstone that
// re-swept on each redelivery would put an authenticated PDS round trip behind
// every replayed event.
func (c *PostEventConsumer) tombstoneRecordIfRevWins(ctx context.Context, uri, rev, authorDID string) (bool, error) {
	// REV GATE + soft delete in one transaction (the repo's SoftDelete is not
	// transaction-aware, and the delete's rev must be recorded atomically with
	// the tombstone: it is what rejects a stale cross-feed copy of the CREATE
	// arriving later and resurrecting the post). The gate row is advanced even
	// when the post was never indexed, so the late create of an already-deleted
	// record is rejected too.
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	won, err := tryAdvanceRecordRev(ctx, tx, uri, rev)
	if err != nil {
		return false, err
	}
	if !won {
		logSkippedStaleRev(ConsumerPosts, "delete", uri, rev)
		return false, nil
	}

	if c.notifications != nil {
		if _, err := c.notifications.ErasureGateTx(ctx, tx, authorDID); err != nil {
			return false, fmt.Errorf("check post author erasure before deleting: %w", err)
		}
	}

	// Same statement as postRepo.SoftDelete, inlined for transactionality.
	// Idempotent: zero rows (already deleted or never indexed) is success.
	if _, err := tx.ExecContext(ctx,
		`UPDATE posts SET deleted_at = NOW() WHERE uri = $1 AND deleted_at IS NULL`, uri,
	); err != nil {
		return false, fmt.Errorf("failed to soft delete post: %w", err)
	}

	if c.notifications != nil {
		if err := c.notifications.RecordPostAuthorDeleteWithdrawalTx(ctx, tx, uri); err != nil {
			return false, fmt.Errorf("record post author-delete withdrawal: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit post delete transaction: %w", err)
	}

	log.Printf("✓ Deleted post: %s", uri)
	return true, nil
}

// storedPost is the slice of an indexed post row the write paths need: the
// identity to update, the columns immutability is checked against, and the two
// watermarks (bridged asOf, indexed_at) the guards compare.
type storedPost struct {
	id           int64
	communityDID string
	authorDID    string
	deletedAt    *time.Time
	bridgedAsOf  *time.Time
	indexedAt    time.Time
}

// loadStoredPost reads the row for uri. found=false means the post has never
// been indexed, which is an ordinary out-of-order arrival rather than an error.
func (c *PostEventConsumer) loadStoredPost(ctx context.Context, uri string) (storedPost, bool, error) {
	var stored storedPost
	err := c.db.QueryRowContext(ctx,
		`SELECT id, community_did, author_did, deleted_at, bridged_stats_as_of, indexed_at FROM posts WHERE uri = $1`,
		uri,
	).Scan(&stored.id, &stored.communityDID, &stored.authorDID,
		&stored.deletedAt, &stored.bridgedAsOf, &stored.indexedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPost{}, false, nil
	}
	if err != nil {
		return storedPost{}, false, fmt.Errorf("failed to load stored post %s: %w", uri, err)
	}
	return stored, true, nil
}

// postContentUpdate is the update payload used only by upsertAuthorPost's
// existing-row branch. Acceptance-triggered direct fetches insert missing rows
// through insertAuthorPost instead.
type postContentUpdate struct {
	uri string
	// authorDID owns the incoming blobs, which are blocked when the post is
	// removed even if the update is skipped.
	authorDID string
	storedID  int64
	rev       string
	cid       string

	title   *string
	content *string
	facets  sql.NullString
	embed   sql.NullString
	labels  sql.NullString

	bridgedUpvotes   int
	bridgedDownvotes int
	// bridgedAsOf nil means "leave the stored bridged columns alone".
	bridgedAsOf *time.Time

	storedAsOf      *time.Time
	storedDeletedAt *time.Time
	storedIndexedAt time.Time
	timeUS          int64
}

// applyPostContentUpdate runs the rev gate and the atomic content UPDATE.
//
// It reports whether the write APPLIED. A false with no error is a skip — the
// stored row already holds a newer state — and every skip here is the system
// working: multi-feed duplicates, dead-letter redrives, and edits of posts
// deleted between the load and the write all land in it. Returning any of them
// as an error would dead-letter healthy events.
func (c *PostEventConsumer) applyPostContentUpdate(ctx context.Context, in postContentUpdate) (bool, error) {
	// Skip soft-deleted rows: a deleted post should not be resurrected by an edit.
	// The author's repo still serves the incoming blobs, so a removed post's
	// recreated images are blocked even though the content is not indexed. The
	// rev gate is checked under its lock (read-only: this path never advances
	// the rev) so an out-of-order event that predates the delete blocks nothing.
	// Event time is not compared here (timeUS 0), as this skip never did.
	if in.storedDeletedAt != nil {
		log.Printf("Update event for soft-deleted post: %s (skipping)", in.uri)
		if err := c.blockIncomingMedia(ctx, in.uri, in.authorDID, in.rev, 0, in.embed); err != nil {
			return false, fmt.Errorf("failed to block media of skipped post update: %w", err)
		}
		return false, nil
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
	if evTime, ok := eventTime(in.timeUS); ok && !in.storedIndexedAt.Before(evTime) {
		log.Printf("INFO: skipping stale post update for %s (event time %s <= last indexed %s; newer state already applied)",
			in.uri, evTime.Format(time.RFC3339Nano), in.storedIndexedAt.Format(time.RFC3339Nano))
		return false, nil
	}

	// Best-effort log only (the write is authoritative and atomic): a
	// strictly-older asOf is dropped by the SQL guard. Kept at debug because
	// the bridge re-sends the same asOf on every content edit, so this is
	// noise, not an anomaly.
	if in.bridgedAsOf != nil && in.storedAsOf != nil && in.bridgedAsOf.Before(*in.storedAsOf) {
		log.Printf("debug: ignoring strictly-older bridgedStats for %s (incoming asOf %s < stored %s)",
			in.uri, in.bridgedAsOf.Format(time.RFC3339), in.storedAsOf.Format(time.RFC3339))
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
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	won, err := tryAdvanceRecordRev(ctx, tx, in.uri, in.rev)
	if err != nil {
		return false, err
	}
	if !won {
		logSkippedStaleRev(ConsumerPosts, "update", in.uri, in.rev)
		return false, nil
	}

	var erased bool
	var storedFacets string
	var storedCreatedAt time.Time
	if c.notifications != nil {
		// Take the erasure lock before locking the post row, avoiding a cycle
		// with account deletion that holds the erasure lock first.
		erased, err = c.notifications.ErasureGateTx(ctx, tx, in.authorDID)
		if err != nil {
			return false, fmt.Errorf("check post author erasure before updating: %w", err)
		}
		err = tx.QueryRowContext(ctx,
			`SELECT COALESCE(content_facets::text, ''), created_at
			 FROM posts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, in.storedID,
		).Scan(&storedFacets, &storedCreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("Update event for post that was deleted between load and write: %s (skipping)", in.uri)
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read stored post before update: %w", err)
		}
	}

	result, err := tx.ExecContext(ctx, updateQuery,
		in.storedID, in.cid, in.title, in.content,
		in.facets, in.embed, in.labels,
		in.bridgedUpvotes, in.bridgedDownvotes, in.bridgedAsOf,
		in.timeUS,
	)
	if err != nil {
		return false, fmt.Errorf("failed to update post: %w", err)
	}

	// A post can be soft-deleted — or overtaken by a concurrent NEWER update (recency
	// guard) — between the load above and this UPDATE; the WHERE guards then match no
	// rows. Report that as a skip instead of falsely logging a successful update
	// (mirrors vote_consumer's RowsAffected check). Both cases are success: the row's
	// current state supersedes this event.
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check post update result: %w", err)
	}
	if rowsAffected == 0 {
		// The deferred rollback also reverts the gate advance — conservative: a
		// replay re-evaluates against whatever state superseded this event.
		log.Printf("Update event for post that was deleted or superseded by a newer update between load and write: %s (skipping)", in.uri)
		return false, nil
	}

	if c.notifications != nil && !erased {
		if err := c.writePostEditNotifications(ctx, tx, in, storedFacets, storedCreatedAt); err != nil {
			return false, err
		}
	}

	if err := commitMediaWrite(ctx, tx, in.uri, c.mediaReconciler, "post"); err != nil {
		return false, fmt.Errorf("failed to commit post update transaction: %w", err)
	}

	if in.bridgedAsOf != nil {
		log.Printf("✓ Updated post: %s (bridgedStats candidate applied if newer-or-equal: up=%d down=%d)",
			in.uri, in.bridgedUpvotes, in.bridgedDownvotes)
	} else {
		log.Printf("✓ Updated post: %s", in.uri)
	}
	return true, nil
}

// parseBridgedAsOf parses a bridgedStats.asOf timestamp, logging (and returning the
// error) on failure so callers can decide to skip applying the aggregate.
func parseBridgedAsOf(asOf, uri string) (time.Time, error) {
	// bridgedvotes.ParseAsOf is the shared rule for both ingestion channels: it
	// rejects the zero time and any stamp more than MaxAsOfSkew ahead of this
	// clock, because a far-future asOf would win the >= guard once and then make
	// every later honest aggregate lose it, from either channel, until repaired.
	t, err := bridgedvotes.ParseAsOf(asOf, time.Now())
	if err != nil {
		log.Printf("Warning: rejecting bridgedStats.asOf %q for %s: %v", asOf, uri, err)
		return time.Time{}, err
	}
	return t, nil
}

// indexPostIfRevWins atomically indexes a post and reconciles comment counts.
// This fixes the race condition where comments arrive before their parent post.
// When notifications are wired, the author's erasure gate is checked before
// touching the post row; an erased author's post is indexed without mentions.
//
// It reports whether the insert APPLIED: false means the rev gate refused the
// event, or the row already existed. Callers that must not act on content they
// did not write — the author-repo path, which opens an admission from the CID
// it just indexed — read that flag rather than assuming the write happened.
func (c *PostEventConsumer) indexPostIfRevWins(ctx context.Context, post *posts.Post, rev string) (bool, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
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
		return false, err
	}
	if !won {
		logSkippedStaleRev(ConsumerPosts, "create", post.URI, rev)
		return false, nil
	}

	var erased bool
	if c.notifications != nil {
		erased, err = c.notifications.ErasureGateTx(ctx, tx, post.AuthorDID)
		if err != nil {
			return false, fmt.Errorf("check post author erasure before indexing: %w", err)
		}
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
	if errors.Is(insertErr, sql.ErrNoRows) {
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
		// The dropped content's blobs are still served from the author's repo,
		// so a removed post blocks them from the incoming embed.
		if commitErr := c.commitIncomingMediaWrite(ctx, tx, post.URI, post.AuthorDID, incomingPostBlobCIDs(embedJSON)); commitErr != nil {
			return false, fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		// Reported as NOT applied: no content was written, so a caller that
		// would record what it just indexed has nothing new to record.
		return false, nil
	}

	if insertErr != nil {
		return false, fmt.Errorf("failed to insert post: %w", insertErr)
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
		return false, fmt.Errorf("failed to reconcile comment_count for %s: %w", post.URI, reconcileErr)
	}

	if c.notifications != nil && !erased {
		if err := c.writePostCreateNotifications(ctx, tx, post); err != nil {
			return false, err
		}
	}

	// Commit transaction
	if err := commitMediaWrite(ctx, tx, post.URI, c.mediaReconciler, "post"); err != nil {
		return false, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return true, nil
}

// writePostCreateNotifications writes eligible mentions in the post insert transaction.
func (c *PostEventConsumer) writePostCreateNotifications(ctx context.Context, tx *sql.Tx, post *posts.Post) error {
	var facetsJSON string
	if post.ContentFacets != nil {
		facetsJSON = *post.ContentFacets
	}
	intents, err := notifications.FanoutPostCreate(ctx, c.notifications.LookupsTx(tx), c.bridgeTrust, notifications.PostRecord{
		URI: post.URI, CID: post.CID, AuthorDID: post.AuthorDID,
		CreatedAt: post.CreatedAt, FacetsJSON: facetsJSON,
	})
	if err != nil {
		return fmt.Errorf("compute post notifications: %w", err)
	}
	if err := c.notifications.ApplyTx(ctx, tx, intents); err != nil {
		return fmt.Errorf("write post notifications: %w", err)
	}
	return nil
}

// writePostEditNotifications writes new mentions in the post update transaction.
func (c *PostEventConsumer) writePostEditNotifications(ctx context.Context, tx *sql.Tx, in postContentUpdate, storedFacets string, storedCreatedAt time.Time) error {
	var facetsJSON string
	if in.facets.Valid {
		facetsJSON = in.facets.String
	}
	// Missing time_us (<= 0) passes a zero EditEventTime, which falls back to
	// index time for freshness; indexedAtForEvent substitutes wall clock instead.
	editEventTime, _ := eventTime(in.timeUS)
	intents, err := notifications.FanoutPostEdit(ctx, c.notifications.LookupsTx(tx), c.bridgeTrust, notifications.PostRecord{
		URI: in.uri, CID: in.cid, AuthorDID: in.authorDID,
		CreatedAt: storedCreatedAt, FacetsJSON: facetsJSON, EditEventTime: editEventTime,
	}, storedFacets)
	if err != nil {
		return fmt.Errorf("compute post edit notifications: %w", err)
	}
	if err := c.notifications.ApplyTx(ctx, tx, intents); err != nil {
		return fmt.Errorf("write post edit notifications: %w", err)
	}
	return nil
}

// errValidationInfra marks an ingestion validation failure caused by an infrastructure fault
// (e.g. a DB error while checking that the community or author exists) rather than a
// policy rejection. The two are logged differently: policy rejections are security
// events (🚨), infra faults are plain operational errors that must NOT masquerade as
// an attack in the logs.
var errValidationInfra = errors.New("validation infrastructure error")

// BridgedStatsFromJetstream is the bridge-asserted aggregate of origin-platform
// votes carried on federated/bridged post and comment records (social.coves
// community.postv2 / community.comment #bridgedStats). A nil pointer means the
// record carried no bridgedStats, which callers treat as "leave stored counts
// alone" rather than "reset to zero".
type BridgedStatsFromJetstream struct {
	Upvotes   int    `json:"upvotes"`
	Downvotes int    `json:"downvotes"`
	AsOf      string `json:"asOf"`
}

// sanitizeFacets drops facets whose byte ranges fall outside the post's
// content (or are otherwise structurally invalid) before indexing. Firehose
// records from federated repos cannot be rejected back to their author, and
// clients must never receive ranges that slice outside the content, so invalid
// facets are dropped rather than failing the event. Returns nil when no
// facets survive, preserving the callers' nil-means-absent serialization.
func sanitizeFacets(facets []interface{}, content *string, uri string) []interface{} {
	if facets == nil {
		return nil
	}
	contentByteLen := 0
	if content != nil {
		contentByteLen = len(*content)
	}
	kept, dropped := richtext.SanitizeFacets(facets, contentByteLen)
	if dropped > 0 {
		log.Printf("Warning: dropped %d invalid facet(s) on post %s during indexing", dropped, uri)
	}
	return kept
}

// serializePostContent marshals the three optional JSON columns a post record
// carries. A marshal failure is returned rather than swallowed: silently
// dropping facets, an embed, or labels would index a post that reads as though
// its author never sent them.
func serializePostContent(facets []interface{}, embed map[string]interface{}, labels *posts.SelfLabels) (facetsJSON, embedJSON, labelsJSON sql.NullString, err error) {
	if facets != nil {
		b, marshalErr := json.Marshal(facets)
		if marshalErr != nil {
			return facetsJSON, embedJSON, labelsJSON, fmt.Errorf("failed to serialize facets: %w", marshalErr)
		}
		facetsJSON.String, facetsJSON.Valid = string(b), true
	}
	if embed != nil {
		b, marshalErr := json.Marshal(embed)
		if marshalErr != nil {
			return facetsJSON, embedJSON, labelsJSON, fmt.Errorf("failed to serialize embed: %w", marshalErr)
		}
		embedJSON.String, embedJSON.Valid = string(b), true
	}
	if labels != nil {
		b, marshalErr := json.Marshal(labels)
		if marshalErr != nil {
			return facetsJSON, embedJSON, labelsJSON, fmt.Errorf("failed to serialize labels: %w", marshalErr)
		}
		labelsJSON.String, labelsJSON.Valid = string(b), true
	}
	return facetsJSON, embedJSON, labelsJSON, nil
}

// parseRecordCreatedAt reads an indexed record's author-supplied createdAt,
// falling back to now when it does not parse.
//
// SECURITY: future timestamps are clamped to now. created_at drives the "new"
// sort and the hot-rank age, so a record asserting a future date (hostile or
// clock-skewed federated repo) could otherwise pin itself to the top of feeds
// until wall-clock catches up.
func parseRecordCreatedAt(raw, uri string) time.Time {
	createdAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		log.Printf("Warning: Failed to parse createdAt timestamp for %s, using current time: %v", uri, err)
		return time.Now()
	}
	if now := time.Now(); createdAt.After(now) {
		log.Printf("Warning: record %s has future createdAt %s, clamping to now", uri, raw)
		return now
	}
	return createdAt
}

// WithPostMediaReconciler reconciles media blocks when a removed post is created or edited.
func WithPostMediaReconciler(reconciler MediaReconciler) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.mediaReconciler = reconciler }
}

// incomingPostBlobCIDs returns the proxy-served blob CIDs of a serialized
// incoming embed. A malformed embed has no served blobs to block.
func incomingPostBlobCIDs(embed sql.NullString) []string {
	if !embed.Valid {
		return nil
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(embed.String), &decoded); err != nil {
		return nil
	}
	return embeds.PostBlobCIDs(decoded)
}

// commitIncomingMediaWrite blocks the incoming blobs of a removed post inside
// tx, commits, and purges cached copies only after the blocks commit.
func (c *PostEventConsumer) commitIncomingMediaWrite(ctx context.Context, tx *sql.Tx, uri, ownerDID string, blobCIDs []string) error {
	var blocks []moderation.MediaBlock
	if c.mediaReconciler != nil {
		var err error
		blocks, err = c.mediaReconciler.ReconcileIncomingTx(ctx, tx, uri, ownerDID, blobCIDs)
		if err != nil {
			return fmt.Errorf("reconcile incoming post media: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if c.mediaReconciler != nil {
		c.mediaReconciler.Purge(ctx, blocks)
	}
	return nil
}

// blockIncomingMedia blocks the blobs of an incoming post event that writes no
// post row. It is a no-op unless the post has an active removal, and a no-op
// for an event that is stale by rev, or by event time when timeUS is positive.
func (c *PostEventConsumer) blockIncomingMedia(ctx context.Context, uri, ownerDID, rev string, timeUS int64, embed sql.NullString) error {
	blobCIDs := incomingPostBlobCIDs(embed)
	if c.mediaReconciler == nil || len(blobCIDs) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin media block transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()
	current, err := lockCurrentIncomingEvent(ctx, tx, lockPostRowQuery, ConsumerPosts, uri, rev, timeUS)
	if err != nil || !current {
		return err
	}
	return c.commitIncomingMediaWrite(ctx, tx, uri, ownerDID, blobCIDs)
}

// The content-row locks taken before an incoming-media active-removal read.
// FOR NO KEY UPDATE conflicts with the FOR SHARE a removal takes when it reads
// the indexed subject, as an ordinary update's row write does.
const (
	lockPostRowQuery    = `SELECT indexed_at FROM posts WHERE uri = $1 FOR NO KEY UPDATE`
	lockCommentRowQuery = `SELECT indexed_at FROM comments WHERE uri = $1 FOR NO KEY UPDATE`
)

// lockContentRow locks the indexed row of a record whose incoming media is
// about to be reconciled, so a removal that has already read the subject
// commits first and the active-removal read that follows sees it. found is
// false when the row is gone; no removal can then be holding it.
func lockContentRow(ctx context.Context, tx *sql.Tx, lockQuery, uri string) (indexedAt time.Time, found bool, err error) {
	err = tx.QueryRowContext(ctx, lockQuery, uri).Scan(&indexedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("failed to lock indexed row %s: %w", uri, err)
	}
	return indexedAt, true, nil
}

// lockCurrentIncomingEvent decides inside tx whether an event the consumer
// refuses to index is still the newest for its record, and holds the locks
// that keep that answer and the active-removal read after it true until tx
// ends. It locks in the order every same-record write does: the rev gate row,
// then the content row. A stale event, by rev or by an event time (when timeUS
// is positive) not after the row's indexed_at, returns false. It advances
// nothing, because the refused content is never applied.
func lockCurrentIncomingEvent(ctx context.Context, tx *sql.Tx, lockQuery, consumer, uri, rev string, timeUS int64) (bool, error) {
	stale, err := lockedRecordRevIsStale(ctx, tx, uri, rev)
	if err != nil {
		return false, err
	}
	if stale {
		logSkippedStaleRev(consumer, "update", uri, rev)
		return false, nil
	}
	indexedAt, found, err := lockContentRow(ctx, tx, lockQuery, uri)
	if err != nil {
		return false, err
	}
	if evTime, ok := eventTime(timeUS); ok && found && !indexedAt.Before(evTime) {
		log.Printf("INFO: not blocking media of stale %s event for %s (event time %s <= last indexed %s)",
			consumer, uri, evTime.Format(time.RFC3339Nano), indexedAt.Format(time.RFC3339Nano))
		return false, nil
	}
	return true, nil
}
