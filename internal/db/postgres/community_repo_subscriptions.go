package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Coves/internal/core/communities"
)

// Subscribe creates a new subscription record
func (r *postgresCommunityRepo) Subscribe(ctx context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	query := `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at, record_uri, record_cid)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, subscribed_at`

	err := r.db.QueryRowContext(ctx, query,
		subscription.UserDID,
		subscription.CommunityDID,
		subscription.SubscribedAt,
		nullString(subscription.RecordURI),
		nullString(subscription.RecordCID),
	).Scan(&subscription.ID, &subscription.SubscribedAt)

	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, communities.ErrSubscriptionAlreadyExists
		}
		if strings.Contains(err.Error(), "foreign key") {
			return nil, communities.ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to create subscription: %w", err)
	}

	return subscription, nil
}

// SubscribeWithCount atomically creates subscription and increments subscriber count
// This is idempotent - safe for Jetstream replays
func (r *postgresCommunityRepo) SubscribeWithCount(ctx context.Context, subscription *communities.Subscription) (*communities.Subscription, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert subscription with ON CONFLICT DO NOTHING for idempotency
	query := `
		INSERT INTO community_subscriptions (user_did, community_did, subscribed_at, record_uri, record_cid)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_did, community_did) DO NOTHING
		RETURNING id, subscribed_at`

	err = tx.QueryRowContext(ctx, query,
		subscription.UserDID,
		subscription.CommunityDID,
		subscription.SubscribedAt,
		nullString(subscription.RecordURI),
		nullString(subscription.RecordCID),
	).Scan(&subscription.ID, &subscription.SubscribedAt)

	// If no rows returned, subscription already existed (idempotent behavior)
	if err == sql.ErrNoRows {
		// Get existing subscription
		query = `SELECT id, subscribed_at FROM community_subscriptions WHERE user_did = $1 AND community_did = $2`
		err = tx.QueryRowContext(ctx, query, subscription.UserDID, subscription.CommunityDID).Scan(&subscription.ID, &subscription.SubscribedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to get existing subscription: %w", err)
		}
		// Don't increment count - subscription already existed
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("failed to commit transaction: %w", err)
		}
		return subscription, nil
	}

	if err != nil {
		if strings.Contains(err.Error(), "foreign key") {
			return nil, communities.ErrCommunityNotFound
		}
		return nil, fmt.Errorf("failed to create subscription: %w", err)
	}

	// Increment subscriber count only if insert succeeded
	incrementQuery := `
		UPDATE communities
		SET subscriber_count = subscriber_count + 1, updated_at = NOW()
		WHERE did = $1`

	_, err = tx.ExecContext(ctx, incrementQuery, subscription.CommunityDID)
	if err != nil {
		return nil, fmt.Errorf("failed to increment subscriber count: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return subscription, nil
}

// Unsubscribe removes a subscription record
func (r *postgresCommunityRepo) Unsubscribe(ctx context.Context, userDID, communityDID string) error {
	query := `DELETE FROM community_subscriptions WHERE user_did = $1 AND community_did = $2`

	result, err := r.db.ExecContext(ctx, query, userDID, communityDID)
	if err != nil {
		return fmt.Errorf("failed to unsubscribe: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check unsubscribe result: %w", err)
	}

	if rowsAffected == 0 {
		return communities.ErrSubscriptionNotFound
	}

	return nil
}

// UnsubscribeWithCount atomically removes subscription and decrements subscriber count
// This is idempotent - safe for Jetstream replays
func (r *postgresCommunityRepo) UnsubscribeWithCount(ctx context.Context, userDID, communityDID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Delete subscription
	deleteQuery := `DELETE FROM community_subscriptions WHERE user_did = $1 AND community_did = $2`
	result, err := tx.ExecContext(ctx, deleteQuery, userDID, communityDID)
	if err != nil {
		return fmt.Errorf("failed to unsubscribe: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check unsubscribe result: %w", err)
	}

	// If no rows deleted, subscription didn't exist (idempotent - not an error)
	if rowsAffected == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}
		return nil
	}

	// Decrement subscriber count only if delete succeeded
	decrementQuery := `
		UPDATE communities
		SET subscriber_count = GREATEST(0, subscriber_count - 1), updated_at = NOW()
		WHERE did = $1`

	_, err = tx.ExecContext(ctx, decrementQuery, communityDID)
	if err != nil {
		return fmt.Errorf("failed to decrement subscriber count: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// GetSubscription retrieves a specific subscription
func (r *postgresCommunityRepo) GetSubscription(ctx context.Context, userDID, communityDID string) (*communities.Subscription, error) {
	subscription := &communities.Subscription{}
	query := `
		SELECT id, user_did, community_did, subscribed_at, record_uri, record_cid
		FROM community_subscriptions
		WHERE user_did = $1 AND community_did = $2`

	var recordURI, recordCID sql.NullString

	err := r.db.QueryRowContext(ctx, query, userDID, communityDID).Scan(
		&subscription.ID,
		&subscription.UserDID,
		&subscription.CommunityDID,
		&subscription.SubscribedAt,
		&recordURI,
		&recordCID,
	)

	if err == sql.ErrNoRows {
		return nil, communities.ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get subscription: %w", err)
	}

	subscription.RecordURI = recordURI.String
	subscription.RecordCID = recordCID.String

	return subscription, nil
}

// ListSubscriptions retrieves all subscriptions for a user
func (r *postgresCommunityRepo) ListSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*communities.Subscription, error) {
	query := `
		SELECT id, user_did, community_did, subscribed_at, record_uri, record_cid
		FROM community_subscriptions
		WHERE user_did = $1
		ORDER BY subscribed_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, userDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list subscriptions: %w", err)
	}
	defer rows.Close()

	result := []*communities.Subscription{}
	for rows.Next() {
		subscription := &communities.Subscription{}
		var recordURI, recordCID sql.NullString

		err := rows.Scan(
			&subscription.ID,
			&subscription.UserDID,
			&subscription.CommunityDID,
			&subscription.SubscribedAt,
			&recordURI,
			&recordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan subscription: %w", err)
		}

		subscription.RecordURI = recordURI.String
		subscription.RecordCID = recordCID.String

		result = append(result, subscription)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating subscriptions: %w", err)
	}

	return result, nil
}

// ListSubscribers retrieves all subscribers for a community
func (r *postgresCommunityRepo) ListSubscribers(ctx context.Context, communityDID string, limit, offset int) ([]*communities.Subscription, error) {
	query := `
		SELECT id, user_did, community_did, subscribed_at, record_uri, record_cid
		FROM community_subscriptions
		WHERE community_did = $1
		ORDER BY subscribed_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, communityDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list subscribers: %w", err)
	}
	defer rows.Close()

	result := []*communities.Subscription{}
	for rows.Next() {
		subscription := &communities.Subscription{}
		var recordURI, recordCID sql.NullString

		err := rows.Scan(
			&subscription.ID,
			&subscription.UserDID,
			&subscription.CommunityDID,
			&subscription.SubscribedAt,
			&recordURI,
			&recordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan subscriber: %w", err)
		}

		subscription.RecordURI = recordURI.String
		subscription.RecordCID = recordCID.String

		result = append(result, subscription)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating subscribers: %w", err)
	}

	return result, nil
}
