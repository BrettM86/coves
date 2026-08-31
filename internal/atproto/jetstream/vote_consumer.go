package jetstream

import (
	"Coves/internal/atproto/utils"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// A vote names its subject by AT-URI and nothing else, so the collection segment
// of that URI is the ONLY thing that says which table the count belongs in. Both
// post collections route to `posts` — posts.IsPostCollection is the shared
// predicate — because both kinds of row live in the same table. New posts use
// social.coves.community.postv2; legacy ingestion has stopped, but pre-flip rows
// persist until §11's drain. A router that knows only one collection stops
// counting the other SILENTLY: the vote row is still indexed and no error is
// raised, the score just never moves.

// Every count mutation here matches its subject by URI ALONE. None of them
// carries `deleted_at IS NULL` on the SUBJECT row, and that is the invariant:
// a LIVE vote row is included in its subject's stored counts unconditionally,
// not merely while the subject is visible. A deletion is a pause rather than an
// end — a comment comes back through indexCommentAndUpdateCounts's re-create
// path with its native counts intact — so a mutation suppressed during a deleted
// window leaves the resurrected subject claiming a vote no live row explains,
// permanently and with nothing left to attribute it to.
//
// Only the withdrawal path can meet a deleted subject today; the ordering gate
// drops creates before any count runs. The filter is absent at all three mutating
// sites (twelve statements) anyway, so the invariant reads the same wherever a
// reader lands.

// VoteEventConsumer consumes vote-related events from Jetstream
// Handles CREATE and DELETE operations for social.coves.feed.vote
type VoteEventConsumer struct {
	voteRepo    votes.Repository
	userService users.UserService
	db          *sql.DB // Direct DB access for atomic vote count updates
	// deletedAccounts gates votes whose SUBJECT lives in an erased account's
	// repo. nil means no gate.
	deletedAccounts DeletedAccountLookup
}

// VoteEventConsumerOption configures optional VoteEventConsumer behaviour.
type VoteEventConsumerOption func(*VoteEventConsumer)

// WithVoteDeletedAccounts installs the erased-account gate for vote subjects.
// Without it no gate runs, and a vote naming an erased account's post is
// treated as an ordinary not-yet-indexed subject.
func WithVoteDeletedAccounts(lookup DeletedAccountLookup) VoteEventConsumerOption {
	return func(c *VoteEventConsumer) { c.deletedAccounts = lookup }
}

// NewVoteEventConsumer creates a new Jetstream consumer for vote events
func NewVoteEventConsumer(
	voteRepo votes.Repository,
	userService users.UserService,
	db *sql.DB,
	opts ...VoteEventConsumerOption,
) *VoteEventConsumer {
	c := &VoteEventConsumer{
		voteRepo:    voteRepo,
		userService: userService,
		db:          db,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RevGated reports whether this consumer applies the per-record rev gate; always true
// for votes (gating is hardwired via c.db). main.go checks this at boot to refuse
// multi-feed operation with an ungated consumer.
func (c *VoteEventConsumer) RevGated() bool { return true }

// ErasureGated reports whether this consumer consults the erasure marker before
// indexing a vote whose subject lives in another repo. Unlike RevGated it is not
// hardwired: the lookup arrives through WithVoteDeletedAccounts, so a wiring
// mistake in cmd/server silently produces a consumer that treats an erased
// account's post as an ordinary late one and indexes votes pointing into content
// somebody asked to have removed. Boot checks this the way they check RevGated,
// which is the only place the mistake is cheap to catch.
func (c *VoteEventConsumer) ErasureGated() bool { return c.deletedAccounts != nil }

// HandleEvent processes a Jetstream event for vote records
func (c *VoteEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for vote records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Handle vote record operations
	if commit.Collection == "social.coves.feed.vote" {
		switch commit.Operation {
		case "create":
			return c.createVote(ctx, event.Did, commit)
		case "delete":
			return c.deleteVote(ctx, event.Did, commit)
		}
	}

	// Silently ignore other operations and collections
	return nil
}

// createVote indexes a new vote from the firehose and updates post counts
func (c *VoteEventConsumer) createVote(ctx context.Context, repoDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: vote create event missing record data", ErrPermanentEvent)
	}

	// Parse the vote record
	voteRecord, err := parseVoteRecord(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse vote record: %w", err)
	}

	// SECURITY: Validate this is a legitimate vote event
	if err := c.validateVoteEvent(ctx, repoDID, voteRecord); err != nil {
		log.Printf("🚨 SECURITY: Rejecting vote event: %v", err)
		return err
	}

	// Build AT-URI for this vote
	// Format: at://voter_did/social.coves.feed.vote/rkey
	uri := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", repoDID, commit.RKey)

	// Parse timestamp from record
	createdAt, err := time.Parse(time.RFC3339, voteRecord.CreatedAt)
	if err != nil {
		log.Printf("Warning: Failed to parse createdAt timestamp, using current time: %v", err)
		createdAt = time.Now()
	}

	// Build vote entity
	vote := &votes.Vote{
		URI:        uri,
		CID:        commit.CID,
		RKey:       commit.RKey,
		VoterDID:   repoDID, // Vote comes from user's repository
		SubjectURI: voteRecord.Subject.URI,
		SubjectCID: voteRecord.Subject.CID,
		Direction:  voteRecord.Direction,
		CreatedAt:  createdAt,
		IndexedAt:  time.Now(),
	}

	// Atomically: Rev-gate + Index vote + Update post counts
	wasNew, err := c.indexVoteAndUpdateCounts(ctx, vote, commit.Rev)
	if err != nil {
		return fmt.Errorf("failed to index vote and update counts: %w", err)
	}

	if wasNew {
		log.Printf("✓ Indexed vote: %s (%s on %s)", uri, vote.Direction, vote.SubjectURI)
	}
	return nil
}

// deleteVote soft-deletes a vote and updates post counts.
//
// The rev-gate claim runs FIRST, inside the same transaction as the load and
// the soft delete (mirrors deletePost). Claiming before reading closes the
// not-found tombstone race: a concurrent create of the same vote (another
// feed's copy, or a DeadLetterRedriver replay) serializes on the gate row
// lock, so it either commits before our read (we see the row and delete it)
// or blocks until our tombstone commits (its equal-or-older rev then loses
// the gate). The gate row is advanced — and committed — even when the vote
// was never indexed, so the create's late copy is rejected too.
func (c *VoteEventConsumer) deleteVote(ctx context.Context, repoDID string, commit *CommitEvent) error {
	// Build AT-URI for the vote being deleted
	uri := fmt.Sprintf("at://%s/social.coves.feed.vote/%s", repoDID, commit.RKey)

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	// 0. REV GATE (see indexVoteAndUpdateCounts): skip duplicate replays and
	// stale cross-feed copies; the claimed row doubles as the tombstone that
	// rejects the create's later copies.
	won, err := tryAdvanceRecordRev(ctx, tx, uri, commit.Rev)
	if err != nil {
		return err
	}
	if !won {
		logSkippedStaleRev(ConsumerVotes, "delete", uri, commit.Rev)
		return nil
	}

	// 1. Load the vote INSIDE the gate transaction: direction and subject
	// drive the count decrement below, and reading under the gate claim means
	// no concurrent create for this URI can commit between this read and our
	// tombstone commit.
	var direction, subjectURI string
	var deletedAt *time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT direction, subject_uri, deleted_at FROM votes WHERE uri = $1`, uri,
	).Scan(&direction, &subjectURI, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Idempotent: vote never indexed. Commit the gate advance anyway — it
		// is the tombstone that rejects a stale cross-feed copy of the CREATE
		// arriving later for a record that no longer exists on the PDS.
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		log.Printf("Vote already deleted or not found: %s", uri)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get existing vote: %w", err)
	}
	if deletedAt != nil {
		// Idempotent: already soft-deleted. Still commit the tombstone advance.
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		log.Printf("Vote already deleted: %s (idempotent)", uri)
		return nil
	}

	// 2. Soft-delete the vote
	deleteQuery := `
		UPDATE votes
		SET deleted_at = NOW()
		WHERE uri = $1 AND deleted_at IS NULL
	`
	result, err := tx.ExecContext(ctx, deleteQuery, uri)
	if err != nil {
		return fmt.Errorf("failed to delete vote: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check delete result: %w", err)
	}
	// Defensive: createVote's stale-vote cleanup (different URI, so a different
	// gate row) can soft-delete this row concurrently. Zero rows then means the
	// vote is already deleted and its count already decremented — commit the
	// tombstone and skip our decrement.
	if rowsAffected == 0 {
		log.Printf("Vote already deleted: %s (idempotent)", uri)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	// 3. Decrement vote counts on the subject (post or comment)
	// Parse collection from subject URI to determine target table
	collection := utils.ExtractCollectionFromURI(subjectURI)

	var updateQuery string
	switch {
	case posts.IsPostCollection(collection):
		// Vote on post - update posts table
		if direction == "up" {
			updateQuery = `
				UPDATE posts
				SET upvote_count = GREATEST(0, upvote_count - 1),
				    score = GREATEST(0, upvote_count - 1) - downvote_count + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		} else { // "down"
			updateQuery = `
				UPDATE posts
				SET downvote_count = GREATEST(0, downvote_count - 1),
				    score = upvote_count - GREATEST(0, downvote_count - 1) + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		}

	case collection == CommentCollection:
		// Vote on comment - update comments table
		if direction == "up" {
			updateQuery = `
				UPDATE comments
				SET upvote_count = GREATEST(0, upvote_count - 1),
				    score = GREATEST(0, upvote_count - 1) - downvote_count + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		} else { // "down"
			updateQuery = `
				UPDATE comments
				SET downvote_count = GREATEST(0, downvote_count - 1),
				    score = upvote_count - GREATEST(0, downvote_count - 1) + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		}

	default:
		// Unknown or unsupported collection
		// Vote is still deleted, we just don't update denormalized counts
		log.Printf("Vote subject has unsupported collection: %s (vote deleted, counts not updated)", collection)
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return nil
	}

	result, err = tx.ExecContext(ctx, updateQuery, subjectURI)
	if err != nil {
		return fmt.Errorf("failed to update vote counts: %w", err)
	}

	rowsAffected, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check update result: %w", err)
	}

	// The decrement no longer filters the subject on deleted_at, so zero rows
	// means the subject row is GONE — hard-deleted, which in practice means an
	// account erasure swept it. There is nothing left to decrement and nothing
	// that can come back to claim this vote; the withdrawal stands on its own.
	if rowsAffected == 0 {
		log.Printf("Warning: Vote subject no longer exists: %s (vote deleted anyway)", subjectURI)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("✓ Deleted vote: %s (%s on %s)", uri, direction, subjectURI)
	return nil
}

// indexVoteAndUpdateCounts atomically indexes a vote and updates post vote counts
// Returns (true, nil) if vote was newly inserted, (false, nil) if already existed (idempotent)
func (c *VoteEventConsumer) indexVoteAndUpdateCounts(ctx context.Context, vote *votes.Vote, rev string) (bool, error) {
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
	// last applied event for this record. Rejects duplicate replays (equal rev)
	// and — critically — a stale cross-feed copy of a CREATE arriving after this
	// vote's DELETE was already applied, which would otherwise restore a phantom
	// vote and re-increment counts. Runs first, inside the transaction, so the
	// gate and the writes commit or roll back together.
	won, err := tryAdvanceRecordRev(ctx, tx, vote.URI, rev)
	if err != nil {
		return false, err
	}
	if !won {
		logSkippedStaleRev(ConsumerVotes, "create", vote.URI, rev)
		return false, nil
	}

	// 1. ORDERING GATE: a vote is only indexed onto a subject that is present
	// and live. The subject's deleted_at is read WITHOUT filtering on it because
	// the three states need three different answers:
	//
	//   - NO ROW: refuse transiently. A vote and its subject always live in
	//     different repos and Jetstream parallelises across repos, so a vote that
	//     arrives first is ordinary delivery, not a malformed event. Deliberately
	//     NOT ErrPermanentEvent: this is an ORDERING failure — the redrive
	//     succeeds once the subject arrives, the shape post_consumer.go's
	//     "community not found" and createSubscription already use. Refusing
	//     before the stale-vote cleanup below, and rolling the whole transaction
	//     back, is what leaves the voter's rows untouched and the rev gate
	//     unadvanced so that replay can win. An ERASED subject is absent for good
	//     and splits off inside that branch.
	//
	//   - DELETED: drop the vote and commit. A deleted subject will not un-delete
	//     because a redriver asked again, so refusing costs the redrive budget and
	//     a dead letter per vote for nothing. Dropping rather than indexing
	//     without counts is what makes this safe under RESURRECTION: comments come
	//     back with their native counts intact (see indexCommentAndUpdateCounts's
	//     re-create path), and an uncounted vote row left live here would have
	//     deleteVote decrement the revived subject for an increment never applied.
	//     No row, nothing to withdraw. The commit keeps the rev advance as a
	//     tombstone — the drop is a decision, not a deferral — so duplicate
	//     deliveries are answered by the gate above (mirrors deleteVote's
	//     not-found branch).
	//
	//   - LIVE: proceed.
	//
	// Both drop branches commit BEFORE the stale-vote cleanup below, deliberately.
	// A drop decides THIS event and nothing else; a stale live vote by the same
	// voter on the same subject only matters when a countable vote next arrives
	// for it, and that arrival runs the cleanup itself.
	var subjectStateQuery string
	switch collection := utils.ExtractCollectionFromURI(vote.SubjectURI); {
	case posts.IsPostCollection(collection):
		subjectStateQuery = `SELECT deleted_at FROM posts WHERE uri = $1`
	case collection == CommentCollection:
		subjectStateQuery = `SELECT deleted_at FROM comments WHERE uri = $1`
	}
	// Unsupported collections have no table to count on in the first place; they
	// keep indexing without counts (see the default branch of the count switch).
	if subjectStateQuery != "" {
		var subjectDeletedAt sql.NullTime
		err := tx.QueryRowContext(ctx, subjectStateQuery, vote.SubjectURI).Scan(&subjectDeletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			// ERASURE is the third reason a subject can be missing, and it is the
			// one "no row" cannot distinguish on its own: account erasure (the
			// migration-036 marker flow; the deletes live in user_repo.DeleteUser)
			// hard-deletes the account's posts and comments, leaving exactly what a
			// subject still in flight leaves. The difference is that this one is
			// never arriving — the post consumer's own gate refuses to re-index it —
			// so it takes the deleted-subject drop's shape, tombstone and all,
			// rather than a refusal that spends the entire redrive budget before
			// dead-lettering forever.
			//
			// Asked only when the row is absent: a subject that exists makes erasure
			// moot, so the lookup stays off the path every ordinary vote takes.
			erased, erasureErr := c.subjectWasErased(ctx, vote.SubjectURI)
			if erasureErr != nil {
				// Fail closed, and transiently: "the marker could not be read" is
				// never "not erased", and the rollback lets the redrive ask again
				// once the table is reachable.
				return false, erasureErr
			}
			if erased {
				if commitErr := tx.Commit(); commitErr != nil {
					return false, fmt.Errorf("failed to commit transaction: %w", commitErr)
				}
				log.Printf("Dropped vote on erased account's subject: %s (%s on %s)", vote.URI, vote.Direction, vote.SubjectURI)
				return false, nil
			}
			// UNRESOLVED REFERENCE, not a plain transient error: the subject
			// lives in another repo, so no in-line retry on this lane can bring
			// it, and a voter who names a subject that does not exist must not
			// be able to buy a 4.2s stall per vote. The redriver converges the
			// genuine early-arrival case off the lane.
			return false, fmt.Errorf("%w: vote subject not indexed: %s - cannot count vote before its subject",
				ErrUnresolvedReference, vote.SubjectURI)
		}
		if err != nil {
			return false, fmt.Errorf("failed to verify vote subject exists: %w", err)
		}
		if subjectDeletedAt.Valid {
			if commitErr := tx.Commit(); commitErr != nil {
				return false, fmt.Errorf("failed to commit transaction: %w", commitErr)
			}
			log.Printf("Dropped vote on deleted subject: %s (%s on %s)", vote.URI, vote.Direction, vote.SubjectURI)
			return false, nil
		}
	}

	// 2. Check for existing active vote with different URI (stale record)
	// This handles cases where:
	// - User voted on another client and we missed the delete event
	// - Vote was reindexed but user created a new vote with different rkey
	// - Any other state mismatch between PDS and AppView
	var existingDirection sql.NullString
	checkQuery := `
		SELECT direction FROM votes
		WHERE voter_did = $1
		  AND subject_uri = $2
		  AND deleted_at IS NULL
		  AND uri != $3
		LIMIT 1
	`
	if err := tx.QueryRowContext(ctx, checkQuery, vote.VoterDID, vote.SubjectURI, vote.URI).Scan(&existingDirection); err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("failed to check existing vote: %w", err)
	}

	// If there's a stale vote, soft-delete it and adjust counts
	if existingDirection.Valid {
		softDeleteQuery := `
			UPDATE votes
			SET deleted_at = NOW()
			WHERE voter_did = $1
			  AND subject_uri = $2
			  AND deleted_at IS NULL
			  AND uri != $3
		`
		if _, err := tx.ExecContext(ctx, softDeleteQuery, vote.VoterDID, vote.SubjectURI, vote.URI); err != nil {
			return false, fmt.Errorf("failed to soft-delete existing votes: %w", err)
		}

		// Decrement the old vote's count (will be re-incremented below if same direction)
		collection := utils.ExtractCollectionFromURI(vote.SubjectURI)
		var decrementQuery string
		if existingDirection.String == "up" {
			if posts.IsPostCollection(collection) {
				decrementQuery = `UPDATE posts SET upvote_count = GREATEST(0, upvote_count - 1), score = GREATEST(0, upvote_count - 1) - downvote_count + bridged_upvote_count - bridged_downvote_count WHERE uri = $1`
			} else if collection == CommentCollection {
				decrementQuery = `UPDATE comments SET upvote_count = GREATEST(0, upvote_count - 1), score = GREATEST(0, upvote_count - 1) - downvote_count + bridged_upvote_count - bridged_downvote_count WHERE uri = $1`
			}
		} else {
			if posts.IsPostCollection(collection) {
				decrementQuery = `UPDATE posts SET downvote_count = GREATEST(0, downvote_count - 1), score = upvote_count - GREATEST(0, downvote_count - 1) + bridged_upvote_count - bridged_downvote_count WHERE uri = $1`
			} else if collection == CommentCollection {
				decrementQuery = `UPDATE comments SET downvote_count = GREATEST(0, downvote_count - 1), score = upvote_count - GREATEST(0, downvote_count - 1) + bridged_upvote_count - bridged_downvote_count WHERE uri = $1`
			}
		}
		if decrementQuery != "" {
			if _, err := tx.ExecContext(ctx, decrementQuery, vote.SubjectURI); err != nil {
				return false, fmt.Errorf("failed to decrement old vote count: %w", err)
			}
		}
		log.Printf("Cleaned up stale vote for %s on %s (was %s)", vote.VoterDID, vote.SubjectURI, existingDirection.String)
	}

	// 3. Index the vote (idempotent with ON CONFLICT DO NOTHING)
	query := `
		INSERT INTO votes (
			uri, cid, rkey, voter_did,
			subject_uri, subject_cid, direction,
			created_at, indexed_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, NOW()
		)
		ON CONFLICT (uri) DO NOTHING
		RETURNING id
	`

	var voteID int64
	err = tx.QueryRowContext(
		ctx, query,
		vote.URI, vote.CID, vote.RKey, vote.VoterDID,
		vote.SubjectURI, vote.SubjectCID, vote.Direction,
		vote.CreatedAt,
	).Scan(&voteID)

	// If no rows returned, vote already exists (idempotent - OK for Jetstream replays)
	if errors.Is(err, sql.ErrNoRows) {
		// KNOWN LIMITATION (accepted): a genuine RE-CREATE of the same rkey while
		// the row is still ACTIVE also lands here and is treated as an idempotent
		// duplicate. Reaching that state requires the exact sequence: create A
		// applied → delete dead-lettered (failed, never applied) → re-create B
		// (same rkey, strictly newer rev, possibly flipped direction) arrives
		// while the row is still active. B's direction is never applied, the gate
		// advances to B's rev, and the redriven delete A is then gate-rejected —
		// the row survives with A's direction. This needs a dead-lettered delete
		// AND an rkey reuse inside the redrive window; rare enough to document
		// rather than plumb a direction-flipping upsert (with paired count
		// adjustments) through the create path. Comments handle their analogous
		// case in place (see indexCommentAndUpdateCounts).
		//
		// Silently handle the common idempotent case - no log needed for replays.
		if commitErr := tx.Commit(); commitErr != nil {
			return false, fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return false, nil // Vote already existed
	}

	if err != nil {
		return false, fmt.Errorf("failed to insert vote: %w", err)
	}

	// 4. Update vote counts on the subject (post or comment)
	// Parse collection from subject URI to determine target table
	collection := utils.ExtractCollectionFromURI(vote.SubjectURI)

	var updateQuery string
	switch {
	case posts.IsPostCollection(collection):
		// Vote on post - update posts table
		if vote.Direction == "up" {
			updateQuery = `
				UPDATE posts
				SET upvote_count = upvote_count + 1,
				    score = upvote_count + 1 - downvote_count + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		} else { // "down"
			updateQuery = `
				UPDATE posts
				SET downvote_count = downvote_count + 1,
				    score = upvote_count - (downvote_count + 1) + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		}

	case collection == CommentCollection:
		// Vote on comment - update comments table
		if vote.Direction == "up" {
			updateQuery = `
				UPDATE comments
				SET upvote_count = upvote_count + 1,
				    score = upvote_count + 1 - downvote_count + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		} else { // "down"
			updateQuery = `
				UPDATE comments
				SET downvote_count = downvote_count + 1,
				    score = upvote_count - (downvote_count + 1) + bridged_upvote_count - bridged_downvote_count
				WHERE uri = $1
			`
		}

	default:
		// Unknown or unsupported collection
		// Vote is still indexed in votes table, we just don't update denormalized counts
		log.Printf("Vote subject has unsupported collection: %s (vote indexed, counts not updated)", collection)
		if commitErr := tx.Commit(); commitErr != nil {
			return false, fmt.Errorf("failed to commit transaction: %w", commitErr)
		}
		return true, nil // Vote was newly indexed
	}

	result, err := tx.ExecContext(ctx, updateQuery, vote.SubjectURI)
	if err != nil {
		return false, fmt.Errorf("failed to update vote counts: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check update result: %w", err)
	}

	// The ordering gate above proved the subject was present and live, this update
	// filters on nothing but its URI, and the gate's read takes no row lock — so
	// zero rows means the subject was hard-deleted between the two, an account
	// erasure landing mid-transaction.
	//
	// Refusing rather than committing an uncounted row hands the decision back to
	// the gate: the rollback undoes the insert AND the rev advance, and the
	// redriven replay finds no subject row at all, so it lands on the erased drop
	// or the absent-subject refusal — whichever is true by then — instead of this
	// path having to guess. Plain error, not ErrPermanentEvent: the redrive is the
	// whole mechanism.
	//
	// Not directly pinned by a test. The race needs a delete to commit inside this
	// transaction's window, which HandleEvent alone cannot arrange without a hook
	// into the transaction; the gate's own tests cover both outcomes the replay
	// can reach.
	if rowsAffected == 0 {
		return false, fmt.Errorf("vote subject %s disappeared while counting the vote: retry once the gate can classify it",
			vote.SubjectURI)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return true, nil // Vote was newly indexed
}

// subjectWasErased reports whether the repo HOSTING this subject carries a
// migration-036 erasure marker.
//
// The DID it asks about comes out of the subject URI's repo segment, which is
// worth saying because every other identity on this path is the voter's: the
// erased party is whoever wrote the post or comment that was voted on, and a
// gate that consulted the voter would answer a different question and let the
// event through.
//
// A lookup failure is an ERROR, never a false — failing open would index votes
// pointing into content a deletion erased. With no lookup wired the gate is
// absent and an erased subject is treated as an ordinary late one.
func (c *VoteEventConsumer) subjectWasErased(ctx context.Context, subjectURI string) (bool, error) {
	if c.deletedAccounts == nil {
		return false, nil
	}
	// A subject that is not an at:// record URI names no repo, so there is no
	// marker to consult; the caller's absent-subject refusal stands.
	subjectRepoDID, _, _, ok := parseRecordURI(subjectURI)
	if !ok {
		return false, nil
	}
	erased, err := c.deletedAccounts.IsAccountDeleted(ctx, subjectRepoDID)
	if err != nil {
		return false, fmt.Errorf("checking the erasure marker for %s: %w", subjectRepoDID, err)
	}
	return erased, nil
}

// validateVoteEvent performs security validation on vote events
func (c *VoteEventConsumer) validateVoteEvent(ctx context.Context, repoDID string, vote *VoteRecordFromJetstream) error {
	// SECURITY: Votes MUST come from user repositories (repo owner = voter DID)
	// The repository owner (repoDID) IS the voter - votes are stored in user repos.
	//
	// We do NOT check if the user exists in AppView because:
	// 1. Vote events may arrive before user events in Jetstream (race condition)
	// 2. The vote came from the user's PDS repository (authenticated by PDS)
	// 3. The database FK constraint was removed to allow out-of-order indexing
	// 4. Orphaned votes (from never-indexed users) are harmless
	//
	// Security is maintained because:
	// - Vote must come from user's own PDS repository (verified by atProto)
	// - Communities cannot create votes in their repos (different collection)
	// - Fake DIDs will fail PDS authentication

	// All rejections below are PERMANENT (ErrPermanentEvent): they depend only on
	// the immutable event payload, so retries and redrives would fail identically.

	// Validate DID format (basic sanity check)
	if !strings.HasPrefix(repoDID, "did:") {
		return fmt.Errorf("%w: invalid voter DID format: %s", ErrPermanentEvent, repoDID)
	}

	// Validate vote direction
	if vote.Direction != "up" && vote.Direction != "down" {
		return fmt.Errorf("%w: invalid vote direction: %s (must be 'up' or 'down')", ErrPermanentEvent, vote.Direction)
	}

	// Validate subject has both URI and CID (strong reference)
	if vote.Subject.URI == "" || vote.Subject.CID == "" {
		return fmt.Errorf("%w: invalid subject: must have both URI and CID (strong reference)", ErrPermanentEvent)
	}

	// The subject URI must be a well-formed record URI, not merely non-empty.
	// Everything downstream reads structure out of it — the collection segment
	// picks the table, the repo segment is the DID the erasure gate asks about —
	// so a URI with its segments in the wrong places routes on a value that was
	// never a collection and looks up a row no record could be keyed on. The
	// ordering gate would then read that miss as "the subject has not arrived
	// yet" and refuse transiently, spending the whole redrive budget on a verdict
	// the payload alone already settles. An unfamiliar collection is NOT rejected
	// here: a bridged app.bsky.feed.post subject parses cleanly and must keep
	// doing so.
	if _, _, _, ok := parseRecordURI(vote.Subject.URI); !ok {
		return fmt.Errorf("%w: invalid subject URI %q: must be at://<repo>/<collection>/<rkey>",
			ErrPermanentEvent, vote.Subject.URI)
	}

	return nil
}

// VoteRecordFromJetstream represents a vote record as received from Jetstream
type VoteRecordFromJetstream struct {
	Subject   StrongRefFromJetstream `json:"subject"`
	Direction string                 `json:"direction"`
	CreatedAt string                 `json:"createdAt"`
}

// StrongRefFromJetstream represents a strong reference (URI + CID)
type StrongRefFromJetstream struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// parseVoteRecord parses a vote record from Jetstream event data
func parseVoteRecord(record map[string]interface{}) (*VoteRecordFromJetstream, error) {
	// Extract subject (strong reference)
	subjectMap, ok := record["subject"].(map[string]interface{})
	if !ok {
		// PERMANENT: structurally invalid record — replays parse identically.
		return nil, fmt.Errorf("%w: missing or invalid subject field", ErrPermanentEvent)
	}

	subjectURI, _ := subjectMap["uri"].(string)
	subjectCID, _ := subjectMap["cid"].(string)

	// Extract direction
	direction, _ := record["direction"].(string)

	// Extract createdAt
	createdAt, _ := record["createdAt"].(string)

	return &VoteRecordFromJetstream{
		Subject: StrongRefFromJetstream{
			URI: subjectURI,
			CID: subjectCID,
		},
		Direction: direction,
		CreatedAt: createdAt,
	}, nil
}
