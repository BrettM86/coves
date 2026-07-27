package votes

import "errors"

var (
	// ErrVoteNotFound indicates the requested vote doesn't exist
	ErrVoteNotFound = errors.New("vote not found")

	// ErrInvalidDirection indicates the vote direction is not "up" or "down"
	ErrInvalidDirection = errors.New("invalid vote direction: must be 'up' or 'down'")

	// ErrInvalidSubject indicates the subject URI is malformed or invalid
	ErrInvalidSubject = errors.New("invalid subject URI")

	// ErrVoteAlreadyExists indicates a vote already exists on this subject
	ErrVoteAlreadyExists = errors.New("vote already exists")

	// ErrNotAuthorized indicates the user is not authorized to perform this action.
	//
	// When the cause is a PDS auth failure, return it wrapped rather than bare:
	//
	//	fmt.Errorf("%w: %w", ErrNotAuthorized, err)
	//
	// Returning the bare sentinel discards which auth failure it was, and the
	// two need opposite responses: a PDS 401 means the session is dead and the
	// client must sign in again, while a 403 means the session simply lacks the
	// scope. Collapsed into one sentinel they both answer 403, so a client whose
	// session expired retries forever with no way out but a manual sign-out.
	ErrNotAuthorized = errors.New("not authorized")

	// ErrBanned indicates the user is banned from the community
	ErrBanned = errors.New("user is banned from this community")
)
