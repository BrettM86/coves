package aggregators

import "time"

// Aggregator represents a service declaration record indexed from the firehose
// Aggregators are autonomous services that can post content to communities after authorization
// Following Bluesky's pattern: app.bsky.feed.generator and app.bsky.labeler.service
type Aggregator struct {
	CreatedAt        time.Time `json:"createdAt" db:"created_at"`
	IndexedAt        time.Time `json:"indexedAt" db:"indexed_at"`
	AvatarURL        string    `json:"avatarUrl,omitempty" db:"avatar_url"`
	DID              string    `json:"did" db:"did"`
	MaintainerDID    string    `json:"maintainerDid,omitempty" db:"maintainer_did"`
	SourceURL        string    `json:"sourceUrl,omitempty" db:"source_url"`
	Description      string    `json:"description,omitempty" db:"description"`
	DisplayName      string    `json:"displayName" db:"display_name"`
	RecordURI        string    `json:"recordUri,omitempty" db:"record_uri"`
	RecordCID        string    `json:"recordCid,omitempty" db:"record_cid"`
	ConfigSchema     []byte    `json:"configSchema,omitempty" db:"config_schema"`
	CommunitiesUsing int       `json:"communitiesUsing" db:"communities_using"`
	PostsCreated     int       `json:"postsCreated" db:"posts_created"`
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
