package postgres

import (
	"Coves/internal/core/aggregators"
	"Coves/internal/crypto/credentialcipher"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type postgresAggregatorRepo struct {
	db     *sql.DB
	cipher *credentialcipher.Cipher
}

// NewAggregatorRepository creates a new PostgreSQL aggregator repository
func NewAggregatorRepository(db *sql.DB, cipher *credentialcipher.Cipher) aggregators.Repository {
	if cipher == nil {
		panic("NewAggregatorRepository: credential cipher is required")
	}
	return &postgresAggregatorRepo{db: db, cipher: cipher}
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
// Returns only public/display fields - use GetAggregatorCredentials for authentication data
func (r *postgresAggregatorRepo) GetAggregator(ctx context.Context, did string) (*aggregators.Aggregator, error) {
	query := `
		SELECT
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, communities_using, posts_created,
			created_at, indexed_at, record_uri, record_cid
		FROM aggregators
		WHERE did = $1`

	agg := &aggregators.Aggregator{}
	var description, avatarURL, maintainerDID, sourceURL, recordURI, recordCID sql.NullString
	var configSchema []byte

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
	)

	if errors.Is(err, sql.ErrNoRows) {
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

	if configSchema != nil {
		agg.ConfigSchema = configSchema
	}

	return agg, nil
}

// GetAggregatorsByDIDs retrieves multiple aggregators by DIDs in a single query (avoids N+1)
//
// KNOWN DEFECT (issue 2026-07-31-repo-minor-pins-batch.md, item 6; cosmetic, but it is one function
// answering the same question two ways):
// an empty request short-circuits to a non-nil empty slice, while a request that matches
// nothing falls through the scan loop and returns nil. The two emptinesses marshal as []
// and null respectively. ListAggregators (below) has the nil half of the same problem.
// (see TestAggregatorRepo_GetAggregatorsByDIDs, TestAggregatorRepo_ListAggregators)
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

// CreateAuthorization indexes a new authorization from the firehose.
//
// An authorization record is keyed by (aggregator_did, community_did), but
// the record itself is keyed by its AT-URI, and the two disagree the moment a
// community UPDATES an existing record to name a different aggregatorDid. The
// upsert alone then fails record_uri's UNIQUE constraint — an error no replay
// can clear — while the OLD aggregator keeps its row and its authorization.
// So the row that this record URI previously produced is removed first, in
// the same transaction: the community rewrote the record, and the aggregator
// it used to name is no longer authorized by it.
func (r *postgresAggregatorRepo) CreateAuthorization(ctx context.Context, auth *aggregators.Authorization) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			log.Printf("Failed to rollback transaction: %v", rollbackErr)
		}
	}()

	if auth.RecordURI != "" {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM aggregator_authorizations
			WHERE record_uri = $1
			  AND (aggregator_did, community_did) IS DISTINCT FROM ($2, $3)`,
			auth.RecordURI, auth.AggregatorDID, auth.CommunityDID,
		); err != nil {
			return fmt.Errorf("failed to retire the authorization this record previously named: %w", err)
		}
	}

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

	err = tx.QueryRowContext(ctx, query,
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
		// Foreign key violations name the missing side so the consumer can
		// classify the refusal (see aggregators.IsNotFound).
		if pqErr := extractPQError(err); pqErr != nil && pqErr.Code == "23503" {
			switch pqErr.Constraint {
			case "fk_aggregator":
				return fmt.Errorf("%w: %v", aggregators.ErrAggregatorNotFound, err)
			case "fk_community":
				return fmt.Errorf("%w: %v", aggregators.ErrCommunityNotFound, err)
			}
		}
		return fmt.Errorf("failed to create authorization: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
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

	if errors.Is(err, sql.ErrNoRows) {
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

	if errors.Is(err, sql.ErrNoRows) {
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
//
// KNOWN DEFECT (issue 2026-07-31-update-authorization-rejects-rows-create-accepted.md): created_by is written through nullString here but verbatim in
// CreateAuthorization, and the column is NOT NULL. An authorization with an empty
// CreatedBy therefore inserts happily and can then never be updated — the update dies on
// a raw not-null violation that the handler can only map to a 500, rather than on a
// validation error. Write it verbatim here too, or reject it before the SQL.
// (see TestAggregatorRepo_UpdateAuthorization)
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
// Note: Returns only public Aggregator fields - use GetCredentialsByAPIKeyHash for credentials
func (r *postgresAggregatorRepo) GetByAPIKeyHash(ctx context.Context, keyHash string) (*aggregators.Aggregator, error) {
	query := `
		SELECT
			did, display_name, description, avatar_url, config_schema,
			maintainer_did, source_url, communities_using, posts_created,
			created_at, indexed_at, record_uri, record_cid,
			api_key_revoked_at
		FROM aggregators
		WHERE api_key_hash = $1`

	agg := &aggregators.Aggregator{}
	var description, avatarURL, maintainerDID, sourceURL, recordURI, recordCID sql.NullString
	var configSchema []byte
	var apiKeyRevokedAt sql.NullTime

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
		&apiKeyRevokedAt,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, aggregators.ErrAggregatorNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get aggregator by API key hash: %w", err)
	}

	// Check if API key is revoked before returning
	if apiKeyRevokedAt.Valid {
		return nil, aggregators.ErrAPIKeyRevoked
	}

	// Map nullable string fields
	agg.Description = description.String
	agg.AvatarURL = avatarURL.String
	agg.MaintainerDID = maintainerDID.String
	agg.SourceURL = sourceURL.String
	agg.RecordURI = recordURI.String
	agg.RecordCID = recordCID.String

	if configSchema != nil {
		agg.ConfigSchema = configSchema
	}

	return agg, nil
}

// SetAPIKey stores API key credentials and OAuth session for an aggregator
// This is called after successful OAuth flow to generate the API key
// SECURITY: OAuth tokens and the DPoP private key are encrypted before storage.
func (r *postgresAggregatorRepo) SetAPIKey(ctx context.Context, did, keyPrefix, keyHash string, oauthCreds *aggregators.OAuthCredentials) error {
	accessTokenCiphertext, err := encryptOptionalCredential(
		r.cipher, oauthCreds.AccessToken, aggregatorOAuthAccessTokenCredentialContext(did))
	if err != nil {
		return fmt.Errorf("failed to encrypt aggregator OAuth access token: %w", err)
	}
	refreshTokenCiphertext, err := encryptOptionalCredential(
		r.cipher, oauthCreds.RefreshToken, aggregatorOAuthRefreshTokenCredentialContext(did))
	if err != nil {
		return fmt.Errorf("failed to encrypt aggregator OAuth refresh token: %w", err)
	}
	dpopPrivateKeyCiphertext, err := encryptOptionalCredential(
		r.cipher, oauthCreds.DPoPPrivateKeyMultibase, aggregatorOAuthDPoPPrivateKeyCredentialContext(did))
	if err != nil {
		return fmt.Errorf("failed to encrypt aggregator OAuth DPoP private key: %w", err)
	}

	query := `
		UPDATE aggregators SET
			api_key_prefix = $2,
			api_key_hash = $3,
			api_key_created_at = NOW(),
			api_key_revoked_at = NULL,
			oauth_access_token_encrypted = $4,
			oauth_refresh_token_encrypted = $5,
			oauth_token_expires_at = $6,
			oauth_pds_url = $7,
			oauth_auth_server_iss = $8,
			oauth_auth_server_token_endpoint = $9,
			oauth_dpop_private_key_encrypted = $10,
			oauth_dpop_authserver_nonce = $11,
			oauth_dpop_pds_nonce = $12
		WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query,
		did,
		keyPrefix,
		keyHash,
		accessTokenCiphertext,
		refreshTokenCiphertext,
		oauthCreds.TokenExpiresAt,
		oauthCreds.PDSURL,
		oauthCreds.AuthServerIss,
		oauthCreds.AuthServerTokenEndpoint,
		dpopPrivateKeyCiphertext,
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
// SECURITY: OAuth tokens are encrypted before storage.
func (r *postgresAggregatorRepo) UpdateOAuthTokens(ctx context.Context, did, accessToken, refreshToken string, expiresAt time.Time) error {
	accessTokenCiphertext, err := r.cipher.Encrypt(accessToken, aggregatorOAuthAccessTokenCredentialContext(did))
	if err != nil {
		return fmt.Errorf("failed to encrypt aggregator OAuth access token: %w", err)
	}
	refreshTokenCiphertext, err := r.cipher.Encrypt(refreshToken, aggregatorOAuthRefreshTokenCredentialContext(did))
	if err != nil {
		return fmt.Errorf("failed to encrypt aggregator OAuth refresh token: %w", err)
	}

	query := `
		UPDATE aggregators SET
			oauth_access_token_encrypted = $2,
			oauth_refresh_token_encrypted = $3,
			oauth_token_expires_at = $4
		WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query, did, accessTokenCiphertext, refreshTokenCiphertext, expiresAt)
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
//
// KNOWN DEFECT (issue 2026-07-31-revoke-api-key-rewrites-revocation-timestamp.md): a repeated revocation overwrites api_key_revoked_at with a fresh NOW(),
// losing the record of when the credential was actually withdrawn. The WHERE clause needs
// `AND api_key_revoked_at IS NULL` for the timestamp to mean what the audit trail reads it
// as. (see TestAggregatorRepo_RevokeAPIKey)
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

// GetAggregatorCredentials retrieves only credential data for an aggregator
// Used by APIKeyService for authentication operations where full aggregator is not needed
func (r *postgresAggregatorRepo) GetAggregatorCredentials(ctx context.Context, did string) (*aggregators.AggregatorCredentials, error) {
	query := `
		SELECT
			did,
			api_key_prefix, api_key_hash, api_key_created_at, api_key_revoked_at, api_key_last_used_at,
			oauth_access_token_encrypted, oauth_refresh_token_encrypted,
			oauth_token_expires_at,
			oauth_pds_url, oauth_auth_server_iss, oauth_auth_server_token_endpoint,
			oauth_dpop_private_key_encrypted,
			oauth_dpop_authserver_nonce, oauth_dpop_pds_nonce
		FROM aggregators
		WHERE did = $1`

	creds := &aggregators.AggregatorCredentials{}
	var apiKeyPrefix, apiKeyHash sql.NullString
	var oauthAccessTokenCiphertext, oauthRefreshTokenCiphertext []byte
	var oauthPDSURL, oauthAuthServerIss, oauthAuthServerTokenEndpoint sql.NullString
	var oauthDPoPPrivateKeyCiphertext []byte
	var oauthDPoPAuthServerNonce, oauthDPoPPDSNonce sql.NullString
	var apiKeyCreatedAt, apiKeyRevokedAt, apiKeyLastUsed, oauthTokenExpiresAt sql.NullTime

	err := r.db.QueryRowContext(ctx, query, did).Scan(
		&creds.DID,
		&apiKeyPrefix,
		&apiKeyHash,
		&apiKeyCreatedAt,
		&apiKeyRevokedAt,
		&apiKeyLastUsed,
		&oauthAccessTokenCiphertext,
		&oauthRefreshTokenCiphertext,
		&oauthTokenExpiresAt,
		&oauthPDSURL,
		&oauthAuthServerIss,
		&oauthAuthServerTokenEndpoint,
		&oauthDPoPPrivateKeyCiphertext,
		&oauthDPoPAuthServerNonce,
		&oauthDPoPPDSNonce,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, aggregators.ErrAggregatorNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get aggregator credentials: %w", err)
	}
	if err := r.decryptOAuthCredentials(
		creds, oauthAccessTokenCiphertext, oauthRefreshTokenCiphertext, oauthDPoPPrivateKeyCiphertext); err != nil {
		return nil, fmt.Errorf("failed to get aggregator credentials: %w", err)
	}

	// Map nullable string fields
	creds.APIKeyPrefix = apiKeyPrefix.String
	creds.APIKeyHash = apiKeyHash.String
	creds.OAuthPDSURL = oauthPDSURL.String
	creds.OAuthAuthServerIss = oauthAuthServerIss.String
	creds.OAuthAuthServerTokenEndpoint = oauthAuthServerTokenEndpoint.String
	creds.OAuthDPoPAuthServerNonce = oauthDPoPAuthServerNonce.String
	creds.OAuthDPoPPDSNonce = oauthDPoPPDSNonce.String

	// Map nullable time fields
	if apiKeyCreatedAt.Valid {
		t := apiKeyCreatedAt.Time
		creds.APIKeyCreatedAt = &t
	}
	if apiKeyRevokedAt.Valid {
		t := apiKeyRevokedAt.Time
		creds.APIKeyRevokedAt = &t
	}
	if apiKeyLastUsed.Valid {
		t := apiKeyLastUsed.Time
		creds.APIKeyLastUsed = &t
	}
	if oauthTokenExpiresAt.Valid {
		t := oauthTokenExpiresAt.Time
		creds.OAuthTokenExpiresAt = &t
	}

	return creds, nil
}

// GetCredentialsByAPIKeyHash looks up credentials by API key hash for authentication
// Returns ErrAPIKeyRevoked if the API key has been revoked
// Returns ErrAPIKeyInvalid if no aggregator found with that hash
func (r *postgresAggregatorRepo) GetCredentialsByAPIKeyHash(ctx context.Context, keyHash string) (*aggregators.AggregatorCredentials, error) {
	query := `
		SELECT
			did,
			api_key_prefix, api_key_hash, api_key_created_at, api_key_revoked_at, api_key_last_used_at,
			oauth_access_token_encrypted, oauth_refresh_token_encrypted,
			oauth_token_expires_at,
			oauth_pds_url, oauth_auth_server_iss, oauth_auth_server_token_endpoint,
			oauth_dpop_private_key_encrypted,
			oauth_dpop_authserver_nonce, oauth_dpop_pds_nonce
		FROM aggregators
		WHERE api_key_hash = $1`

	creds := &aggregators.AggregatorCredentials{}
	var apiKeyPrefix, apiKeyHash sql.NullString
	var oauthAccessTokenCiphertext, oauthRefreshTokenCiphertext []byte
	var oauthPDSURL, oauthAuthServerIss, oauthAuthServerTokenEndpoint sql.NullString
	var oauthDPoPPrivateKeyCiphertext []byte
	var oauthDPoPAuthServerNonce, oauthDPoPPDSNonce sql.NullString
	var apiKeyCreatedAt, apiKeyRevokedAt, apiKeyLastUsed, oauthTokenExpiresAt sql.NullTime

	err := r.db.QueryRowContext(ctx, query, keyHash).Scan(
		&creds.DID,
		&apiKeyPrefix,
		&apiKeyHash,
		&apiKeyCreatedAt,
		&apiKeyRevokedAt,
		&apiKeyLastUsed,
		&oauthAccessTokenCiphertext,
		&oauthRefreshTokenCiphertext,
		&oauthTokenExpiresAt,
		&oauthPDSURL,
		&oauthAuthServerIss,
		&oauthAuthServerTokenEndpoint,
		&oauthDPoPPrivateKeyCiphertext,
		&oauthDPoPAuthServerNonce,
		&oauthDPoPPDSNonce,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, aggregators.ErrAPIKeyInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get credentials by API key hash: %w", err)
	}
	if err := r.decryptOAuthCredentials(
		creds, oauthAccessTokenCiphertext, oauthRefreshTokenCiphertext, oauthDPoPPrivateKeyCiphertext); err != nil {
		return nil, fmt.Errorf("failed to get credentials by API key hash: %w", err)
	}

	// Map nullable string fields
	creds.APIKeyPrefix = apiKeyPrefix.String
	creds.APIKeyHash = apiKeyHash.String
	creds.OAuthPDSURL = oauthPDSURL.String
	creds.OAuthAuthServerIss = oauthAuthServerIss.String
	creds.OAuthAuthServerTokenEndpoint = oauthAuthServerTokenEndpoint.String
	creds.OAuthDPoPAuthServerNonce = oauthDPoPAuthServerNonce.String
	creds.OAuthDPoPPDSNonce = oauthDPoPPDSNonce.String

	// Map nullable time fields
	if apiKeyCreatedAt.Valid {
		t := apiKeyCreatedAt.Time
		creds.APIKeyCreatedAt = &t
	}
	if apiKeyRevokedAt.Valid {
		t := apiKeyRevokedAt.Time
		creds.APIKeyRevokedAt = &t
	}
	if apiKeyLastUsed.Valid {
		t := apiKeyLastUsed.Time
		creds.APIKeyLastUsed = &t
	}
	if oauthTokenExpiresAt.Valid {
		t := oauthTokenExpiresAt.Time
		creds.OAuthTokenExpiresAt = &t
	}

	// Check if API key is revoked
	if creds.APIKeyRevokedAt != nil {
		return nil, aggregators.ErrAPIKeyRevoked
	}

	return creds, nil
}

// ListAggregatorsNeedingTokenRefresh returns aggregators with active API keys
// whose OAuth tokens expire within the given buffer period.
// Used by background job to proactively refresh tokens before they expire.
//
// KNOWN DEFECT (issue 2026-07-31-token-refresh-window-nanoseconds-as-seconds.md): the expiry window is a billion times wider than the caller asks for, so
// this returns every aggregator with an active key and any recorded expiry. expiryBuffer
// is a time.Duration, which database/sql passes to the driver as its int64 NANOSECOND
// count; `NOW() + $1` then makes Postgres coerce that bare integer to an interval measured
// in SECONDS. cmd/server passes time.Hour, which arrives as 3.6e12 seconds (~114,000
// years). The hourly job therefore decrypts every stored access token, refresh token and
// DPoP private key in the table on every cycle before RefreshTokensIfNeeded discards
// almost all of them in its own Go-side expiry check. Binding
// make_interval(secs => $1) with the buffer in seconds would fix it.
// (see TestAggregatorRepo_ListAggregatorsNeedingTokenRefresh)
func (r *postgresAggregatorRepo) ListAggregatorsNeedingTokenRefresh(ctx context.Context, expiryBuffer time.Duration) ([]*aggregators.AggregatorCredentials, error) {
	query := `
		SELECT
			did,
			api_key_prefix, api_key_hash, api_key_created_at, api_key_revoked_at, api_key_last_used_at,
			oauth_access_token_encrypted, oauth_refresh_token_encrypted,
			oauth_token_expires_at,
			oauth_pds_url, oauth_auth_server_iss, oauth_auth_server_token_endpoint,
			oauth_dpop_private_key_encrypted,
			oauth_dpop_authserver_nonce, oauth_dpop_pds_nonce
		FROM aggregators
		WHERE api_key_hash IS NOT NULL
			AND api_key_revoked_at IS NULL
			AND oauth_token_expires_at IS NOT NULL
			AND oauth_token_expires_at <= NOW() + $1`

	rows, err := r.db.QueryContext(ctx, query, expiryBuffer)
	if err != nil {
		return nil, fmt.Errorf("failed to list aggregators needing token refresh: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []*aggregators.AggregatorCredentials
	for rows.Next() {
		creds := &aggregators.AggregatorCredentials{}
		var apiKeyPrefix, apiKeyHash sql.NullString
		var oauthAccessTokenCiphertext, oauthRefreshTokenCiphertext []byte
		var oauthPDSURL, oauthAuthServerIss, oauthAuthServerTokenEndpoint sql.NullString
		var oauthDPoPPrivateKeyCiphertext []byte
		var oauthDPoPAuthServerNonce, oauthDPoPPDSNonce sql.NullString
		var apiKeyCreatedAt, apiKeyRevokedAt, apiKeyLastUsed, oauthTokenExpiresAt sql.NullTime

		err := rows.Scan(
			&creds.DID,
			&apiKeyPrefix,
			&apiKeyHash,
			&apiKeyCreatedAt,
			&apiKeyRevokedAt,
			&apiKeyLastUsed,
			&oauthAccessTokenCiphertext,
			&oauthRefreshTokenCiphertext,
			&oauthTokenExpiresAt,
			&oauthPDSURL,
			&oauthAuthServerIss,
			&oauthAuthServerTokenEndpoint,
			&oauthDPoPPrivateKeyCiphertext,
			&oauthDPoPAuthServerNonce,
			&oauthDPoPPDSNonce,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan aggregator credentials: %w", err)
		}
		if err := r.decryptOAuthCredentials(
			creds, oauthAccessTokenCiphertext, oauthRefreshTokenCiphertext, oauthDPoPPrivateKeyCiphertext); err != nil {
			return nil, fmt.Errorf("failed to decrypt aggregator credentials needing token refresh: %w", err)
		}

		// Map nullable string fields
		creds.APIKeyPrefix = apiKeyPrefix.String
		creds.APIKeyHash = apiKeyHash.String
		creds.OAuthPDSURL = oauthPDSURL.String
		creds.OAuthAuthServerIss = oauthAuthServerIss.String
		creds.OAuthAuthServerTokenEndpoint = oauthAuthServerTokenEndpoint.String
		creds.OAuthDPoPAuthServerNonce = oauthDPoPAuthServerNonce.String
		creds.OAuthDPoPPDSNonce = oauthDPoPPDSNonce.String

		// Map nullable time fields
		if apiKeyCreatedAt.Valid {
			t := apiKeyCreatedAt.Time
			creds.APIKeyCreatedAt = &t
		}
		if apiKeyRevokedAt.Valid {
			t := apiKeyRevokedAt.Time
			creds.APIKeyRevokedAt = &t
		}
		if apiKeyLastUsed.Valid {
			t := apiKeyLastUsed.Time
			creds.APIKeyLastUsed = &t
		}
		if oauthTokenExpiresAt.Valid {
			t := oauthTokenExpiresAt.Time
			creds.OAuthTokenExpiresAt = &t
		}

		results = append(results, creds)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating aggregators needing token refresh: %w", err)
	}

	return results, nil
}

// ===== Helper Functions =====

func (r *postgresAggregatorRepo) decryptOAuthCredentials(
	credentials *aggregators.AggregatorCredentials,
	accessTokenCiphertext, refreshTokenCiphertext, dpopPrivateKeyCiphertext []byte,
) error {
	var err error
	credentials.OAuthAccessToken, err = decryptOptionalCredential(
		r.cipher, accessTokenCiphertext, aggregatorOAuthAccessTokenCredentialContext(credentials.DID))
	if err != nil {
		return fmt.Errorf("decrypt OAuth access token for DID %s: %w", credentials.DID, err)
	}
	credentials.OAuthRefreshToken, err = decryptOptionalCredential(
		r.cipher, refreshTokenCiphertext, aggregatorOAuthRefreshTokenCredentialContext(credentials.DID))
	if err != nil {
		return fmt.Errorf("decrypt OAuth refresh token for DID %s: %w", credentials.DID, err)
	}
	credentials.OAuthDPoPPrivateKeyMultibase, err = decryptOptionalCredential(
		r.cipher, dpopPrivateKeyCiphertext, aggregatorOAuthDPoPPrivateKeyCredentialContext(credentials.DID))
	if err != nil {
		return fmt.Errorf("decrypt OAuth DPoP private key for DID %s: %w", credentials.DID, err)
	}
	return nil
}

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
