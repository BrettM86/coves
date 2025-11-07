package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"Coves/internal/core/users"

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
				return nil, fmt.Errorf("handle already taken")
			}
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return user, nil
}

// GetByDID retrieves a user by their DID
func (r *postgresUserRepo) GetByDID(ctx context.Context, did string) (*users.User, error) {
	user := &users.User{}
	query := `SELECT did, handle, pds_url, created_at, updated_at FROM users WHERE did = $1`

	err := r.db.QueryRowContext(ctx, query, did).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user by DID: %w", err)
	}

	return user, nil
}

// GetByHandle retrieves a user by their handle
func (r *postgresUserRepo) GetByHandle(ctx context.Context, handle string) (*users.User, error) {
	user := &users.User{}
	query := `SELECT did, handle, pds_url, created_at, updated_at FROM users WHERE handle = $1`

	err := r.db.QueryRowContext(ctx, query, handle).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user by handle: %w", err)
	}

	return user, nil
}

// UpdateHandle updates the handle for a user with the given DID
func (r *postgresUserRepo) UpdateHandle(ctx context.Context, did, newHandle string) (*users.User, error) {
	user := &users.User{}
	query := `
		UPDATE users
		SET handle = $2, updated_at = NOW()
		WHERE did = $1
		RETURNING did, handle, pds_url, created_at, updated_at`

	err := r.db.QueryRowContext(ctx, query, did, newHandle).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		// Check for unique constraint violation on handle
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "users_handle_key") {
			return nil, fmt.Errorf("handle already taken")
		}
		return nil, fmt.Errorf("failed to update handle: %w", err)
	}

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
	query := `SELECT did, handle, pds_url, created_at, updated_at FROM users WHERE did = ANY($1)`

	rows, err := r.db.QueryContext(ctx, query, pq.Array(dids))
	if err != nil {
		return nil, fmt.Errorf("failed to query users by DIDs: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Warning: Failed to close rows: %v", closeErr)
		}
	}()

	// Build map of results
	result := make(map[string]*users.User, len(dids))
	for rows.Next() {
		user := &users.User{}
		err := rows.Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user row: %w", err)
		}
		result[user.DID] = user
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating user rows: %w", err)
	}

	return result, nil
}
