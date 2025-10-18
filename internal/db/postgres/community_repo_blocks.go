package postgres

import (
	"Coves/internal/core/communities"
	"context"
	"database/sql"
	"fmt"
	"log"
)

// BlockCommunity creates a new block record (idempotent)
func (r *postgresCommunityRepo) BlockCommunity(ctx context.Context, block *communities.CommunityBlock) (*communities.CommunityBlock, error) {
	query := `
		INSERT INTO community_blocks (user_did, community_did, blocked_at, record_uri, record_cid)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_did, community_did) DO UPDATE SET
			record_uri = EXCLUDED.record_uri,
			record_cid = EXCLUDED.record_cid,
			blocked_at = EXCLUDED.blocked_at
		RETURNING id, blocked_at`

	err := r.db.QueryRowContext(ctx, query,
		block.UserDID,
		block.CommunityDID,
		block.BlockedAt,
		block.RecordURI,
		block.RecordCID,
	).Scan(&block.ID, &block.BlockedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create block: %w", err)
	}

	return block, nil
}

// UnblockCommunity removes a block record
func (r *postgresCommunityRepo) UnblockCommunity(ctx context.Context, userDID, communityDID string) error {
	query := `DELETE FROM community_blocks WHERE user_did = $1 AND community_did = $2`

	result, err := r.db.ExecContext(ctx, query, userDID, communityDID)
	if err != nil {
		return fmt.Errorf("failed to unblock community: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check unblock result: %w", err)
	}

	if rowsAffected == 0 {
		return communities.ErrBlockNotFound
	}

	return nil
}

// GetBlock retrieves a block record by user DID and community DID
func (r *postgresCommunityRepo) GetBlock(ctx context.Context, userDID, communityDID string) (*communities.CommunityBlock, error) {
	query := `
		SELECT id, user_did, community_did, blocked_at, record_uri, record_cid
		FROM community_blocks
		WHERE user_did = $1 AND community_did = $2`

	var block communities.CommunityBlock

	err := r.db.QueryRowContext(ctx, query, userDID, communityDID).Scan(
		&block.ID,
		&block.UserDID,
		&block.CommunityDID,
		&block.BlockedAt,
		&block.RecordURI,
		&block.RecordCID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, communities.ErrBlockNotFound
		}
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	return &block, nil
}

// GetBlockByURI retrieves a block record by its AT-URI (for Jetstream DELETE operations)
func (r *postgresCommunityRepo) GetBlockByURI(ctx context.Context, recordURI string) (*communities.CommunityBlock, error) {
	query := `
		SELECT id, user_did, community_did, blocked_at, record_uri, record_cid
		FROM community_blocks
		WHERE record_uri = $1`

	var block communities.CommunityBlock

	err := r.db.QueryRowContext(ctx, query, recordURI).Scan(
		&block.ID,
		&block.UserDID,
		&block.CommunityDID,
		&block.BlockedAt,
		&block.RecordURI,
		&block.RecordCID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, communities.ErrBlockNotFound
		}
		return nil, fmt.Errorf("failed to get block by URI: %w", err)
	}

	return &block, nil
}

// ListBlockedCommunities retrieves all communities blocked by a user
func (r *postgresCommunityRepo) ListBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*communities.CommunityBlock, error) {
	query := `
		SELECT id, user_did, community_did, blocked_at, record_uri, record_cid
		FROM community_blocks
		WHERE user_did = $1
		ORDER BY blocked_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, userDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list blocked communities: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			// Log error but don't override the main error
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	var blocks []*communities.CommunityBlock
	for rows.Next() {
		// Allocate a new block for each iteration to avoid pointer reuse bug
		block := &communities.CommunityBlock{}

		err = rows.Scan(
			&block.ID,
			&block.UserDID,
			&block.CommunityDID,
			&block.BlockedAt,
			&block.RecordURI,
			&block.RecordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan block: %w", err)
		}

		blocks = append(blocks, block)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating blocks: %w", err)
	}

	return blocks, nil
}

// IsBlocked checks if a user has blocked a specific community (fast EXISTS check)
func (r *postgresCommunityRepo) IsBlocked(ctx context.Context, userDID, communityDID string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM community_blocks
			WHERE user_did = $1 AND community_did = $2
		)`

	var exists bool
	err := r.db.QueryRowContext(ctx, query, userDID, communityDID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check if blocked: %w", err)
	}

	return exists, nil
}

