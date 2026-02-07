package userblocks

import (
	"Coves/internal/atproto/pds"
	"context"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// PDSClientFactory creates PDS clients from session data.
// This is primarily used for test injection via NewServiceWithPDSFactory to allow
// password-based auth in integration tests instead of OAuth DPoP.
type PDSClientFactory func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error)

// Repository defines the interface for user block data persistence.
// This is the AppView's indexed view of blocks from the firehose.
type Repository interface {
	// BlockUser creates a new block record (idempotent via ON CONFLICT DO UPDATE)
	BlockUser(ctx context.Context, block *UserBlock) (*UserBlock, error)

	// UnblockUser removes a block record. Returns ErrBlockNotFound if not exists.
	UnblockUser(ctx context.Context, blockerDID, blockedDID string) error

	// GetBlock retrieves a block by blocker + blocked DID pair.
	// Returns ErrBlockNotFound if not exists.
	GetBlock(ctx context.Context, blockerDID, blockedDID string) (*UserBlock, error)

	// GetBlockByURI retrieves a block by its AT-URI.
	// Used by Jetstream consumer for DELETE operations (which don't include record data).
	// Returns ErrBlockNotFound if not exists.
	GetBlockByURI(ctx context.Context, recordURI string) (*UserBlock, error)

	// ListBlockedUsers retrieves all users blocked by the given blocker, paginated.
	// Results are ordered by blocked_at DESC.
	ListBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error)

	// IsBlocked checks if blockerDID has blocked blockedDID (fast EXISTS check).
	IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error)

	// AreBlocked checks which of the given DIDs are blocked by blockerDID.
	// Returns a map of blockedDID → true for each DID that is blocked.
	// Used for batch hydration of viewer state on lists of posts/comments.
	AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error)
}

// Service defines the interface for user block business logic.
// Coordinates between Repository and external services (PDS, identity, etc.)
// Uses write-forward pattern: Service → PDS → Firehose → Consumer → Repository
type Service interface {
	// BlockUser creates a block against another user.
	// Uses OAuth session for DPoP-authenticated PDS write-forward.
	// The identifier can be a DID or handle.
	// Returns a BlockResult with the record URI and CID from PDS.
	// The block is indexed asynchronously via the firehose consumer.
	// Returns ErrCannotBlockSelf if the user attempts to block themselves.
	// Returns ErrBlockAlreadyExists if the block already exists on PDS.
	BlockUser(ctx context.Context, session *oauth.ClientSessionData, identifier string) (*BlockResult, error)

	// UnblockUser removes a block against another user.
	// Uses OAuth session for DPoP-authenticated PDS delete.
	// The identifier can be a DID or handle.
	UnblockUser(ctx context.Context, session *oauth.ClientSessionData, identifier string) error

	// GetBlockedUsers retrieves all users blocked by the given user, paginated.
	GetBlockedUsers(ctx context.Context, blockerDID string, limit, offset int) ([]*UserBlock, error)

	// IsBlocked checks if blockerDID has blocked blockedDID.
	IsBlocked(ctx context.Context, blockerDID, blockedDID string) (bool, error)
}
