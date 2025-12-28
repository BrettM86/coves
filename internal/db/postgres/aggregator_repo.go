package postgres

import (
	"Coves/internal/core/aggregators"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type postgresAggregatorRepo struct {
	db *sql.DB
}

// NewAggregatorRepository creates a new PostgreSQL aggregator repository
func NewAggregatorRepository(db *sql.DB) aggregators.Repository {
	return &postgresAggregatorRepo{db: db}
}

// ===== Aggregator CRUD Operations =====

// CreateAggregator indexes a new aggregator service declaration from the firehose
func (r *postgresAggregatorRepo) CreateAggregator(ctx context.Context, agg *aggregators.Aggregator) error {
	query := `
		INSERT INTO aggregators (
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, created_at, indexed_at, record_uri, record_cid
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		)
		ON CONFLICT (did) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			description = EXCLUDED.description,
			avatar_url = EXCLUDED.avatar_url,
			config_schema = EXCLUDED.config_schema,
			maintainer_did = EXCLUDED.maintainer_did,
			source_url = EXCLUDED.source_url,
			created_at = EXCLUDED.created_at,
			indexed_at = EXCLUDED.indexed_at,
			record_uri = EXCLUDED.record_uri,
			record_cid = EXCLUDED.record_cid`

	var configSchema interface{}
	if len(agg.ConfigSchema) > 0 {
		configSchema = agg.ConfigSchema
	} else {
		configSchema = nil
	}

	_, err := r.db.ExecContext(ctx, query,
		agg.DID,
		agg.DisplayName,
		nullString(agg.Description),
		nullString(agg.AvatarURL),
		configSchema,
		nullString(agg.MaintainerDID),
		nullString(agg.SourceURL),
		agg.CreatedAt,
		agg.IndexedAt,
		nullString(agg.RecordURI),
		nullString(agg.RecordCID),
	)
	if err != nil {
		return fmt.Errorf("failed to create aggregator: %w", err)
	}

	return nil
}

// GetAggregator retrieves an aggregator by DID
// Includes API key and OAuth columns (decrypted) for GetAPIKeyInfo and token refresh operations
func (r *postgresAggregatorRepo) GetAggregator(ctx context.Context, did string) (*aggregators.Aggregator, error) {
	query := `
		SELECT
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, communities_using, posts_created,
			created_at, indexed_at, record_uri, record_cid,
			api_key_prefix, api_key_hash, api_key_created_at, api_key_revoked_at, api_key_last_used_at,
			CASE
				WHEN oauth_access_token_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(oauth_access_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as oauth_access_token,
			CASE
				WHEN oauth_refresh_token_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(oauth_refresh_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as oauth_refresh_token,
			oauth_token_expires_at,
			oauth_pds_url, oauth_auth_server_iss, oauth_auth_server_token_endpoint,
			CASE
				WHEN oauth_dpop_private_key_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(oauth_dpop_private_key_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as oauth_dpop_private_key_multibase,
			oauth_dpop_authserver_nonce, oauth_dpop_pds_nonce
		FROM aggregators
		WHERE did = $1`

	agg := &aggregators.Aggregator{}
	var description, avatarURL, maintainerDID, sourceURL, recordURI, recordCID sql.NullString
	var apiKeyPrefix, apiKeyHash sql.NullString
	var oauthAccessToken, oauthRefreshToken sql.NullString
	var oauthPDSURL, oauthAuthServerIss, oauthAuthServerTokenEndpoint sql.NullString
	var oauthDPoPPrivateKey, oauthDPoPAuthServerNonce, oauthDPoPPDSNonce sql.NullString
	var configSchema []byte
	var apiKeyCreatedAt, apiKeyRevokedAt, apiKeyLastUsed, oauthTokenExpiresAt sql.NullTime

	err := r.db.QueryRowContext(ctx, query, did).Scan(
		&agg.DID,
		&agg.DisplayName,
		&description,
		&avatarURL,
		&configSchema,
		&maintainerDID,
		&sourceURL,
		&agg.CommunitiesUsing,
		&agg.PostsCreated,
		&agg.CreatedAt,
		&agg.IndexedAt,
		&recordURI,
		&recordCID,
		&apiKeyPrefix,
		&apiKeyHash,
		&apiKeyCreatedAt,
		&apiKeyRevokedAt,
		&apiKeyLastUsed,
		&oauthAccessToken,
		&oauthRefreshToken,
		&oauthTokenExpiresAt,
		&oauthPDSURL,
		&oauthAuthServerIss,
		&oauthAuthServerTokenEndpoint,
		&oauthDPoPPrivateKey,
		&oauthDPoPAuthServerNonce,
		&oauthDPoPPDSNonce,
	)

	if err == sql.ErrNoRows {
		return nil, aggregators.ErrAggregatorNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get aggregator: %w", err)
	}

	// Map nullable string fields
	agg.Description = description.String
	agg.AvatarURL = avatarURL.String
	agg.MaintainerDID = maintainerDID.String
	agg.SourceURL = sourceURL.String
	agg.RecordURI = recordURI.String
	agg.RecordCID = recordCID.String
	agg.APIKeyPrefix = apiKeyPrefix.String
	agg.APIKeyHash = apiKeyHash.String
	agg.OAuthAccessToken = oauthAccessToken.String
	agg.OAuthRefreshToken = oauthRefreshToken.String
	agg.OAuthPDSURL = oauthPDSURL.String
	agg.OAuthAuthServerIss = oauthAuthServerIss.String
	agg.OAuthAuthServerTokenEndpoint = oauthAuthServerTokenEndpoint.String
	agg.OAuthDPoPPrivateKeyMultibase = oauthDPoPPrivateKey.String
	agg.OAuthDPoPAuthServerNonce = oauthDPoPAuthServerNonce.String
	agg.OAuthDPoPPDSNonce = oauthDPoPPDSNonce.String

	if configSchema != nil {
		agg.ConfigSchema = configSchema
	}

	// Map nullable time fields
	if apiKeyCreatedAt.Valid {
		t := apiKeyCreatedAt.Time
		agg.APIKeyCreatedAt = &t
	}
	if apiKeyRevokedAt.Valid {
		t := apiKeyRevokedAt.Time
		agg.APIKeyRevokedAt = &t
	}
	if apiKeyLastUsed.Valid {
		t := apiKeyLastUsed.Time
		agg.APIKeyLastUsed = &t
	}
	if oauthTokenExpiresAt.Valid {
		t := oauthTokenExpiresAt.Time
		agg.OAuthTokenExpiresAt = &t
	}

	return agg, nil
}

// GetAggregatorsByDIDs retrieves multiple aggregators by DIDs in a single query (avoids N+1)
func (r *postgresAggregatorRepo) GetAggregatorsByDIDs(ctx context.Context, dids []string) ([]*aggregators.Aggregator, error) {
	if len(dids) == 0 {
		return []*aggregators.Aggregator{}, nil
	}

	// Build IN clause with placeholders
	placeholders := make([]string, len(dids))
	args := make([]interface{}, len(dids))
	for i, did := range dids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = did
	}

	query := fmt.Sprintf(`
		SELECT
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, communities_using, posts_created,
			created_at, indexed_at, record_uri, record_cid
		FROM aggregators
		WHERE did IN (%s)`, strings.Join(placeholders, ", "))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get aggregators: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []*aggregators.Aggregator
	for rows.Next() {
		agg := &aggregators.Aggregator{}
		var description, avatarCID, maintainerDID, homepageURL, recordURI, recordCID sql.NullString
		var configSchema []byte

		err := rows.Scan(
			&agg.DID,
			&agg.DisplayName,
			&description,
			&avatarCID,
			&configSchema,
			&maintainerDID,
			&homepageURL,
			&agg.CommunitiesUsing,
			&agg.PostsCreated,
			&agg.CreatedAt,
			&agg.IndexedAt,
			&recordURI,
			&recordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan aggregator: %w", err)
		}

		// Map nullable fields
		agg.Description = description.String
		agg.AvatarURL = avatarCID.String
		agg.MaintainerDID = maintainerDID.String
		agg.SourceURL = homepageURL.String
		agg.RecordURI = recordURI.String
		agg.RecordCID = recordCID.String
		if configSchema != nil {
			agg.ConfigSchema = configSchema
		}

		results = append(results, agg)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating aggregators: %w", err)
	}

	return results, nil
}

// UpdateAggregator updates an existing aggregator
func (r *postgresAggregatorRepo) UpdateAggregator(ctx context.Context, agg *aggregators.Aggregator) error {
	query := `
		UPDATE aggregators SET
			display_name = $2,
			description = $3,
			avatar_url = $4,
			config_schema = $5,
			maintainer_did = $6,
			source_url = $7,
			created_at = $8,
			indexed_at = $9,
			record_uri = $10,
			record_cid = $11
		WHERE did = $1`

	var configSchema interface{}
	if len(agg.ConfigSchema) > 0 {
		configSchema = agg.ConfigSchema
	} else {
		configSchema = nil
	}

	result, err := r.db.ExecContext(ctx, query,
		agg.DID,
		agg.DisplayName,
		nullString(agg.Description),
		nullString(agg.AvatarURL),
		configSchema,
		nullString(agg.MaintainerDID),
		nullString(agg.SourceURL),
		agg.CreatedAt,
		agg.IndexedAt,
		nullString(agg.RecordURI),
		nullString(agg.RecordCID),
	)
	if err != nil {
		return fmt.Errorf("failed to update aggregator: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// DeleteAggregator removes an aggregator (cascade deletes authorizations and posts via FK)
func (r *postgresAggregatorRepo) DeleteAggregator(ctx context.Context, did string) error {
	query := `DELETE FROM aggregators WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query, did)
	if err != nil {
		return fmt.Errorf("failed to delete aggregator: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// ListAggregators retrieves all aggregators with pagination
func (r *postgresAggregatorRepo) ListAggregators(ctx context.Context, limit, offset int) ([]*aggregators.Aggregator, error) {
	query := `
		SELECT
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, communities_using, posts_created,
			created_at, indexed_at, record_uri, record_cid
		FROM aggregators
		ORDER BY communities_using DESC, display_name ASC
		LIMIT $1 OFFSET $2`

	rows, err := r.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list aggregators: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var aggs []*aggregators.Aggregator
	for rows.Next() {
		agg := &aggregators.Aggregator{}
		var description, avatarCID, maintainerDID, homepageURL, recordURI, recordCID sql.NullString
		var configSchema []byte

		err := rows.Scan(
			&agg.DID,
			&agg.DisplayName,
			&description,
			&avatarCID,
			&configSchema,
			&maintainerDID,
			&homepageURL,
			&agg.CommunitiesUsing,
			&agg.PostsCreated,
			&agg.CreatedAt,
			&agg.IndexedAt,
			&recordURI,
			&recordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan aggregator: %w", err)
		}

		// Map nullable fields
		agg.Description = description.String
		agg.AvatarURL = avatarCID.String
		agg.MaintainerDID = maintainerDID.String
		agg.SourceURL = homepageURL.String
		agg.RecordURI = recordURI.String
		agg.RecordCID = recordCID.String
		if configSchema != nil {
			agg.ConfigSchema = configSchema
		}

		aggs = append(aggs, agg)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating aggregators: %w", err)
	}

	return aggs, nil
}

// IsAggregator performs a fast existence check for post creation handler
func (r *postgresAggregatorRepo) IsAggregator(ctx context.Context, did string) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM aggregators WHERE did = $1)`

	var exists bool
	err := r.db.QueryRowContext(ctx, query, did).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check if aggregator exists: %w", err)
	}

	return exists, nil
}

// ===== Authorization CRUD Operations =====

// CreateAuthorization indexes a new authorization from the firehose
func (r *postgresAggregatorRepo) CreateAuthorization(ctx context.Context, auth *aggregators.Authorization) error {
	query := `
		INSERT INTO aggregator_authorizations (
			aggregator_did, community_did, enabled, config,
			created_at, created_by, disabled_at, disabled_by,
			indexed_at, record_uri, record_cid
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		)
		ON CONFLICT (aggregator_did, community_did) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			config = EXCLUDED.config,
			created_at = EXCLUDED.created_at,
			created_by = EXCLUDED.created_by,
			disabled_at = EXCLUDED.disabled_at,
			disabled_by = EXCLUDED.disabled_by,
			indexed_at = EXCLUDED.indexed_at,
			record_uri = EXCLUDED.record_uri,
			record_cid = EXCLUDED.record_cid
		RETURNING id`

	var config interface{}
	if len(auth.Config) > 0 {
		config = auth.Config
	} else {
		config = nil
	}

	var disabledAt interface{}
	if auth.DisabledAt != nil {
		disabledAt = *auth.DisabledAt
	} else {
		disabledAt = nil
	}

	err := r.db.QueryRowContext(ctx, query,
		auth.AggregatorDID,
		auth.CommunityDID,
		auth.Enabled,
		config,
		auth.CreatedAt,
		auth.CreatedBy, // Required field, no nullString needed
		disabledAt,
		nullString(auth.DisabledBy),
		auth.IndexedAt,
		nullString(auth.RecordURI),
		nullString(auth.RecordCID),
	).Scan(&auth.ID)
	if err != nil {
		// Check for foreign key violations
		if strings.Contains(err.Error(), "fk_aggregator") {
			return aggregators.ErrAggregatorNotFound
		}
		return fmt.Errorf("failed to create authorization: %w", err)
	}

	return nil
}

// GetAuthorization retrieves an authorization by aggregator and community DID
func (r *postgresAggregatorRepo) GetAuthorization(ctx context.Context, aggregatorDID, communityDID string) (*aggregators.Authorization, error) {
	query := `
		SELECT
			id, aggregator_did, community_did, enabled, config,
			created_at, created_by, disabled_at, disabled_by,
			indexed_at, record_uri, record_cid
		FROM aggregator_authorizations
		WHERE aggregator_did = $1 AND community_did = $2`

	auth := &aggregators.Authorization{}
	var config []byte
	var createdBy, disabledBy, recordURI, recordCID sql.NullString
	var disabledAt sql.NullTime

	err := r.db.QueryRowContext(ctx, query, aggregatorDID, communityDID).Scan(
		&auth.ID,
		&auth.AggregatorDID,
		&auth.CommunityDID,
		&auth.Enabled,
		&config,
		&auth.CreatedAt,
		&createdBy,
		&disabledAt,
		&disabledBy,
		&auth.IndexedAt,
		&recordURI,
		&recordCID,
	)

	if err == sql.ErrNoRows {
		return nil, aggregators.ErrAuthorizationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get authorization: %w", err)
	}

	// Map nullable fields
	auth.CreatedBy = createdBy.String
	auth.DisabledBy = disabledBy.String
	if disabledAt.Valid {
		disabledAtVal := disabledAt.Time
		auth.DisabledAt = &disabledAtVal
	}
	auth.RecordURI = recordURI.String
	auth.RecordCID = recordCID.String
	if config != nil {
		auth.Config = config
	}

	return auth, nil
}

// GetAuthorizationByURI retrieves an authorization by record URI (for Jetstream delete operations)
func (r *postgresAggregatorRepo) GetAuthorizationByURI(ctx context.Context, recordURI string) (*aggregators.Authorization, error) {
	query := `
		SELECT
			id, aggregator_did, community_did, enabled, config,
			created_at, created_by, disabled_at, disabled_by,
			indexed_at, record_uri, record_cid
		FROM aggregator_authorizations
		WHERE record_uri = $1`

	auth := &aggregators.Authorization{}
	var config []byte
	var createdBy, disabledBy, recordURIField, recordCID sql.NullString
	var disabledAt sql.NullTime

	err := r.db.QueryRowContext(ctx, query, recordURI).Scan(
		&auth.ID,
		&auth.AggregatorDID,
		&auth.CommunityDID,
		&auth.Enabled,
		&config,
		&auth.CreatedAt,
		&createdBy,
		&disabledAt,
		&disabledBy,
		&auth.IndexedAt,
		&recordURIField,
		&recordCID,
	)

	if err == sql.ErrNoRows {
		return nil, aggregators.ErrAuthorizationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get authorization by URI: %w", err)
	}

	// Map nullable fields
	auth.CreatedBy = createdBy.String
	auth.DisabledBy = disabledBy.String
	if disabledAt.Valid {
		disabledAtVal := disabledAt.Time
		auth.DisabledAt = &disabledAtVal
	}
	auth.RecordURI = recordURIField.String
	auth.RecordCID = recordCID.String
	if config != nil {
		auth.Config = config
	}

	return auth, nil
}

// UpdateAuthorization updates an existing authorization
func (r *postgresAggregatorRepo) UpdateAuthorization(ctx context.Context, auth *aggregators.Authorization) error {
	query := `
		UPDATE aggregator_authorizations SET
			enabled = $3,
			config = $4,
			created_at = $5,
			created_by = $6,
			disabled_at = $7,
			disabled_by = $8,
			indexed_at = $9,
			record_uri = $10,
			record_cid = $11
		WHERE aggregator_did = $1 AND community_did = $2`

	var config interface{}
	if len(auth.Config) > 0 {
		config = auth.Config
	} else {
		config = nil
	}

	var disabledAt interface{}
	if auth.DisabledAt != nil {
		disabledAt = *auth.DisabledAt
	} else {
		disabledAt = nil
	}

	result, err := r.db.ExecContext(ctx, query,
		auth.AggregatorDID,
		auth.CommunityDID,
		auth.Enabled,
		config,
		auth.CreatedAt,
		nullString(auth.CreatedBy),
		disabledAt,
		nullString(auth.DisabledBy),
		auth.IndexedAt,
		nullString(auth.RecordURI),
		nullString(auth.RecordCID),
	)
	if err != nil {
		return fmt.Errorf("failed to update authorization: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAuthorizationNotFound
	}

	return nil
}

// DeleteAuthorization removes an authorization
func (r *postgresAggregatorRepo) DeleteAuthorization(ctx context.Context, aggregatorDID, communityDID string) error {
	query := `DELETE FROM aggregator_authorizations WHERE aggregator_did = $1 AND community_did = $2`

	result, err := r.db.ExecContext(ctx, query, aggregatorDID, communityDID)
	if err != nil {
		return fmt.Errorf("failed to delete authorization: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAuthorizationNotFound
	}

	return nil
}

// DeleteAuthorizationByURI removes an authorization by record URI (for Jetstream delete operations)
func (r *postgresAggregatorRepo) DeleteAuthorizationByURI(ctx context.Context, recordURI string) error {
	query := `DELETE FROM aggregator_authorizations WHERE record_uri = $1`

	result, err := r.db.ExecContext(ctx, query, recordURI)
	if err != nil {
		return fmt.Errorf("failed to delete authorization by URI: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAuthorizationNotFound
	}

	return nil
}

// ===== Authorization Query Operations =====

// ListAuthorizationsForAggregator retrieves all communities that authorized an aggregator
func (r *postgresAggregatorRepo) ListAuthorizationsForAggregator(ctx context.Context, aggregatorDID string, enabledOnly bool, limit, offset int) ([]*aggregators.Authorization, error) {
	baseQuery := `
		SELECT
			id, aggregator_did, community_did, enabled, config,
			created_at, created_by, disabled_at, disabled_by,
			indexed_at, record_uri, record_cid
		FROM aggregator_authorizations
		WHERE aggregator_did = $1`

	var query string
	var args []interface{}

	if enabledOnly {
		query = baseQuery + ` AND enabled = true ORDER BY created_at DESC LIMIT $2 OFFSET $3`
		args = []interface{}{aggregatorDID, limit, offset}
	} else {
		query = baseQuery + ` ORDER BY created_at DESC LIMIT $2 OFFSET $3`
		args = []interface{}{aggregatorDID, limit, offset}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list authorizations for aggregator: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanAuthorizations(rows)
}

// ListAuthorizationsForCommunity retrieves all aggregators authorized by a community
func (r *postgresAggregatorRepo) ListAuthorizationsForCommunity(ctx context.Context, communityDID string, enabledOnly bool, limit, offset int) ([]*aggregators.Authorization, error) {
	baseQuery := `
		SELECT
			id, aggregator_did, community_did, enabled, config,
			created_at, created_by, disabled_at, disabled_by,
			indexed_at, record_uri, record_cid
		FROM aggregator_authorizations
		WHERE community_did = $1`

	var query string
	var args []interface{}

	if enabledOnly {
		query = baseQuery + ` AND enabled = true ORDER BY created_at DESC LIMIT $2 OFFSET $3`
		args = []interface{}{communityDID, limit, offset}
	} else {
		query = baseQuery + ` ORDER BY created_at DESC LIMIT $2 OFFSET $3`
		args = []interface{}{communityDID, limit, offset}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list authorizations for community: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanAuthorizations(rows)
}

// IsAuthorized performs a fast authorization check (enabled=true)
// Uses the optimized partial index: idx_aggregator_auth_enabled
func (r *postgresAggregatorRepo) IsAuthorized(ctx context.Context, aggregatorDID, communityDID string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM aggregator_authorizations
			WHERE aggregator_did = $1 AND community_did = $2 AND enabled = true
		)`

	var authorized bool
	err := r.db.QueryRowContext(ctx, query, aggregatorDID, communityDID).Scan(&authorized)
	if err != nil {
		return false, fmt.Errorf("failed to check authorization: %w", err)
	}

	return authorized, nil
}

// ===== Post Tracking Operations =====

// RecordAggregatorPost tracks a post created by an aggregator (for rate limiting and stats)
func (r *postgresAggregatorRepo) RecordAggregatorPost(ctx context.Context, aggregatorDID, communityDID, postURI, postCID string) error {
	query := `
		INSERT INTO aggregator_posts (aggregator_did, community_did, post_uri, post_cid, created_at)
		VALUES ($1, $2, $3, $4, NOW())`

	_, err := r.db.ExecContext(ctx, query, aggregatorDID, communityDID, postURI, postCID)
	if err != nil {
		return fmt.Errorf("failed to record aggregator post: %w", err)
	}

	return nil
}

// CountRecentPosts counts posts created by an aggregator in a community since a given time
// Uses the optimized index: idx_aggregator_posts_rate_limit
func (r *postgresAggregatorRepo) CountRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) (int, error) {
	query := `
		SELECT COUNT(*)
		FROM aggregator_posts
		WHERE aggregator_did = $1 AND community_did = $2 AND created_at >= $3`

	var count int
	err := r.db.QueryRowContext(ctx, query, aggregatorDID, communityDID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count recent posts: %w", err)
	}

	return count, nil
}

// GetRecentPosts retrieves recent posts created by an aggregator in a community
func (r *postgresAggregatorRepo) GetRecentPosts(ctx context.Context, aggregatorDID, communityDID string, since time.Time) ([]*aggregators.AggregatorPost, error) {
	query := `
		SELECT id, aggregator_did, community_did, post_uri, created_at
		FROM aggregator_posts
		WHERE aggregator_did = $1 AND community_did = $2 AND created_at >= $3
		ORDER BY created_at DESC`

	rows, err := r.db.QueryContext(ctx, query, aggregatorDID, communityDID, since)
	if err != nil {
		return nil, fmt.Errorf("failed to get recent posts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var posts []*aggregators.AggregatorPost
	for rows.Next() {
		post := &aggregators.AggregatorPost{}
		err := rows.Scan(
			&post.ID,
			&post.AggregatorDID,
			&post.CommunityDID,
			&post.PostURI,
			&post.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan aggregator post: %w", err)
		}
		posts = append(posts, post)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating aggregator posts: %w", err)
	}

	return posts, nil
}

// ===== API Key Authentication Operations =====

// GetByAPIKeyHash looks up an aggregator by their API key hash for authentication
// Returns ErrAggregatorNotFound if no aggregator exists with that key hash
// Returns ErrAPIKeyRevoked if the API key has been revoked
func (r *postgresAggregatorRepo) GetByAPIKeyHash(ctx context.Context, keyHash string) (*aggregators.Aggregator, error) {
	query := `
		SELECT
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, communities_using, posts_created,
			created_at, indexed_at, record_uri, record_cid,
			api_key_prefix, api_key_hash, api_key_created_at, api_key_revoked_at, api_key_last_used_at,
			CASE
				WHEN oauth_access_token_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(oauth_access_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as oauth_access_token,
			CASE
				WHEN oauth_refresh_token_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(oauth_refresh_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as oauth_refresh_token,
			oauth_token_expires_at,
			oauth_pds_url, oauth_auth_server_iss, oauth_auth_server_token_endpoint,
			CASE
				WHEN oauth_dpop_private_key_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(oauth_dpop_private_key_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as oauth_dpop_private_key_multibase,
			oauth_dpop_authserver_nonce, oauth_dpop_pds_nonce
		FROM aggregators
		WHERE api_key_hash = $1`

	agg := &aggregators.Aggregator{}
	var description, avatarURL, maintainerDID, sourceURL, recordURI, recordCID sql.NullString
	var apiKeyPrefix, apiKeyHash sql.NullString
	var oauthAccessToken, oauthRefreshToken sql.NullString
	var oauthPDSURL, oauthAuthServerIss, oauthAuthServerTokenEndpoint sql.NullString
	var oauthDPoPPrivateKey, oauthDPoPAuthServerNonce, oauthDPoPPDSNonce sql.NullString
	var configSchema []byte
	var apiKeyCreatedAt, apiKeyRevokedAt, apiKeyLastUsed, oauthTokenExpiresAt sql.NullTime

	err := r.db.QueryRowContext(ctx, query, keyHash).Scan(
		&agg.DID,
		&agg.DisplayName,
		&description,
		&avatarURL,
		&configSchema,
		&maintainerDID,
		&sourceURL,
		&agg.CommunitiesUsing,
		&agg.PostsCreated,
		&agg.CreatedAt,
		&agg.IndexedAt,
		&recordURI,
		&recordCID,
		&apiKeyPrefix,
		&apiKeyHash,
		&apiKeyCreatedAt,
		&apiKeyRevokedAt,
		&apiKeyLastUsed,
		&oauthAccessToken,
		&oauthRefreshToken,
		&oauthTokenExpiresAt,
		&oauthPDSURL,
		&oauthAuthServerIss,
		&oauthAuthServerTokenEndpoint,
		&oauthDPoPPrivateKey,
		&oauthDPoPAuthServerNonce,
		&oauthDPoPPDSNonce,
	)

	if err == sql.ErrNoRows {
		return nil, aggregators.ErrAggregatorNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get aggregator by API key hash: %w", err)
	}

	// Map nullable string fields
	agg.Description = description.String
	agg.AvatarURL = avatarURL.String
	agg.MaintainerDID = maintainerDID.String
	agg.SourceURL = sourceURL.String
	agg.RecordURI = recordURI.String
	agg.RecordCID = recordCID.String
	agg.APIKeyPrefix = apiKeyPrefix.String
	agg.APIKeyHash = apiKeyHash.String
	agg.OAuthAccessToken = oauthAccessToken.String
	agg.OAuthRefreshToken = oauthRefreshToken.String
	agg.OAuthPDSURL = oauthPDSURL.String
	agg.OAuthAuthServerIss = oauthAuthServerIss.String
	agg.OAuthAuthServerTokenEndpoint = oauthAuthServerTokenEndpoint.String
	agg.OAuthDPoPPrivateKeyMultibase = oauthDPoPPrivateKey.String
	agg.OAuthDPoPAuthServerNonce = oauthDPoPAuthServerNonce.String
	agg.OAuthDPoPPDSNonce = oauthDPoPPDSNonce.String

	if configSchema != nil {
		agg.ConfigSchema = configSchema
	}

	// Map nullable time fields
	if apiKeyCreatedAt.Valid {
		t := apiKeyCreatedAt.Time
		agg.APIKeyCreatedAt = &t
	}
	if apiKeyRevokedAt.Valid {
		t := apiKeyRevokedAt.Time
		agg.APIKeyRevokedAt = &t
	}
	if apiKeyLastUsed.Valid {
		t := apiKeyLastUsed.Time
		agg.APIKeyLastUsed = &t
	}
	if oauthTokenExpiresAt.Valid {
		t := oauthTokenExpiresAt.Time
		agg.OAuthTokenExpiresAt = &t
	}

	// Check if API key is revoked
	if agg.APIKeyRevokedAt != nil {
		return nil, aggregators.ErrAPIKeyRevoked
	}

	return agg, nil
}

// SetAPIKey stores API key credentials and OAuth session for an aggregator
// This is called after successful OAuth flow to generate the API key
// SECURITY: OAuth tokens and DPoP private key are encrypted at rest using pgp_sym_encrypt
func (r *postgresAggregatorRepo) SetAPIKey(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *aggregators.OAuthCredentials) error {
	query := `
		UPDATE aggregators SET
			api_key_prefix = $2,
			api_key_hash = $3,
			api_key_created_at = NOW(),
			api_key_revoked_at = NULL,
			oauth_access_token_encrypted = CASE WHEN $4 != '' THEN pgp_sym_encrypt($4, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) ELSE NULL END,
			oauth_refresh_token_encrypted = CASE WHEN $5 != '' THEN pgp_sym_encrypt($5, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) ELSE NULL END,
			oauth_token_expires_at = $6,
			oauth_pds_url = $7,
			oauth_auth_server_iss = $8,
			oauth_auth_server_token_endpoint = $9,
			oauth_dpop_private_key_encrypted = CASE WHEN $10 != '' THEN pgp_sym_encrypt($10, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) ELSE NULL END,
			oauth_dpop_authserver_nonce = $11,
			oauth_dpop_pds_nonce = $12
		WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query,
		did,
		keyPrefix,
		keyHash,
		oauthCreds.AccessToken,
		oauthCreds.RefreshToken,
		oauthCreds.TokenExpiresAt,
		oauthCreds.PDSURL,
		oauthCreds.AuthServerIss,
		oauthCreds.AuthServerTokenEndpoint,
		oauthCreds.DPoPPrivateKeyMultibase,
		oauthCreds.DPoPAuthServerNonce,
		oauthCreds.DPoPPDSNonce,
	)
	if err != nil {
		return fmt.Errorf("failed to set API key: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// UpdateOAuthTokens updates OAuth tokens after a refresh operation
// Called after successfully refreshing an expired access token
// SECURITY: OAuth tokens are encrypted at rest using pgp_sym_encrypt
func (r *postgresAggregatorRepo) UpdateOAuthTokens(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error {
	query := `
		UPDATE aggregators SET
			oauth_access_token_encrypted = pgp_sym_encrypt($2, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)),
			oauth_refresh_token_encrypted = pgp_sym_encrypt($3, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)),
			oauth_token_expires_at = $4
		WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query, did, accessToken, refreshToken, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to update OAuth tokens: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// UpdateOAuthNonces updates DPoP nonces after token operations
// Nonces are updated after each request to the auth server or PDS
func (r *postgresAggregatorRepo) UpdateOAuthNonces(ctx context.Context, did, authServerNonce, pdsNonce string) error {
	query := `
		UPDATE aggregators SET
			oauth_dpop_authserver_nonce = COALESCE(NULLIF($2, ''), oauth_dpop_authserver_nonce),
			oauth_dpop_pds_nonce = COALESCE(NULLIF($3, ''), oauth_dpop_pds_nonce)
		WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query, did, authServerNonce, pdsNonce)
	if err != nil {
		return fmt.Errorf("failed to update OAuth nonces: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// UpdateAPIKeyLastUsed updates the last_used_at timestamp for audit purposes
// Called on each successful authentication to track API key usage
func (r *postgresAggregatorRepo) UpdateAPIKeyLastUsed(ctx context.Context, did string) error {
	query := `
		UPDATE aggregators SET
			api_key_last_used_at = NOW()
		WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query, did)
	if err != nil {
		return fmt.Errorf("failed to update API key last used: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// RevokeAPIKey marks an API key as revoked (sets api_key_revoked_at)
// After revocation, the aggregator must complete OAuth flow again to get a new key
func (r *postgresAggregatorRepo) RevokeAPIKey(ctx context.Context, did string) error {
	query := `
		UPDATE aggregators SET
			api_key_revoked_at = NOW()
		WHERE did = $1 AND api_key_hash IS NOT NULL`

	result, err := r.db.ExecContext(ctx, query, did)
	if err != nil {
		return fmt.Errorf("failed to revoke API key: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return aggregators.ErrAggregatorNotFound
	}

	return nil
}

// ===== Helper Functions =====

// scanAuthorizations is a helper to scan multiple authorization rows
func scanAuthorizations(rows *sql.Rows) ([]*aggregators.Authorization, error) {
	var auths []*aggregators.Authorization

	for rows.Next() {
		auth := &aggregators.Authorization{}
		var config []byte
		var createdBy, disabledBy, recordURI, recordCID sql.NullString
		var disabledAt sql.NullTime

		err := rows.Scan(
			&auth.ID,
			&auth.AggregatorDID,
			&auth.CommunityDID,
			&auth.Enabled,
			&config,
			&auth.CreatedAt,
			&createdBy,
			&disabledAt,
			&disabledBy,
			&auth.IndexedAt,
			&recordURI,
			&recordCID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan authorization: %w", err)
		}

		// Map nullable fields
		auth.CreatedBy = createdBy.String
		auth.DisabledBy = disabledBy.String
		if disabledAt.Valid {
			disabledAtVal := disabledAt.Time
			auth.DisabledAt = &disabledAtVal
		}
		auth.RecordURI = recordURI.String
		auth.RecordCID = recordCID.String
		if config != nil {
			auth.Config = config
		}

		auths = append(auths, auth)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating authorizations: %w", err)
	}

	return auths, nil
}
