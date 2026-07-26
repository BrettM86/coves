package communityFeeds

import (
	"errors"

	coreerrors "Coves/internal/core/errors"
)

var (
	// ErrCommunityNotFound is returned when the community doesn't exist
	ErrCommunityNotFound = errors.New("community not found")

	// ErrInvalidCursor is returned when the pagination cursor is invalid
	ErrInvalidCursor = errors.New("invalid pagination cursor")
)

// ValidationError is the shared validation error type. It is aliased rather
// than redefined so that one errors.As at the API boundary matches validation
// failures from every domain package, instead of each handler needing to know
// which domains it might hear from.
type ValidationError = coreerrors.ValidationError

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return coreerrors.NewValidationError(field, message)
}

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	return coreerrors.IsValidationError(err)
}
