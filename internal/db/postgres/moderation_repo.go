package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/embeds"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ModerationRepository is the Postgres moderation.Store.
type ModerationRepository struct {
	db *sql.DB
}

// NewModerationRepository builds the Postgres moderation store.
func NewModerationRepository(db *sql.DB) *ModerationRepository {
	return &ModerationRepository{db: db}
}

// InTransaction runs fn in one transaction.
func (r *ModerationRepository) InTransaction(ctx context.Context, fn func(ctx context.Context, tx moderation.Transaction) error) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := fn(ctx, &moderationTransaction{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// SubjectModeration reads the stored moderation state of a subject.
func (r *ModerationRepository) SubjectModeration(ctx context.Context, authorityDID, subjectURI string) (*moderation.SubjectModeration, error) {
	var version int64
	row := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(s.version, 0),
		       a.id, a.actor_did, a.authority_did, a.scope_kind, a.scope_community_did,
		       a.subject_uri, a.subject_collection, a.subject_community_did, a.observed_cid,
		       a.action, a.label_value, a.reason, a.private_classification, a.private_note,
		       a.reverses_action_id, a.origin, a.created_at
		FROM (SELECT $2::text AS uri) AS target
		LEFT JOIN moderation_subjects s ON s.subject_uri = target.uri
		LEFT JOIN moderation_decisions d ON d.subject_uri = target.uri
		    AND d.authority_did = $1 AND d.scope_kind = 'instance'
		    AND d.kind = 'removal' AND d.active
		LEFT JOIN moderation_actions a ON a.id = d.active_action_id
	`, authorityDID, subjectURI)
	action, err := scanModerationAction(row, &version)
	if err != nil {
		return nil, err
	}
	return &moderation.SubjectModeration{Version: version, ActiveRemoval: action}, nil
}

type moderationTransaction struct {
	tx *sql.Tx
}

var moderationActionClock = newModerationActionClock()

func newModerationActionClock() *syntax.TIDClock {
	var clockID [2]byte
	if _, err := rand.Read(clockID[:]); err != nil {
		panic(fmt.Sprintf("moderation action clock: %v", err))
	}
	return syntax.NewTIDClock(uint(binary.BigEndian.Uint16(clockID[:]) % 1024))
}

func (t *moderationTransaction) LockActor(ctx context.Context, actorDID string) error {
	// One lock per actor serializes admission for distinct idempotency keys as
	// well as replays. Namespace the hash away from other advisory lock users.
	digest := sha256.Sum256([]byte("coves/moderation-actor/" + actorDID))
	key := int64(binary.BigEndian.Uint64(digest[:8]))
	_, err := t.tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key)
	return err
}

func (t *moderationTransaction) LiveIdempotencyRecord(ctx context.Context, actorDID, authorityDID, key string, now time.Time) (*moderation.IdempotencyRecord, error) {
	var record moderation.IdempotencyRecord
	var storedResult []byte
	err := t.tx.QueryRowContext(ctx, `
		SELECT fingerprint, stored_result, created_at, expires_at
		FROM moderation_idempotency_keys
		WHERE actor_did = $1 AND authority_did = $2 AND key = $3 AND expires_at > $4
	`, actorDID, authorityDID, key, now).Scan(&record.Fingerprint, &storedResult, &record.CreatedAt, &record.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(storedResult, &record.Result); err != nil {
		return nil, fmt.Errorf("decode stored moderation result: %w", err)
	}
	record.ActorDID, record.AuthorityDID, record.Key = actorDID, authorityDID, key
	return &record, nil
}

func (t *moderationTransaction) CountLiveIdempotencyKeys(ctx context.Context, actorDID string, now time.Time) (int, error) {
	var count int
	err := t.tx.QueryRowContext(ctx, `
		SELECT count(*) FROM moderation_idempotency_keys WHERE actor_did = $1 AND expires_at > $2
	`, actorDID, now).Scan(&count)
	return count, err
}

func (t *moderationTransaction) SaveIdempotencyRecord(ctx context.Context, record moderation.IdempotencyRecord) error {
	result, err := json.Marshal(record.Result)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx, `
		INSERT INTO moderation_idempotency_keys
		    (actor_did, authority_did, key, fingerprint, stored_result, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (actor_did, authority_did, key) DO UPDATE SET
		    fingerprint = EXCLUDED.fingerprint, stored_result = EXCLUDED.stored_result,
		    created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at
		WHERE moderation_idempotency_keys.expires_at <= EXCLUDED.created_at
	`, record.ActorDID, record.AuthorityDID, record.Key, record.Fingerprint, result, record.CreatedAt, record.ExpiresAt)
	return err
}

func (t *moderationTransaction) LockSubject(ctx context.Context, subjectURI string) (int64, error) {
	if _, err := t.tx.ExecContext(ctx, `
		INSERT INTO moderation_subjects (subject_uri, version) VALUES ($1, 0)
		ON CONFLICT (subject_uri) DO NOTHING
	`, subjectURI); err != nil {
		return 0, err
	}
	var version int64
	err := t.tx.QueryRowContext(ctx, `
		SELECT version FROM moderation_subjects WHERE subject_uri = $1 FOR UPDATE
	`, subjectURI).Scan(&version)
	return version, err
}

func (t *moderationTransaction) ReadIndexedComment(ctx context.Context, subjectURI string) (*moderation.IndexedComment, error) {
	var comment moderation.IndexedComment
	var communityDID, embed sql.NullString
	err := t.tx.QueryRowContext(ctx, `
		SELECT c.uri, c.cid, c.commenter_did,
		       (c.deleted_at IS NOT NULL AND c.deletion_reason = 'author'),
		       p.community_did, c.embed
		FROM comments c
		LEFT JOIN posts p ON p.uri = c.root_uri
		WHERE c.uri = $1 FOR SHARE OF c
	`, subjectURI).Scan(&comment.URI, &comment.CID, &comment.OwnerDID,
		&comment.AuthorDeleted, &communityDID, &embed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, moderation.ErrSubjectNotIndexed
	}
	if err != nil {
		return nil, err
	}
	comment.CommunityDID = communityDID.String
	if embed.Valid {
		// The embed is author-controlled and stored unvalidated. A shape the
		// comment view cannot serve images from has no images to block, and
		// must never make the comment impossible to moderate.
		var decoded any
		if err := json.Unmarshal([]byte(embed.String), &decoded); err == nil {
			if object, isObject := decoded.(map[string]any); isObject {
				comment.ImageCIDs = embeds.CommentImageCIDs(object)
			}
		}
	}
	return &comment, nil
}

const moderationActionColumns = `
	a.id, a.actor_did, a.authority_did, a.scope_kind, a.scope_community_did,
	a.subject_uri, a.subject_collection, a.subject_community_did, a.observed_cid,
	a.action, a.label_value, a.reason, a.private_classification, a.private_note,
	a.reverses_action_id, a.origin, a.created_at`

type moderationRow interface {
	Scan(dest ...any) error
}

func scanModerationAction(row moderationRow, version *int64) (*moderation.Action, error) {
	var id, actorDID, authorityDID, scopeKind, scopeCommunityDID sql.NullString
	var subjectURI, subjectCollection, subjectCommunityDID, observedCID sql.NullString
	var actionKind, labelValue, reason, privateClassification, privateNote sql.NullString
	var reversesActionID, origin sql.NullString
	var createdAt sql.NullTime
	dest := []any{
		&id, &actorDID, &authorityDID, &scopeKind, &scopeCommunityDID,
		&subjectURI, &subjectCollection, &subjectCommunityDID, &observedCID,
		&actionKind, &labelValue, &reason, &privateClassification, &privateNote,
		&reversesActionID, &origin, &createdAt,
	}
	if version != nil {
		dest = append([]any{version}, dest...)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if !id.Valid {
		return nil, nil
	}
	return &moderation.Action{
		ID: id.String, ActorDID: actorDID.String, AuthorityDID: authorityDID.String,
		ScopeKind: scopeKind.String, ScopeCommunityDID: scopeCommunityDID.String,
		SubjectURI: subjectURI.String, SubjectCollection: subjectCollection.String,
		SubjectCommunityDID: subjectCommunityDID.String, ObservedCID: observedCID.String,
		Action: actionKind.String, LabelValue: labelValue.String, Reason: reason.String,
		PrivateNote: privateNote.String, ReversesActionID: reversesActionID.String,
		Origin: origin.String, CreatedAt: createdAt.Time,
	}, nil
}

func (t *moderationTransaction) GetAction(ctx context.Context, actionID string) (*moderation.Action, error) {
	action, err := scanModerationAction(t.tx.QueryRowContext(ctx,
		`SELECT `+moderationActionColumns+` FROM moderation_actions a WHERE a.id = $1`, actionID), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, moderation.ErrDecisionNotFound
	}
	return action, err
}

// ActiveRemoval reads the active removal and share-locks its decision row
// until the transaction ends. The comment consumer reconciles media blocks
// through this read, and when the comment row is absent (purged, then
// recreated by a fresh insert) no content-row lock orders it against a
// restore. The share lock does: a restore that already deactivated the
// decision makes this read wait and then re-check d.active against the
// committed row, and a restore that arrives later waits at its decision
// update for this transaction's blocks to commit, so its later
// DeactivateMediaBlocks sees them. Mutations take it after the subject lock
// and the consumer after its own row write; neither then waits on a lock the
// other holds, so the order cannot deadlock.
func (t *moderationTransaction) ActiveRemoval(ctx context.Context, authorityDID, subjectURI string) (*moderation.Action, error) {
	action, err := scanModerationAction(t.tx.QueryRowContext(ctx, `
		SELECT `+moderationActionColumns+` FROM moderation_actions a
		JOIN moderation_decisions d ON d.active_action_id = a.id
		WHERE d.authority_did = $1 AND d.subject_uri = $2
		  AND d.scope_kind = 'instance' AND d.kind = 'removal' AND d.active
		FOR SHARE OF d
	`, authorityDID, subjectURI), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return action, err
}

func (t *moderationTransaction) InsertAction(ctx context.Context, action moderation.Action) (*moderation.Action, error) {
	action.ID = moderationActionClock.Next().String()
	// Postgres timestamps have microsecond precision. Return the same instant
	// that will be read back, without a monotonic clock or local time zone.
	action.CreatedAt = action.CreatedAt.UTC().Truncate(time.Microsecond)
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, observed_cid,
		     action, label_value, reason, private_note, reverses_action_id, origin, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), NULLIF($9, ''),
		        $10, NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''), $15, $16)
	`, action.ID, action.ActorDID, action.AuthorityDID, action.ScopeKind, action.ScopeCommunityDID,
		action.SubjectURI, action.SubjectCollection, action.SubjectCommunityDID, action.ObservedCID,
		action.Action, action.LabelValue, action.Reason, action.PrivateNote, action.ReversesActionID,
		action.Origin, action.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &action, nil
}

func (t *moderationTransaction) SetRemovalDecision(ctx context.Context, authorityDID, subjectURI, actionID string, active bool) error {
	if !active {
		result, err := t.tx.ExecContext(ctx, `
			UPDATE moderation_decisions SET active = FALSE
			WHERE authority_did = $1 AND subject_uri = $2 AND scope_kind = 'instance'
			  AND kind = 'removal' AND active_action_id = $3 AND active
		`, authorityDID, subjectURI, actionID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("active moderation decision not found for restore")
		}
		return nil
	}
	_, err := t.tx.ExecContext(ctx, `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)
		ON CONFLICT ON CONSTRAINT moderation_decisions_key DO UPDATE SET
		    active_action_id = EXCLUDED.active_action_id, active = TRUE
	`, authorityDID, subjectURI, actionID)
	return err
}

func (t *moderationTransaction) SetSubjectVersion(ctx context.Context, subjectURI string, version int64) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE moderation_subjects SET version = $2, updated_at = NOW() WHERE subject_uri = $1
	`, subjectURI, version)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("moderation subject version row not found")
	}
	return nil
}

func (t *moderationTransaction) InsertMediaBlocks(ctx context.Context, blocks []moderation.MediaBlock) error {
	_, err := t.InsertNewMediaBlocks(ctx, blocks)
	return err
}

// InsertNewMediaBlocks returns only blocks inserted by this transaction, for
// post-commit cache purging. The NULLS NOT DISTINCT constraint deduplicates
// ownerless blocks as well as owner-scoped blocks.
func (t *moderationTransaction) InsertNewMediaBlocks(ctx context.Context, blocks []moderation.MediaBlock) ([]moderation.MediaBlock, error) {
	var inserted []moderation.MediaBlock
	for _, block := range blocks {
		var id int64
		err := t.tx.QueryRowContext(ctx, `
			INSERT INTO moderation_media_blocks (owner_did, blob_cid, action_id, active)
			VALUES (NULLIF($1, ''), $2, $3, TRUE)
			ON CONFLICT ON CONSTRAINT moderation_media_block_key DO NOTHING
			RETURNING id
		`, block.OwnerDID, block.BlobCID, block.ActionID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		inserted = append(inserted, block)
	}
	return inserted, nil
}

func (t *moderationTransaction) DeactivateMediaBlocks(ctx context.Context, actionID string) error {
	_, err := t.tx.ExecContext(ctx, `
		UPDATE moderation_media_blocks SET active = FALSE WHERE action_id = $1 AND active
	`, actionID)
	return err
}

// DeleteExpiredIdempotencyKeys deletes idempotency rows expired at now.
func (r *ModerationRepository) DeleteExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, `DELETE FROM moderation_idempotency_keys WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// IsBlocked reports whether an active media block covers the owner's blob.
func (r *ModerationRepository) IsBlocked(ctx context.Context, ownerDID, cid string) (bool, error) {
	var blocked bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM moderation_media_blocks
			WHERE owner_did = $1 AND blob_cid = $2 AND active
		) OR EXISTS (
			SELECT 1 FROM moderation_media_blocks
			WHERE owner_did IS NULL AND blob_cid = $2 AND active
		)
	`, ownerDID, cid).Scan(&blocked)
	return blocked, err
}

// ListActiveBlockedBlobs lists every blob an active media block covers, once
// each. An every-owner block comes back with an empty OwnerDID.
func (r *ModerationRepository) ListActiveBlockedBlobs(ctx context.Context) ([]imageproxy.BlockedBlob, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT COALESCE(owner_did, ''), blob_cid FROM moderation_media_blocks WHERE active
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blobs []imageproxy.BlockedBlob
	for rows.Next() {
		var blob imageproxy.BlockedBlob
		if err := rows.Scan(&blob.OwnerDID, &blob.CID); err != nil {
			return nil, err
		}
		blobs = append(blobs, blob)
	}
	return blobs, rows.Err()
}

// BindTransaction binds media reconciliation operations to tx.
func (r *ModerationRepository) BindTransaction(tx *sql.Tx) moderation.MediaTransaction {
	return &moderationTransaction{tx: tx}
}
