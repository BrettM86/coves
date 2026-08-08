package posts

import (
	"errors"
	"fmt"
	"strings"

	coreerrors "Coves/internal/core/errors"
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

	// ErrRateLimitExceeded is returned when an aggregator exceeds rate limits
	ErrRateLimitExceeded = errors.New("rate limit exceeded")

	// ErrInvalidCursor is returned when a pagination cursor is malformed
	ErrInvalidCursor = errors.New("invalid pagination cursor")

	// ErrActorNotFound is returned when the requested actor does not exist
	ErrActorNotFound = errors.New("actor not found")

	// ErrDuplicateSubmission is returned when an author resubmits content
	// identical to something already on the submission ledger for the current
	// dedupe window (PRD_AUTHOR_OWNED_POSTS.md §8).
	//
	// THE WORDING IS LOAD-BEARING. IsConflict below classifies an error by
	// looking for "duplicate key", "already exists" or "already indexed" in its
	// text, because a genuine index conflict arrives from the driver as a
	// string rather than as a typed error. This sentinel must therefore avoid
	// all three phrasings: a duplicate SUBMISSION is a client being refused at
	// the admission gate, while a conflict is the indexer meeting a record it
	// already has, and collapsing them would let a refused post be reported as
	// successfully indexed.
	ErrDuplicateSubmission = errors.New("an identical submission from this author to this community was refused as a repeat")
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

// NewValidationErrorFrom creates a validation error that keeps cause matchable
// via errors.Is while presenting a field-scoped message to the client.
func NewValidationErrorFrom(field string, cause error) error {
	return coreerrors.NewValidationErrorFrom(field, cause)
}

// IsValidationError checks if error is a validation error
func IsValidationError(err error) bool {
	return coreerrors.IsValidationError(err)
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

// NotFoundError is the shared not-found error type, aliased for the same
// reason as ValidationError above.
type NotFoundError = coreerrors.NotFoundError

// NewNotFoundError creates a new not found error
func NewNotFoundError(resource, id string) error {
	return coreerrors.NewNotFoundError(resource, id)
}

// IsNotFound checks if error is a not found error.
//
// errors.Is rather than == : these sentinels travel up through service layers
// that wrap them with %w for context, and an == comparison stops matching the
// moment anyone adds that context — silently turning a 404 into a 500.
func IsNotFound(err error) bool {
	return coreerrors.IsNotFound(err) ||
		errors.Is(err, ErrCommunityNotFound) ||
		errors.Is(err, ErrNotFound)
}

// IsConflict checks if error is due to duplicate/conflict.
//
// This inspects the message because the conflict usually originates in the
// PostgreSQL driver as a unique-violation string rather than as a typed error
// the repository translates. Typed conflicts (coreerrors.ConflictError) are
// checked first so callers that do return one are matched exactly.
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	if coreerrors.IsConflict(err) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "already indexed") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "already exists")
}
