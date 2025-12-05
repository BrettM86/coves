package comments

import "errors"

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

	// ErrNotAuthorized indicates the user is not authorized to perform this action
	ErrNotAuthorized = errors.New("not authorized")

	// ErrBanned indicates the user is banned from the community
	ErrBanned = errors.New("user is banned from this community")

	// ErrCommentAlreadyExists indicates a comment with this URI already exists
	ErrCommentAlreadyExists = errors.New("comment already exists")

	// ErrConcurrentModification indicates the comment was modified since it was loaded
	ErrConcurrentModification = errors.New("comment was modified by another operation")
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
		errors.Is(err, ErrContentEmpty)
}
