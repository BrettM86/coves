package communities

import (
	"errors"
	"fmt"
)

// Domain errors for communities
var (
	// ErrCommunityNotFound is returned when a community doesn't exist
	ErrCommunityNotFound = errors.New("community not found")

	// ErrCommunityAlreadyExists is returned when trying to create a community with duplicate DID
	ErrCommunityAlreadyExists = errors.New("community already exists")

	// ErrHandleTaken is returned when a community handle is already in use
	ErrHandleTaken = errors.New("community handle is already taken")

	// ErrInvalidHandle is returned when a handle doesn't match the required format
	ErrInvalidHandle = errors.New("invalid community handle format")

	// ErrInvalidVisibility is returned when visibility value is not valid
	ErrInvalidVisibility = errors.New("invalid visibility value")

	// ErrUnauthorized is returned when a user lacks permission for an action
	ErrUnauthorized = errors.New("unauthorized")

	// ErrSubscriptionAlreadyExists is returned when user is already subscribed
	ErrSubscriptionAlreadyExists = errors.New("already subscribed to this community")

	// ErrSubscriptionNotFound is returned when subscription doesn't exist
	ErrSubscriptionNotFound = errors.New("subscription not found")

	// ErrBlockNotFound is returned when block doesn't exist
	ErrBlockNotFound = errors.New("block not found")

	// ErrMembershipNotFound is returned when membership doesn't exist
	ErrMembershipNotFound = errors.New("membership not found")

	// ErrMemberBanned is returned when trying to perform action as banned member
	ErrMemberBanned = errors.New("user is banned from this community")

	// ErrInvalidInput is returned for general validation failures
	ErrInvalidInput = errors.New("invalid input")
)

// ValidationError wraps input validation errors with field details
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// NewValidationError creates a new validation error
func NewValidationError(field, message string) *ValidationError {
	return &ValidationError{
		Field:   field,
		Message: message,
	}
}

// IsNotFound checks if error is a "not found" error
func IsNotFound(err error) bool {
	return errors.Is(err, ErrCommunityNotFound) ||
		errors.Is(err, ErrSubscriptionNotFound) ||
		errors.Is(err, ErrBlockNotFound) ||
		errors.Is(err, ErrMembershipNotFound)
}

// IsConflict checks if error is a conflict error (duplicate)
func IsConflict(err error) bool {
	return errors.Is(err, ErrCommunityAlreadyExists) ||
		errors.Is(err, ErrHandleTaken) ||
		errors.Is(err, ErrSubscriptionAlreadyExists)
}

// IsValidationError checks if error is a validation error
func IsValidationError(err error) bool {
	var valErr *ValidationError
	return errors.As(err, &valErr) || errors.Is(err, ErrInvalidInput)
}
