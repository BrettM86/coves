package identity

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// postgresCache implements IdentityCache using PostgreSQL
type postgresCache struct {
	db  *sql.DB
	ttl time.Duration
}

// NewPostgresCache creates a new PostgreSQL-backed identity cache
func NewPostgresCache(db *sql.DB, ttl time.Duration) IdentityCache {
	return &postgresCache{
		db:  db,
		ttl: ttl,
	}
}

// Get retrieves a cached identity by handle or DID
func (r *postgresCache) Get(ctx context.Context, identifier string) (*Identity, error) {
	identifier = normalizeIdentifier(identifier)

	query := `
		SELECT did, handle, pds_url, resolved_at, resolution_method, expires_at
		FROM identity_cache
		WHERE identifier = $1 AND expires_at > NOW()
	`

	var i Identity
	var method string
	var expiresAt time.Time

	err := r.db.QueryRowContext(ctx, query, identifier).Scan(
		&i.DID,
		&i.Handle,
		&i.PDSURL,
		&i.ResolvedAt,
		&method,
		&expiresAt,
	)

	if err == sql.ErrNoRows {
		return nil, &ErrCacheMiss{Identifier: identifier}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query identity cache: %w", err)
	}

	// Convert string method to ResolutionMethod type
	i.Method = MethodCache // It's from cache now

	return &i, nil
}

// Set caches an identity bidirectionally (by handle and by DID)
func (r *postgresCache) Set(ctx context.Context, i *Identity) error {
	expiresAt := time.Now().UTC().Add(r.ttl)

	// Debug logging for cache operations (helps diagnose TTL issues)
	log.Printf("[identity-cache] Caching: handle=%s, did=%s, expires=%s (TTL=%s)",
		i.Handle, i.DID, expiresAt.Format(time.RFC3339), r.ttl)

	query := `
		INSERT INTO identity_cache (identifier, did, handle, pds_url, resolved_at, resolution_method, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (identifier)
		DO UPDATE SET
			did = EXCLUDED.did,
			handle = EXCLUDED.handle,
			pds_url = EXCLUDED.pds_url,
			resolved_at = EXCLUDED.resolved_at,
			resolution_method = EXCLUDED.resolution_method,
			expires_at = EXCLUDED.expires_at,
			updated_at = NOW()
	`

	// Cache by handle if present
	if i.Handle != "" {
		normalizedHandle := normalizeIdentifier(i.Handle)
		_, err := r.db.ExecContext(ctx, query,
			normalizedHandle, i.DID, i.Handle, i.PDSURL,
			i.ResolvedAt, string(i.Method), expiresAt,
		)
		if err != nil {
			return fmt.Errorf("failed to cache identity by handle: %w", err)
		}
	}

	// Cache by DID
	_, err := r.db.ExecContext(ctx, query,
		i.DID, i.DID, i.Handle, i.PDSURL,
		i.ResolvedAt, string(i.Method), expiresAt,
	)
	if err != nil {
		return fmt.Errorf("failed to cache identity by DID: %w", err)
	}

	return nil
}

// Delete removes a cached identity by identifier
func (r *postgresCache) Delete(ctx context.Context, identifier string) error {
	identifier = normalizeIdentifier(identifier)

	query := `DELETE FROM identity_cache WHERE identifier = $1`
	_, err := r.db.ExecContext(ctx, query, identifier)
	if err != nil {
		return fmt.Errorf("failed to delete from identity cache: %w", err)
	}

	return nil
}

// Purge removes all cache entries associated with an identifier
// This removes both handle and DID entries in a single atomic query
func (r *postgresCache) Purge(ctx context.Context, identifier string) error {
	identifier = normalizeIdentifier(identifier)

	// Single atomic query: find related entries and delete all at once
	// This prevents race conditions and is more efficient than multiple queries
	query := `
		WITH related AS (
			SELECT did, handle
			FROM identity_cache
			WHERE identifier = $1
			LIMIT 1
		)
		DELETE FROM identity_cache
		WHERE identifier = $1
		   OR identifier IN (SELECT did FROM related WHERE did IS NOT NULL)
		   OR identifier IN (SELECT handle FROM related WHERE handle IS NOT NULL AND handle != '')
	`

	result, err := r.db.ExecContext(ctx, query, identifier)
	if err != nil {
		return fmt.Errorf("failed to purge identity cache: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err == nil && rowsAffected > 0 {
		log.Printf("[identity-cache] Purged %d entries for: %s", rowsAffected, identifier)
	}

	return nil
}

// normalizeIdentifier normalizes handles to lowercase, leaves DIDs as-is
func normalizeIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)

	// DIDs are case-sensitive, handles are not
	if strings.HasPrefix(identifier, "did:") {
		return identifier
	}

	// It's a handle, normalize to lowercase
	return strings.ToLower(identifier)
}
