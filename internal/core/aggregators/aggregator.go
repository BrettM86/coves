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

	// API Key Authentication (not exposed in JSON responses)
	APIKeyPrefix    string     `json:"-" db:"api_key_prefix"`
	APIKeyHash      string     `json:"-" db:"api_key_hash"`
	APIKeyCreatedAt *time.Time `json:"-" db:"api_key_created_at"`
	APIKeyRevokedAt *time.Time `json:"-" db:"api_key_revoked_at"`
	APIKeyLastUsed  *time.Time `json:"-" db:"api_key_last_used_at"`

	// OAuth Session Credentials (sensitive - not exposed in JSON)
	OAuthAccessToken            string     `json:"-" db:"oauth_access_token"`
	OAuthRefreshToken           string     `json:"-" db:"oauth_refresh_token"`
	OAuthTokenExpiresAt         *time.Time `json:"-" db:"oauth_token_expires_at"`
	OAuthPDSURL                 string     `json:"-" db:"oauth_pds_url"`
	OAuthAuthServerIss          string     `json:"-" db:"oauth_auth_server_iss"`
	OAuthAuthServerTokenEndpoint string     `json:"-" db:"oauth_auth_server_token_endpoint"`
	OAuthDPoPPrivateKeyMultibase string     `json:"-" db:"oauth_dpop_private_key_multibase"`
	OAuthDPoPAuthServerNonce    string     `json:"-" db:"oauth_dpop_authserver_nonce"`
	OAuthDPoPPDSNonce           string     `json:"-" db:"oauth_dpop_pds_nonce"`
}

// OAuthCredentials holds OAuth session data for aggregator authentication
// Used when setting up or refreshing API key authentication
type OAuthCredentials struct {
	AccessToken            string
	RefreshToken           string
	TokenExpiresAt         time.Time
	PDSURL                 string
	AuthServerIss          string
	AuthServerTokenEndpoint string
	DPoPPrivateKeyMultibase string
	DPoPAuthServerNonce    string
	DPoPPDSNonce           string
}

// HasActiveAPIKey returns true if the aggregator has an active (non-revoked) API key
func (a *Aggregator) HasActiveAPIKey() bool {
	return a.APIKeyHash != "" && a.APIKeyRevokedAt == nil
}

// IsOAuthTokenExpired returns true if the OAuth access token has expired
func (a *Aggregator) IsOAuthTokenExpired() bool {
	if a.OAuthTokenExpiresAt == nil {
		return true
	}
	// Consider expired if within 5 minutes of expiry (buffer for clock skew)
	return time.Now().Add(5 * time.Minute).After(*a.OAuthTokenExpiresAt)
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
