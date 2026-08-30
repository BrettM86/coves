package jetstream

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/richtext"
	"Coves/internal/core/users"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
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
	// bridgeTrust gates whether a post's author repo may assert bridgedStats.
	// nil means default-deny (bridgedStats are ignored for every post).
	bridgeTrust *BridgeTrust
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
// The applied flag exists for the author-repo path's acceptance sweep, which
// must fire once per deletion rather than once per DELIVERY of it: the
// connector rewinds its cursor after every reconnect, so a tombstone that
// re-swept on each redelivery would put an authenticated PDS round trip behind
// every replayed event.
func (c *PostEventConsumer) tombstoneRecordIfRevWins(ctx context.Context, uri, rev string) (bool, error) {
	// REV GATE + soft delete in one transaction (the repo's SoftDelete is not
	// transaction-aware, and the delete's rev must be recorded atomically with
	// the tombstone: it is what rejects a stale cross-feed copy of the CREATE
	// arriving later and resurrecting the post). The gate row is advanced even
	// when the post was never indexed, so the late create of an already-deleted
	// record is rejected too.
	tx, err := c.db.BeginTx(ctx, nil)
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

	// Same statement as postRepo.SoftDelete, inlined for transactionality.
	// Idempotent: zero rows (already deleted or never indexed) is success.
	if _, err := tx.ExecContext(ctx,
		`UPDATE posts SET deleted_at = NOW() WHERE uri = $1 AND deleted_at IS NULL`, uri,
	); err != nil {
		return false, fmt.Errorf("failed to soft delete post: %w", err)
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
	uri      string
	storedID int64
	rev      string
	cid      string

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
	if in.storedDeletedAt != nil {
		log.Printf("Update event for soft-deleted post: %s (skipping)", in.uri)
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
	tx, err := c.db.BeginTx(ctx, nil)
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

	if err := tx.Commit(); err != nil {
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
	t, err := time.Parse(time.RFC3339, asOf)
	if err != nil {
		log.Printf("Warning: failed to parse bridgedStats.asOf %q for %s: %v", asOf, uri, err)
		return time.Time{}, err
	}
	return t, nil
}

// indexPostIfRevWins atomically indexes a post and reconciles comment counts.
// This fixes the race condition where comments arrive before their parent post.
//
// It reports whether the insert APPLIED: false means the rev gate refused the
// event, or the row already existed. Callers that must not act on content they
// did not write — the author-repo path, which opens an admission from the CID
// it just indexed — read that flag rather than assuming the write happened.
func (c *PostEventConsumer) indexPostIfRevWins(ctx context.Context, post *posts.Post, rev string) (bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
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
		if commitErr := tx.Commit(); commitErr != nil {
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

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return true, nil
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
