package posts

import (
	"errors"
	"fmt"
)

// Sentinel errors for common post operations
var (
	// ErrCommunityNotFound is returned when the community doesn't exist in AppView
	ErrCommunityNotFound = errors.New("community not found")

	// ErrNotAuthorized is returned when user isn't authorized to post in community
	// (e.g., banned, private community without membership - Beta)
	ErrNotAuthorized = errors.New("user not authorized to post in this community")

	// ErrBanned is returned when user is banned from community (Beta)
	ErrBanned = errors.New("user is banned from this community")

	// ErrInvalidContent is returned for general content violations
	ErrInvalidContent = errors.New("invalid post content")

	// ErrNotFound is returned when a post is not found by URI
	ErrNotFound = errors.New("post not found")
)

// ValidationError represents a validation error with field context
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error (%s): %s", e.Field, e.Message)
}

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return &ValidationError{
		Field:   field,
		Message: message,
	}
}

// IsValidationError checks if error is a validation error
func IsValidationError(err error) bool {
	var valErr *ValidationError
	return errors.As(err, &valErr)
}

// ContentRuleViolation represents a violation of community content rules
// (Deferred to Beta - included here for future compatibility)
type ContentRuleViolation struct {
	Rule    string // e.g., "requireText", "allowedEmbedTypes"
	Message string // Human-readable explanation
}

func (e *ContentRuleViolation) Error() string {
	return fmt.Sprintf("content rule violation (%s): %s", e.Rule, e.Message)
}

// NewContentRuleViolation creates a new content rule violation error
func NewContentRuleViolation(rule, message string) error {
	return &ContentRuleViolation{
		Rule:    rule,
		Message: message,
	}
}

// IsContentRuleViolation checks if error is a content rule violation
func IsContentRuleViolation(err error) bool {
	var violation *ContentRuleViolation
	return errors.As(err, &violation)
}

// NotFoundError represents a resource not found error
type NotFoundError struct {
	Resource string // e.g., "post", "community"
	ID       string // Resource identifier
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s not found: %s", e.Resource, e.ID)
}

// NewNotFoundError creates a new not found error
func NewNotFoundError(resource, id string) error {
	return &NotFoundError{
		Resource: resource,
		ID:       id,
	}
}

// IsNotFound checks if error is a not found error
func IsNotFound(err error) bool {
	var notFoundErr *NotFoundError
	return errors.As(err, &notFoundErr) || err == ErrCommunityNotFound || err == ErrNotFound
}

// IsConflict checks if error is due to duplicate/conflict
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	// Check for common conflict indicators in error message
	errStr := err.Error()
	return contains(errStr, "already indexed") ||
		contains(errStr, "duplicate key") ||
		contains(errStr, "already exists")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && anySubstring(s, substr)
}

func anySubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
