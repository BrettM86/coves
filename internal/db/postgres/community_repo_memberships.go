package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"Coves/internal/core/communities"
)

// CreateMembership creates a new membership record
func (r *postgresCommunityRepo) CreateMembership(ctx context.Context, membership *communities.Membership) (*communities.Membership, error) {
	query := `
		INSERT INTO community_memberships (
			user_did, community_did, reputation_score, contribution_count,
			joined_at, last_active_at, is_banned, is_moderator
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, joined_at, last_active_at`

	err := r.db.QueryRowContext(ctx, query,
		membership.UserDID,
		membership.CommunityDID,
		membership.ReputationScore,
		membership.ContributionCount,
		membership.JoinedAt,
		membership.LastActiveAt,
		membership.IsBanned,
		membership.IsModerator,
	).Scan(&membership.ID, &membership.JoinedAt, &membership.LastActiveAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("membership already exists")
		}
		if strings.Contains(err.Error(), "foreign key") {
			return nil, communities.ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to create membership: %w", err)
	}

	return membership, nil
}

// GetMembership retrieves a specific membership
func (r *postgresCommunityRepo) GetMembership(ctx context.Context, userDID, communityDID string) (*communities.Membership, error) {
	membership := &communities.Membership{}
	query := `
		SELECT id, user_did, community_did, reputation_score, contribution_count,
			joined_at, last_active_at, is_banned, is_moderator
		FROM community_memberships
		WHERE user_did = $1 AND community_did = $2`

	err := r.db.QueryRowContext(ctx, query, userDID, communityDID).Scan(
		&membership.ID,
		&membership.UserDID,
		&membership.CommunityDID,
		&membership.ReputationScore,
		&membership.ContributionCount,
		&membership.JoinedAt,
		&membership.LastActiveAt,
		&membership.IsBanned,
		&membership.IsModerator,
	)

	if err == sql.ErrNoRows {
		return nil, communities.ErrMembershipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get membership: %w", err)
	}

	return membership, nil
}

// UpdateMembership updates an existing membership
func (r *postgresCommunityRepo) UpdateMembership(ctx context.Context, membership *communities.Membership) (*communities.Membership, error) {
	query := `
		UPDATE community_memberships
		SET reputation_score = $3,
			contribution_count = $4,
			last_active_at = $5,
			is_banned = $6,
			is_moderator = $7
		WHERE user_did = $1 AND community_did = $2
		RETURNING last_active_at`

	err := r.db.QueryRowContext(ctx, query,
		membership.UserDID,
		membership.CommunityDID,
		membership.ReputationScore,
		membership.ContributionCount,
		membership.LastActiveAt,
		membership.IsBanned,
		membership.IsModerator,
	).Scan(&membership.LastActiveAt)

	if err == sql.ErrNoRows {
		return nil, communities.ErrMembershipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update membership: %w", err)
	}

	return membership, nil
}

// ListMembers retrieves members of a community ordered by reputation
func (r *postgresCommunityRepo) ListMembers(ctx context.Context, communityDID string, limit, offset int) ([]*communities.Membership, error) {
	query := `
		SELECT id, user_did, community_did, reputation_score, contribution_count,
			joined_at, last_active_at, is_banned, is_moderator
		FROM community_memberships
		WHERE community_did = $1
		ORDER BY reputation_score DESC, joined_at ASC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, communityDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list members: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	result := []*communities.Membership{}
	for rows.Next() {
		membership := &communities.Membership{}

		scanErr := rows.Scan(
			&membership.ID,
			&membership.UserDID,
			&membership.CommunityDID,
			&membership.ReputationScore,
			&membership.ContributionCount,
			&membership.JoinedAt,
			&membership.LastActiveAt,
			&membership.IsBanned,
			&membership.IsModerator,
		)
		if scanErr != nil {
			return nil, fmt.Errorf("failed to scan member: %w", scanErr)
		}

		result = append(result, membership)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating members: %w", err)
	}

	return result, nil
}

// CreateModerationAction records a moderation action
func (r *postgresCommunityRepo) CreateModerationAction(ctx context.Context, action *communities.ModerationAction) (*communities.ModerationAction, error) {
	query := `
		INSERT INTO community_moderation (
			community_did, action, reason, instance_did, broadcast, created_at, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`

	err := r.db.QueryRowContext(ctx, query,
		action.CommunityDID,
		action.Action,
		nullString(action.Reason),
		action.InstanceDID,
		action.Broadcast,
		action.CreatedAt,
		action.ExpiresAt,
	).Scan(&action.ID, &action.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "foreign key") {
			return nil, communities.ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to create moderation action: %w", err)
	}

	return action, nil
}

// ListModerationActions retrieves moderation actions for a community
func (r *postgresCommunityRepo) ListModerationActions(ctx context.Context, communityDID string, limit, offset int) ([]*communities.ModerationAction, error) {
	query := `
		SELECT id, community_did, action, reason, instance_did, broadcast, created_at, expires_at
		FROM community_moderation
		WHERE community_did = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, communityDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list moderation actions: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	result := []*communities.ModerationAction{}
	for rows.Next() {
		action := &communities.ModerationAction{}
		var reason sql.NullString

		scanErr := rows.Scan(
			&action.ID,
			&action.CommunityDID,
			&action.Action,
			&reason,
			&action.InstanceDID,
			&action.Broadcast,
			&action.CreatedAt,
			&action.ExpiresAt,
		)
		if scanErr != nil {
			return nil, fmt.Errorf("failed to scan moderation action: %w", scanErr)
		}

		action.Reason = reason.String
		result = append(result, action)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating moderation actions: %w", err)
	}

	return result, nil
}

// Statistics methods
func (r *postgresCommunityRepo) IncrementMemberCount(ctx context.Context, communityDID string) error {
	query := `UPDATE communities SET member_count = member_count + 1 WHERE did = $1`
	_, err := r.db.ExecContext(ctx, query, communityDID)
	if err != nil {
		return fmt.Errorf("failed to increment member count: %w", err)
	}
	return nil
}

func (r *postgresCommunityRepo) DecrementMemberCount(ctx context.Context, communityDID string) error {
	query := `UPDATE communities SET member_count = GREATEST(0, member_count - 1) WHERE did = $1`
	_, err := r.db.ExecContext(ctx, query, communityDID)
	if err != nil {
		return fmt.Errorf("failed to decrement member count: %w", err)
	}
	return nil
}

func (r *postgresCommunityRepo) IncrementSubscriberCount(ctx context.Context, communityDID string) error {
	query := `UPDATE communities SET subscriber_count = subscriber_count + 1 WHERE did = $1`
	_, err := r.db.ExecContext(ctx, query, communityDID)
	if err != nil {
		return fmt.Errorf("failed to increment subscriber count: %w", err)
	}
	return nil
}

func (r *postgresCommunityRepo) DecrementSubscriberCount(ctx context.Context, communityDID string) error {
	query := `UPDATE communities SET subscriber_count = GREATEST(0, subscriber_count - 1) WHERE did = $1`
	_, err := r.db.ExecContext(ctx, query, communityDID)
	if err != nil {
		return fmt.Errorf("failed to decrement subscriber count: %w", err)
	}
	return nil
}

func (r *postgresCommunityRepo) IncrementPostCount(ctx context.Context, communityDID string) error {
	query := `UPDATE communities SET post_count = post_count + 1 WHERE did = $1`
	_, err := r.db.ExecContext(ctx, query, communityDID)
	if err != nil {
		return fmt.Errorf("failed to increment post count: %w", err)
	}
	return nil
}
