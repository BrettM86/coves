package votes

import "context"

// Repository defines the data access interface for votes
// Used by Jetstream consumer to index votes from firehose
//
// Architecture: Votes are written directly by clients to their PDS using
// com.atproto.repo.createRecord/deleteRecord. This AppView indexes votes
// from Jetstream for aggregation and querying.
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
	GetByVoterAndSubject(ctx context.Context, voterDID, subjectURI string) (*Vote, error)

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
