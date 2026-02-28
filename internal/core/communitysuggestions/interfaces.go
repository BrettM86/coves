package communitysuggestions

import (
	"context"
	"time"
)

// Repository defines the data access layer for community suggestions
type Repository interface {
	// Create stores a new suggestion in the database
	// Returns the suggestion with ID, CreatedAt, and UpdatedAt populated
	Create(ctx context.Context, suggestion *CommunitySuggestion) error

	// GetByID retrieves a single suggestion by its ID
	// Returns ErrSuggestionNotFound if the suggestion does not exist
	GetByID(ctx context.Context, id int64) (*CommunitySuggestion, error)

	// List retrieves suggestions with optional filtering and sorting
	List(ctx context.Context, req ListSuggestionsRequest) ([]*CommunitySuggestion, error)

	// CountBySubmitterSince counts the number of suggestions created by a submitter since a given time
	// Used for rate limiting suggestion creation
	CountBySubmitterSince(ctx context.Context, submitterDID string, since time.Time) (int, error)

	// UpdateStatus updates a suggestion's status
	// Returns ErrSuggestionNotFound if the suggestion does not exist
	UpdateStatus(ctx context.Context, id int64, status Status) error

	// UpsertVote inserts or updates a vote for a suggestion
	// Returns the delta to apply to the suggestion's vote count
	UpsertVote(ctx context.Context, suggestionID int64, voterDID string, value int) (int, error)

	// DeleteVote removes a vote from a suggestion
	// Returns the delta to apply to the suggestion's vote count
	DeleteVote(ctx context.Context, suggestionID int64, voterDID string) (int, error)

	// GetVote retrieves a single vote by suggestion ID and voter DID
	// Returns ErrVoteNotFound if the vote does not exist
	GetVote(ctx context.Context, suggestionID int64, voterDID string) (*SuggestionVote, error)

	// GetVotesForViewer retrieves the votes cast by a viewer on a set of suggestions
	// Returns a map of suggestion ID to vote value
	GetVotesForViewer(ctx context.Context, voterDID string, suggestionIDs []int64) (map[int64]int, error)

	// AtomicVote atomically handles voting with toggle semantics in a single transaction
	// If no existing vote: creates the vote
	// If existing vote in same direction: removes the vote (toggle off)
	// If existing vote in opposite direction: flips the vote
	AtomicVote(ctx context.Context, suggestionID int64, voterDID string, value int) error
}

// Service defines the business logic layer for community suggestions
type Service interface {
	// CreateSuggestion validates and creates a new community suggestion
	// Enforces rate limiting (max 3 suggestions per day per user)
	CreateSuggestion(ctx context.Context, req CreateSuggestionRequest) (*CommunitySuggestion, error)

	// GetSuggestion retrieves a single suggestion by ID
	// If viewerDID is non-empty, populates the viewer state with the viewer's vote
	GetSuggestion(ctx context.Context, id int64, viewerDID string) (*CommunitySuggestion, error)

	// ListSuggestions retrieves suggestions with filtering, sorting, and pagination
	// Populates viewer state for authenticated viewers
	ListSuggestions(ctx context.Context, req ListSuggestionsRequest) ([]*CommunitySuggestion, error)

	// Vote casts or toggles a vote on a suggestion
	// If the user already voted in the same direction, the vote is removed (toggle off)
	// If the user already voted in the opposite direction, the vote is flipped
	Vote(ctx context.Context, req VoteRequest) error

	// RemoveVote removes a user's vote from a suggestion
	RemoveVote(ctx context.Context, suggestionID int64, voterDID string) error

	// UpdateStatus updates a suggestion's status (admin only)
	UpdateStatus(ctx context.Context, req UpdateStatusRequest) error
}
