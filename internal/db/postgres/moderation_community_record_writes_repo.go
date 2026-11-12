package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"

	"github.com/lib/pq"
)

// IsHostedCommunity is credential presence: this AppView stores the
// community's PDS refresh token. hosted_by_did is never consulted, because a
// firehose-indexed community copies it from its own profile record (see
// posts.NewCommunityRepoFactory).
func (t *moderationTransaction) IsHostedCommunity(ctx context.Context, communityDID string) (bool, error) {
	var hosted bool
	err := t.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM communities
		    WHERE did = $1 AND pds_refresh_token_encrypted IS NOT NULL
		      AND octet_length(pds_refresh_token_encrypted) > 0)
	`, communityDID).Scan(&hosted)
	if err != nil {
		return false, fmt.Errorf("check community hosting: %w", err)
	}
	return hosted, nil
}

// RequireCommunityRecordRemoval inserts the subject's row or takes it over for
// write.RemovalActionID. What the row records about the repo is kept; the
// claim is kept too, so an attempt in flight still persists what it wrote.
func (t *moderationTransaction) RequireCommunityRecordRemoval(ctx context.Context, write moderation.CommunityRecordWrite) error {
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO moderation_community_record_writes (
		    community_did, subject_uri, subject_collection, subject_cid,
		    removal_action_id, code, removal_created_at, desired)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'removed')
		ON CONFLICT (community_did, subject_uri) DO UPDATE SET
		    subject_collection = EXCLUDED.subject_collection,
		    subject_cid = EXCLUDED.subject_cid,
		    removal_action_id = EXCLUDED.removal_action_id,
		    code = EXCLUDED.code,
		    removal_created_at = EXCLUDED.removal_created_at,
		    desired = 'removed', restore_acceptance_cid = NULL,
		    state = 'pending', attempts = 0, last_failure_code = NULL, next_attempt_at = NOW(),
		    generation = moderation_community_record_writes.generation + 1,
		    updated_at = NOW()
	`, write.CommunityDID, write.SubjectURI, write.SubjectCollection, write.SubjectCID,
		write.RemovalActionID, string(write.Code), write.RemovalCreatedAt)
	if err != nil {
		return fmt.Errorf("require community record removal: %w", err)
	}
	return nil
}

// RequestCommunityRecordRestore turns the row into a restore when
// removalActionID owns it, and reports whether it did.
func (t *moderationTransaction) RequestCommunityRecordRestore(ctx context.Context, communityDID, subjectURI, removalActionID, acceptanceCID string) (bool, error) {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE moderation_community_record_writes SET
		    desired = 'restored', restore_acceptance_cid = NULLIF($4, ''),
		    state = 'pending', attempts = 0, last_failure_code = NULL, next_attempt_at = NOW(),
		    generation = generation + 1, updated_at = NOW()
		WHERE community_did = $1 AND subject_uri = $2 AND removal_action_id = $3
	`, communityDID, subjectURI, removalActionID, acceptanceCID)
	if err != nil {
		return false, fmt.Errorf("request community record restore: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("request community record restore: %w", err)
	}
	return changed > 0, nil
}

// ClaimDueCommunityRecordWrites leases due community record write rows. A row
// is due when its next attempt is at or before claim.DueAt, and free when its
// lease ended at or before claim.DueAt. A new lease runs claim.Lease from the
// later of DueAt and the database clock, so a row claimed late in a sweep whose
// cutoff is frozen still gets its whole lease. Locked rows are skipped, not
// waited on, so concurrent claimers never share a row.
func (r *ModerationRepository) ClaimDueCommunityRecordWrites(ctx context.Context, claim moderation.CommunityRecordWriteClaim) ([]moderation.CommunityRecordWrite, error) {
	if claim.Limit <= 0 {
		return nil, nil
	}
	var onlyCommunity, onlySubject string
	if claim.Subject != nil {
		onlyCommunity, onlySubject = claim.Subject.CommunityDID, claim.Subject.SubjectURI
	}
	var excludedCommunities, excludedSubjects []string
	for _, subject := range claim.ExcludedSubjects() {
		excludedCommunities = append(excludedCommunities, subject.CommunityDID)
		excludedSubjects = append(excludedSubjects, subject.SubjectURI)
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH due AS (
		    SELECT community_did, subject_uri FROM moderation_community_record_writes
		    WHERE state = 'pending' AND next_attempt_at <= $1
		      AND (lease_expires_at IS NULL OR lease_expires_at <= $1)
		      AND (NOT $2 OR (community_did = $3 AND subject_uri = $4))
		      AND (community_did, subject_uri) NOT IN (
		          SELECT community_did, subject_uri
		          FROM unnest($5::text[], $6::text[]) AS excluded(community_did, subject_uri))
		    ORDER BY next_attempt_at, community_did, subject_uri
		    LIMIT $7 FOR UPDATE SKIP LOCKED
		)
		UPDATE moderation_community_record_writes AS w SET
		    lease_expires_at = GREATEST($1::timestamptz, NOW()) + make_interval(secs => $8::double precision),
		    claim = w.claim + 1, updated_at = NOW()
		FROM due
		WHERE w.community_did = due.community_did AND w.subject_uri = due.subject_uri
		RETURNING w.community_did, w.subject_uri, w.subject_collection, w.subject_cid,
		    w.removal_action_id, w.code, w.removal_created_at, w.desired,
		    COALESCE(w.restore_acceptance_cid, ''), COALESCE(w.owned_removal_cid, ''),
		    w.acceptance_seen, w.withdrew_acceptance, w.last_write_started_at,
		    COALESCE(w.last_write_subject_cid, ''), COALESCE(w.last_write_code, ''),
		    COALESCE(w.last_write_record_created_at, ''), COALESCE(w.restore_stamp_rev, ''),
		    COALESCE(w.restore_stamp_acceptance_uri, ''), COALESCE(w.restore_stamp_acceptance_rkey, ''),
		    COALESCE(w.restore_stamp_pinned_cid, ''),
		    w.state = 'completed', w.attempts, w.next_attempt_at,
		    COALESCE(w.last_failure_code, ''), w.generation, w.claim
	`, claim.DueAt, claim.Subject != nil, onlyCommunity, onlySubject,
		pq.Array(excludedCommunities), pq.Array(excludedSubjects), claim.Limit, claim.Lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim due community record writes: %w", err)
	}
	defer rows.Close()
	var writes []moderation.CommunityRecordWrite
	for rows.Next() {
		var write moderation.CommunityRecordWrite
		var code, desired, lastWriteCode string
		var startedAt sql.NullTime
		if err := rows.Scan(&write.CommunityDID, &write.SubjectURI, &write.SubjectCollection, &write.SubjectCID,
			&write.RemovalActionID, &code, &write.RemovalCreatedAt, &desired,
			&write.RestoreAcceptanceCID, &write.OwnedRemovalCID,
			&write.AcceptanceSeen, &write.WithdrewAcceptance, &startedAt,
			&write.LastWriteSubjectCID, &lastWriteCode, &write.LastWriteRecordCreatedAt, &write.RestoreStampRev,
			&write.RestoreStampAcceptanceURI, &write.RestoreStampAcceptanceRkey, &write.RestoreStampPinnedCID,
			&write.Completed, &write.Attempts, &write.NextAttemptAt,
			&write.LastFailureCode, &write.Generation, &write.Claim); err != nil {
			return nil, fmt.Errorf("scan claimed community record write: %w", err)
		}
		write.Code = posts.DecisionCode(code)
		write.LastWriteCode = posts.DecisionCode(lastWriteCode)
		write.Desired = moderation.CommunityRecordDesired(desired)
		if startedAt.Valid {
			started := startedAt.Time
			write.LastWriteStartedAt = &started
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claimed community record writes: %w", err)
	}
	return writes, nil
}

// recordWriteProgressSet is the SET list that persists an attempt's progress,
// from parameters $4 to $14 of recordWriteProgressArgs.
const recordWriteProgressSet = `
    owned_removal_cid = NULLIF($4, ''), acceptance_seen = $5, withdrew_acceptance = $6,
    last_write_started_at = $7,
    last_write_subject_cid = NULLIF($8, ''), last_write_code = NULLIF($9, ''),
    last_write_record_created_at = NULLIF($10, ''),
    restore_stamp_rev = NULLIF($11, ''), restore_stamp_acceptance_uri = NULLIF($12, ''),
    restore_stamp_acceptance_rkey = NULLIF($13, ''), restore_stamp_pinned_cid = NULLIF($14, ''),
    updated_at = NOW()`

// recordWriteProgressArgs is the row key and claim ($1 to $3), the progress
// ($4 to $14), then extra from $15.
func recordWriteProgressArgs(write moderation.CommunityRecordWrite, extra ...any) []any {
	return append([]any{
		write.CommunityDID, write.SubjectURI, write.Claim,
		write.OwnedRemovalCID, write.AcceptanceSeen, write.WithdrewAcceptance, nullableRecordWriteTime(write.LastWriteStartedAt),
		write.LastWriteSubjectCID, string(write.LastWriteCode), write.LastWriteRecordCreatedAt,
		write.RestoreStampRev, write.RestoreStampAcceptanceURI, write.RestoreStampAcceptanceRkey, write.RestoreStampPinnedCID,
	}, extra...)
}

// execUnderClaim runs a progress update and reports
// moderation.ErrCommunityRecordClaimLost when it changed no row: the claim
// passed to another attempt.
func (r *ModerationRepository) execUnderClaim(ctx context.Context, operation, query string, write moderation.CommunityRecordWrite, args []any) error {
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if changed == 0 {
		return fmt.Errorf("%s community=%s subject=%s claim=%d: %w",
			operation, write.CommunityDID, write.SubjectURI, write.Claim, moderation.ErrCommunityRecordClaimLost)
	}
	return nil
}

// SaveCommunityRecordWriteProgress persists an attempt's progress under its claim.
func (r *ModerationRepository) SaveCommunityRecordWriteProgress(ctx context.Context, write moderation.CommunityRecordWrite) error {
	return r.execUnderClaim(ctx, "save community record write progress", `
		UPDATE moderation_community_record_writes SET`+recordWriteProgressSet+`
		WHERE community_did = $1 AND subject_uri = $2 AND claim = $3
	`, write, recordWriteProgressArgs(write))
}

// CompleteCommunityRecordWrite persists the attempt's progress under its claim
// and releases the lease; the row completes only if its generation is still
// the one the attempt claimed.
func (r *ModerationRepository) CompleteCommunityRecordWrite(ctx context.Context, write moderation.CommunityRecordWrite) error {
	return r.execUnderClaim(ctx, "complete community record write", `
		UPDATE moderation_community_record_writes SET`+recordWriteProgressSet+`,
		    state = CASE WHEN generation = $15 THEN 'completed' ELSE state END,
		    lease_expires_at = NULL
		WHERE community_did = $1 AND subject_uri = $2 AND claim = $3
	`, write, recordWriteProgressArgs(write, write.Generation))
}

// RescheduleCommunityRecordWrite persists the attempt's progress under its
// claim and releases the lease. If the generation is unchanged it makes the
// row due at nextAttemptAt, and a non-empty failureCode counts a failed
// attempt; a row whose desired state changed stays due now with its new
// generation's attempts and failure code untouched.
func (r *ModerationRepository) RescheduleCommunityRecordWrite(ctx context.Context, write moderation.CommunityRecordWrite, nextAttemptAt time.Time, failureCode string) error {
	return r.execUnderClaim(ctx, "reschedule community record write", `
		UPDATE moderation_community_record_writes SET`+recordWriteProgressSet+`,
		    next_attempt_at = CASE WHEN generation = $15 THEN $16 ELSE next_attempt_at END,
		    attempts = attempts + CASE WHEN generation = $15 AND $17::text <> '' THEN 1 ELSE 0 END,
		    last_failure_code = CASE WHEN generation = $15 AND $17::text <> '' THEN $17 ELSE last_failure_code END,
		    lease_expires_at = NULL
		WHERE community_did = $1 AND subject_uri = $2 AND claim = $3
	`, write, recordWriteProgressArgs(write, write.Generation, nextAttemptAt, failureCode))
}

// nullableRecordWriteTime is a nil time as SQL NULL.
func nullableRecordWriteTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
