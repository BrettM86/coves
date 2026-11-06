package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/communities"
)

// CommunityBanRepository is the Postgres community ban index.
type CommunityBanRepository struct {
	db *sql.DB
}

// NewCommunityBanRepository builds the ban index repository.
func NewCommunityBanRepository(db *sql.DB) *CommunityBanRepository {
	return &CommunityBanRepository{db: db}
}

// GetCommunityBan returns the indexed ban on subjectDID in communityDID, or
// communities.ErrCommunityBanNotFound. It does not apply expiry; see
// communities.ActiveCommunityBan.
func (r *CommunityBanRepository) GetCommunityBan(ctx context.Context, communityDID, subjectDID string) (*communities.CommunityBan, error) {
	ban := communities.CommunityBan{}
	var reason sql.NullString
	var expiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT community_did, subject_did, record_key, record_cid, reason, expires_at, indexed_at
		FROM community_bans
		WHERE community_did = $1 AND subject_did = $2
	`, communityDID, subjectDID).Scan(
		&ban.CommunityDID, &ban.SubjectDID, &ban.RecordKey, &ban.RecordCID, &reason, &expiresAt, &ban.IndexedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, communities.ErrCommunityBanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get community ban: %w", err)
	}
	ban.Reason = reason.String
	if expiresAt.Valid {
		expiry := expiresAt.Time
		ban.ExpiresAt = &expiry
	}
	return &ban, nil
}

// UpsertCommunityBan indexes a ban, overwriting any earlier ban on the same
// subject in the same community. The ban history records it at a new Coves
// index order, and an overwritten ban is lifted at that same order.
func (r *CommunityBanRepository) UpsertCommunityBan(ctx context.Context, ban communities.CommunityBan) error {
	var expiresAt sql.NullTime
	if ban.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: ban.ExpiresAt.UTC(), Valid: true}
	}
	return r.inTransaction(ctx, func(tx *sql.Tx) error {
		if err := lockBanSubject(ctx, tx, ban.SubjectDID); err != nil {
			return err
		}
		var order int64
		if err := tx.QueryRowContext(ctx, `SELECT nextval('coves_index_order')`).Scan(&order); err != nil {
			return fmt.Errorf("draw ban index order: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE community_ban_history SET lifted_order = $3
			WHERE community_did = $1 AND subject_did = $2 AND lifted_order IS NULL
		`, ban.CommunityDID, ban.SubjectDID, order); err != nil {
			return fmt.Errorf("lift overwritten community ban: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO community_ban_history
			    (community_did, subject_did, record_key, record_cid, expires_at, index_order)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, ban.CommunityDID, ban.SubjectDID, ban.RecordKey, ban.RecordCID, expiresAt, order); err != nil {
			return fmt.Errorf("record community ban history: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO community_bans
			    (community_did, subject_did, record_key, record_cid, reason, expires_at, indexed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (community_did, subject_did) DO UPDATE SET
			    record_key = EXCLUDED.record_key,
			    record_cid = EXCLUDED.record_cid,
			    reason = EXCLUDED.reason,
			    expires_at = EXCLUDED.expires_at,
			    indexed_at = EXCLUDED.indexed_at
		`, ban.CommunityDID, ban.SubjectDID, ban.RecordKey, ban.RecordCID, nullString(ban.Reason), expiresAt, time.Now().UTC()); err != nil {
			return fmt.Errorf("upsert community ban: %w", err)
		}
		return nil
	})
}

// DeleteCommunityBan removes the ban recorded at recordKey in communityDID's
// repo and lifts it in the ban history at a new Coves index order. Deleting a
// ban that is not indexed is not an error.
func (r *CommunityBanRepository) DeleteCommunityBan(ctx context.Context, communityDID, recordKey string) error {
	return r.inTransaction(ctx, func(tx *sql.Tx) error {
		// The subject's lock comes before the lift's index order is drawn; the
		// lift below re-checks lifted_order IS NULL after it.
		var subjectDID string
		err := tx.QueryRowContext(ctx, `
			SELECT subject_did FROM community_ban_history
			WHERE community_did = $1 AND record_key = $2 AND lifted_order IS NULL
		`, communityDID, recordKey).Scan(&subjectDID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find deleted community ban subject: %w", err)
		}
		if err == nil {
			if err := lockBanSubject(ctx, tx, subjectDID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE community_ban_history SET lifted_order = nextval('coves_index_order')
				WHERE community_did = $1 AND record_key = $2 AND subject_did = $3 AND lifted_order IS NULL
			`, communityDID, recordKey, subjectDID); err != nil {
				return fmt.Errorf("lift deleted community ban: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM community_bans WHERE community_did = $1 AND record_key = $2
		`, communityDID, recordKey); err != nil {
			return fmt.Errorf("delete community ban: %w", err)
		}
		return nil
	})
}

// lockBanSubject takes the ban-subject advisory lock on subjectDID
// (communities.BanSubjectLockKeySQL). Its holders: ban indexing; comment, vote
// and post indexing, on the author before drawing an index order; moderation
// LockSubject on at://<did>; userRepo.Create and communityRepo.Create.
func lockBanSubject(ctx context.Context, tx *sql.Tx, subjectDID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, subjectDID); err != nil {
		return fmt.Errorf("lock community ban subject %s: %w", subjectDID, err)
	}
	return nil
}

func (r *CommunityBanRepository) inTransaction(ctx context.Context, apply func(tx *sql.Tx) error) (err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin community ban transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ContentCommunity returns the community of an indexed post, or of the post a
// comment's parent chain reaches. The comment's own root_uri is chosen by the
// commenter and is not used. found is false when the content, or any link of
// its chain, is not indexed.
func (r *CommunityBanRepository) ContentCommunity(ctx context.Context, contentURI string) (string, bool, error) {
	var communityDID sql.NullString
	err := r.db.QueryRowContext(ctx, `
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
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve community of %s: %w", contentURI, err)
	}
	if !communityDID.Valid {
		return "", false, nil
	}
	return communityDID.String, true, nil
}
