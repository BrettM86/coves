package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"Coves/internal/core/moderation"
)

// RoleRecordDeleteRepository is the Postgres store of pending role record
// deletes (migration 086).
type RoleRecordDeleteRepository struct {
	db *sql.DB
}

var _ moderation.RoleRecordDeleteStore = (*RoleRecordDeleteRepository)(nil)

// NewRoleRecordDeleteRepository builds the role record delete store.
func NewRoleRecordDeleteRepository(db *sql.DB) *RoleRecordDeleteRepository {
	return &RoleRecordDeleteRepository{db: db}
}

// enqueueRoleRecordDelete records, inside tx, that the role record at
// (communityDID, recordKey) is to be deleted from the community's repo when it
// still carries recordCID. A key already pending takes the new CID, is due
// now with no attempts counted and no lease, and moves to a new generation so
// a claim taken before cannot finish it.
func enqueueRoleRecordDelete(ctx context.Context, tx *sql.Tx, communityDID, recordKey, recordCID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO community_role_record_deletes (community_did, record_key, record_cid)
		VALUES ($1, $2, $3)
		ON CONFLICT (community_did, record_key) DO UPDATE SET
		    record_cid = EXCLUDED.record_cid,
		    attempts = 0, next_attempt_at = NOW(), lease_expires_at = NULL,
		    last_failure_code = NULL,
		    generation = community_role_record_deletes.generation + 1,
		    updated_at = NOW()
	`, communityDID, recordKey, recordCID); err != nil {
		return fmt.Errorf("enqueue role record delete in %s: %w", communityDID, err)
	}
	return nil
}

// ClaimDueRoleRecordDeletes leases up to claim.Limit rows due at claim.DueAt
// whose lease has passed, counting a new claim on each.
func (r *RoleRecordDeleteRepository) ClaimDueRoleRecordDeletes(ctx context.Context, claim moderation.RoleRecordDeleteClaim) ([]moderation.RoleRecordDelete, error) {
	if claim.Limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH due AS (
		    SELECT id FROM community_role_record_deletes
		    WHERE next_attempt_at <= $1
		      AND (lease_expires_at IS NULL OR lease_expires_at <= $1)
		    ORDER BY next_attempt_at, id
		    LIMIT $2 FOR UPDATE SKIP LOCKED
		)
		UPDATE community_role_record_deletes AS d SET
		    lease_expires_at = GREATEST($1::timestamptz, NOW()) + make_interval(secs => $3::double precision),
		    claim = d.claim + 1, updated_at = NOW()
		FROM due
		WHERE d.id = due.id
		RETURNING d.id, d.community_did, d.record_key, d.record_cid, d.attempts, d.next_attempt_at,
		    COALESCE(d.last_failure_code, ''), d.generation, d.claim
	`, claim.DueAt, claim.Limit, claim.Lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim due role record deletes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var claimed []moderation.RoleRecordDelete
	for rows.Next() {
		var d moderation.RoleRecordDelete
		if err := rows.Scan(&d.ID, &d.CommunityDID, &d.RecordKey, &d.RecordCID, &d.Attempts, &d.NextAttemptAt,
			&d.LastFailureCode, &d.Generation, &d.Claim); err != nil {
			return nil, fmt.Errorf("scan claimed role record delete: %w", err)
		}
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim due role record deletes: %w", err)
	}
	return claimed, nil
}

// CompleteRoleRecordDelete removes a finished row under its claim.
func (r *RoleRecordDeleteRepository) CompleteRoleRecordDelete(ctx context.Context, d moderation.RoleRecordDelete) error {
	return r.execUnderClaim(ctx, "complete role record delete", d, `
		DELETE FROM community_role_record_deletes
		WHERE id = $1 AND generation = $2 AND claim = $3`, d.ID, d.Generation, d.Claim)
}

// RescheduleRoleRecordDelete records a failed attempt under its claim.
func (r *RoleRecordDeleteRepository) RescheduleRoleRecordDelete(ctx context.Context, d moderation.RoleRecordDelete, nextAttemptAt time.Time, failureCode string) error {
	return r.execUnderClaim(ctx, "reschedule role record delete", d, `
		UPDATE community_role_record_deletes SET
		    attempts = attempts + 1, next_attempt_at = $4, last_failure_code = NULLIF($5, ''),
		    lease_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND generation = $2 AND claim = $3`, d.ID, d.Generation, d.Claim, nextAttemptAt, failureCode)
}

// execUnderClaim runs query and reports moderation.ErrRoleRecordDeleteClaimLost
// when it changed no row: the row was finished, re-enqueued or claimed again.
func (r *RoleRecordDeleteRepository) execUnderClaim(ctx context.Context, operation string, d moderation.RoleRecordDelete, query string, args ...any) error {
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if changed == 0 {
		return fmt.Errorf("%s community=%s id=%d claim=%d: %w",
			operation, d.CommunityDID, d.ID, d.Claim, moderation.ErrRoleRecordDeleteClaimLost)
	}
	return nil
}
