package postgres

import (
	"Coves/internal/core/users"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lib/pq"
)

type postgresUserRepo struct {
	db *sql.DB
}

// NewUserRepository creates a new PostgreSQL user repository
func NewUserRepository(db *sql.DB) users.UserRepository {
	return &postgresUserRepo{db: db}
}

// Create inserts a new user into the users table
func (r *postgresUserRepo) Create(ctx context.Context, user *users.User) (*users.User, error) {
	query := `
		INSERT INTO users (did, handle, pds_url)
		VALUES ($1, $2, $3)
		RETURNING did, handle, pds_url, created_at, updated_at`

	err := r.db.QueryRowContext(ctx, query, user.DID, user.Handle, user.PDSURL).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		// Check for unique constraint violations
		if strings.Contains(err.Error(), "duplicate key") {
			if strings.Contains(err.Error(), "users_pkey") {
				return nil, fmt.Errorf("user with DID already exists")
			}
			if strings.Contains(err.Error(), "users_handle_key") {
				return nil, users.ErrHandleAlreadyTaken
			}
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return user, nil
}

// GetByDID retrieves a user by their DID
func (r *postgresUserRepo) GetByDID(ctx context.Context, did string) (*users.User, error) {
	user := &users.User{}
	query := `SELECT did, handle, pds_url, created_at, updated_at, display_name, bio, avatar_cid, banner_cid FROM users WHERE did = $1`

	var displayName, bio, avatarCID, bannerCID sql.NullString
	err := r.db.QueryRowContext(ctx, query, did).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt,
			&displayName, &bio, &avatarCID, &bannerCID)

	if err == sql.ErrNoRows {
		return nil, users.ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user by DID: %w", err)
	}

	user.DisplayName = displayName.String
	user.Bio = bio.String
	user.AvatarCID = avatarCID.String
	user.BannerCID = bannerCID.String

	return user, nil
}

// GetByHandle retrieves a user by their handle
func (r *postgresUserRepo) GetByHandle(ctx context.Context, handle string) (*users.User, error) {
	user := &users.User{}
	query := `SELECT did, handle, pds_url, created_at, updated_at, display_name, bio, avatar_cid, banner_cid FROM users WHERE handle = $1`

	var displayName, bio, avatarCID, bannerCID sql.NullString
	err := r.db.QueryRowContext(ctx, query, handle).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt,
			&displayName, &bio, &avatarCID, &bannerCID)

	if err == sql.ErrNoRows {
		return nil, users.ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user by handle: %w", err)
	}

	user.DisplayName = displayName.String
	user.Bio = bio.String
	user.AvatarCID = avatarCID.String
	user.BannerCID = bannerCID.String

	return user, nil
}

// UpdateHandle updates the handle for a user with the given DID
func (r *postgresUserRepo) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	user := &users.User{}
	query := `
		UPDATE users
		SET handle = $2, updated_at = NOW()
		WHERE did = $1
		RETURNING did, handle, pds_url, created_at, updated_at, display_name, bio, avatar_cid, banner_cid`

	var displayName, bio, avatarCID, bannerCID sql.NullString
	err := r.db.QueryRowContext(ctx, query, did, newHandle).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt,
			&displayName, &bio, &avatarCID, &bannerCID)

	if err == sql.ErrNoRows {
		return nil, users.ErrUserNotFound
	}
	if err != nil {
		// Check for unique constraint violation on handle
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "users_handle_key") {
			return nil, users.ErrHandleAlreadyTaken
		}
		return nil, fmt.Errorf("failed to update handle: %w", err)
	}

	user.DisplayName = displayName.String
	user.Bio = bio.String
	user.AvatarCID = avatarCID.String
	user.BannerCID = bannerCID.String

	return user, nil
}

const MaxBatchSize = 1000

// GetByDIDs retrieves multiple users by their DIDs in a single query
// Returns a map of DID -> User for efficient lookups
// Missing users are not included in the result map (no error for missing users)
func (r *postgresUserRepo) GetByDIDs(ctx context.Context, dids []string) (map[string]*users.User, error) {
	if len(dids) == 0 {
		return make(map[string]*users.User), nil
	}

	// Validate batch size to prevent excessive memory usage and query timeouts
	if len(dids) > MaxBatchSize {
		return nil, fmt.Errorf("batch size %d exceeds maximum %d", len(dids), MaxBatchSize)
	}

	// Validate DID format to prevent SQL injection and malformed queries
	// All atProto DIDs must start with "did:" prefix
	for _, did := range dids {
		if !strings.HasPrefix(did, "did:") {
			return nil, fmt.Errorf("invalid DID format: %s", did)
		}
	}

	// Build parameterized query with IN clause
	// Use ANY($1) for PostgreSQL array support with pq.Array() for type conversion
	query := `SELECT did, handle, pds_url, created_at, updated_at, display_name, bio, avatar_cid, banner_cid FROM users WHERE did = ANY($1)`

	rows, err := r.db.QueryContext(ctx, query, pq.Array(dids))
	if err != nil {
		return nil, fmt.Errorf("failed to query users by DIDs: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.Warn("failed to close rows", slog.String("error", closeErr.Error()))
		}
	}()

	// Build map of results
	result := make(map[string]*users.User, len(dids))
	for rows.Next() {
		user := &users.User{}
		var displayName, bio, avatarCID, bannerCID sql.NullString
		err := rows.Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt,
			&displayName, &bio, &avatarCID, &bannerCID)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user row: %w", err)
		}
		user.DisplayName = displayName.String
		user.Bio = bio.String
		user.AvatarCID = avatarCID.String
		user.BannerCID = bannerCID.String
		result[user.DID] = user
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating user rows: %w", err)
	}

	return result, nil
}

// GetProfileStats retrieves aggregated statistics for a user profile
// This performs a single query with scalar subqueries for efficiency
func (r *postgresUserRepo) GetProfileStats(ctx context.Context, did string) (*users.ProfileStats, error) {
	// Validate DID format
	if !strings.HasPrefix(did, "did:") {
		return nil, fmt.Errorf("invalid DID format: %s", did)
	}

	// Note: reputation sums ALL memberships (including banned) intentionally.
	// Reputation represents historical contributions, while membership_count
	// reflects current active community access. A banned user keeps their
	// earned reputation but loses the membership count.
	query := `
		SELECT
			(SELECT COUNT(*) FROM posts WHERE author_did = $1 AND deleted_at IS NULL) as post_count,
			(SELECT COUNT(*) FROM comments WHERE commenter_did = $1 AND deleted_at IS NULL) as comment_count,
			(SELECT COUNT(*) FROM community_subscriptions WHERE user_did = $1) as community_count,
			(SELECT COUNT(*) FROM community_memberships WHERE user_did = $1 AND is_banned = false) as membership_count,
			(SELECT COALESCE(SUM(reputation_score), 0) FROM community_memberships WHERE user_did = $1) as reputation
	`

	stats := &users.ProfileStats{}
	err := r.db.QueryRowContext(ctx, query, did).Scan(
		&stats.PostCount,
		&stats.CommentCount,
		&stats.CommunityCount,
		&stats.MembershipCount,
		&stats.Reputation,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get profile stats: %w", err)
	}

	return stats, nil
}

// Delete removes a user and all associated data from the AppView database.
// This performs a cascading delete across all tables that reference the user's DID.
// The operation is atomic - either all data is deleted or none.
//
// This ONLY deletes AppView indexed data, NOT the user's atProto identity on their PDS.
func (r *postgresUserRepo) Delete(ctx context.Context, did string) error {
	// Validate DID format
	if !strings.HasPrefix(did, "did:") {
		return &users.InvalidDIDError{DID: did, Reason: "must start with 'did:'"}
	}

	// Start transaction for atomic deletion
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction for did=%s: %w", did, err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			slog.Error("failed to rollback transaction",
				slog.String("did", did),
				slog.String("error", err.Error()),
			)
		}
	}()

	// Delete in correct order to avoid foreign key violations
	// Tables without FK constraints on user_did are deleted first

	// 1. Delete OAuth sessions (explicit DELETE)
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_sessions WHERE did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete oauth_sessions for did=%s: %w", did, err)
	}

	// 2. Delete OAuth requests (explicit DELETE)
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_requests WHERE did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete oauth_requests for did=%s: %w", did, err)
	}

	// 3. Delete community subscriptions (explicit DELETE)
	if _, err := tx.ExecContext(ctx, `DELETE FROM community_subscriptions WHERE user_did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete community_subscriptions for did=%s: %w", did, err)
	}

	// 4. Delete community memberships (explicit DELETE)
	if _, err := tx.ExecContext(ctx, `DELETE FROM community_memberships WHERE user_did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete community_memberships for did=%s: %w", did, err)
	}

	// 5. Delete community blocks (explicit DELETE)
	if _, err := tx.ExecContext(ctx, `DELETE FROM community_blocks WHERE user_did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete community_blocks for did=%s: %w", did, err)
	}

	// 6. Delete comments (explicit DELETE)
	if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE commenter_did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete comments for did=%s: %w", did, err)
	}

	// 7. Delete votes (explicit DELETE - FK constraint removed in migration 014)
	if _, err := tx.ExecContext(ctx, `DELETE FROM votes WHERE voter_did = $1`, did); err != nil {
		return fmt.Errorf("failed to delete votes for did=%s: %w", did, err)
	}

	// 8. Delete user (FK CASCADE deletes posts)
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE did = $1`, did)
	if err != nil {
		return fmt.Errorf("failed to delete user did=%s: %w", did, err)
	}

	// Check if user was actually deleted
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected for did=%s: %w", did, err)
	}
	if rowsAffected == 0 {
		return users.ErrUserNotFound
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction for did=%s: %w", did, err)
	}

	return nil
}

// UpdateProfile updates a user's profile fields (display name, bio, avatar, banner).
// Nil values in the input mean "don't change this field" - only non-nil values are updated.
// Empty string values will clear the field in the database.
// Returns the updated user with all fields populated.
// Returns ErrUserNotFound if the user does not exist.
func (r *postgresUserRepo) UpdateProfile(ctx context.Context, did string, input users.UpdateProfileInput) (*users.User, error) {
	// Validate DID format
	if !strings.HasPrefix(did, "did:") {
		return nil, &users.InvalidDIDError{DID: did, Reason: "must start with 'did:'"}
	}

	// Build dynamic UPDATE query based on which fields are provided
	setClauses := []string{"updated_at = NOW()"}
	args := []interface{}{}
	argNum := 1

	if input.DisplayName != nil {
		setClauses = append(setClauses, fmt.Sprintf("display_name = $%d", argNum))
		args = append(args, *input.DisplayName)
		argNum++
	}
	if input.Bio != nil {
		setClauses = append(setClauses, fmt.Sprintf("bio = $%d", argNum))
		args = append(args, *input.Bio)
		argNum++
	}
	if input.AvatarCID != nil {
		setClauses = append(setClauses, fmt.Sprintf("avatar_cid = $%d", argNum))
		args = append(args, *input.AvatarCID)
		argNum++
	}
	if input.BannerCID != nil {
		setClauses = append(setClauses, fmt.Sprintf("banner_cid = $%d", argNum))
		args = append(args, *input.BannerCID)
		argNum++
	}

	// Add the DID as the final parameter for the WHERE clause
	args = append(args, did)

	query := fmt.Sprintf(`
		UPDATE users
		SET %s
		WHERE did = $%d
		RETURNING did, handle, pds_url, created_at, updated_at, display_name, bio, avatar_cid, banner_cid`,
		strings.Join(setClauses, ", "), argNum)

	user := &users.User{}
	var displayNameVal, bioVal, avatarCIDVal, bannerCIDVal sql.NullString

	err := r.db.QueryRowContext(ctx, query, args...).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt,
			&displayNameVal, &bioVal, &avatarCIDVal, &bannerCIDVal)

	if err == sql.ErrNoRows {
		return nil, users.ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update profile: %w", err)
	}

	user.DisplayName = displayNameVal.String
	user.Bio = bioVal.String
	user.AvatarCID = avatarCIDVal.String
	user.BannerCID = bannerCIDVal.String

	return user, nil
}
