package communities

import "context"

// Repository defines the interface for community data persistence
// This is the AppView's indexed view of communities from the firehose
type Repository interface {
	// Community CRUD
	Create(ctx context.Context, community *Community) (*Community, error)
	GetByDID(ctx context.Context, did string) (*Community, error)
	GetByHandle(ctx context.Context, handle string) (*Community, error)
	Update(ctx context.Context, community *Community) (*Community, error)
	Delete(ctx context.Context, did string) error

	// Credential Management (for token refresh)
	UpdateCredentials(ctx context.Context, did, accessToken, refreshToken string) error

	// Listing & Search
	List(ctx context.Context, req ListCommunitiesRequest) ([]*Community, int, error) // Returns communities + total count
	Search(ctx context.Context, req SearchCommunitiesRequest) ([]*Community, int, error)

	// Subscriptions (lightweight feed follows)
	Subscribe(ctx context.Context, subscription *Subscription) (*Subscription, error)
	SubscribeWithCount(ctx context.Context, subscription *Subscription) (*Subscription, error) // Atomic: subscribe + increment count
	Unsubscribe(ctx context.Context, userDID, communityDID string) error
	UnsubscribeWithCount(ctx context.Context, userDID, communityDID string) error // Atomic: unsubscribe + decrement count
	GetSubscription(ctx context.Context, userDID, communityDID string) (*Subscription, error)
	GetSubscriptionByURI(ctx context.Context, recordURI string) (*Subscription, error) // For Jetstream delete operations
	ListSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*Subscription, error)
	ListSubscribers(ctx context.Context, communityDID string, limit, offset int) ([]*Subscription, error)

	// Community Blocks
	BlockCommunity(ctx context.Context, block *CommunityBlock) (*CommunityBlock, error)
	UnblockCommunity(ctx context.Context, userDID, communityDID string) error
	GetBlock(ctx context.Context, userDID, communityDID string) (*CommunityBlock, error)
	GetBlockByURI(ctx context.Context, recordURI string) (*CommunityBlock, error) // For Jetstream delete operations
	ListBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*CommunityBlock, error)
	IsBlocked(ctx context.Context, userDID, communityDID string) (bool, error)

	// Memberships (active participation with reputation)
	CreateMembership(ctx context.Context, membership *Membership) (*Membership, error)
	GetMembership(ctx context.Context, userDID, communityDID string) (*Membership, error)
	UpdateMembership(ctx context.Context, membership *Membership) (*Membership, error)
	ListMembers(ctx context.Context, communityDID string, limit, offset int) ([]*Membership, error)

	// Moderation (V2 feature, prepared interface)
	CreateModerationAction(ctx context.Context, action *ModerationAction) (*ModerationAction, error)
	ListModerationActions(ctx context.Context, communityDID string, limit, offset int) ([]*ModerationAction, error)

	// Statistics
	IncrementMemberCount(ctx context.Context, communityDID string) error
	DecrementMemberCount(ctx context.Context, communityDID string) error
	IncrementSubscriberCount(ctx context.Context, communityDID string) error
	DecrementSubscriberCount(ctx context.Context, communityDID string) error
	IncrementPostCount(ctx context.Context, communityDID string) error
}

// Service defines the interface for community business logic
// Coordinates between Repository and external services (PDS, identity, etc.)
type Service interface {
	// Community operations (write-forward pattern: Service -> PDS -> Firehose -> Consumer -> Repository)
	CreateCommunity(ctx context.Context, req CreateCommunityRequest) (*Community, error)
	GetCommunity(ctx context.Context, identifier string) (*Community, error) // identifier can be DID or handle
	UpdateCommunity(ctx context.Context, req UpdateCommunityRequest) (*Community, error)
	ListCommunities(ctx context.Context, req ListCommunitiesRequest) ([]*Community, int, error)
	SearchCommunities(ctx context.Context, req SearchCommunitiesRequest) ([]*Community, int, error)

	// Subscription operations (write-forward: creates record in user's PDS)
	SubscribeToCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string, contentVisibility int) (*Subscription, error)
	UnsubscribeFromCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string) error
	GetUserSubscriptions(ctx context.Context, userDID string, limit, offset int) ([]*Subscription, error)
	GetCommunitySubscribers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*Subscription, error)

	// Block operations (write-forward: creates record in user's PDS)
	BlockCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string) (*CommunityBlock, error)
	UnblockCommunity(ctx context.Context, userDID, userAccessToken, communityIdentifier string) error
	GetBlockedCommunities(ctx context.Context, userDID string, limit, offset int) ([]*CommunityBlock, error)
	IsBlocked(ctx context.Context, userDID, communityIdentifier string) (bool, error)

	// Membership operations (indexed from firehose, reputation managed internally)
	GetMembership(ctx context.Context, userDID, communityIdentifier string) (*Membership, error)
	ListCommunityMembers(ctx context.Context, communityIdentifier string, limit, offset int) ([]*Membership, error)

	// Validation helpers
	ValidateHandle(handle string) error
	ResolveCommunityIdentifier(ctx context.Context, identifier string) (string, error) // Returns DID from handle or DID
}
