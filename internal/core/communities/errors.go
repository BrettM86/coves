package communities

import (
	"errors"

	coreerrors "Coves/internal/core/errors"
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

	// ErrBlockAlreadyExists is returned when user has already blocked the community
	ErrBlockAlreadyExists = errors.New("community already blocked")

	// ErrMembershipNotFound is returned when membership doesn't exist
	ErrMembershipNotFound = errors.New("membership not found")

	// ErrMemberBanned is returned when trying to perform action as banned member
	ErrMemberBanned = errors.New("user is banned from this community")

	// ErrInvalidInput is returned for general validation failures
	ErrInvalidInput = errors.New("invalid input")

	// ErrAmbiguousCommunity is returned when a name@origin identifier matches
	// more than one indexed community. (name, origin) is not unique on the
	// communities table — a bridge that suffixes colliding handle labels can
	// index two rows that both self-assert the same origin and name — and
	// picking one silently would route a client to the wrong community.
	ErrAmbiguousCommunity = errors.New("community identifier is ambiguous: more than one community matches this name and origin")
)

// ValidationError is the shared validation error type. It is aliased rather
// than redefined so that one errors.As at the API boundary matches validation
// failures from every domain package, instead of each handler needing to know
// which domains it might hear from.
type ValidationError = coreerrors.ValidationError

// NewValidationError creates a new validation error
func NewValidationError(field, message string) *ValidationError {
	return coreerrors.NewValidationError(field, message)
}

// IsNotFound checks if error is a "not found" error
func IsNotFound(err error) bool {
	return errors.Is(err, ErrCommunityNotFound) ||
		errors.Is(err, ErrSubscriptionNotFound) ||
		errors.Is(err, ErrBlockNotFound) ||
		errors.Is(err, ErrMembershipNotFound)
}

// IsAmbiguous checks if error is an ambiguous-identifier error
func IsAmbiguous(err error) bool {
	return errors.Is(err, ErrAmbiguousCommunity)
}

// IsConflict checks if error is a conflict error (duplicate)
func IsConflict(err error) bool {
	return errors.Is(err, ErrCommunityAlreadyExists) ||
		errors.Is(err, ErrHandleTaken) ||
		errors.Is(err, ErrSubscriptionAlreadyExists) ||
		errors.Is(err, ErrBlockAlreadyExists)
}

// IsValidationError checks if error is a validation error
func IsValidationError(err error) bool {
	return coreerrors.IsValidationError(err) || errors.Is(err, ErrInvalidInput)
}
