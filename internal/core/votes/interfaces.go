package votes

import "context"

// Service defines the business logic interface for votes
// Coordinates between Repository, user PDS, and vote validation
type Service interface {
	// CreateVote creates a new vote or toggles an existing vote
	// Flow: Validate -> Check existing vote -> Handle toggle logic -> Write to user's PDS -> Return URI/CID
	// AppView indexing happens asynchronously via Jetstream consumer
	// Toggle logic:
	//   - No vote -> Create vote
	//   - Same direction -> Delete vote (toggle off)
	//   - Different direction -> Delete old + Create new (toggle direction)
	CreateVote(ctx context.Context, voterDID string, userAccessToken string, req CreateVoteRequest) (*CreateVoteResponse, error)

	// DeleteVote removes a vote from a post/comment
	// Flow: Find vote -> Verify ownership -> Delete from user's PDS
	// AppView decrements vote count asynchronously via Jetstream consumer
	DeleteVote(ctx context.Context, voterDID string, userAccessToken string, req DeleteVoteRequest) error

	// GetVote retrieves a user's vote on a specific subject
	// Used to check vote state before creating/toggling
	GetVote(ctx context.Context, voterDID string, subjectURI string) (*Vote, error)
}

// Repository defines the data access interface for votes
// Used by Jetstream consumer to index votes from firehose
type Repository interface {
	// Create inserts a new vote into the AppView database
	// Called by Jetstream consumer after vote is created on PDS
	// Idempotent: ON CONFLICT DO NOTHING for duplicate URIs
	Create(ctx context.Context, vote *Vote) error

	// GetByURI retrieves a vote by its AT-URI
	// Used for Jetstream DELETE operations
	GetByURI(ctx context.Context, uri string) (*Vote, error)

	// GetByVoterAndSubject retrieves a user's vote on a specific subject
	// Used to check existing vote state
	GetByVoterAndSubject(ctx context.Context, voterDID string, subjectURI string) (*Vote, error)

	// Delete soft-deletes a vote (sets deleted_at)
	// Called by Jetstream consumer after vote is deleted from PDS
	Delete(ctx context.Context, uri string) error

	// ListBySubject retrieves all votes on a specific post/comment
	// Future: Used for vote detail views
	ListBySubject(ctx context.Context, subjectURI string, limit, offset int) ([]*Vote, error)

	// ListByVoter retrieves all votes by a specific user
	// Future: Used for user voting history
	ListByVoter(ctx context.Context, voterDID string, limit, offset int) ([]*Vote, error)
}
