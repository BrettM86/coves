package aggregators

import (
	"errors"
	"fmt"
)

// Domain errors
var (
	ErrAggregatorNotFound     = errors.New("aggregator not found")
	ErrAuthorizationNotFound  = errors.New("authorization not found")
	ErrNotAuthorized          = errors.New("aggregator not authorized for this community")
	ErrAlreadyAuthorized      = errors.New("aggregator already authorized for this community")
	ErrRateLimitExceeded      = errors.New("aggregator rate limit exceeded")
	ErrInvalidConfig          = errors.New("invalid aggregator configuration")
	ErrConfigSchemaValidation = errors.New("configuration does not match aggregator's schema")
	ErrNotModerator           = errors.New("user is not a moderator of this community")
	ErrNotImplemented         = errors.New("feature not yet implemented") // For Phase 2 write-forward operations

	// API Key authentication errors
	ErrAPIKeyRevoked         = errors.New("API key has been revoked")
	ErrAPIKeyInvalid         = errors.New("invalid API key")
	ErrAPIKeyNotFound        = errors.New("API key not found for this aggregator")
	ErrOAuthTokenExpired     = errors.New("OAuth token has expired and needs refresh")
	ErrOAuthRefreshFailed    = errors.New("failed to refresh OAuth token")
	ErrOAuthSessionMismatch  = errors.New("OAuth session DID does not match aggregator DID")
)

// ValidationError represents a validation error with field details
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s - %s", e.Field, e.Message)
}

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return &ValidationError{
		Field:   field,
		Message: message,
	}
}

// Error classification helpers for handlers to map to HTTP status codes
func IsNotFound(err error) bool {
	return errors.Is(err, ErrAggregatorNotFound) ||
		errors.Is(err, ErrAuthorizationNotFound) ||
		errors.Is(err, ErrAPIKeyNotFound)
}

func IsValidationError(err error) bool {
	var validationErr *ValidationError
	return errors.As(err, &validationErr) || errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrConfigSchemaValidation)
}

func IsUnauthorized(err error) bool {
	return errors.Is(err, ErrNotAuthorized) || errors.Is(err, ErrNotModerator)
}

func IsConflict(err error) bool {
	return errors.Is(err, ErrAlreadyAuthorized)
}

func IsRateLimited(err error) bool {
	return errors.Is(err, ErrRateLimitExceeded)
}

func IsNotImplemented(err error) bool {
	return errors.Is(err, ErrNotImplemented)
}

func IsAPIKeyError(err error) bool {
	return errors.Is(err, ErrAPIKeyRevoked) ||
		errors.Is(err, ErrAPIKeyInvalid) ||
		errors.Is(err, ErrAPIKeyNotFound)
}

func IsOAuthError(err error) bool {
	return errors.Is(err, ErrOAuthTokenExpired) ||
		errors.Is(err, ErrOAuthRefreshFailed)
}
