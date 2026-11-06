package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/moderation"
)

// AccountBanReader reads an account's active instance ban decision made by
// this instance.
type AccountBanReader struct {
	db          *sql.DB
	instanceDID string
}

// NewAccountBanReader returns the account-ban read over db for bans whose
// authority is instanceDID.
func NewAccountBanReader(db *sql.DB, instanceDID string) *AccountBanReader {
	return &AccountBanReader{db: db, instanceDID: instanceDID}
}

// ActiveBan returns the account's active ban decision, or nil when there is
// none. An active decision whose expiry has passed is still returned: whether
// it is in force is Ban.InForce's call on the server's clock. Only a decision
// whose action records the account subject kind is a ban: a community removal
// records the community kind.
func (r *AccountBanReader) ActiveBan(ctx context.Context, accountDID string) (*accountbans.Ban, error) {
	var ban accountbans.Ban
	var reason sql.NullString
	var expiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT a.id, a.reason, a.expires_at
		FROM moderation_decisions d
		JOIN moderation_actions a ON a.id = d.active_action_id
		WHERE d.subject_uri = $1 AND d.authority_did = $2 AND d.scope_kind = 'instance' AND d.kind = 'removal' AND d.active
		  AND a.subject_kind = 'account'
	`, moderation.AccountSubjectURI(accountDID), r.instanceDID).Scan(&ban.ActionID, &reason, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading account ban: %w", err)
	}
	ban.Reason = reason.String
	if expiresAt.Valid {
		expiry := expiresAt.Time.UTC()
		ban.ExpiresAt = &expiry
	}
	return &ban, nil
}
