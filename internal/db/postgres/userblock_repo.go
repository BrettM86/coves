package postgres

import (
	"Coves/internal/core/userblocks"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/lib/pq"
)

// postgresUserBlockRepo implements userblocks.Repository using PostgreSQL
type postgresUserBlockRepo struct {
	db *sql.DB
}

// NewUserBlockRepository creates a new PostgreSQL-backed user block repository
func NewUserBlockRepository(db *sql.DB) userblocks.Repository {
	return &postgresUserBlockRepo{db: db}
}

// BlockUser creates a new block record (idempotent via ON CONFLICT DO UPDATE)
func (r *postgresUserBlockRepo) BlockUser(ctx context.Context, block *userblocks.UserBlock) (*userblocks.UserBlock, error) {
	query := `
		INSERT INTO user_blocks (blocker_did, blocked_did, blocked_at, record_uri, record_cid)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (blocker_did, blocked_did) DO UPDATE SET
			record_uri = EXCLUDED.record_uri,
			record_cid = EXCLUDED.record_cid,
			blocked_at = EXCLUDED.blocked_at
		RETURNING id, blocked_at`

	err := r.db.QueryRowContext(ctx, query,
		block.BlockerDID,
		block.BlockedDID,
		block.BlockedAt,
		block.RecordURI,
		block.RecordCID,
	).Scan(&block.ID, &block.BlockedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create user block: %w", err)
	}

	return block, nil
}

// UnblockUser removes a block record. Returns ErrBlockNotFound if not exists.
func (r *postgresUserBlockRepo) UnblockUser(ctx context.Context, blockerDID, blockedDID string) error {
	query := `DELETE FROM user_blocks WHERE blocker_did = $1 AND blocked_did = $2`

	result, err := r.db.ExecContext(ctx, query, blockerDID, blockedDID)
	if err != nil {
		return fmt.Errorf("failed to unblock user: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check unblock result: %w", err)
	}

	if rowsAffected == 0 {
		return userblocks.ErrBlockNotFound
	}

	return nil
}

// GetBlock retrieves a block by blocker + blocked DID pair.
func (r *postgresUserBlockRepo) GetBlock(ctx context.Context, blockerDID, blockedDID string) (*userblocks.UserBlock, error) {
	query := `
		SELECT id, blocker_did, blocked_did, blocked_at, record_uri, record_cid
		FROM user_blocks
		WHERE blocker_did = $1 AND blocked_did = $2`

	var block userblocks.UserBlock

	err := r.db.QueryRowContext(ctx, query, blockerDID, blockedDID).Scan(
		&block.ID,
		&block.BlockerDID,
		&block.BlockedDID,
		&block.BlockedAt,
		&block.RecordURI,
		&block.RecordCID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, userblocks.ErrBlockNotFound
		}
		return nil, fmt.Errorf("failed to get user block: %w", err)
	}

	return &block, nil
}

// GetBlockByURI retrieves a block by its AT-URI (for Jetstream DELETE operations).
func (r *postgresUserBlockRepo) GetBlockByURI(ctx context.Context, recordURI string) (*userblocks.UserBlock, error) {
	query := `
		SELECT id, blocker_did, blocked_did, blocked_at, record_uri, record_cid
		FROM user_blocks
		WHERE record_uri = $1`

	var block userblocks.UserBlock

	err := r.db.QueryRowContext(ctx, query, recordURI).Scan(
		&block.ID,
		&block.BlockerDID,
		&block.BlockedDID,
		&block.BlockedAt,
		&block.RecordURI,
		&block.RecordCID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, userblocks.ErrBlockNotFound
		}
		return nil, fmt.Errorf("failed to get user block by URI: %w", err)
	}

	return &block, nil
}

// ListBlockedUsers retrieves all users blocked by the given blocker, paginated.
// Results are ordered by blocked_at DESC.
func (r *postgresUserBlockRepo) ListBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*userblocks.UserBlock, error) {
	query := `
		SELECT id, blocker_did, blocked_did, blocked_at, record_uri, record_cid
		FROM user_blocks
		WHERE blocker_did = $1
		ORDER BY blocked_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.QueryContext(ctx, query, blockerDID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list blocked users: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	var blocks []*userblocks.UserBlock
	for rows.Next() {
		block := &userblocks.UserBlock{}

		err = rows.Scan(
			&block.ID,
			&block.BlockerDID,
			&block.BlockedDID,
			&block.BlockedAt,
			&block.RecordURI,
			&block.RecordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user block: %w", err)
		}

		blocks = append(blocks, block)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating user blocks: %w", err)
	}

	return blocks, nil
}

// IsBlocked checks if blockerDID has blocked blockedDID (fast EXISTS check).
func (r *postgresUserBlockRepo) IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM user_blocks
			WHERE blocker_did = $1 AND blocked_did = $2
		)`

	var exists bool
	err := r.db.QueryRowContext(ctx, query, blockerDID, blockedDID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check if user is blocked: %w", err)
	}

	return exists, nil
}

// AreBlocked checks which of the given DIDs are blocked by blockerDID.
// Returns a map of blockedDID -> true for each DID that is blocked.
func (r *postgresUserBlockRepo) AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error) {
	result := make(map[string]bool)

	if len(blockedDIDs) == 0 {
		return result, nil
	}

	query := `
		SELECT blocked_did
		FROM user_blocks
		WHERE blocker_did = $1 AND blocked_did = ANY($2)`

	rows, err := r.db.QueryContext(ctx, query, blockerDID, pq.Array(blockedDIDs))
	if err != nil {
		return nil, fmt.Errorf("failed to batch check blocked users: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	for rows.Next() {
		var blockedDID string
		if err = rows.Scan(&blockedDID); err != nil {
			return nil, fmt.Errorf("failed to scan blocked DID: %w", err)
		}
		result[blockedDID] = true
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating blocked DIDs: %w", err)
	}

	return result, nil
}
