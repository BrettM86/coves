package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"Coves/internal/core/communities"

	"github.com/lib/pq"
)

type postgresCommunityRepo struct {
	db *sql.DB
}

// NewCommunityRepository creates a new PostgreSQL community repository
func NewCommunityRepository(db *sql.DB) communities.Repository {
	return &postgresCommunityRepo{db: db}
}

// Create inserts a new community into the communities table
func (r *postgresCommunityRepo) Create(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	// Validate that handle is always provided (constructed by consumer)
	if community.Handle == "" {
		return nil, fmt.Errorf("handle is required (should be constructed by consumer before insert)")
	}

	query := `
		INSERT INTO communities (
			did, handle, name, display_name, description, description_facets,
			avatar_cid, banner_cid, owner_did, created_by_did, hosted_by_did,
			pds_email, pds_password_encrypted,
			pds_access_token_encrypted, pds_refresh_token_encrypted, pds_url,
			visibility, allow_external_discovery, moderation_type, content_warnings,
			member_count, subscriber_count, post_count,
			federated_from, federated_id, created_at, updated_at,
			record_uri, record_cid
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			$12,
			CASE WHEN $13 != '' THEN pgp_sym_encrypt($13, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) ELSE NULL END,
			CASE WHEN $14 != '' THEN pgp_sym_encrypt($14, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) ELSE NULL END,
			CASE WHEN $15 != '' THEN pgp_sym_encrypt($15, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)) ELSE NULL END,
			$16,
			$17, $18, $19, $20,
			$21, $22, $23, $24, $25, $26, $27, $28, $29
		)
		RETURNING id, created_at, updated_at`

	// Handle JSONB field - use sql.NullString with valid JSON or NULL
	var descFacets interface{}
	if len(community.DescriptionFacets) > 0 {
		descFacets = community.DescriptionFacets
	} else {
		descFacets = nil
	}

	err := r.db.QueryRowContext(ctx, query,
		community.DID,
		community.Handle, // Always non-empty - constructed by AppView consumer
		community.Name,
		nullString(community.DisplayName),
		nullString(community.Description),
		descFacets,
		nullString(community.AvatarCID),
		nullString(community.BannerCID),
		community.OwnerDID,
		community.CreatedByDID,
		community.HostedByDID,
		// V2.0: PDS credentials for community account (encrypted at rest)
		nullString(community.PDSEmail),
		nullString(community.PDSPassword),     // Encrypted by pgp_sym_encrypt
		nullString(community.PDSAccessToken),  // Encrypted by pgp_sym_encrypt
		nullString(community.PDSRefreshToken), // Encrypted by pgp_sym_encrypt
		nullString(community.PDSURL),
		// V2.0: No key columns - PDS manages all keys
		community.Visibility,
		community.AllowExternalDiscovery,
		nullString(community.ModerationType),
		pq.Array(community.ContentWarnings),
		community.MemberCount,
		community.SubscriberCount,
		community.PostCount,
		nullString(community.FederatedFrom),
		nullString(community.FederatedID),
		community.CreatedAt,
		community.UpdatedAt,
		nullString(community.RecordURI),
		nullString(community.RecordCID),
	).Scan(&community.ID, &community.CreatedAt, &community.UpdatedAt)
	if err != nil {
		// Check for unique constraint violations
		if strings.Contains(err.Error(), "duplicate key") {
			if strings.Contains(err.Error(), "communities_did_key") {
				return nil, communities.ErrCommunityAlreadyExists
			}
			if strings.Contains(err.Error(), "communities_handle_key") {
				return nil, communities.ErrHandleTaken
			}
		}
		return nil, fmt.Errorf("failed to create community: %w", err)
	}

	return community, nil
}

// GetByDID retrieves a community by its DID
// Note: PDS credentials are included (for internal service use only)
// Handlers MUST use json:"-" tags to prevent credential exposure in APIs
//
// V2.0: Key columns not included - PDS manages all keys
func (r *postgresCommunityRepo) GetByDID(ctx context.Context, did string) (*communities.Community, error) {
	community := &communities.Community{}
	query := `
		SELECT id, did, handle, name, display_name, description, description_facets,
			avatar_cid, banner_cid, owner_did, created_by_did, hosted_by_did,
			pds_email,
			CASE
				WHEN pds_password_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(pds_password_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as pds_password,
			CASE
				WHEN pds_access_token_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(pds_access_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as pds_access_token,
			CASE
				WHEN pds_refresh_token_encrypted IS NOT NULL
				THEN pgp_sym_decrypt(pds_refresh_token_encrypted, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1))
				ELSE NULL
			END as pds_refresh_token,
			pds_url,
			visibility, allow_external_discovery, moderation_type, content_warnings,
			member_count, subscriber_count, post_count,
			federated_from, federated_id, created_at, updated_at,
			record_uri, record_cid
		FROM communities
		WHERE did = $1`

	var displayName, description, avatarCID, bannerCID, moderationType sql.NullString
	var federatedFrom, federatedID, recordURI, recordCID sql.NullString
	var pdsEmail, pdsPassword, pdsAccessToken, pdsRefreshToken, pdsURL sql.NullString
	var descFacets []byte
	var contentWarnings []string

	err := r.db.QueryRowContext(ctx, query, did).Scan(
		&community.ID, &community.DID, &community.Handle, &community.Name,
		&displayName, &description, &descFacets,
		&avatarCID, &bannerCID,
		&community.OwnerDID, &community.CreatedByDID, &community.HostedByDID,
		// V2.0: PDS credentials (decrypted from pgp_sym_encrypt)
		&pdsEmail, &pdsPassword, &pdsAccessToken, &pdsRefreshToken, &pdsURL,
		&community.Visibility, &community.AllowExternalDiscovery,
		&moderationType, pq.Array(&contentWarnings),
		&community.MemberCount, &community.SubscriberCount, &community.PostCount,
		&federatedFrom, &federatedID,
		&community.CreatedAt, &community.UpdatedAt,
		&recordURI, &recordCID,
	)

	if err == sql.ErrNoRows {
		return nil, communities.ErrCommunityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get community by DID: %w", err)
	}

	// Map nullable fields
	community.DisplayName = displayName.String
	community.Description = description.String
	community.AvatarCID = avatarCID.String
	community.BannerCID = bannerCID.String
	community.PDSEmail = pdsEmail.String
	community.PDSPassword = pdsPassword.String
	community.PDSAccessToken = pdsAccessToken.String
	community.PDSRefreshToken = pdsRefreshToken.String
	community.PDSURL = pdsURL.String
	// V2.0: No key fields - PDS manages all keys
	community.RotationKeyPEM = "" // Empty - PDS-managed
	community.SigningKeyPEM = ""  // Empty - PDS-managed
	community.ModerationType = moderationType.String
	community.ContentWarnings = contentWarnings
	community.FederatedFrom = federatedFrom.String
	community.FederatedID = federatedID.String
	community.RecordURI = recordURI.String
	community.RecordCID = recordCID.String
	if descFacets != nil {
		community.DescriptionFacets = descFacets
	}

	return community, nil
}

// GetByHandle retrieves a community by its scoped handle
func (r *postgresCommunityRepo) GetByHandle(ctx context.Context, handle string) (*communities.Community, error) {
	community := &communities.Community{}
	query := `
		SELECT id, did, handle, name, display_name, description, description_facets,
			avatar_cid, banner_cid, owner_did, created_by_did, hosted_by_did,
			visibility, allow_external_discovery, moderation_type, content_warnings,
			member_count, subscriber_count, post_count,
			federated_from, federated_id, created_at, updated_at,
			record_uri, record_cid
		FROM communities
		WHERE handle = $1`

	var displayName, description, avatarCID, bannerCID, moderationType sql.NullString
	var federatedFrom, federatedID, recordURI, recordCID sql.NullString
	var descFacets []byte
	var contentWarnings []string

	err := r.db.QueryRowContext(ctx, query, handle).Scan(
		&community.ID, &community.DID, &community.Handle, &community.Name,
		&displayName, &description, &descFacets,
		&avatarCID, &bannerCID,
		&community.OwnerDID, &community.CreatedByDID, &community.HostedByDID,
		&community.Visibility, &community.AllowExternalDiscovery,
		&moderationType, pq.Array(&contentWarnings),
		&community.MemberCount, &community.SubscriberCount, &community.PostCount,
		&federatedFrom, &federatedID,
		&community.CreatedAt, &community.UpdatedAt,
		&recordURI, &recordCID,
	)

	if err == sql.ErrNoRows {
		return nil, communities.ErrCommunityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get community by handle: %w", err)
	}

	// Map nullable fields
	community.DisplayName = displayName.String
	community.Description = description.String
	community.AvatarCID = avatarCID.String
	community.BannerCID = bannerCID.String
	community.ModerationType = moderationType.String
	community.ContentWarnings = contentWarnings
	community.FederatedFrom = federatedFrom.String
	community.FederatedID = federatedID.String
	community.RecordURI = recordURI.String
	community.RecordCID = recordCID.String
	if descFacets != nil {
		community.DescriptionFacets = descFacets
	}

	return community, nil
}

// Update modifies an existing community's metadata
func (r *postgresCommunityRepo) Update(ctx context.Context, community *communities.Community) (*communities.Community, error) {
	query := `
		UPDATE communities
		SET display_name = $2, description = $3, description_facets = $4,
			avatar_cid = $5, banner_cid = $6,
			visibility = $7, allow_external_discovery = $8,
			moderation_type = $9, content_warnings = $10,
			updated_at = NOW(),
			record_uri = $11, record_cid = $12
		WHERE did = $1
		RETURNING updated_at`

	// Handle JSONB field - use sql.NullString with valid JSON or NULL
	var descFacets interface{}
	if len(community.DescriptionFacets) > 0 {
		descFacets = community.DescriptionFacets
	} else {
		descFacets = nil
	}

	err := r.db.QueryRowContext(ctx, query,
		community.DID,
		nullString(community.DisplayName),
		nullString(community.Description),
		descFacets,
		nullString(community.AvatarCID),
		nullString(community.BannerCID),
		community.Visibility,
		community.AllowExternalDiscovery,
		nullString(community.ModerationType),
		pq.Array(community.ContentWarnings),
		nullString(community.RecordURI),
		nullString(community.RecordCID),
	).Scan(&community.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, communities.ErrCommunityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update community: %w", err)
	}

	return community, nil
}

// UpdateCredentials atomically updates community's PDS access and refresh tokens
// CRITICAL: Both tokens must be updated together because refresh tokens are single-use
// After a successful token refresh, the old refresh token is immediately revoked by the PDS
func (r *postgresCommunityRepo) UpdateCredentials(ctx context.Context, did, accessToken, refreshToken string) error {
	query := `
		UPDATE communities
		SET
			pds_access_token_encrypted = pgp_sym_encrypt($2, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)),
			pds_refresh_token_encrypted = pgp_sym_encrypt($3, (SELECT encode(key_data, 'hex') FROM encryption_keys WHERE id = 1)),
			updated_at = NOW()
		WHERE did = $1
		RETURNING did`

	var returnedDID string
	err := r.db.QueryRowContext(ctx, query, did, accessToken, refreshToken).Scan(&returnedDID)

	if err == sql.ErrNoRows {
		return communities.ErrCommunityNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to update credentials: %w", err)
	}

	return nil
}

// Delete removes a community from the database
func (r *postgresCommunityRepo) Delete(ctx context.Context, did string) error {
	query := `DELETE FROM communities WHERE did = $1`

	result, err := r.db.ExecContext(ctx, query, did)
	if err != nil {
		return fmt.Errorf("failed to delete community: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check delete result: %w", err)
	}

	if rowsAffected == 0 {
		return communities.ErrCommunityNotFound
	}

	return nil
}

// List retrieves communities with filtering and pagination
func (r *postgresCommunityRepo) List(ctx context.Context, req communities.ListCommunitiesRequest) ([]*communities.Community, error) {
	// Build query with filters
	whereClauses := []string{}
	args := []interface{}{}
	argCount := 1

	if req.Visibility != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("visibility = $%d", argCount))
		args = append(args, req.Visibility)
		argCount++
	}

	// TODO: Add category filter when DB schema supports it
	// if req.Category != "" { ... }

	// TODO: Add language filter when DB schema supports it
	// if req.Language != "" { ... }

	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Build sort clause - map sort enum to DB columns
	sortColumn := "subscriber_count" // default: popular
	sortOrder := "DESC"

	switch req.Sort {
	case "popular":
		// Most subscribers (default)
		sortColumn = "subscriber_count"
		sortOrder = "DESC"
	case "active":
		// Most posts/activity
		sortColumn = "post_count"
		sortOrder = "DESC"
	case "new":
		// Recently created
		sortColumn = "created_at"
		sortOrder = "DESC"
	case "alphabetical":
		// Sorted by name A-Z
		sortColumn = "name"
		sortOrder = "ASC"
	default:
		// Fallback to popular if empty or invalid (should be validated in handler)
		sortColumn = "subscriber_count"
		sortOrder = "DESC"
	}

	// Get communities with pagination
	query := fmt.Sprintf(`
		SELECT id, did, handle, name, display_name, description, description_facets,
			avatar_cid, banner_cid, owner_did, created_by_did, hosted_by_did,
			visibility, allow_external_discovery, moderation_type, content_warnings,
			member_count, subscriber_count, post_count,
			federated_from, federated_id, created_at, updated_at,
			record_uri, record_cid
		FROM communities
		%s
		ORDER BY %s %s
		LIMIT $%d OFFSET $%d`,
		whereClause, sortColumn, sortOrder, argCount, argCount+1)

	args = append(args, req.Limit, req.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list communities: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	result := []*communities.Community{}
	for rows.Next() {
		community := &communities.Community{}
		var displayName, description, avatarCID, bannerCID, moderationType sql.NullString
		var federatedFrom, federatedID, recordURI, recordCID sql.NullString
		var descFacets []byte
		var contentWarnings []string

		scanErr := rows.Scan(
			&community.ID, &community.DID, &community.Handle, &community.Name,
			&displayName, &description, &descFacets,
			&avatarCID, &bannerCID,
			&community.OwnerDID, &community.CreatedByDID, &community.HostedByDID,
			&community.Visibility, &community.AllowExternalDiscovery,
			&moderationType, pq.Array(&contentWarnings),
			&community.MemberCount, &community.SubscriberCount, &community.PostCount,
			&federatedFrom, &federatedID,
			&community.CreatedAt, &community.UpdatedAt,
			&recordURI, &recordCID,
		)
		if scanErr != nil {
			return nil, fmt.Errorf("failed to scan community: %w", scanErr)
		}

		// Map nullable fields
		community.DisplayName = displayName.String
		community.Description = description.String
		community.AvatarCID = avatarCID.String
		community.BannerCID = bannerCID.String
		community.ModerationType = moderationType.String
		community.ContentWarnings = contentWarnings
		community.FederatedFrom = federatedFrom.String
		community.FederatedID = federatedID.String
		community.RecordURI = recordURI.String
		community.RecordCID = recordCID.String
		if descFacets != nil {
			community.DescriptionFacets = descFacets
		}

		result = append(result, community)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating communities: %w", err)
	}

	return result, nil
}

// Search searches communities by name/description using fuzzy matching
func (r *postgresCommunityRepo) Search(ctx context.Context, req communities.SearchCommunitiesRequest) ([]*communities.Community, int, error) {
	// Build query with fuzzy search and visibility filter
	whereClauses := []string{
		"(name ILIKE '%' || $1 || '%' OR description ILIKE '%' || $1 || '%')",
	}
	args := []interface{}{req.Query}
	argCount := 2

	if req.Visibility != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("visibility = $%d", argCount))
		args = append(args, req.Visibility)
		argCount++
	}

	whereClause := "WHERE " + strings.Join(whereClauses, " AND ")

	// Get total count
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM communities %s", whereClause)
	var totalCount int
	err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count search results: %w", err)
	}

	// Search with relevance ranking using pg_trgm similarity
	// Filter out results with very low relevance (< 0.2) to avoid noise
	query := fmt.Sprintf(`
		SELECT id, did, handle, name, display_name, description, description_facets,
			avatar_cid, banner_cid, owner_did, created_by_did, hosted_by_did,
			visibility, allow_external_discovery, moderation_type, content_warnings,
			member_count, subscriber_count, post_count,
			federated_from, federated_id, created_at, updated_at,
			record_uri, record_cid,
			similarity(name, $1) + similarity(COALESCE(description, ''), $1) as relevance
		FROM communities
		%s AND (similarity(name, $1) + similarity(COALESCE(description, ''), $1)) > 0.2
		ORDER BY relevance DESC, member_count DESC
		LIMIT $%d OFFSET $%d`,
		whereClause, argCount, argCount+1)

	args = append(args, req.Limit, req.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to search communities: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			log.Printf("Failed to close rows: %v", closeErr)
		}
	}()

	result := []*communities.Community{}
	for rows.Next() {
		community := &communities.Community{}
		var displayName, description, avatarCID, bannerCID, moderationType sql.NullString
		var federatedFrom, federatedID, recordURI, recordCID sql.NullString
		var descFacets []byte
		var contentWarnings []string
		var relevance float64

		scanErr := rows.Scan(
			&community.ID, &community.DID, &community.Handle, &community.Name,
			&displayName, &description, &descFacets,
			&avatarCID, &bannerCID,
			&community.OwnerDID, &community.CreatedByDID, &community.HostedByDID,
			&community.Visibility, &community.AllowExternalDiscovery,
			&moderationType, pq.Array(&contentWarnings),
			&community.MemberCount, &community.SubscriberCount, &community.PostCount,
			&federatedFrom, &federatedID,
			&community.CreatedAt, &community.UpdatedAt,
			&recordURI, &recordCID,
			&relevance,
		)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("failed to scan community: %w", scanErr)
		}

		// Map nullable fields
		community.DisplayName = displayName.String
		community.Description = description.String
		community.AvatarCID = avatarCID.String
		community.BannerCID = bannerCID.String
		community.ModerationType = moderationType.String
		community.ContentWarnings = contentWarnings
		community.FederatedFrom = federatedFrom.String
		community.FederatedID = federatedID.String
		community.RecordURI = recordURI.String
		community.RecordCID = recordCID.String
		if descFacets != nil {
			community.DescriptionFacets = descFacets
		}

		result = append(result, community)
	}

	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("error iterating search results: %w", err)
	}

	return result, totalCount, nil
}

// Helper functions
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
