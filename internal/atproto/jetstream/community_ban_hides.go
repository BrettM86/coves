package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/notifications"
)

// Hide reasons recorded on comments and votes at index time (migration 069).
// NULL means not hidden.
const (
	// hideReasonCommunityBan: a ban on the author in the subject's community was
	// in force at the record's Coves index order and index time.
	hideReasonCommunityBan = comments.HideReasonCommunityBan
	// hideReasonPendingBanCheck: the subject's community is not known yet (the
	// thread root, or a link of the parent chain, is not indexed). The record is
	// classified when the missing link arrives.
	hideReasonPendingBanCheck = "pending_ban_check"
	// hideReasonAccountBan: an instance account ban on the author was in force
	// at the record's Coves index time (migration 073), in any community. It
	// takes precedence over the community checks, and pending classification
	// never rewrites it. Only a ban made by the consumer's account-ban authority
	// (WithCommentAccountBanAuthority, WithVoteAccountBanAuthority) counts.
	// Judged on the consumer clock (accountBanInForceTx, accountbans.InForceSQL)
	// under lockBanSubjectTx, which BanAccount and UnbanAccount also take on
	// the account DID, so a ban commit and an index-time classification
	// serialize.
	hideReasonAccountBan = comments.HideReasonAccountBan
)

// WithCommentClock sets the Coves clock the comment consumer uses to decide
// whether a community ban had expired when a comment was indexed.
func WithCommentClock(now func() time.Time) CommentEventConsumerOption {
	return func(c *CommentEventConsumer) { c.now = now }
}

// WithVoteClock sets the Coves clock the vote consumer uses to decide whether
// a community ban had expired when a vote was indexed.
func WithVoteClock(now func() time.Time) VoteEventConsumerOption {
	return func(c *VoteEventConsumer) { c.now = now }
}

// WithCommentAccountBanAuthority sets the instance whose in-force account bans
// the comment consumer applies at index time (hideReasonAccountBan) and to edit
// fan-out. Without it no account ban applies.
func WithCommentAccountBanAuthority(instanceDID string) CommentEventConsumerOption {
	return func(c *CommentEventConsumer) { c.accountBanAuthorityDID = instanceDID }
}

// WithVoteAccountBanAuthority sets the instance whose in-force account bans the
// vote consumer applies at index time (hideReasonAccountBan). Without it no
// account ban applies.
func WithVoteAccountBanAuthority(instanceDID string) VoteEventConsumerOption {
	return func(c *VoteEventConsumer) { c.accountBanAuthorityDID = instanceDID }
}

// accountBanInForceTx reports whether an account ban made by authorityDID on
// accountDID is in force at the Coves time at. Callers hold lockBanSubjectTx on
// accountDID, so the answer cannot change before they commit.
func accountBanInForceTx(ctx context.Context, tx *sql.Tx, accountDID, authorityDID string, at time.Time) (bool, error) {
	var banned bool
	if err := tx.QueryRowContext(ctx, `SELECT `+accountbans.InForceSQL("$1::text", "$2::timestamptz", "$3::text"),
		accountDID, at, authorityDID).Scan(&banned); err != nil {
		return false, fmt.Errorf("check account ban of %s: %w", accountDID, err)
	}
	return banned, nil
}

// accountStatusRestrictedTx reports whether accountDID has a stored
// PDS-confirmed takendown or suspended status (account_status_restrictions).
// Like the read predicate (accountRestrictedSQL), it applies only when an
// authorityDID is configured. The status suppresses reply and mention fan-out
// at index time but records no hide_reason, so clearing it needs no undo.
func accountStatusRestrictedTx(ctx context.Context, tx *sql.Tx, accountDID, authorityDID string) (bool, error) {
	if authorityDID == "" {
		return false, nil
	}
	var restricted bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM account_status_restrictions WHERE did = $1)`,
		accountDID).Scan(&restricted); err != nil {
		return false, fmt.Errorf("check account status restriction of %s: %w", accountDID, err)
	}
	return restricted, nil
}

// accountRestrictionSuppressesFanoutTx reports whether accountDID's comment
// writes notify no one: an account ban made by authorityDID is in force at the
// Coves time at, or the account's PDS-confirmed status restriction is stored.
func accountRestrictionSuppressesFanoutTx(ctx context.Context, tx *sql.Tx, accountDID, authorityDID string, at time.Time) (bool, error) {
	banned, err := accountBanInForceTx(ctx, tx, accountDID, authorityDID, at)
	if err != nil || banned {
		return banned, err
	}
	return accountStatusRestrictedTx(ctx, tx, accountDID, authorityDID)
}

// banInForceCondition is a SQL condition that is true when the ban history
// holds a ban on author in community that was indexed before order, was not
// lifted (deleted or overwritten) before order, and had not expired at time.
// The arguments are SQL expressions written by this package, never input.
func banInForceCondition(community, author, order, at string) string {
	return `EXISTS (
		SELECT 1 FROM community_ban_history h
		WHERE h.community_did = ` + community + ` AND h.subject_did = ` + author + `
		  AND h.index_order < ` + order + `
		  AND (h.lifted_order IS NULL OR h.lifted_order > ` + order + `)
		  AND (h.expires_at IS NULL OR h.expires_at > ` + at + `))`
}

// Lock order. Ban, comment, vote, post and root-arrival transactions take their
// locks in this order, and none waits on an advisory lock while holding a
// comment, vote or post row:
//
//	(a) the record rev gate,
//	(b) the erasure gate, then the post root lifecycle locks
//	    (lockCommentRootLifecycle; comment transactions only),
//	(c) the ban subject lock on the author's or voter's DID (lockBanSubjectTx;
//	    post indexing takes it so a community created on the author's DID
//	    sees the post or commits first),
//	(d) the thread root lock on the comment's declared root_uri
//	    (lockThreadRootTx; comment indexing and root classification only),
//	(e) content rows: comment rows before vote rows.

// lockBanSubjectTx takes the transaction-scoped advisory lock on an author's
// DID that ban indexing takes on the banned DID (communities.BanSubjectLockKeySQL),
// before either draws an index order. Order is drawn at the start of a
// transaction but ban history is seen only once committed, so without the lock
// a comment or vote could draw an order after a ban's yet classify before the
// ban commits (or the reverse for an unban). Held until commit. Step (c) of the
// lock order above.
func lockBanSubjectTx(ctx context.Context, tx *sql.Tx, authorDID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, authorDID); err != nil {
		return fmt.Errorf("lock community ban subject %s: %w", authorDID, err)
	}
	return nil
}

// lockThreadRootTx takes the transaction-scoped advisory lock on a thread root
// URI, step (d) of the lock order above. Comment indexing takes it on the
// comment's declared root_uri before any comment row, and root classification
// (classifyPendingForRoot) on the root it classifies. The root post commits
// before its classification starts, so either the comment transaction holds
// the lock first and commits pending before the classification reads it, or
// the classification goes first and the comment's later community resolution
// sees the committed root. Within a thread it also serializes a parent
// comment's classification of pending replies with those replies' indexing.
// A comment whose root_uri does not match its parent chain is not ordered
// this way and may stay pending, which keeps it hidden: fail closed.
func lockThreadRootTx(ctx context.Context, tx *sql.Tx, rootURI string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('comment-thread-root:' || $1))`, rootURI); err != nil {
		return fmt.Errorf("lock comment thread root %s: %w", rootURI, err)
	}
	return nil
}

// nextIndexOrder draws the next Coves index order, shared by ban, comment and
// vote indexing. Comment and vote indexing call it holding lockBanSubjectTx.
func nextIndexOrder(ctx context.Context, tx *sql.Tx) (int64, error) {
	var order int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval('coves_index_order')`).Scan(&order); err != nil {
		return 0, fmt.Errorf("draw index order: %w", err)
	}
	return order, nil
}

// contentCommunityTx returns the community of an indexed post, or of the post
// a comment's parent chain reaches; the commenter-chosen root_uri is not used.
// found is false when the content, or any link of its chain, is not indexed.
// Same resolution as postgres.CommunityBanRepository.ContentCommunity, read
// inside the indexing transaction.
func contentCommunityTx(ctx context.Context, tx *sql.Tx, contentURI string) (string, bool, error) {
	var communityDID sql.NullString
	err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE chain (uri, parent_uri) AS (
		    SELECT uri, parent_uri FROM comments WHERE uri = $1
		    UNION
		    SELECT c.uri, c.parent_uri FROM comments c JOIN chain ON c.uri = chain.parent_uri
		),
		root (post_uri) AS (
		    SELECT chain.parent_uri FROM chain
		    WHERE NOT EXISTS (SELECT 1 FROM comments c WHERE c.uri = chain.parent_uri)
		    UNION ALL
		    SELECT $1::text WHERE NOT EXISTS (SELECT 1 FROM comments WHERE uri = $1)
		)
		SELECT p.community_did FROM root LEFT JOIN posts p ON p.uri = root.post_uri
		LIMIT 1
	`, contentURI).Scan(&communityDID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("resolve community of %s: %w", contentURI, err)
	}
	return communityDID.String, communityDID.Valid, nil
}

// classifyIndexedComment records the hide reason of a comment the transaction
// just inserted (or re-created after a delete), at a new index order and the
// consumer clock's time, and returns the recorded reason (NULL when not
// hidden). An in-force account ban on the commenter is checked first, so it
// applies even while the comment's community is still unknown. A comment whose community resolves also settles the pending
// comments and votes beneath it. The caller holds lockBanSubjectTx on the
// commenter and lockThreadRootTx on the comment's root, both taken before the
// comment row was written.
func (c *CommentEventConsumer) classifyIndexedComment(ctx context.Context, tx *sql.Tx, commentID int64, uri, commenterDID string) (sql.NullString, error) {
	order, err := nextIndexOrder(ctx, tx)
	if err != nil {
		return sql.NullString{}, err
	}
	communityDID, found, err := contentCommunityTx(ctx, tx, uri)
	if err != nil {
		return sql.NullString{}, err
	}
	community := sql.NullString{String: communityDID, Valid: found}
	var hideReason sql.NullString
	if err := tx.QueryRowContext(ctx, `
		UPDATE comments SET
		    hide_reason = CASE
		        WHEN `+accountbans.InForceSQL("$2::text", "$4::timestamptz", "$6::text")+` THEN '`+hideReasonAccountBan+`'
		        WHEN $1::text IS NULL THEN '`+hideReasonPendingBanCheck+`'
		        WHEN `+banInForceCondition("$1", "$2", "$3", "$4")+` THEN '`+hideReasonCommunityBan+`'
		    END,
		    coves_index_order = $3,
		    coves_index_time = $4
		WHERE id = $5
		RETURNING hide_reason
	`, community, commenterDID, order, c.now().UTC(), commentID, c.accountBanAuthorityDID).Scan(&hideReason); err != nil {
		return sql.NullString{}, fmt.Errorf("record ban hide of comment %s: %w", uri, err)
	}
	if !found {
		return hideReason, nil
	}
	if err := classifyPendingBelow(ctx, tx, uri, communityDID, countedVoteNotifier{
		notifications: c.notifications, bridgeTrust: c.bridgeTrust,
	}); err != nil {
		return sql.NullString{}, err
	}
	return hideReason, nil
}

// classifyIndexedVote returns the hide reason of a vote being indexed at order
// and time. A vote by an account under an in-force account ban is hidden
// whatever its subject; otherwise a vote on a comment whose community is still
// pending is pending too. The comment row is read FOR NO KEY UPDATE, the lock the vote's count
// update takes, so a concurrent root arrival either sees this vote once it
// commits or has already classified the comment, and two votes on the same
// comment queue on the row instead of deadlocking on a lock upgrade.
func classifyIndexedVote(ctx context.Context, tx *sql.Tx, subjectURI, voterDID, accountBanAuthorityDID string, order int64, at time.Time) (sql.NullString, error) {
	var subjectHide sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT hide_reason FROM comments WHERE uri = $1 FOR NO KEY UPDATE`, subjectURI).Scan(&subjectHide)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, fmt.Errorf("read vote subject ban check state: %w", err)
	}
	accountBanned, err := accountBanInForceTx(ctx, tx, voterDID, accountBanAuthorityDID, at)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("check account ban of vote: %w", err)
	}
	if accountBanned {
		return sql.NullString{String: hideReasonAccountBan, Valid: true}, nil
	}
	if subjectHide.String == hideReasonPendingBanCheck {
		return sql.NullString{String: hideReasonPendingBanCheck, Valid: true}, nil
	}
	communityDID, found, err := contentCommunityTx(ctx, tx, subjectURI)
	if err != nil || !found {
		return sql.NullString{}, err
	}
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT `+banInForceCondition("$1", "$2", "$3", "$4"),
		communityDID, voterDID, order, at).Scan(&hidden); err != nil {
		return sql.NullString{}, fmt.Errorf("check community ban of vote: %w", err)
	}
	if !hidden {
		return sql.NullString{}, nil
	}
	return sql.NullString{String: hideReasonCommunityBan, Valid: true}, nil
}

// pendingThreadCTE walks $1 and every comment whose parent chain passes
// through it.
const pendingThreadCTE = `
	WITH RECURSIVE thread (uri) AS (
	    SELECT $1::text
	    UNION
	    SELECT c.uri FROM comments c JOIN thread ON c.parent_uri = thread.uri
	)`

// countedVoteNotifier runs upvote group fan-out for a pending vote that enters
// its subject's tallies. A nil repository writes nothing.
type countedVoteNotifier struct {
	notifications notifications.Repository
	bridgeTrust   *BridgeTrust
}

// classifyPendingBelow classifies the pending comments at or under uri, whose
// community is now known to be communityDID, and then the pending votes on
// them, each as of its own index order and index time. A vote that comes out
// not hidden is counted now, with the group fan-out a counted vote gets at
// index time; a hidden one never is. Comments go first: their row locks order
// this against a vote that read its subject FOR NO KEY UPDATE.
func classifyPendingBelow(ctx context.Context, tx *sql.Tx, uri, communityDID string, notifier countedVoteNotifier) error {
	if _, err := tx.ExecContext(ctx, pendingThreadCTE+`
		UPDATE comments c SET hide_reason = CASE
		    WHEN `+banInForceCondition("$2", "c.commenter_did", "c.coves_index_order", "c.coves_index_time")+`
		    THEN '`+hideReasonCommunityBan+`'
		END
		FROM thread
		WHERE c.uri = thread.uri AND c.hide_reason = '`+hideReasonPendingBanCheck+`'
	`, uri, communityDID); err != nil {
		return fmt.Errorf("classify pending comments under %s: %w", uri, err)
	}
	// The erasure marker is read without the erasure gate's lock: this
	// transaction already holds content rows, so waiting on that lock could
	// deadlock with erasure. Groups that lose their last voter to an erasure
	// are hidden at read time.
	rows, err := tx.QueryContext(ctx, pendingThreadCTE+`,
		classified AS (
		    UPDATE votes v SET hide_reason = CASE
		        WHEN `+banInForceCondition("$2", "v.voter_did", "v.coves_index_order", "v.coves_index_time")+`
		        THEN '`+hideReasonCommunityBan+`'
		    END
		    FROM thread
		    WHERE v.subject_uri = thread.uri AND v.hide_reason = '`+hideReasonPendingBanCheck+`'
		    RETURNING v.uri, v.voter_did, v.subject_uri, v.direction, v.created_at,
		              v.coves_index_order, v.hide_reason, v.deleted_at
		),
		tally AS (
		    SELECT subject_uri,
		           COUNT(*) FILTER (WHERE direction = 'up') AS up,
		           COUNT(*) FILTER (WHERE direction = 'down') AS down
		    FROM classified
		    WHERE hide_reason IS NULL AND deleted_at IS NULL
		    GROUP BY subject_uri
		),
		counted_subject AS (
		    UPDATE comments c SET
		        upvote_count = c.upvote_count + tally.up,
		        downvote_count = c.downvote_count + tally.down,
		        score = c.upvote_count + tally.up - c.downvote_count - tally.down
		                + c.bridged_upvote_count - c.bridged_downvote_count
		    FROM tally
		    WHERE c.uri = tally.subject_uri
		    RETURNING c.uri, c.root_uri
		)
		SELECT classified.uri, classified.voter_did, classified.subject_uri, counted_subject.root_uri,
		       classified.direction, classified.created_at,
		       EXISTS (SELECT 1 FROM deleted_accounts d WHERE d.did = classified.voter_did)
		FROM classified JOIN counted_subject ON counted_subject.uri = classified.subject_uri
		WHERE classified.hide_reason IS NULL AND classified.deleted_at IS NULL
		ORDER BY classified.coves_index_order
	`, uri, communityDID)
	if err != nil {
		return fmt.Errorf("classify pending votes under %s: %w", uri, err)
	}
	var counted []notifications.VoteRecord
	for rows.Next() {
		var vote notifications.VoteRecord
		if err := rows.Scan(&vote.URI, &vote.VoterDID, &vote.SubjectURI, &vote.SubjectRootURI,
			&vote.Direction, &vote.CreatedAt, &vote.VoterErased); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read counted pending vote under %s: %w", uri, err)
		}
		counted = append(counted, vote)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close counted pending votes under %s: %w", uri, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read counted pending votes under %s: %w", uri, err)
	}
	return notifier.fanOutCountedVotes(ctx, tx, counted)
}

// fanOutCountedVotes runs, for each vote that just entered its subject's
// tallies, the group fan-out the vote consumer runs for a counted vote. The
// transaction holds each subject's row from the count update, which
// delete-if-empty needs.
func (n countedVoteNotifier) fanOutCountedVotes(ctx context.Context, tx *sql.Tx, counted []notifications.VoteRecord) error {
	if n.notifications == nil {
		return nil
	}
	for _, vote := range counted {
		intent, err := notifications.FanoutVoteCreate(ctx, n.notifications.LookupsTx(tx), n.bridgeTrust, vote)
		if err != nil {
			return fmt.Errorf("compute notifications of counted pending vote %s: %w", vote.URI, err)
		}
		if err := n.notifications.ApplyUpvoteGroupTx(ctx, tx, intent); err != nil {
			return fmt.Errorf("write notifications of counted pending vote %s: %w", vote.URI, err)
		}
	}
	return nil
}

// countedVoteNotifier is the post consumer's fan-out for pending votes counted
// when a root arrives.
func (c *PostEventConsumer) countedVoteNotifier() countedVoteNotifier {
	return countedVoteNotifier{notifications: c.notifications, bridgeTrust: c.bridgeTrust}
}

// classifyPendingForRoot settles the comments and votes left pending under a
// root post that has just been indexed in communityDID. It takes the thread
// root lock first (see the lock order on lockThreadRootTx), so a comment
// transaction still recording one of them pending commits before this reads.
func classifyPendingForRoot(ctx context.Context, db *sql.DB, rootURI, communityDID string, notifier countedVoteNotifier) (err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin pending ban check transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := lockThreadRootTx(ctx, tx, rootURI); err != nil {
		return err
	}
	if err := classifyPendingBelow(ctx, tx, rootURI, communityDID, notifier); err != nil {
		return err
	}
	return tx.Commit()
}
