package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/lib/pq"
)

// ErasureLockKeySQL is the advisory-lock key for one account's erasure, with the
// DID as $1. Delete takes the exclusive lock on it; every notification writer
// takes the shared lock on the actor's key before touching content rows.
const ErasureLockKeySQL = "hashtext('erasure:' || $1)"

// ErrErasureGateRequiresReadCommitted is returned by ErasureGateTx for a
// transaction not at READ COMMITTED: a snapshot taken before the erasure lock
// was awaited cannot see a marker committed while waiting.
var ErrErasureGateRequiresReadCommitted = errors.New("notification erasure gate requires read committed isolation")

// ErrNotificationActivationMissing is returned by ActivatedAt when the
// notification_activation singleton row is absent. The cutoff is unknown, so
// the caller must fail the write rather than treat it as the zero time.
var ErrNotificationActivationMissing = errors.New("notification activation row is missing")

type postgresNotificationRepo struct {
	db                  *sql.DB
	bridgedUpvoteTotals bool
	countUnreadSQL      string
	listSQL             string
	listUpvotesSQL      string
}

// NotificationRepositoryOption configures NewNotificationRepository.
type NotificationRepositoryOption func(*postgresNotificationRepo)

// WithBridgedUpvoteTotals includes stored bridged totals in upvote groups.
// Turning it off lets the retention sweep permanently delete bridged-only
// groups; turning it back on does not restore deleted groups.
func WithBridgedUpvoteTotals() NotificationRepositoryOption {
	return func(r *postgresNotificationRepo) { r.bridgedUpvoteTotals = true }
}

// NewNotificationRepository returns the Postgres notifications repository.
func NewNotificationRepository(db *sql.DB, options ...NotificationRepositoryOption) notifications.Repository {
	r := &postgresNotificationRepo{db: db}
	for _, option := range options {
		option(r)
	}
	r.countUnreadSQL = buildCountUnreadNotificationsSQL(r.bridgedUpvoteTotals)
	r.listSQL = buildListNotificationsSQL(r.bridgedUpvoteTotals)
	r.listUpvotesSQL = buildListNotificationUpvotesSQL(r.bridgedUpvoteTotals)
	return r
}

func (r *postgresNotificationRepo) CountsBridgedUpvoteTotals() bool {
	return r.bridgedUpvoteTotals
}

// upvoteGroupAliveSQL is the shared native-or-bridged liveness rule for a group.
func upvoteGroupAliveSQL(subjectExpr, recipientExpr string, bridgedTotals bool) string {
	requireUpvoteGroupOuterExpression("subject", subjectExpr)
	requireUpvoteGroupOuterExpression("recipient", recipientExpr)
	alive := "EXISTS (SELECT 1 FROM votes v WHERE " + qualifyingUpvoteSQL("v", subjectExpr, recipientExpr) + ")"
	if bridgedTotals {
		// OFFSET 0 keeps each correlated URI probe from becoming a hashed
		// full-table scan when this fragment is used in a larger visibility query.
		alive += " OR EXISTS (SELECT 1 FROM posts bridged_post WHERE bridged_post.uri = " + subjectExpr + " AND bridged_post.bridged_upvote_count > 0 OFFSET 0)" +
			" OR EXISTS (SELECT 1 FROM comments bridged_comment WHERE bridged_comment.uri = " + subjectExpr + " AND bridged_comment.bridged_upvote_count > 0 OFFSET 0)"
	}
	return "(" + alive + ")"
}

func bridgedUpvoteTotalSQL(subjectExpr string, bridgedTotals bool) string {
	requireUpvoteGroupOuterExpression("subject", subjectExpr)
	if !bridgedTotals {
		return "0"
	}
	return "COALESCE((SELECT bridged_post.bridged_upvote_count FROM posts bridged_post WHERE bridged_post.uri = " + subjectExpr + "), " +
		"(SELECT bridged_comment.bridged_upvote_count FROM comments bridged_comment WHERE bridged_comment.uri = " + subjectExpr + "), 0)"
}

func requireUpvoteGroupOuterExpression(role, expression string) {
	requireQualifyingUpvoteOuterExpression(role, expression, "v")
	if match := sqlQualifiedColumnPattern.FindStringSubmatch(expression); match != nil &&
		(match[1] == "bridged_post" || match[1] == "bridged_comment") {
		panic(fmt.Sprintf("upvote group: %s expression %q uses a fragment-owned alias", role, expression))
	}
}

// ErasureGateTx checks the marker after acquiring the lock in its own statement:
// at READ COMMITTED, this sees an erasure committed while the lock was awaited.
// The isolation level is checked before the lock, so a wrong-isolation
// transaction fails without waiting on an erasure in progress.
func (r *postgresNotificationRepo) ErasureGateTx(ctx context.Context, tx *sql.Tx, actorDID string) (bool, error) {
	var isolation string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&isolation); err != nil {
		return false, fmt.Errorf("check notification erasure gate isolation: %w", err)
	}
	if isolation != "read committed" {
		return false, fmt.Errorf("%w, got %s", ErrErasureGateRequiresReadCommitted, isolation)
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock_shared("+ErasureLockKeySQL+")", actorDID); err != nil {
		return false, fmt.Errorf("lock notification actor against erasure: %w", err)
	}
	var erased bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM deleted_accounts WHERE did = $1)`, actorDID).Scan(&erased); err != nil {
		return false, fmt.Errorf("check notification actor erasure marker: %w", err)
	}
	return erased, nil
}

func (r *postgresNotificationRepo) LookupsTx(tx *sql.Tx) notifications.Lookups {
	return notificationLookups{tx: tx, indexTime: new(time.Time)}
}

// ReplaceUpvoteGroupRootTx repoints recipientDID's upvote group on subjectURI
// when a comment is resurrected under a different root; ApplyUpvoteGroupTx
// writes the root only on insert. The recipient makes it a unique-index lookup.
// A comment's root is checked only for AT-URI shape, so rootPostURI may name a
// non-post record. root_post_uri must name a post, so the group then keeps its
// old root.
func (r *postgresNotificationRepo) ReplaceUpvoteGroupRootTx(ctx context.Context, tx *sql.Tx, recipientDID, subjectURI, rootPostURI string) error {
	if !posts.IsPostCollection(posts.CollectionOfPostURI(rootPostURI)) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET root_post_uri = $3
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, recipientDID, subjectURI, rootPostURI); err != nil {
		return fmt.Errorf("replace upvote group root: %w", err)
	}
	return nil
}

// RecordPostAuthorDeleteWithdrawalTx records that a publicly admitted postv2
// was deleted by its author, before its acceptance is withdrawn. An active admin
// removal does not prevent the marker: the post was still public before the
// removal. The admission predicate deliberately does not inspect deleted_at, so
// this follows the soft delete.
func (r *postgresNotificationRepo) RecordPostAuthorDeleteWithdrawalTx(ctx context.Context, tx *sql.Tx, postURI string) error {
	joinSQL, whereSQL := admittedPostsPredicate(anonymousViewerSQL)
	_, err := tx.ExecContext(ctx, `INSERT INTO notification_public_post_withdrawals (post_uri, kind)
		SELECT p.uri, 'authorDelete' FROM posts p`+joinSQL+`
		WHERE p.uri = $1 AND split_part(p.uri, '/', 4) = '`+posts.PostV2Collection+`'
			AND `+whereSQL+`
		ON CONFLICT DO NOTHING`, postURI)
	if err != nil {
		return fmt.Errorf("record post author-delete withdrawal: %w", err)
	}
	return nil
}

// RepairResurrectedCommentNotificationsTx reconciles kept rows with new
// threading before a different-parent resurrection's create fan-out. An empty
// replySubjectURI means no reply resolves, so every reply row is deleted.
func (r *postgresNotificationRepo) RepairResurrectedCommentNotificationsTx(ctx context.Context, tx *sql.Tx, recordURI, replySubjectURI, rootPostURI string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM notifications WHERE record_uri = $1
		AND reason IN ('postReply', 'commentReply') AND ($2 = '' OR subject_uri <> $2)`, recordURI, replySubjectURI); err != nil {
		return fmt.Errorf("repair resurrected comment notifications: delete old replies: %w", err)
	}
	// The record URI is checked only for AT-URI shape, so the new root may name a
	// non-post record. root_post_uri must name a post, so the kept rows then keep
	// their old root.
	if !posts.IsPostCollection(posts.CollectionOfPostURI(rootPostURI)) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET root_post_uri = $2
		WHERE record_uri = $1 AND root_post_uri <> $2`, recordURI, rootPostURI); err != nil {
		return fmt.Errorf("repair resurrected comment notifications: update root: %w", err)
	}
	return nil
}

// DeleteReplyRecipientMentionsTx applies reply precedence after a resurrection's
// fan-out: a recipient holding a reply row for the record loses its mention row.
func (r *postgresNotificationRepo) DeleteReplyRecipientMentionsTx(ctx context.Context, tx *sql.Tx, recordURI string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM notifications WHERE reason = 'mention' AND record_uri = $1
		AND recipient_did IN (SELECT recipient_did FROM notifications
			WHERE reason IN ('postReply', 'commentReply') AND record_uri = $1)`, recordURI); err != nil {
		return fmt.Errorf("repair resurrected comment notifications: delete reply recipient mentions: %w", err)
	}
	return nil
}

// ApplyUpvoteGroupTx inserts, bumps, or deletes an empty upvote group in the
// caller's transaction. A no-change intent executes no statement. RootPostURI
// is written only when the group is inserted; a bump raises sort_at to
// clock_timestamp() read after any wait on the group row lock, and keeps the
// stored root, so a comment re-created under a different root needs its own
// update of the group.
//
// The upsert runs in its own savepoint. A 23503 on notifications_recipient_did_fkey
// means the recipient was erased after the fan-out read its users row: the
// savepoint is rolled back, the group is skipped, and nil is returned, so the
// caller's earlier writes in tx survive. Every other error is returned and
// leaves the transaction to the caller.
//
// Delete-if-empty is one DELETE, with no savepoint, that removes the group only
// when no qualifying native upvote remains on its subject and, with bridged
// totals enabled, the bridged total is 0. The caller must hold
// the subject's posts or comments row lock, take it first, or re-check in a
// separate statement. At READ COMMITTED a DELETE that waits on a concurrent
// bump re-checks the bumped row but evaluates NOT EXISTS against its original
// snapshot, so without that lock it deletes a group whose new upvote is live.
func (r *postgresNotificationRepo) ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	switch intent.Action {
	case notifications.UpvoteGroupNoChange:
		return nil
	case notifications.UpvoteGroupBump:
		if _, err := tx.ExecContext(ctx, `SAVEPOINT notification_upvote_group`); err != nil {
			return fmt.Errorf("save upvote group: %w", err)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO notifications (recipient_did, reason, subject_uri, root_post_uri, sort_at)
			VALUES ($1, 'upvote', $2, $3, clock_timestamp())
			ON CONFLICT (recipient_did, subject_uri) WHERE reason = 'upvote'
			DO UPDATE SET sort_at = GREATEST(notifications.sort_at, clock_timestamp())`,
			intent.RecipientDID, intent.SubjectURI, intent.RootPostURI)
		if err != nil {
			if !isRecipientForeignKeyViolation(err) {
				return fmt.Errorf("upsert upvote group: %w", err)
			}
			if _, rollbackError := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notification_upvote_group`); rollbackError != nil {
				return fmt.Errorf("rollback erased upvote group recipient: %w (after upsert error: %w)", rollbackError, err)
			}
			if _, releaseError := tx.ExecContext(ctx, `RELEASE SAVEPOINT notification_upvote_group`); releaseError != nil {
				return fmt.Errorf("release erased upvote group recipient: %w (after upsert error: %w)", releaseError, err)
			}
			slog.DebugContext(ctx, "skipped upvote group for recipient erased during upsert",
				"subject_uri", intent.SubjectURI,
				"recipient_did", intent.RecipientDID,
			)
			return nil
		}
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT notification_upvote_group`); err != nil {
			return fmt.Errorf("release upvote group: %w", err)
		}
		return nil
	case notifications.UpvoteGroupDeleteIfEmpty:
		if _, err := tx.ExecContext(ctx, `DELETE FROM notifications
			WHERE recipient_did = $1 AND subject_uri = $2 AND reason = 'upvote'
			AND NOT `+upvoteGroupAliveSQL("$2", "$1", r.bridgedUpvoteTotals),
			intent.RecipientDID, intent.SubjectURI); err != nil {
			return fmt.Errorf("delete upvote group: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown upvote group action: %d", intent.Action)
	}
}

// ApplyTx inserts intents through tx, the caller's index transaction, and
// leaves ending tx to the caller.
//
// Every savepoint that writes allocates a subtransaction XID, and Postgres
// caches only 64 of them per backend. Past that the cache overflows, and
// snapshot visibility checks must look up pg_subtrans. The common path
// therefore writes all intents with one INSERT inside one savepoint,
// notification_batch. The savepoint exists so that a failed batch can be rolled
// back without discarding the caller's earlier writes in tx.
//
// Only a 23503 on notifications_recipient_did_fkey is an expected outcome: a
// recipient was erased (erasure deletes its users row) between the fan-out's
// recipient-facts read and this insert. ApplyTx then rolls the batch back and
// inserts each intent in its own savepoint, skipping erased recipients. That
// fallback opens one savepoint per intent, up to one reply plus
// richtext.MaxFacets mentions, so in this rare race it can exceed the 64-entry
// cache. Every other error is a defect or a transient failure and is returned,
// so the caller aborts the index transaction and the event is retried or
// dead-lettered.
func (r *postgresNotificationRepo) ApplyTx(ctx context.Context, tx *sql.Tx, intents []notifications.Intent) error {
	if len(intents) == 0 {
		return nil
	}
	recipients := make([]string, 0, len(intents))
	reasons := make([]string, 0, len(intents))
	recordURIs := make([]string, 0, len(intents))
	recordCIDs := make([]string, 0, len(intents))
	actors := make([]string, 0, len(intents))
	subjectURIs := make([]string, 0, len(intents))
	rootPostURIs := make([]string, 0, len(intents))
	createdAt := make([]time.Time, 0, len(intents))
	for _, intent := range intents {
		recipients = append(recipients, intent.RecipientDID)
		reasons = append(reasons, string(intent.Reason))
		recordURIs = append(recordURIs, intent.RecordURI)
		recordCIDs = append(recordCIDs, intent.RecordCID)
		actors = append(actors, intent.ActorDID)
		subjectURIs = append(subjectURIs, intent.SubjectURI)
		rootPostURIs = append(rootPostURIs, intent.RootPostURI)
		createdAt = append(createdAt, intent.RecordCreatedAt)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notification_batch`); err != nil {
		return fmt.Errorf("save notification batch: %w", err)
	}
	// sort_at is clock_timestamp(), not the column default now() (transaction
	// start), so time spent waiting on locks before this write no longer makes
	// the row look older than a seen_at set meanwhile. A small INSERT-to-COMMIT
	// window remains: an updateSeen landing between this write and commit still
	// marks the row read.
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did,
			subject_uri, root_post_uri, record_created_at, sort_at)
		SELECT recipient, reason, NULLIF(record_uri, ''), NULLIF(record_cid, ''), NULLIF(actor, ''),
			NULLIF(subject_uri, ''), root_post_uri, created_at, clock_timestamp()
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[],
			$6::text[], $7::text[], $8::timestamptz[])
			AS intent(recipient, reason, record_uri, record_cid, actor, subject_uri, root_post_uri, created_at)
		ON CONFLICT (recipient_did, reason, record_uri) WHERE reason <> 'upvote' DO NOTHING`,
		pq.Array(recipients), pq.Array(reasons), pq.Array(recordURIs), pq.Array(recordCIDs),
		pq.Array(actors), pq.Array(subjectURIs), pq.Array(rootPostURIs), pq.Array(createdAt))
	if err == nil {
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT notification_batch`); err != nil {
			return fmt.Errorf("release notification batch: %w", err)
		}
		return nil
	}
	if !isRecipientForeignKeyViolation(err) {
		return fmt.Errorf("insert notification batch: %w", err)
	}
	if _, rollbackError := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notification_batch`); rollbackError != nil {
		return fmt.Errorf("rollback notification batch: %w (after insert error: %w)", rollbackError, err)
	}
	if _, releaseError := tx.ExecContext(ctx, `RELEASE SAVEPOINT notification_batch`); releaseError != nil {
		return fmt.Errorf("release notification batch: %w (after insert error: %w)", releaseError, err)
	}
	slog.InfoContext(ctx, "notification batch hit an erased recipient, inserting intents one at a time",
		"record_uri", intents[0].RecordURI,
		"intent_count", len(intents),
	)
	for _, intent := range intents {
		if _, err := tx.ExecContext(ctx, `SAVEPOINT notification_intent`); err != nil {
			return fmt.Errorf("save notification intent: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did,
				subject_uri, root_post_uri, record_created_at, sort_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, clock_timestamp())
			ON CONFLICT (recipient_did, reason, record_uri) WHERE reason <> 'upvote' DO NOTHING`,
			intent.RecipientDID, intent.Reason, nullString(intent.RecordURI), nullString(intent.RecordCID),
			nullString(intent.ActorDID), nullString(intent.SubjectURI), intent.RootPostURI, intent.RecordCreatedAt)
		if err != nil {
			if isRecipientForeignKeyViolation(err) {
				if _, rollbackError := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notification_intent`); rollbackError != nil {
					return fmt.Errorf("rollback erased notification recipient intent: %w (after insert error: %w)", rollbackError, err)
				}
				if _, releaseError := tx.ExecContext(ctx, `RELEASE SAVEPOINT notification_intent`); releaseError != nil {
					return fmt.Errorf("release erased notification recipient intent: %w (after insert error: %w)", releaseError, err)
				}
				slog.DebugContext(ctx, "skipped notification for recipient erased during insert",
					"reason", intent.Reason,
					"record_uri", intent.RecordURI,
					"recipient_did", intent.RecipientDID,
				)
				continue
			}
			return fmt.Errorf("insert notification intent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT notification_intent`); err != nil {
			return fmt.Errorf("release notification intent: %w", err)
		}
	}
	return nil
}

// isRecipientForeignKeyViolation reports whether err is the foreign-key
// violation raised when a notification's recipient has no users row.
func isRecipientForeignKeyViolation(err error) bool {
	var databaseError *pq.Error
	return errors.As(err, &databaseError) && databaseError.Code == "23503" &&
		databaseError.Constraint == "notifications_recipient_did_fkey"
}

type notificationLookups struct {
	tx *sql.Tx
	// indexTime memoizes now() read alongside activated_at: now() is constant
	// within a transaction, so IndexTime can reuse it without a second round
	// trip. LookupsTx always allocates it; the zero value means neither
	// ActivatedAt nor IndexTime has read it yet.
	indexTime *time.Time
}

func (l notificationLookups) LegacyPostAuthor(ctx context.Context, postURI string) (string, bool, error) {
	var authorDID string
	err := l.tx.QueryRowContext(ctx, `SELECT author_did FROM posts WHERE uri = $1`, postURI).Scan(&authorDID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("look up legacy post author: %w", err)
	}
	return authorDID, true, nil
}

func (l notificationLookups) ActivatedAt(ctx context.Context) (time.Time, error) {
	var activatedAt, indexTime time.Time
	err := l.tx.QueryRowContext(ctx, `SELECT activated_at, now() FROM notification_activation`).Scan(&activatedAt, &indexTime)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("select notification_activation: %w", ErrNotificationActivationMissing)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("select notification_activation: %w", err)
	}
	*l.indexTime = indexTime
	return activatedAt, nil
}

func (l notificationLookups) IndexTime(ctx context.Context) (time.Time, error) {
	if !l.indexTime.IsZero() {
		return *l.indexTime, nil
	}
	var indexTime time.Time
	if err := l.tx.QueryRowContext(ctx, `SELECT now()`).Scan(&indexTime); err != nil {
		return time.Time{}, fmt.Errorf("select now(): %w", err)
	}
	*l.indexTime = indexTime
	return indexTime, nil
}

func (l notificationLookups) IsAggregator(ctx context.Context, did string) (bool, error) {
	var aggregator bool
	if err := l.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aggregators WHERE did = $1)`, did).Scan(&aggregator); err != nil {
		return false, fmt.Errorf("select aggregators: %w", err)
	}
	return aggregator, nil
}

// ExistingMentionRecipients reads surviving mention recipients for a record
// inside the caller's index transaction.
func (l notificationLookups) ExistingMentionRecipients(ctx context.Context, recordURI string) ([]string, error) {
	rows, err := l.tx.QueryContext(ctx,
		`SELECT recipient_did FROM notifications WHERE reason = 'mention' AND record_uri = $1`, recordURI)
	if err != nil {
		return nil, fmt.Errorf("select existing mention recipients: %w", err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var recipientDID string
		if err := rows.Scan(&recipientDID); err != nil {
			return nil, fmt.Errorf("select existing mention recipients: %w", err)
		}
		recipients = append(recipients, recipientDID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select existing mention recipients: %w", err)
	}
	return recipients, nil
}

func (l notificationLookups) EarlierUpvoteExists(ctx context.Context, voterDID, subjectURI, voteURI string) (bool, error) {
	var exists bool
	if err := l.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM votes WHERE voter_did = $1 AND subject_uri = $2 AND direction = 'up' AND uri <> $3)`,
		voterDID, subjectURI, voteURI).Scan(&exists); err != nil {
		return false, fmt.Errorf("select earlier upvotes: %w", err)
	}
	return exists, nil
}

func (l notificationLookups) RecipientFacts(ctx context.Context, actorDID string, recipientDIDs []string) (map[string]notifications.RecipientFacts, error) {
	facts := make(map[string]notifications.RecipientFacts, len(recipientDIDs))
	if len(recipientDIDs) == 0 {
		return facts, nil
	}
	rows, err := l.tx.QueryContext(ctx, `
		SELECT u.did, u.pds_url,
			EXISTS(SELECT 1 FROM deleted_accounts WHERE did = u.did),
			EXISTS(SELECT 1 FROM aggregators WHERE did = u.did),
			EXISTS(SELECT 1 FROM communities WHERE did = u.did),
			EXISTS(SELECT 1 FROM user_blocks WHERE
				(blocker_did = u.did AND blocked_did = $1) OR
				(blocker_did = $1 AND blocked_did = u.did))
		FROM users u WHERE u.did = ANY($2)`, actorDID, pq.Array(recipientDIDs))
	if err != nil {
		return nil, fmt.Errorf("query notification recipient facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var did string
		var recipient notifications.RecipientFacts
		if err := rows.Scan(&did, &recipient.PDSURL, &recipient.Erased, &recipient.Aggregator,
			&recipient.Community, &recipient.BlockedWithActor); err != nil {
			return nil, fmt.Errorf("scan notification recipient facts: %w", err)
		}
		facts[did] = recipient
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification recipient facts: %w", err)
	}
	return facts, nil
}

var (
	sqlIdentifierPattern      = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	sqlPlaceholderPattern     = regexp.MustCompile(`^\$[1-9][0-9]*$`)
	sqlQualifiedColumnPattern = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\.[a-z_][a-z0-9_]*$`)
)

// qualifyingUpvoteReservedAliasPrefix starts every alias qualifyingUpvoteSQL
// gives its own subqueries.
const qualifyingUpvoteReservedAliasPrefix = "qualifying_upvote_"

// qualifyingUpvoteSQL is the condition under which the votes row aliased
// voteAlias is a live qualifying upvote on subjectExpr for recipientExpr.
//
// The arguments are spliced into SQL, so they are constants, never values.
// voteAlias is a lowercase identifier. subjectExpr and recipientExpr are each a
// bind placeholder ($2) or a column qualified with an outer alias
// (n.subject_uri), never with voteAlias. The fragment resolves an unqualified
// name against votes and user_blocks before any outer row, so a bare
// subject_uri compiles to voteAlias.subject_uri = voteAlias.subject_uri, which
// is always true. Aliases starting with qualifying_upvote_ are reserved for
// the fragment's own subqueries. Any other argument panics.
//
// A DELETE guarded by NOT EXISTS over this fragment is correct only while the
// caller holds the subject's posts or comments row lock, takes it first, or
// re-checks in a separate statement; ApplyUpvoteGroupTx says why.
func qualifyingUpvoteSQL(voteAlias, subjectExpr, recipientExpr string) string {
	if !sqlIdentifierPattern.MatchString(voteAlias) || strings.HasPrefix(voteAlias, qualifyingUpvoteReservedAliasPrefix) {
		panic(fmt.Sprintf("qualifyingUpvoteSQL: vote alias %q must be a lowercase identifier not starting with %q",
			voteAlias, qualifyingUpvoteReservedAliasPrefix))
	}
	requireQualifyingUpvoteOuterExpression("subject", subjectExpr, voteAlias)
	requireQualifyingUpvoteOuterExpression("recipient", recipientExpr, voteAlias)
	return fmt.Sprintf(`(%[1]s.subject_uri = %[2]s
		AND %[1]s.deleted_at IS NULL
		AND %[1]s.direction = 'up'
		AND %[1]s.voter_did <> %[3]s
		AND NOT EXISTS (SELECT 1 FROM deleted_accounts qualifying_upvote_erasure
			WHERE qualifying_upvote_erasure.did = %[1]s.voter_did)
		AND NOT EXISTS (SELECT 1 FROM aggregators qualifying_upvote_aggregator
			WHERE qualifying_upvote_aggregator.did = %[1]s.voter_did)
		AND NOT EXISTS (SELECT 1 FROM user_blocks qualifying_upvote_block
			WHERE (qualifying_upvote_block.blocker_did = %[1]s.voter_did
				AND qualifying_upvote_block.blocked_did = %[3]s)
				OR (qualifying_upvote_block.blocker_did = %[3]s
					AND qualifying_upvote_block.blocked_did = %[1]s.voter_did)))`,
		voteAlias, subjectExpr, recipientExpr)
}

// requireQualifyingUpvoteOuterExpression panics unless expression is a bind
// placeholder or a column qualified with an alias from outside the fragment.
func requireQualifyingUpvoteOuterExpression(role, expression, voteAlias string) {
	if sqlPlaceholderPattern.MatchString(expression) {
		return
	}
	match := sqlQualifiedColumnPattern.FindStringSubmatch(expression)
	if match == nil {
		panic(fmt.Sprintf("qualifyingUpvoteSQL: %s expression %q must be a bind placeholder ($N) or an outer-alias-qualified column (alias.column)",
			role, expression))
	}
	if outerAlias := match[1]; outerAlias == voteAlias || strings.HasPrefix(outerAlias, qualifyingUpvoteReservedAliasPrefix) {
		panic(fmt.Sprintf("qualifyingUpvoteSQL: %s expression %q is qualified with %q, an alias the fragment binds itself; qualify it with an outer alias",
			role, expression, outerAlias))
	}
}

// SweepReadNotifications deletes rows older than each recipient's last seen
// position, falling back to that recipient's newest row when never seen.
func (r *postgresNotificationRepo) SweepReadNotifications(ctx context.Context) (int64, error) {
	return r.sweepNotificationDelete(ctx, `DELETE FROM notifications WHERE id IN (
		SELECT n.id FROM notifications n
		LEFT JOIN notification_state state ON state.did = n.recipient_did
		WHERE n.sort_at < COALESCE(state.seen_at,
			(SELECT newest.sort_at FROM notifications newest
			 WHERE newest.recipient_did = n.recipient_did
			 ORDER BY newest.sort_at DESC, newest.id DESC LIMIT 1))
			- ($1::bigint * INTERVAL '1 hour')
		LIMIT $2 FOR UPDATE OF n SKIP LOCKED
	)`, int64(notifications.RetentionReadWindow/time.Hour), notifications.RetentionBatchSize)
}

// SweepUnreadOverflow removes old unread rows only for recipients whose unread
// count exceeds the cap. The count and newest timestamp reflect this statement.
func (r *postgresNotificationRepo) SweepUnreadOverflow(ctx context.Context) (int64, error) {
	return r.sweepNotificationDelete(ctx, `WITH overflowing AS MATERIALIZED (
		SELECT state.did, state.seen_at, MAX(all_rows.sort_at) AS newest
		FROM notification_state state
		JOIN notifications all_rows ON all_rows.recipient_did = state.did
		WHERE state.seen_at IS NOT NULL
		GROUP BY state.did, state.seen_at
		HAVING COUNT(*) FILTER (WHERE all_rows.sort_at > state.seen_at) > $1
	)
	DELETE FROM notifications WHERE id IN (
		SELECT n.id FROM notifications n
		JOIN overflowing ON overflowing.did = n.recipient_did
		WHERE n.sort_at > overflowing.seen_at
			AND n.sort_at < overflowing.newest - ($2::bigint * INTERVAL '1 hour')
		LIMIT $3 FOR UPDATE OF n SKIP LOCKED
	)`, notifications.RetentionUnreadCap, int64(notifications.RetentionUnreadWindow/time.Hour), notifications.RetentionBatchSize)
}

// sweepNotificationDelete runs one bounded DELETE in its own transaction.
func (r *postgresNotificationRepo) sweepNotificationDelete(ctx context.Context, query string, args ...any) (int64, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin notification retention sweep: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("delete notification retention batch: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count notification retention batch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit notification retention sweep: %w", err)
	}
	return removed, nil
}

var _ notifications.RetentionSweeper = (*postgresNotificationRepo)(nil)

// sweepHiddenReferenceNotificationsSQL selects through the read-side reference
// states only, so a row hidden by a block, a disabled preference or the upvote
// alive rule is kept; placeholders (deleted, removed) never read hidden.
var sweepHiddenReferenceNotificationsSQL = func() string {
	visibility := notificationVisibility(false)
	return `DELETE FROM notifications WHERE id IN (
		SELECT n.id FROM notifications n` + visibility.joins + `
		WHERE n.sort_at < now() - ($1::bigint * INTERVAL '1 hour')
			AND NOT (` + visibility.referencesVisible + `)
		LIMIT $2 FOR UPDATE OF n SKIP LOCKED
	)`
}()

// SweepHiddenReferenceNotifications deletes rows whose required reference reads
// hidden and whose sort_at is older than now() - RetentionHiddenReferenceWindow:
// such a row never lists, and a reference that stayed hidden that long is not
// expected to become public. There is no lower age bound, so a row missed during
// downtime or hidden after it aged past the window is still removed. The lateral
// reference lookups run across that old tail, bounded per statement by
// RetentionBatchSize.
func (r *postgresNotificationRepo) SweepHiddenReferenceNotifications(ctx context.Context) (int64, error) {
	return r.sweepNotificationDelete(ctx, sweepHiddenReferenceNotificationsSQL,
		int64(notifications.RetentionHiddenReferenceWindow/time.Hour), notifications.RetentionBatchSize)
}

// SweepEmptyUpvoteGroups locks empty candidates, then re-checks eligibility in
// a second statement with a fresh READ COMMITTED snapshot. A concurrent bump
// either precedes that snapshot or waits for these row locks to be released.
func (r *postgresNotificationRepo) SweepEmptyUpvoteGroups(ctx context.Context) (int64, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin empty upvote group sweep: %w", err)
	}
	defer tx.Rollback()
	alive := upvoteGroupAliveSQL("n.subject_uri", "n.recipient_did", r.bridgedUpvoteTotals)
	rows, err := tx.QueryContext(ctx, `SELECT n.id FROM notifications n
		WHERE n.reason = 'upvote' AND NOT `+alive+`
		LIMIT $1 FOR UPDATE OF n SKIP LOCKED`, notifications.RetentionBatchSize)
	if err != nil {
		return 0, fmt.Errorf("select empty upvote group candidates: %w", err)
	}
	var candidateIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan empty upvote group candidates: %w", err)
		}
		candidateIDs = append(candidateIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read empty upvote group candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close empty upvote group candidates: %w", err)
	}
	if len(candidateIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit empty upvote group sweep: %w", err)
		}
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM notifications n
		WHERE n.id = ANY($1::bigint[]) AND NOT `+alive, pq.Array(candidateIDs))
	if err != nil {
		return 0, fmt.Errorf("delete empty upvote groups: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count empty upvote groups: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit empty upvote group sweep: %w", err)
	}
	return removed, nil
}

// ReferenceStates classifies withdrawn references in one read through the index
// transaction. Indexed live references are absent from the result. A post or
// comment URI with no indexed row is Unindexed, unless an active admin removal
// decision names it (the decision can predate indexing or outlive a delete), in
// which case the removal classifies it. A URI in any other collection can never
// be indexed, so it is not a withdrawable reference and stays absent (a comment
// whose parent is an unsupported record still notifies its mentions).
// Precedence follows the read side: a community-removed post reads
// RemovedByServerAdmin under an active instance removal, else
// RemovedByModerator; a deleted row stays Deleted; otherwise any active
// instance removal is RemovedByServerAdmin and community-scope-only removals are
// RemovedByModerator. Label decisions and inactive removals add nothing.
func (l notificationLookups) ReferenceStates(ctx context.Context, uris []string) (map[string]notifications.ReferenceState, error) {
	states := make(map[string]notifications.ReferenceState)
	if len(uris) == 0 {
		return states, nil
	}
	rows, err := l.tx.QueryContext(ctx, `
		WITH removals AS (
			SELECT d.subject_uri AS uri, bool_or(d.scope_kind = 'instance') AS by_instance
			FROM moderation_decisions d
			WHERE d.subject_uri = ANY($1::text[]) AND d.kind = 'removal' AND d.active
			GROUP BY d.subject_uri
		), indexed AS (
			SELECT c.uri, c.deleted_at IS NOT NULL AS deleted, false AS community_removed
			FROM comments c
			WHERE c.uri = ANY($1::text[])
			UNION ALL
			SELECT p.uri, p.deleted_at IS NOT NULL, COALESCE(a.status = 'removed', false)
			FROM posts p
			LEFT JOIN community_post_admissions a
				ON a.community_did = p.community_did AND a.post_uri = p.uri
			WHERE p.uri = ANY($1::text[])
		)
		SELECT uri, state FROM (
			SELECT requested.uri, CASE
				WHEN i.community_removed THEN CASE WHEN r.by_instance THEN $4::int ELSE $3::int END
				WHEN i.deleted THEN $2::int
				WHEN r.by_instance THEN $4::int
				WHEN r.uri IS NOT NULL THEN $3::int
				WHEN i.uri IS NULL AND split_part(requested.uri, '/', 4) = ANY($6::text[]) THEN $5::int
			END AS state
			FROM (SELECT DISTINCT unnest($1::text[]) AS uri) requested
			LEFT JOIN indexed i ON i.uri = requested.uri
			LEFT JOIN removals r ON r.uri = requested.uri
		) classified
		WHERE state IS NOT NULL`,
		pq.Array(uris), int(notifications.ReferenceDeleted), int(notifications.ReferenceRemovedByModerator),
		int(notifications.ReferenceRemovedByServerAdmin), int(notifications.ReferenceUnindexed),
		pq.Array([]string{posts.PostV2Collection, posts.LegacyPostCollection, "social.coves.community.comment"}))
	if err != nil {
		return nil, fmt.Errorf("query notification reference states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uri string
		var state notifications.ReferenceState
		if err := rows.Scan(&uri, &state); err != nil {
			return nil, fmt.Errorf("scan notification reference states: %w", err)
		}
		states[uri] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification reference states: %w", err)
	}
	return states, nil
}
