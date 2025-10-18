package communities

import (
	"time"
)

// Community represents a Coves community indexed from the firehose
// Communities are federated, instance-scoped forums built on atProto
type Community struct {
	CreatedAt              time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt              time.Time `json:"updatedAt" db:"updated_at"`
	RecordURI              string    `json:"recordUri,omitempty" db:"record_uri"`
	FederatedFrom          string    `json:"federatedFrom,omitempty" db:"federated_from"`
	DisplayName            string    `json:"displayName" db:"display_name"`
	Description            string    `json:"description" db:"description"`
	PDSURL                 string    `json:"-" db:"pds_url"`
	AvatarCID              string    `json:"avatarCid,omitempty" db:"avatar_cid"`
	BannerCID              string    `json:"bannerCid,omitempty" db:"banner_cid"`
	OwnerDID               string    `json:"ownerDid" db:"owner_did"`
	CreatedByDID           string    `json:"createdByDid" db:"created_by_did"`
	HostedByDID            string    `json:"hostedByDid" db:"hosted_by_did"`
	PDSEmail               string    `json:"-" db:"pds_email"`
	PDSPassword            string    `json:"-" db:"pds_password_encrypted"`
	Name                   string    `json:"name" db:"name"`
	RecordCID              string    `json:"recordCid,omitempty" db:"record_cid"`
	FederatedID            string    `json:"federatedId,omitempty" db:"federated_id"`
	PDSAccessToken         string    `json:"-" db:"pds_access_token"`
	SigningKeyPEM          string    `json:"-" db:"signing_key_encrypted"`
	ModerationType         string    `json:"moderationType,omitempty" db:"moderation_type"`
	Handle                 string    `json:"handle" db:"handle"`
	PDSRefreshToken        string    `json:"-" db:"pds_refresh_token"`
	Visibility             string    `json:"visibility" db:"visibility"`
	RotationKeyPEM         string    `json:"-" db:"rotation_key_encrypted"`
	DID                    string    `json:"did" db:"did"`
	ContentWarnings        []string  `json:"contentWarnings,omitempty" db:"content_warnings"`
	DescriptionFacets      []byte    `json:"descriptionFacets,omitempty" db:"description_facets"`
	PostCount              int       `json:"postCount" db:"post_count"`
	SubscriberCount        int       `json:"subscriberCount" db:"subscriber_count"`
	MemberCount            int       `json:"memberCount" db:"member_count"`
	ID                     int       `json:"id" db:"id"`
	AllowExternalDiscovery bool      `json:"allowExternalDiscovery" db:"allow_external_discovery"`
}

// Subscription represents a lightweight feed follow (user subscribes to see posts)
type Subscription struct {
	SubscribedAt      time.Time `json:"subscribedAt" db:"subscribed_at"`
	UserDID           string    `json:"userDid" db:"user_did"`
	CommunityDID      string    `json:"communityDid" db:"community_did"`
	RecordURI         string    `json:"recordUri,omitempty" db:"record_uri"`
	RecordCID         string    `json:"recordCid,omitempty" db:"record_cid"`
	ContentVisibility int       `json:"contentVisibility" db:"content_visibility"` // Feed slider: 1-5 (1=best content only, 5=all content)
	ID                int       `json:"id" db:"id"`
}

// CommunityBlock represents a user blocking a community
// Block records live in the user's repository (at://user_did/social.coves.community.block/{rkey})
type CommunityBlock struct {
	BlockedAt    time.Time `json:"blockedAt" db:"blocked_at"`
	UserDID      string    `json:"userDid" db:"user_did"`
	CommunityDID string    `json:"communityDid" db:"community_did"`
	RecordURI    string    `json:"recordUri,omitempty" db:"record_uri"`
	RecordCID    string    `json:"recordCid,omitempty" db:"record_cid"`
	ID           int       `json:"id" db:"id"`
}

// Membership represents active participation with reputation tracking
type Membership struct {
	JoinedAt          time.Time `json:"joinedAt" db:"joined_at"`
	LastActiveAt      time.Time `json:"lastActiveAt" db:"last_active_at"`
	UserDID           string    `json:"userDid" db:"user_did"`
	CommunityDID      string    `json:"communityDid" db:"community_did"`
	ID                int       `json:"id" db:"id"`
	ReputationScore   int       `json:"reputationScore" db:"reputation_score"`
	ContributionCount int       `json:"contributionCount" db:"contribution_count"`
	IsBanned          bool      `json:"isBanned" db:"is_banned"`
	IsModerator       bool      `json:"isModerator" db:"is_moderator"`
}

// ModerationAction represents a moderation action taken against a community
type ModerationAction struct {
	CreatedAt    time.Time  `json:"createdAt" db:"created_at"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty" db:"expires_at"`
	CommunityDID string     `json:"communityDid" db:"community_did"`
	Action       string     `json:"action" db:"action"`
	Reason       string     `json:"reason,omitempty" db:"reason"`
	InstanceDID  string     `json:"instanceDid" db:"instance_did"`
	ID           int        `json:"id" db:"id"`
	Broadcast    bool       `json:"broadcast" db:"broadcast"`
}

// CreateCommunityRequest represents input for creating a new community
type CreateCommunityRequest struct {
	Name                   string   `json:"name"`
	DisplayName            string   `json:"displayName,omitempty"`
	Description            string   `json:"description"`
	Language               string   `json:"language,omitempty"`
	Visibility             string   `json:"visibility"`
	CreatedByDID           string   `json:"createdByDid"`
	HostedByDID            string   `json:"hostedByDid"`
	AvatarBlob             []byte   `json:"avatarBlob,omitempty"`
	BannerBlob             []byte   `json:"bannerBlob,omitempty"`
	Rules                  []string `json:"rules,omitempty"`
	Categories             []string `json:"categories,omitempty"`
	AllowExternalDiscovery bool     `json:"allowExternalDiscovery"`
}

// UpdateCommunityRequest represents input for updating community metadata
type UpdateCommunityRequest struct {
	CommunityDID           string   `json:"communityDid"`
	UpdatedByDID           string   `json:"updatedByDid"` // User making the update (for authorization)
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
	Visibility string `json:"visibility,omitempty"`
	HostedBy   string `json:"hostedBy,omitempty"`
	SortBy     string `json:"sortBy,omitempty"`
	SortOrder  string `json:"sortOrder,omitempty"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

// SearchCommunitiesRequest represents query parameters for searching communities
type SearchCommunitiesRequest struct {
	Query      string `json:"query"`
	Visibility string `json:"visibility,omitempty"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}
