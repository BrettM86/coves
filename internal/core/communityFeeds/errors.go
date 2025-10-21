package communityFeeds

import (
	"errors"
	"fmt"
)

var (
	// ErrCommunityNotFound is returned when the community doesn't exist
	ErrCommunityNotFound = errors.New("community not found")

	// ErrInvalidCursor is returned when the pagination cursor is invalid
	ErrInvalidCursor = errors.New("invalid pagination cursor")
)

// ValidationError represents an input validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s: %s", e.Field, e.Message)
}

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return &ValidationError{
		Field:   field,
		Message: message,
	}
}

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}
