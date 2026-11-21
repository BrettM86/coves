package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/notifications"
)

const communityInvitationColumns = `id, community_did, invitee_did, role, inviter_did, inviter_admin, state, created_at, expires_at`

// invitationQuerier reads invitation rows from a pool or a transaction.
type invitationQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanCommunityInvitation(row interface{ Scan(dest ...any) error }) (communities.Invitation, error) {
	var inv communities.Invitation
	var state string
	if err := row.Scan(&inv.ID, &inv.CommunityDID, &inv.InviteeDID, &inv.Role, &inv.InviterDID, &inv.InviterAdmin,
		&state, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
		return communities.Invitation{}, err
	}
	inv.State = communities.InvitationState(state)
	return inv, nil
}

// communityInvitationByID reads one invitation, or nil when there is none.
// suffix ends the query (a row lock).
func communityInvitationByID(ctx context.Context, querier invitationQuerier, invitationID, suffix string) (*communities.Invitation, error) {
	inv, err := scanCommunityInvitation(querier.QueryRowContext(ctx, `
		SELECT `+communityInvitationColumns+` FROM community_role_invitations WHERE id = $1 `+suffix, invitationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read community invitation: %w", err)
	}
	return &inv, nil
}

// InvitationByID reads an invitation outside any action transaction, or nil.
func (r *CommunityActionRepository) InvitationByID(ctx context.Context, invitationID string) (*communities.Invitation, error) {
	return communityInvitationByID(ctx, r.db, invitationID, "")
}

// InvitationByID reads the invitation with its row locked, or nil.
func (t *communityActionTransaction) InvitationByID(ctx context.Context, invitationID string) (*communities.Invitation, error) {
	return communityInvitationByID(ctx, t.tx, invitationID, "FOR UPDATE")
}

// CreateInvitation supersedes the invitee's open invitation in the community
// (lapsed or not), inserts inv as given, and, for an open invitation, writes
// the invitee's communityInvitation notification unless the notification
// recipient rules exclude the invitee. It first takes the shared erasure
// gates of the inviter and the invitee in ascending DID order (before any
// invitation row write): when either is erased it writes nothing and returns
// communities.ErrInvitationPartyErased, and an erasure that starts later
// waits for this transaction and then deletes what it wrote.
func (t *communityActionTransaction) CreateInvitation(ctx context.Context, inv communities.Invitation) error {
	erased, err := gateRoleSubjects(ctx, t.tx, inv.InviterDID, inv.InviteeDID)
	if err != nil {
		return fmt.Errorf("check invitation parties' erasure: %w", err)
	}
	if len(erased) > 0 {
		return communities.ErrInvitationPartyErased
	}
	if _, err := t.tx.ExecContext(ctx, `
		UPDATE community_role_invitations SET state = 'superseded', updated_at = NOW()
		WHERE community_did = $1 AND invitee_did = $2 AND state = 'open'
	`, inv.CommunityDID, inv.InviteeDID); err != nil {
		return fmt.Errorf("supersede the invitee's open invitation: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, `
		INSERT INTO community_role_invitations (`+communityInvitationColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, inv.ID, inv.CommunityDID, inv.InviteeDID, inv.Role, inv.InviterDID, inv.InviterAdmin, string(inv.State),
		inv.CreatedAt.UTC(), inv.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("insert community invitation: %w", err)
	}
	if inv.State != communities.InvitationOpen {
		return nil
	}
	intents, err := notifications.FanoutCommunityInvitation(ctx, t.notifications.LookupsTx(t.tx),
		inv.InviterDID, inv.InviteeDID, inv.CommunityDID, inv.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("plan the invitation notification: %w", err)
	}
	if err := t.notifications.ApplyTx(ctx, t.tx, intents); err != nil {
		return fmt.Errorf("write the invitation notification: %w", err)
	}
	return nil
}

// SetInvitationState moves the open invitation to state. An invitation that
// is not open is an error: the caller read it open under its row lock.
func (t *communityActionTransaction) SetInvitationState(ctx context.Context, invitationID string, state communities.InvitationState) error {
	result, err := t.tx.ExecContext(ctx, `
		UPDATE community_role_invitations SET state = $2, updated_at = NOW() WHERE id = $1 AND state = 'open'
	`, invitationID, string(state))
	if err != nil {
		return fmt.Errorf("set community invitation state: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check community invitation state change: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("set community invitation state: no open invitation %s", invitationID)
	}
	return nil
}

// SaveIdempotencyRecord stores record under (actor, community, key), kept
// for ttl from the database clock. It replaces the key's stored record: the
// caller holds the actor's account lock and found no unexpired one.
func (t *communityActionTransaction) SaveIdempotencyRecord(ctx context.Context, actorDID, communityDID, key string, record moderation.CommunityIdempotencyRecord, ttl time.Duration) error {
	stored, err := json.Marshal(record.Result)
	if err != nil {
		return fmt.Errorf("encode community action result: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, `
		INSERT INTO moderation_idempotency_keys (actor_did, authority_did, key, fingerprint, stored_result, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW() + $6 * INTERVAL '1 microsecond')
		ON CONFLICT (actor_did, authority_did, key) DO UPDATE SET
		    fingerprint = EXCLUDED.fingerprint, stored_result = EXCLUDED.stored_result,
		    created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at
	`, actorDID, communityDID, key, record.Fingerprint, stored, ttl.Microseconds()); err != nil {
		return fmt.Errorf("save community idempotency record: %w", err)
	}
	return nil
}

// GetCommunityBan reads the community ban index in the action transaction.
func (t *communityActionTransaction) GetCommunityBan(ctx context.Context, communityDID, subjectDID string) (*communities.CommunityBan, error) {
	return getCommunityBan(ctx, t.tx, communityDID, subjectDID)
}

// CommunityInvitationRepository reads pending community role invitations.
type CommunityInvitationRepository struct {
	db *sql.DB
}

var _ communities.InvitationReader = (*CommunityInvitationRepository)(nil)

// NewCommunityInvitationRepository builds the invitation reader.
func NewCommunityInvitationRepository(db *sql.DB) *CommunityInvitationRepository {
	return &CommunityInvitationRepository{db: db}
}

// ListPendingInvitationsForCommunity lists the community's pending invitations, newest first.
func (r *CommunityInvitationRepository) ListPendingInvitationsForCommunity(ctx context.Context, communityDID string, now time.Time) ([]communities.Invitation, error) {
	return pendingInvitations(ctx, r.db, `community_did = $1`, communityDID, now)
}

// ListPendingInvitationsForInvitee lists the invitee's pending invitations, newest first.
func (r *CommunityInvitationRepository) ListPendingInvitationsForInvitee(ctx context.Context, inviteeDID string, now time.Time) ([]communities.Invitation, error) {
	return pendingInvitations(ctx, r.db, `invitee_did = $1`, inviteeDID, now)
}

// PendingInvitation is the invitee's pending invitation in the community, or nil.
func (r *CommunityInvitationRepository) PendingInvitation(ctx context.Context, communityDID, inviteeDID string, now time.Time) (*communities.Invitation, error) {
	inv, err := scanCommunityInvitation(r.db.QueryRowContext(ctx, `
		SELECT `+communityInvitationColumns+` FROM community_role_invitations
		WHERE community_did = $1 AND invitee_did = $2 AND state = 'open' AND expires_at > $3
	`, communityDID, inviteeDID, now.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the pending community invitation: %w", err)
	}
	return &inv, nil
}

// pendingInvitations lists the open invitations not lapsed at now that match
// filter, a fixed condition on $1, newest first.
func pendingInvitations(ctx context.Context, querier invitationQuerier, filter, value string, now time.Time) ([]communities.Invitation, error) {
	rows, err := querier.QueryContext(ctx, `
		SELECT `+communityInvitationColumns+` FROM community_role_invitations
		WHERE `+filter+` AND state = 'open' AND expires_at > $2
		ORDER BY created_at DESC, id DESC
	`, value, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list pending community invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	invitations := []communities.Invitation{}
	for rows.Next() {
		inv, err := scanCommunityInvitation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan community invitation: %w", err)
		}
		invitations = append(invitations, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending community invitations: %w", err)
	}
	return invitations, nil
}
