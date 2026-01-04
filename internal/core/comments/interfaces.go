package comments

import (
	"context"
	"database/sql"
)

// Repository defines the data access interface for comments
// Used by Jetstream consumer to index comments from firehose
//
// Architecture: Comments are written directly by clients to their PDS using
// com.atproto.repo.createRecord/updateRecord/deleteRecord. This AppView indexes
// comments from Jetstream for aggregation and querying.
type Repository interface {
	// Create inserts a new comment into the AppView database
	// Called by Jetstream consumer after comment is created on PDS
	// Idempotent: ON CONFLICT DO NOTHING for duplicate URIs
	Create(ctx context.Context, comment *Comment) error

	// Update modifies an existing comment's content fields
	// Called by Jetstream consumer after comment is updated on PDS
	// Preserves vote counts and created_at timestamp
	Update(ctx context.Context, comment *Comment) error

	// GetByURI retrieves a comment by its AT-URI
	// Used for Jetstream UPDATE/DELETE operations and queries
	GetByURI(ctx context.Context, uri string) (*Comment, error)

	// Delete soft-deletes a comment (sets deleted_at)
	// Called by Jetstream consumer after comment is deleted from PDS
	// Deprecated: Use SoftDeleteWithReason for new code to preserve thread structure
	Delete(ctx context.Context, uri string) error

	// SoftDeleteWithReason performs a soft delete that blanks content but preserves thread structure
	// This allows deleted comments to appear as "[deleted]" placeholders in thread views
	// reason: "author" (user deleted) or "moderator" (mod removed)
	// deletedByDID: DID of the actor who performed the deletion
	SoftDeleteWithReason(ctx context.Context, uri, reason, deletedByDID string) error

	// ListByRoot retrieves all comments in a thread (flat)
	// Used for fetching entire comment threads on posts
	ListByRoot(ctx context.Context, rootURI string, limit, offset int) ([]*Comment, error)

	// ListByParent retrieves direct replies to a post or comment
	// Used for building nested/threaded comment views
	ListByParent(ctx context.Context, parentURI string, limit, offset int) ([]*Comment, error)

	// CountByParent counts direct replies to a post or comment
	// Used for showing reply counts in threading UI
	CountByParent(ctx context.Context, parentURI string) (int, error)

	// ListByCommenter retrieves all comments by a specific user
	// Deprecated: Use ListByCommenterWithCursor for cursor-based pagination
	ListByCommenter(ctx context.Context, commenterDID string, limit, offset int) ([]*Comment, error)

	// ListByCommenterWithCursor retrieves comments by a user with cursor-based pagination
	// Used for user profile comment history (social.coves.actor.getComments)
	// Supports optional community filtering and returns next page cursor
	ListByCommenterWithCursor(ctx context.Context, req ListByCommenterRequest) ([]*Comment, *string, error)

	// ListByParentWithHotRank retrieves direct replies to a post or comment with sorting and pagination
	// Supports hot, top, and new sorting with cursor-based pagination
	// Returns comments with author info hydrated and next page cursor
	ListByParentWithHotRank(
		ctx context.Context,
		parentURI string,
		sort string, // "hot", "top", "new"
		timeframe string, // "hour", "day", "week", "month", "year", "all" (for "top" only)
		limit int,
		cursor *string,
	) ([]*Comment, *string, error)

	// GetByURIsBatch retrieves multiple comments by their AT-URIs in a single query
	// Returns map[uri]*Comment for efficient lookups
	// Used for hydrating comment threads without N+1 queries
	GetByURIsBatch(ctx context.Context, uris []string) (map[string]*Comment, error)

	// GetVoteStateForComments retrieves the viewer's votes on a batch of comments
	// Returns map[commentURI]*Vote for efficient lookups
	// Future: Used when votes table is implemented
	GetVoteStateForComments(ctx context.Context, viewerDID string, commentURIs []string) (map[string]interface{}, error)

	// ListByParentsBatch retrieves direct replies to multiple parents in a single query
	// Returns map[parentURI][]*Comment grouped by parent
	// Used to prevent N+1 queries when loading nested replies
	// Limits results per parent to avoid memory exhaustion
	ListByParentsBatch(
		ctx context.Context,
		parentURIs []string,
		sort string,
		limitPerParent int,
	) (map[string][]*Comment, error)
}

// RepositoryTx provides transaction-aware operations for consumers that need atomicity
// Used by Jetstream consumer to perform atomic delete + count updates
// Implementations that support transactions should also implement this interface
type RepositoryTx interface {
	// SoftDeleteWithReasonTx performs a soft delete within a transaction
	// If tx is nil, executes directly against the database
	// Returns rows affected count for callers that need to check idempotency
	// reason: must be DeletionReasonAuthor or DeletionReasonModerator
	// deletedByDID: DID of the actor who performed the deletion
	SoftDeleteWithReasonTx(ctx context.Context, tx *sql.Tx, uri, reason, deletedByDID string) (int64, error)
}
