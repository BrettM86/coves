package votes

import (
	"context"
	"strings"
)

// SubjectExistsFunc is a function type that checks if a subject exists
type SubjectExistsFunc func(ctx context.Context, uri string) (bool, error)

// CompositeSubjectValidator validates subjects by checking both posts and comments
type CompositeSubjectValidator struct {
	postExists    SubjectExistsFunc
	commentExists SubjectExistsFunc
}

// NewCompositeSubjectValidator creates a validator that checks both posts and comments
// Pass nil for either function to skip validation for that type
func NewCompositeSubjectValidator(postExists, commentExists SubjectExistsFunc) *CompositeSubjectValidator {
	return &CompositeSubjectValidator{
		postExists:    postExists,
		commentExists: commentExists,
	}
}

// SubjectExists checks if a post or comment exists at the given URI
// Determines type from the collection in the URI (e.g., social.coves.feed.post vs social.coves.feed.comment)
func (v *CompositeSubjectValidator) SubjectExists(ctx context.Context, uri string) (bool, error) {
	// Parse collection from AT-URI: at://did/collection/rkey
	// Example: at://did:plc:xxx/social.coves.feed.post/abc123
	if strings.Contains(uri, "/social.coves.feed.post/") {
		if v.postExists != nil {
			return v.postExists(ctx, uri)
		}
		// If no post checker, assume exists (for testing)
		return true, nil
	}

	if strings.Contains(uri, "/social.coves.feed.comment/") {
		if v.commentExists != nil {
			return v.commentExists(ctx, uri)
		}
		// If no comment checker, assume exists (for testing)
		return true, nil
	}

	// Unknown collection type - could be from another app
	// For now, allow voting on unknown types (future-proofing)
	return true, nil
}
