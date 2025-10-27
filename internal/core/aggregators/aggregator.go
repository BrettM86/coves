package aggregators

import "time"

// Aggregator represents a service declaration record indexed from the firehose
// Aggregators are autonomous services that can post content to communities after authorization
// Following Bluesky's pattern: app.bsky.feed.generator and app.bsky.labeler.service
type Aggregator struct {
	DID              string    `json:"did" db:"did"`                                // Aggregator's DID (primary key)
	DisplayName      string    `json:"displayName" db:"display_name"`               // Human-readable name
	Description      string    `json:"description,omitempty" db:"description"`      // What the aggregator does
	AvatarURL        string    `json:"avatarUrl,omitempty" db:"avatar_url"`         // Optional avatar image URL
	ConfigSchema     []byte    `json:"configSchema,omitempty" db:"config_schema"`   // JSON Schema for configuration (JSONB)
	MaintainerDID    string    `json:"maintainerDid,omitempty" db:"maintainer_did"` // Contact for support/issues
	SourceURL        string    `json:"sourceUrl,omitempty" db:"source_url"`         // Source code URL (transparency)
	CommunitiesUsing int       `json:"communitiesUsing" db:"communities_using"`     // Auto-updated by trigger
	PostsCreated     int       `json:"postsCreated" db:"posts_created"`             // Auto-updated by trigger
	CreatedAt        time.Time `json:"createdAt" db:"created_at"`                   // When aggregator was created (from lexicon)
	IndexedAt        time.Time `json:"indexedAt" db:"indexed_at"`                   // When we indexed this record
	RecordURI        string    `json:"recordUri,omitempty" db:"record_uri"`         // at://did/social.coves.aggregator.service/self
	RecordCID        string    `json:"recordCid,omitempty" db:"record_cid"`         // Content hash
}

// Authorization represents a community's authorization for an aggregator
// Stored in community's repository: at://community_did/social.coves.aggregator.authorization/{rkey}
type Authorization struct {
	ID            int        `json:"id" db:"id"`                            // Database ID
	AggregatorDID string     `json:"aggregatorDid" db:"aggregator_did"`     // Which aggregator
	CommunityDID  string     `json:"communityDid" db:"community_did"`       // Which community
	Enabled       bool       `json:"enabled" db:"enabled"`                  // Current status
	Config        []byte     `json:"config,omitempty" db:"config"`          // Aggregator-specific config (JSONB)
	CreatedBy     string     `json:"createdBy,omitempty" db:"created_by"`   // Moderator DID who enabled it
	DisabledBy    string     `json:"disabledBy,omitempty" db:"disabled_by"` // Moderator DID who disabled it
	CreatedAt     time.Time  `json:"createdAt" db:"created_at"`             // When authorization was created
	DisabledAt    *time.Time `json:"disabledAt,omitempty" db:"disabled_at"` // When authorization was disabled (for modlog/audit)
	IndexedAt     time.Time  `json:"indexedAt" db:"indexed_at"`             // When we indexed this record
	RecordURI     string     `json:"recordUri,omitempty" db:"record_uri"`   // at://community_did/social.coves.aggregator.authorization/{rkey}
	RecordCID     string     `json:"recordCid,omitempty" db:"record_cid"`   // Content hash
}

// AggregatorPost represents tracking of posts created by aggregators
// AppView-only table for rate limiting and statistics
type AggregatorPost struct {
	ID            int       `json:"id" db:"id"`
	AggregatorDID string    `json:"aggregatorDid" db:"aggregator_did"`
	CommunityDID  string    `json:"communityDid" db:"community_did"`
	PostURI       string    `json:"postUri" db:"post_uri"`
	PostCID       string    `json:"postCid" db:"post_cid"`
	CreatedAt     time.Time `json:"createdAt" db:"created_at"`
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
