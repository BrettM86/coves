package aggregators

import "time"

// Aggregator represents a service declaration record indexed from the firehose
// Aggregators are autonomous services that can post content to communities after authorization
// Following Bluesky's pattern: app.bsky.feed.generator and app.bsky.labeler.service
type Aggregator struct {
	// Core timestamps
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
	IndexedAt time.Time `json:"indexedAt" db:"indexed_at"`

	// Identity and display
	DID         string `json:"did" db:"did"`
	DisplayName string `json:"displayName" db:"display_name"`
	Description string `json:"description,omitempty" db:"description"`
	AvatarURL   string `json:"avatarUrl,omitempty" db:"avatar_url"`

	// Metadata
	MaintainerDID string `json:"maintainerDid,omitempty" db:"maintainer_did"`
	SourceURL     string `json:"sourceUrl,omitempty" db:"source_url"`
	RecordURI     string `json:"recordUri,omitempty" db:"record_uri"`
	RecordCID     string `json:"recordCid,omitempty" db:"record_cid"`
	ConfigSchema  []byte `json:"configSchema,omitempty" db:"config_schema"`

	// Stats
	CommunitiesUsing int `json:"communitiesUsing" db:"communities_using"`
	PostsCreated     int `json:"postsCreated" db:"posts_created"`
}

// OAuthCredentials holds OAuth session data for aggregator authentication
// Used when setting up or refreshing API key authentication
type OAuthCredentials struct {
	AccessToken             string
	RefreshToken            string
	TokenExpiresAt          time.Time
	PDSURL                  string
	AuthServerIss           string
	AuthServerTokenEndpoint string
	DPoPPrivateKeyMultibase string
	DPoPAuthServerNonce     string
	DPoPPDSNonce            string
}

// Validate checks that all required OAuthCredentials fields are present and valid.
// Returns an error describing the first validation failure, or nil if valid.
func (c *OAuthCredentials) Validate() error {
	if c.AccessToken == "" {
		return NewValidationError("accessToken", "access token is required")
	}
	if c.RefreshToken == "" {
		return NewValidationError("refreshToken", "refresh token is required")
	}
	if c.TokenExpiresAt.IsZero() {
		return NewValidationError("tokenExpiresAt", "token expiry time is required")
	}
	if c.PDSURL == "" {
		return NewValidationError("pdsUrl", "PDS URL is required")
	}
	if c.AuthServerIss == "" {
		return NewValidationError("authServerIss", "auth server issuer is required")
	}
	if c.AuthServerTokenEndpoint == "" {
		return NewValidationError("authServerTokenEndpoint", "auth server token endpoint is required")
	}
	if c.DPoPPrivateKeyMultibase == "" {
		return NewValidationError("dpopPrivateKey", "DPoP private key is required")
	}
	return nil
}

// AggregatorCredentials holds sensitive authentication data for aggregators.
// This is the preferred type for authentication operations - separates concerns
// from the public Aggregator type and prevents credential leakage.
type AggregatorCredentials struct {
	DID string `db:"did"`

	// API Key Authentication
	APIKeyPrefix    string     `db:"api_key_prefix"`
	APIKeyHash      string     `db:"api_key_hash"`
	APIKeyCreatedAt *time.Time `db:"api_key_created_at"`
	APIKeyRevokedAt *time.Time `db:"api_key_revoked_at"`
	APIKeyLastUsed  *time.Time `db:"api_key_last_used_at"`

	// OAuth Session Credentials
	OAuthAccessToken             string     `db:"oauth_access_token"`
	OAuthRefreshToken            string     `db:"oauth_refresh_token"`
	OAuthTokenExpiresAt          *time.Time `db:"oauth_token_expires_at"`
	OAuthPDSURL                  string     `db:"oauth_pds_url"`
	OAuthAuthServerIss           string     `db:"oauth_auth_server_iss"`
	OAuthAuthServerTokenEndpoint string     `db:"oauth_auth_server_token_endpoint"`
	OAuthDPoPPrivateKeyMultibase string     `db:"oauth_dpop_private_key_multibase"`
	OAuthDPoPAuthServerNonce     string     `db:"oauth_dpop_authserver_nonce"`
	OAuthDPoPPDSNonce            string     `db:"oauth_dpop_pds_nonce"`
}

// HasActiveAPIKey returns true if the credentials have an active (non-revoked) API key.
// An active key has a non-empty hash and has not been revoked.
func (c *AggregatorCredentials) HasActiveAPIKey() bool {
	return c.APIKeyHash != "" && c.APIKeyRevokedAt == nil
}

// IsOAuthTokenExpired returns true if the OAuth access token has expired or will expire soon.
// Uses a 5-minute buffer before actual expiry to allow proactive token refresh,
// accounting for clock skew and network latency during refresh operations.
func (c *AggregatorCredentials) IsOAuthTokenExpired() bool {
	if c.OAuthTokenExpiresAt == nil {
		return true
	}
	return time.Now().Add(5 * time.Minute).After(*c.OAuthTokenExpiresAt)
}

// Authorization represents a community's authorization for an aggregator
// Stored in community's repository: at://community_did/social.coves.aggregator.authorization/{rkey}
type Authorization struct {
	CreatedAt     time.Time  `json:"createdAt" db:"created_at"`
	IndexedAt     time.Time  `json:"indexedAt" db:"indexed_at"`
	DisabledAt    *time.Time `json:"disabledAt,omitempty" db:"disabled_at"`
	AggregatorDID string     `json:"aggregatorDid" db:"aggregator_did"`
	CommunityDID  string     `json:"communityDid" db:"community_did"`
	CreatedBy     string     `json:"createdBy,omitempty" db:"created_by"`
	DisabledBy    string     `json:"disabledBy,omitempty" db:"disabled_by"`
	RecordURI     string     `json:"recordUri,omitempty" db:"record_uri"`
	RecordCID     string     `json:"recordCid,omitempty" db:"record_cid"`
	Config        []byte     `json:"config,omitempty" db:"config"`
	ID            int        `json:"id" db:"id"`
	Enabled       bool       `json:"enabled" db:"enabled"`
}

// AggregatorPost represents tracking of posts created by aggregators
// AppView-only table for rate limiting and statistics
type AggregatorPost struct {
	CreatedAt     time.Time `json:"createdAt" db:"created_at"`
	AggregatorDID string    `json:"aggregatorDid" db:"aggregator_did"`
	CommunityDID  string    `json:"communityDid" db:"community_did"`
	PostURI       string    `json:"postUri" db:"post_uri"`
	PostCID       string    `json:"postCid" db:"post_cid"`
	ID            int       `json:"id" db:"id"`
}

// EnableAggregatorRequest represents input for enabling an aggregator in a community
type EnableAggregatorRequest struct {
	CommunityDID   string                 `json:"communityDid"`     // Which community (resolved from identifier)
	AggregatorDID  string                 `json:"aggregatorDid"`    // Which aggregator
	Config         map[string]interface{} `json:"config,omitempty"` // Aggregator-specific configuration
	EnabledByDID   string                 `json:"enabledByDid"`     // Moderator making the change (from JWT)
	EnabledByToken string                 `json:"-"`                // User's access token for PDS write
}

// DisableAggregatorRequest represents input for disabling an aggregator
type DisableAggregatorRequest struct {
	CommunityDID    string `json:"communityDid"`  // Which community (resolved from identifier)
	AggregatorDID   string `json:"aggregatorDid"` // Which aggregator
	DisabledByDID   string `json:"disabledByDid"` // Moderator making the change (from JWT)
	DisabledByToken string `json:"-"`             // User's access token for PDS write
}

// UpdateConfigRequest represents input for updating an aggregator's configuration
type UpdateConfigRequest struct {
	CommunityDID   string                 `json:"communityDid"`  // Which community (resolved from identifier)
	AggregatorDID  string                 `json:"aggregatorDid"` // Which aggregator
	Config         map[string]interface{} `json:"config"`        // New configuration
	UpdatedByDID   string                 `json:"updatedByDid"`  // Moderator making the change (from JWT)
	UpdatedByToken string                 `json:"-"`             // User's access token for PDS write
}

// GetServicesRequest represents query parameters for fetching aggregator details
type GetServicesRequest struct {
	DIDs []string `json:"dids"` // List of aggregator DIDs to fetch
}

// GetAuthorizationsRequest represents query parameters for listing authorizations
type GetAuthorizationsRequest struct {
	AggregatorDID string `json:"aggregatorDid"`         // Which aggregator
	EnabledOnly   bool   `json:"enabledOnly,omitempty"` // Only return enabled authorizations
	Limit         int    `json:"limit"`
	Offset        int    `json:"offset"`
}

// ListForCommunityRequest represents query parameters for listing aggregators for a community
type ListForCommunityRequest struct {
	CommunityDID string `json:"communityDid"`          // Which community (resolved from identifier)
	EnabledOnly  bool   `json:"enabledOnly,omitempty"` // Only return enabled aggregators
	Limit        int    `json:"limit"`
	Offset       int    `json:"offset"`
}
