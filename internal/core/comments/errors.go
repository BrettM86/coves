package comments

import (
	"errors"

	"Coves/internal/core/richtext"
)

var (
	// ErrCommentNotFound indicates the requested comment doesn't exist
	ErrCommentNotFound = errors.New("comment not found")

	// ErrInvalidReply indicates the reply reference is malformed or invalid
	ErrInvalidReply = errors.New("invalid reply reference")

	// ErrParentNotFound indicates the parent post/comment doesn't exist
	ErrParentNotFound = errors.New("parent post or comment not found")

	// ErrRootNotFound indicates the root post doesn't exist
	ErrRootNotFound = errors.New("root post not found")

	// ErrContentTooLong indicates comment content exceeds 10000 graphemes
	ErrContentTooLong = errors.New("comment content exceeds 10000 graphemes")

	// ErrContentEmpty indicates comment content is empty
	ErrContentEmpty = errors.New("comment content is required")

	// ErrInvalidFacets indicates rich text facets are malformed or reference
	// byte ranges outside the comment content
	ErrInvalidFacets = errors.New("invalid rich text facets")

	// ErrNotAuthorized indicates the user is not authorized to perform this action
	ErrNotAuthorized = errors.New("not authorized")

	// ErrBanned indicates the user is banned from the community
	ErrBanned = errors.New("user is banned from this community")

	// ErrCommentAlreadyExists indicates a comment with this URI already exists
	ErrCommentAlreadyExists = errors.New("comment already exists")

	// ErrConcurrentModification indicates the comment was modified since it was loaded
	ErrConcurrentModification = errors.New("comment was modified by another operation")

	// ErrInvalidCursor indicates a malformed or sort-mismatched pagination cursor
	ErrInvalidCursor = errors.New("invalid pagination cursor")

	// ErrInvalidEmbed indicates the comment embed fails the lexicon or carries an
	// image that is not a well-formed image blob ref
	ErrInvalidEmbed = errors.New("invalid embed")

	// ErrTooManyImages marks a comment refused because its images embed carries more than eight images (XRPC TooManyImages, 400)
	ErrTooManyImages = errors.New("too many images")

	// ErrInvalidRecord indicates the comment record about to be written fails
	// the social.coves.community.comment lexicon somewhere other than its embed
	ErrInvalidRecord = errors.New("comment record does not satisfy the lexicon")

	// ErrInvalidImageFacet marks a comment refused because an #image facet
	// breaks a write rule (XRPC InvalidImageFacet, 400). It is richtext's
	// sentinel, so the rule check's own error matches it without re-wrapping.
	// It does not match ErrInvalidFacets or ErrInvalidEmbed.
	ErrInvalidImageFacet = richtext.ErrInvalidImageFacet
)

// IsNotFound checks if an error is a "not found" error
func IsNotFound(err error) bool {
	return errors.Is(err, ErrCommentNotFound) ||
		errors.Is(err, ErrParentNotFound) ||
		errors.Is(err, ErrRootNotFound)
}

// IsConflict checks if an error is a conflict/already exists error
func IsConflict(err error) bool {
	return errors.Is(err, ErrCommentAlreadyExists) ||
		errors.Is(err, ErrConcurrentModification)
}

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	return errors.Is(err, ErrInvalidReply) ||
		errors.Is(err, ErrContentTooLong) ||
		errors.Is(err, ErrContentEmpty) ||
		errors.Is(err, ErrInvalidFacets) ||
		errors.Is(err, ErrInvalidCursor) ||
		errors.Is(err, ErrInvalidRecord)
}
