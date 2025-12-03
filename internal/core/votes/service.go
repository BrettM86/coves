package votes

import (
	"context"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// Service defines the business logic interface for vote operations
// Implements write-forward pattern: validates requests, then forwards to user's PDS
//
// Architecture:
//   - Service validates input and checks authorization
//   - Queries user's PDS directly via com.atproto.repo.listRecords to check existing votes
//     (avoids eventual consistency issues with AppView database)
//   - Creates/deletes vote records via com.atproto.repo.createRecord/deleteRecord
//   - AppView indexes resulting records from Jetstream firehose for aggregate counts
type Service interface {
	// CreateVote creates a new vote or toggles off an existing vote
	// Returns URI and CID of created vote, or empty strings if toggled off
	//
	// Validation:
	// - Direction must be "up" or "down" (returns ErrInvalidDirection)
	// - Subject URI must be valid AT-URI (returns ErrInvalidSubject)
	// - Subject CID must be provided (returns ErrInvalidSubject)
	//
	// Note: Subject existence is NOT validated. Votes on non-existent or deleted
	// subjects are allowed - the Jetstream consumer handles orphaned votes correctly
	// by only updating counts for non-deleted subjects.
	//
	// Behavior:
	// - If no vote exists: creates new vote with given direction
	// - If vote exists with same direction: deletes vote (toggle off)
	// - If vote exists with different direction: updates to new direction
	CreateVote(ctx context.Context, session *oauthlib.ClientSessionData, req CreateVoteRequest) (*CreateVoteResponse, error)

	// DeleteVote removes a vote on the specified subject
	//
	// Validation:
	// - Subject URI must be valid AT-URI (returns ErrInvalidSubject)
	// - Vote must exist (returns ErrVoteNotFound)
	//
	// Behavior:
	// - Deletes the user's vote record from their PDS
	// - AppView will soft-delete via Jetstream consumer
	DeleteVote(ctx context.Context, session *oauthlib.ClientSessionData, req DeleteVoteRequest) error
}

// CreateVoteRequest contains the parameters for creating a vote
type CreateVoteRequest struct {
	// Subject is the post or comment being voted on
	Subject StrongRef `json:"subject"`

	// Direction is either "up" or "down"
	Direction string `json:"direction"`
}

// CreateVoteResponse contains the result of creating a vote
type CreateVoteResponse struct {
	// URI is the AT-URI of the created vote record
	// Empty string if vote was toggled off (deleted)
	URI string `json:"uri"`

	// CID is the content identifier of the created vote record
	// Empty string if vote was toggled off (deleted)
	CID string `json:"cid"`
}

// DeleteVoteRequest contains the parameters for deleting a vote
type DeleteVoteRequest struct {
	// Subject is the post or comment whose vote should be removed
	Subject StrongRef `json:"subject"`
}
