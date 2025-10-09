package communities

import (
	"time"
)

// Community represents a Coves community indexed from the firehose
// Communities are federated, instance-scoped forums built on atProto
type Community struct {
	ID          int       `json:"id" db:"id"`
	DID         string    `json:"did" db:"did"`                     // Permanent community identifier (did:plc:xxx)
	Handle      string    `json:"handle" db:"handle"`               // Scoped handle (!gaming@coves.social)
	Name        string    `json:"name" db:"name"`                   // Short name (local part of handle)
	DisplayName string    `json:"displayName" db:"display_name"`    // Display name for UI
	Description string    `json:"description" db:"description"`     // Community description
	DescriptionFacets []byte `json:"descriptionFacets,omitempty" db:"description_facets"` // Rich text annotations (JSONB)

	// Media
	AvatarCID string `json:"avatarCid,omitempty" db:"avatar_cid"` // CID of avatar image
	BannerCID string `json:"bannerCid,omitempty" db:"banner_cid"` // CID of banner image

	// Ownership
	OwnerDID     string `json:"ownerDid" db:"owner_did"`           // Instance DID in V1, community DID in V3
	CreatedByDID string `json:"createdByDid" db:"created_by_did"`  // User who created the community
	HostedByDID  string `json:"hostedByDid" db:"hosted_by_did"`    // Instance hosting this community

	// Visibility & Federation
	Visibility              string `json:"visibility" db:"visibility"`                               // public, unlisted, private
	AllowExternalDiscovery  bool   `json:"allowExternalDiscovery" db:"allow_external_discovery"`    // Can other instances index?

	// Moderation
	ModerationType   string   `json:"moderationType,omitempty" db:"moderation_type"`       // moderator, sortition
	ContentWarnings  []string `json:"contentWarnings,omitempty" db:"content_warnings"`     // NSFW, violence, spoilers

	// Statistics (cached counts)
	MemberCount     int `json:"memberCount" db:"member_count"`
	SubscriberCount int `json:"subscriberCount" db:"subscriber_count"`
	PostCount       int `json:"postCount" db:"post_count"`

	// Federation metadata (future: Lemmy interop)
	FederatedFrom string `json:"federatedFrom,omitempty" db:"federated_from"` // lemmy, coves
	FederatedID   string `json:"federatedId,omitempty" db:"federated_id"`     // Original ID on source platform

	// Timestamps
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" db:"updated_at"`

	// AT-Proto metadata
	RecordURI string `json:"recordUri,omitempty" db:"record_uri"` // AT-URI of community profile record
	RecordCID string `json:"recordCid,omitempty" db:"record_cid"` // CID of community profile record
}

// Subscription represents a lightweight feed follow (user subscribes to see posts)
type Subscription struct {
	ID           int       `json:"id" db:"id"`
	UserDID      string    `json:"userDid" db:"user_did"`
	CommunityDID string    `json:"communityDid" db:"community_did"`
	SubscribedAt time.Time `json:"subscribedAt" db:"subscribed_at"`

	// AT-Proto metadata (subscription is a record in user's repo)
	RecordURI string `json:"recordUri,omitempty" db:"record_uri"`
	RecordCID string `json:"recordCid,omitempty" db:"record_cid"`
}

// Membership represents active participation with reputation tracking
type Membership struct {
	ID                int       `json:"id" db:"id"`
	UserDID           string    `json:"userDid" db:"user_did"`
	CommunityDID      string    `json:"communityDid" db:"community_did"`
	ReputationScore   int       `json:"reputationScore" db:"reputation_score"`       // Gained through participation
	ContributionCount int       `json:"contributionCount" db:"contribution_count"`   // Posts + comments + actions
	JoinedAt          time.Time `json:"joinedAt" db:"joined_at"`
	LastActiveAt      time.Time `json:"lastActiveAt" db:"last_active_at"`

	// Moderation status
	IsBanned     bool `json:"isBanned" db:"is_banned"`
	IsModerator  bool `json:"isModerator" db:"is_moderator"`
}

// ModerationAction represents a moderation action taken against a community
type ModerationAction struct {
	ID           int       `json:"id" db:"id"`
	CommunityDID string    `json:"communityDid" db:"community_did"`
	Action       string    `json:"action" db:"action"`               // delist, quarantine, remove
	Reason       string    `json:"reason,omitempty" db:"reason"`
	InstanceDID  string    `json:"instanceDid" db:"instance_did"`    // Which instance took this action
	Broadcast    bool      `json:"broadcast" db:"broadcast"`         // Share signal with network?
	CreatedAt    time.Time `json:"createdAt" db:"created_at"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty" db:"expires_at"` // Optional: temporary moderation
}

// CreateCommunityRequest represents input for creating a new community
type CreateCommunityRequest struct {
	Name                   string   `json:"name"`
	DisplayName            string   `json:"displayName,omitempty"`
	Description            string   `json:"description"`
	AvatarBlob             []byte   `json:"avatarBlob,omitempty"`           // Raw image data
	BannerBlob             []byte   `json:"bannerBlob,omitempty"`           // Raw image data
	Rules                  []string `json:"rules,omitempty"`
	Categories             []string `json:"categories,omitempty"`
	Language               string   `json:"language,omitempty"`
	Visibility             string   `json:"visibility"`                     // public, unlisted, private
	AllowExternalDiscovery bool     `json:"allowExternalDiscovery"`
	CreatedByDID           string   `json:"createdByDid"`                   // User creating the community
	HostedByDID            string   `json:"hostedByDid"`                    // Instance hosting the community
}

// UpdateCommunityRequest represents input for updating community metadata
type UpdateCommunityRequest struct {
	CommunityDID           string   `json:"communityDid"`
	UpdatedByDID           string   `json:"updatedByDid"`                     // User making the update (for authorization)
	DisplayName            *string  `json:"displayName,omitempty"`
	Description            *string  `json:"description,omitempty"`
	AvatarBlob             []byte   `json:"avatarBlob,omitempty"`
	BannerBlob             []byte   `json:"bannerBlob,omitempty"`
	Visibility             *string  `json:"visibility,omitempty"`
	AllowExternalDiscovery *bool    `json:"allowExternalDiscovery,omitempty"`
	ModerationType         *string  `json:"moderationType,omitempty"`
	ContentWarnings        []string `json:"contentWarnings,omitempty"`
}

// ListCommunitiesRequest represents query parameters for listing communities
type ListCommunitiesRequest struct {
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	Visibility string `json:"visibility,omitempty"`    // Filter by visibility
	HostedBy   string `json:"hostedBy,omitempty"`      // Filter by hosting instance
	SortBy     string `json:"sortBy,omitempty"`        // created_at, member_count, post_count
	SortOrder  string `json:"sortOrder,omitempty"`     // asc, desc
}

// SearchCommunitiesRequest represents query parameters for searching communities
type SearchCommunitiesRequest struct {
	Query      string `json:"query"`                   // Search term
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	Visibility string `json:"visibility,omitempty"`    // Filter by visibility
}
