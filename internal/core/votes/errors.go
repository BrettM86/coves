package votes

import "errors"

var (
	// ErrVoteNotFound indicates the requested vote doesn't exist
	ErrVoteNotFound = errors.New("vote not found")

	// ErrSubjectNotFound indicates the post/comment being voted on doesn't exist
	ErrSubjectNotFound = errors.New("subject not found")

	// ErrInvalidDirection indicates the vote direction is not "up" or "down"
	ErrInvalidDirection = errors.New("invalid vote direction: must be 'up' or 'down'")

	// ErrInvalidSubject indicates the subject URI is malformed or invalid
	ErrInvalidSubject = errors.New("invalid subject URI")

	// ErrVoteAlreadyExists indicates a vote already exists on this subject
	ErrVoteAlreadyExists = errors.New("vote already exists")

	// ErrNotAuthorized indicates the user is not authorized to perform this action
	ErrNotAuthorized = errors.New("not authorized")

	// ErrBanned indicates the user is banned from the community
	ErrBanned = errors.New("user is banned from this community")
)
