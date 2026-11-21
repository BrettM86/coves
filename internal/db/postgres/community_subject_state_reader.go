package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
)

// CommunitySubjectStateReader reads a subject's community moderation state and
// version.
type CommunitySubjectStateReader struct {
	db *sql.DB
}

// NewCommunitySubjectStateReader builds the community subject state reader.
func NewCommunitySubjectStateReader(db *sql.DB) *CommunitySubjectStateReader {
	return &CommunitySubjectStateReader{db: db}
}

var _ moderation.CommunitySubjectStateReader = (*CommunitySubjectStateReader)(nil)

// ReadCommunitySubjectState reads the subject's version and its kind's state
// in one REPEATABLE READ snapshot, so the version describes the state read
// with it. A subject with no version row is at version 0.
func (r *CommunitySubjectStateReader) ReadCommunitySubjectState(ctx context.Context, communityDID, subject string, kind moderation.CommunitySubjectKind) (*moderation.CommunityState, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin community subject state read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state := &moderation.CommunityState{CommunityDID: communityDID, Subject: subject, Kind: kind}
	err = tx.QueryRowContext(ctx, `
		SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $2
	`, communityDID, subject).Scan(&state.Version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read community subject version: %w", err)
	}

	switch kind {
	case moderation.CommunitySubjectPost:
		err = readAdmissionStatus(ctx, tx, state)
	case moderation.CommunitySubjectComment:
		err = readCommentRemoved(ctx, tx, state)
	case moderation.CommunitySubjectAccount:
		err = readInForceBan(ctx, tx, state)
	case moderation.CommunitySubjectRoster:
		err = readRoster(ctx, tx, state)
	default:
		return nil, fmt.Errorf("read community subject state: unknown subject kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("finish community subject state read: %w", err)
	}
	return state, nil
}

// readAdmissionStatus reads the post's admission status in the community, or
// "none" when it has no admission row there.
func readAdmissionStatus(ctx context.Context, tx *sql.Tx, state *moderation.CommunityState) error {
	err := tx.QueryRowContext(ctx, `
		SELECT status FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2
	`, state.CommunityDID, state.Subject).Scan(&state.AdmissionStatus)
	if errors.Is(err, sql.ErrNoRows) {
		state.AdmissionStatus = "none"
		return nil
	}
	if err != nil {
		return fmt.Errorf("read post admission status: %w", err)
	}
	return nil
}

// readCommentRemoved reads whether the community's removal of the comment is
// in effect.
func readCommentRemoved(ctx context.Context, tx *sql.Tx, state *moderation.CommunityState) error {
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM moderation_decisions
		    WHERE authority_did = $1 AND scope_kind = 'community' AND scope_community_did = $1
		      AND subject_uri = $2 AND kind = 'removal' AND active
		)
	`, state.CommunityDID, state.Subject).Scan(&state.CommentRemoved); err != nil {
		return fmt.Errorf("read comment removal: %w", err)
	}
	return nil
}

// readInForceBan reads the account's ban in the community when it is in
// force: it has no expiry or expires in the future.
func readInForceBan(ctx context.Context, tx *sql.Tx, state *moderation.CommunityState) error {
	ban := communities.CommunityBan{}
	var reason sql.NullString
	var expiresAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT community_did, subject_did, record_key, record_cid, reason, expires_at, indexed_at
		FROM community_bans
		WHERE community_did = $1 AND subject_did = $2 AND (expires_at IS NULL OR expires_at > NOW())
	`, state.CommunityDID, state.Subject).Scan(
		&ban.CommunityDID, &ban.SubjectDID, &ban.RecordKey, &ban.RecordCID, &reason, &expiresAt, &ban.IndexedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read community ban: %w", err)
	}
	ban.Reason = reason.String
	if expiresAt.Valid {
		expiry := expiresAt.Time
		ban.ExpiresAt = &expiry
	}
	state.Ban = &ban
	return nil
}

// readRoster resolves the community's role rows into its effective roster.
func readRoster(ctx context.Context, tx *sql.Tx, state *moderation.CommunityState) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+communityRoleColumns+`
		FROM community_roles roles
		WHERE roles.community_did = $1
		ORDER BY roles.record_key
	`, state.CommunityDID)
	if err != nil {
		return fmt.Errorf("read community roster: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var roles []communities.CommunityRole
	for rows.Next() {
		var role communities.CommunityRole
		if err := rows.Scan(&role.CommunityDID, &role.SubjectDID, &role.Role, &role.RecordKey,
			&role.RecordCID, &role.RecordCreatedAt, &role.IndexedAt); err != nil {
			return fmt.Errorf("scan community role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read community roster: %w", err)
	}
	roster := communities.EffectiveRoster(roles)
	state.Roster = &roster
	return nil
}
