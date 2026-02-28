package communitysuggestions

import "errors"

var (
	// ErrSuggestionNotFound indicates the requested suggestion does not exist
	ErrSuggestionNotFound = errors.New("suggestion not found")

	// ErrTitleRequired indicates the suggestion title was not provided
	ErrTitleRequired = errors.New("suggestion title is required")

	// ErrTitleTooLong indicates the suggestion title exceeds the maximum length
	ErrTitleTooLong = errors.New("suggestion title exceeds maximum length of 200 characters")

	// ErrDescriptionRequired indicates the suggestion description was not provided
	ErrDescriptionRequired = errors.New("suggestion description is required")

	// ErrDescriptionTooLong indicates the suggestion description exceeds the maximum length
	ErrDescriptionTooLong = errors.New("suggestion description exceeds maximum length of 5000 characters")

	// ErrInvalidStatus indicates the suggestion status is not a valid value
	ErrInvalidStatus = errors.New("invalid suggestion status: must be one of open, under_review, approved, declined")

	// ErrInvalidVoteValue indicates the vote value is not valid (must be 1 or -1)
	ErrInvalidVoteValue = errors.New("invalid vote value: must be 1 or -1")

	// ErrInvalidSuggestionID indicates the suggestion ID is invalid
	ErrInvalidSuggestionID = errors.New("invalid suggestion ID: must be a positive integer")

	// ErrVoterRequired indicates the voter DID was not provided
	ErrVoterRequired = errors.New("voter DID is required")

	// ErrSubmitterRequired indicates the submitter DID was not provided
	ErrSubmitterRequired = errors.New("submitter DID is required")

	// ErrNotAuthorized indicates the user is not authorized to perform the action
	ErrNotAuthorized = errors.New("not authorized to perform this action")

	// ErrRateLimitExceeded indicates the user has exceeded the suggestion creation rate limit
	ErrRateLimitExceeded = errors.New("rate limit exceeded: maximum 3 suggestions per day")

	// ErrVoteNotFound indicates the requested vote does not exist
	ErrVoteNotFound = errors.New("vote not found")
)

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	return errors.Is(err, ErrTitleRequired) ||
		errors.Is(err, ErrTitleTooLong) ||
		errors.Is(err, ErrDescriptionRequired) ||
		errors.Is(err, ErrDescriptionTooLong) ||
		errors.Is(err, ErrInvalidStatus) ||
		errors.Is(err, ErrInvalidVoteValue) ||
		errors.Is(err, ErrInvalidSuggestionID) ||
		errors.Is(err, ErrVoterRequired) ||
		errors.Is(err, ErrSubmitterRequired)
}

// IsNotFound checks if an error is a "not found" error
func IsNotFound(err error) bool {
	return errors.Is(err, ErrSuggestionNotFound) ||
		errors.Is(err, ErrVoteNotFound)
}

// IsRateLimitError checks if an error is a rate limit error
func IsRateLimitError(err error) bool {
	return errors.Is(err, ErrRateLimitExceeded)
}

// IsAuthorizationError checks if an error is an authorization error
func IsAuthorizationError(err error) bool {
	return errors.Is(err, ErrNotAuthorized)
}
