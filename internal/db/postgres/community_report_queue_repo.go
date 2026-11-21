package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"Coves/internal/core/adminreports"
)

type communityReportQueueRepo struct {
	db *sql.DB
}

// NewCommunityReportQueueRepository creates the PostgreSQL community report
// queue repository.
func NewCommunityReportQueueRepository(db *sql.DB) adminreports.CommunityQueueRepository {
	return &communityReportQueueRepo{db: db}
}

// communityReportSQL is the predicate that the admin_reports row aliased
// reportAlias is one of the community-visible open reports of the community
// bound to communityParam: it is open, its reason is not admin-only (csam,
// illegal), the community is hosted here, and the reported content is indexed
// in that community. A post's community is posts.community_did; a comment's is
// the community of the indexed post its parent chain reaches. The community is
// never read from the reported URI's authority or from the report request.
// A community removal resolves these reports (resolveCommunityReportsTx).
// reportAlias and communityParam are fixed SQL fragments, never caller input.
func communityReportSQL(reportAlias, communityParam string) string {
	return `(` + reportAlias + `.status = 'open'
		AND ` + reportAlias + `.reason NOT IN ('csam', 'illegal')
		AND EXISTS (SELECT 1 FROM communities report_community
			WHERE report_community.did = ` + communityParam + `
			  AND report_community.pds_refresh_token_encrypted IS NOT NULL)
		AND (
			(` + reportAlias + `.target_type = 'post' AND EXISTS (
				SELECT 1 FROM posts report_post
				WHERE report_post.uri = ` + reportAlias + `.target_uri
				  AND report_post.community_did = ` + communityParam + `))
			OR (` + reportAlias + `.target_type = 'comment' AND EXISTS (
				SELECT 1 FROM comments report_comment
				WHERE report_comment.uri = ` + reportAlias + `.target_uri
				  AND ` + commentChainCommunitySQL("report_comment") + ` = ` + communityParam + `))
		))`
}

// communityVisibleReportSQL is communityReportSQL narrowed to the reports the
// queue lists and a dismissal reaches: the subject is not deleted by its
// author and the community has no active removal of it (a post's admission
// in the community is not removed; a comment has no active community removal
// decision). A report on a subject that leaves the queue this way stays open.
func communityVisibleReportSQL(reportAlias, communityParam string) string {
	return `(` + communityReportSQL(reportAlias, communityParam) + `
		AND (
			(` + reportAlias + `.target_type = 'post' AND EXISTS (
				SELECT 1 FROM posts live_post
				WHERE live_post.uri = ` + reportAlias + `.target_uri
				  AND live_post.deleted_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM community_post_admissions removed_admission
					WHERE removed_admission.community_did = ` + communityParam + `
					  AND removed_admission.post_uri = live_post.uri
					  AND removed_admission.status = 'removed')))
			OR (` + reportAlias + `.target_type = 'comment' AND EXISTS (
				SELECT 1 FROM comments live_comment
				WHERE live_comment.uri = ` + reportAlias + `.target_uri
				  AND live_comment.deleted_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM moderation_decisions removal_decision
					WHERE removal_decision.subject_uri = live_comment.uri
					  AND removal_decision.authority_did = ` + communityParam + `
					  AND removal_decision.scope_kind = 'community'
					  AND removal_decision.scope_community_did = ` + communityParam + `
					  AND removal_decision.kind = 'removal'
					  AND removal_decision.active)))
		))`
}

// communityQueueCursor is the position after the last entry of a page.
type communityQueueCursor struct {
	LastReportedAt time.Time `json:"t"`
	SubjectURI     string    `json:"u"`
}

func encodeCommunityQueueCursor(entry adminreports.CommunityQueueEntry) (string, error) {
	raw, err := json.Marshal(communityQueueCursor{LastReportedAt: entry.LastReportedAt.UTC(), SubjectURI: entry.SubjectURI})
	if err != nil {
		return "", fmt.Errorf("encode community report queue cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCommunityQueueCursor(cursor string) (*communityQueueCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed cursor", adminreports.ErrInvalidQueueRequest)
	}
	var decoded communityQueueCursor
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.SubjectURI == "" || decoded.LastReportedAt.IsZero() {
		return nil, fmt.Errorf("%w: malformed cursor", adminreports.ErrInvalidQueueRequest)
	}
	return &decoded, nil
}

// ListCommunityQueue returns one page of a community's report queue: one
// entry per reported subject, newest last report first, ties broken by
// subject URI ascending.
func (r *communityReportQueueRepo) ListCommunityQueue(ctx context.Context, communityDID string, limit int, cursor string) (adminreports.CommunityQueuePage, error) {
	if limit <= 0 {
		limit = adminreports.DefaultCommunityQueueLimit
	}
	after, err := decodeCommunityQueueCursor(cursor)
	if err != nil {
		return adminreports.CommunityQueuePage{}, err
	}
	var afterTime sql.NullTime
	var afterURI sql.NullString
	if after != nil {
		afterTime = sql.NullTime{Time: after.LastReportedAt, Valid: true}
		afterURI = sql.NullString{String: after.SubjectURI, Valid: true}
	}

	query := `
		SELECT subject_uri, subject_type, reasons, report_count, first_reported_at, last_reported_at
		FROM (
			SELECT r.target_uri AS subject_uri,
			       MIN(r.target_type) AS subject_type,
			       array_agg(DISTINCT r.reason ORDER BY r.reason) AS reasons,
			       COUNT(*) AS report_count,
			       MIN(r.created_at) AS first_reported_at,
			       MAX(r.created_at) AS last_reported_at
			FROM admin_reports r
			WHERE ` + communityVisibleReportSQL("r", "$1") + `
			GROUP BY r.target_uri
		) entries
		WHERE $3::timestamptz IS NULL
		   OR last_reported_at < $3::timestamptz
		   OR (last_reported_at = $3::timestamptz AND subject_uri > $4::text)
		ORDER BY last_reported_at DESC, subject_uri ASC
		LIMIT $2`
	rows, err := r.db.QueryContext(ctx, query, communityDID, limit+1, afterTime, afterURI)
	if err != nil {
		return adminreports.CommunityQueuePage{}, fmt.Errorf("list community report queue: %w", err)
	}
	defer rows.Close()

	entries := []adminreports.CommunityQueueEntry{}
	for rows.Next() {
		var entry adminreports.CommunityQueueEntry
		var subjectType string
		var reasons []string
		if err := rows.Scan(&entry.SubjectURI, &subjectType, pq.Array(&reasons), &entry.ReportCount,
			&entry.FirstReportedAt, &entry.LastReportedAt); err != nil {
			return adminreports.CommunityQueuePage{}, fmt.Errorf("scan community report queue entry: %w", err)
		}
		entry.SubjectType = adminreports.TargetType(subjectType)
		entry.Reasons = make([]adminreports.Reason, 0, len(reasons))
		for _, reason := range reasons {
			entry.Reasons = append(entry.Reasons, adminreports.Reason(reason))
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return adminreports.CommunityQueuePage{}, fmt.Errorf("iterate community report queue: %w", err)
	}

	page := adminreports.CommunityQueuePage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		next, err := encodeCommunityQueueCursor(page.Entries[limit-1])
		if err != nil {
			return adminreports.CommunityQueuePage{}, err
		}
		page.Cursor = next
	}
	return page, nil
}

// DismissCommunitySubject dismisses the community-visible open reports on one
// subject in one transaction. The (moderator, community, key) tuple is claimed
// first: a replay with the same subject returns the stored count and changes
// nothing; the same key for a different subject is ErrIdempotencyConflict.
func (r *communityReportQueueRepo) DismissCommunitySubject(ctx context.Context, req adminreports.DismissCommunitySubjectRequest) (adminreports.DismissResult, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return adminreports.DismissResult{}, fmt.Errorf("begin community report dismissal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	erased, err := erasureGateTx(ctx, tx, req.ModeratorDID)
	if err != nil {
		return adminreports.DismissResult{}, err
	}
	if erased {
		return adminreports.DismissResult{}, fmt.Errorf("%w: the moderator's account has been erased", adminreports.ErrCommunityQueueForbidden)
	}

	// Claim the tuple. A concurrent claim of the same tuple waits here until the
	// first commits, then finds its row.
	var dismissalID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO community_report_dismissals
			(community_did, subject_uri, dismissed_by_did, idempotency_key, dismissed_count)
		VALUES ($1, $2, $3, $4, 0)
		ON CONFLICT (dismissed_by_did, community_did, idempotency_key) DO NOTHING
		RETURNING id`,
		req.CommunityDID, req.SubjectURI, req.ModeratorDID, req.IdempotencyKey,
	).Scan(&dismissalID)
	if errors.Is(err, sql.ErrNoRows) {
		var storedSubject string
		var storedCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT subject_uri, dismissed_count FROM community_report_dismissals
			WHERE dismissed_by_did = $1 AND community_did = $2 AND idempotency_key = $3`,
			req.ModeratorDID, req.CommunityDID, req.IdempotencyKey,
		).Scan(&storedSubject, &storedCount); err != nil {
			return adminreports.DismissResult{}, fmt.Errorf("read the existing community report dismissal: %w", err)
		}
		if storedSubject != req.SubjectURI {
			return adminreports.DismissResult{}, fmt.Errorf("%w: key %q was used for %s",
				adminreports.ErrIdempotencyConflict, req.IdempotencyKey, storedSubject)
		}
		return adminreports.DismissResult{DismissedCount: storedCount}, nil
	}
	if err != nil {
		return adminreports.DismissResult{}, fmt.Errorf("claim community report dismissal: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE admin_reports r
		SET status = 'dismissed', resolved_by = $3, resolved_at = NOW()
		WHERE r.target_uri = $2 AND `+communityVisibleReportSQL("r", "$1"),
		req.CommunityDID, req.SubjectURI, req.ModeratorDID)
	if err != nil {
		return adminreports.DismissResult{}, fmt.Errorf("dismiss community reports: %w", err)
	}
	dismissed, err := result.RowsAffected()
	if err != nil {
		return adminreports.DismissResult{}, fmt.Errorf("count dismissed community reports: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE community_report_dismissals SET dismissed_count = $2 WHERE id = $1`, dismissalID, dismissed); err != nil {
		return adminreports.DismissResult{}, fmt.Errorf("record community report dismissal count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return adminreports.DismissResult{}, fmt.Errorf("commit community report dismissal: %w", err)
	}
	return adminreports.DismissResult{DismissedCount: int(dismissed)}, nil
}

// resolveCommunityReportsTx marks the community-visible open reports on
// subjectURI in communityDID resolved by actorDID (NULL for an actorless
// removal), inside the transaction that records the community's removal of
// the subject. csam and illegal reports stay open for the instance admins.
//
// A removal can complete after its actor's account was erased (the PDS
// response was lost and the sweep or the firehose copy completes it), so the
// actor's shared erasure gate is taken and an erased actor is written as
// NULL. Every caller already holds that gate, taken before any index row its
// completion locks (lockCommunityAction for a local action and the sweep;
// gateRemovalIntentActors for a firehose copy), so this re-acquires a lock the
// transaction holds and waits on nothing.
func resolveCommunityReportsTx(ctx context.Context, tx *sql.Tx, communityDID, subjectURI, actorDID string) error {
	if actorDID != "" {
		erased, err := erasureGateTx(ctx, tx, actorDID)
		if err != nil {
			return fmt.Errorf("resolve community reports on %s: %w", subjectURI, err)
		}
		if erased {
			actorDID = ""
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE admin_reports r
		SET status = 'resolved', resolved_by = NULLIF($3, ''), resolved_at = NOW()
		WHERE r.target_uri = $2 AND `+communityReportSQL("r", "$1"),
		communityDID, subjectURI, actorDID); err != nil {
		return fmt.Errorf("resolve community reports on %s: %w", subjectURI, err)
	}
	return nil
}

// gateRemovalIntentActors takes, in ascending DID order, the shared erasure
// gates of the actors of the open removal intents on removalURI, so a
// firehose copy that completes one of them holds its actor's gate before it
// locks any index row an erasure deletes (the post's removal record and
// admission rows, the comment rows). The caller holds removalURI's rev-gate
// row, which every writer of an intent on it holds too, so no intent opens or
// settles on it while the caller runs and the plain read is the set its
// completion can pick from.
func gateRemovalIntentActors(ctx context.Context, tx *sql.Tx, removalURI string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT actor_did FROM community_write_intents
		WHERE target_uri = $1 AND state = 'open' AND op IN ('create', 'update') AND actor_did <> ''
	`, removalURI)
	if err != nil {
		return fmt.Errorf("read the actors of the open removal intents on %s: %w", removalURI, err)
	}
	var actors []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan an open removal intent actor on %s: %w", removalURI, err)
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read the actors of the open removal intents on %s: %w", removalURI, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close the open removal intent actors on %s: %w", removalURI, err)
	}
	if _, err := gateRoleSubjects(ctx, tx, actors...); err != nil {
		return err
	}
	return nil
}
