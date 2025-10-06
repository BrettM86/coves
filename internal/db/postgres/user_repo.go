package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"Coves/internal/core/users"
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
