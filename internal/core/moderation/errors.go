package moderation

import "errors"

// One sentinel per error code the moderation lexicons declare (PRD §14.5),
// plus two that never reach the wire on their own: the reader-level
// ErrSubjectNotIndexed, and ErrResolverUnavailable, which always accompanies
// ErrModerationUnavailable to say an identity or community resolver, not the
// store, was unavailable.
var (
	ErrAuthRequired          = errors.New("moderation: authentication required")
	ErrForbidden             = errors.New("moderation: forbidden")
	ErrInvalidRequest        = errors.New("moderation: invalid request")
	ErrInvalidSubject        = errors.New("moderation: invalid subject")
	ErrSubjectNotFound       = errors.New("moderation: subject not found")
	ErrDecisionNotFound      = errors.New("moderation: decision not found")
	ErrInvalidDecision       = errors.New("moderation: invalid decision")
	ErrContentChanged        = errors.New("moderation: content changed")
	ErrStateConflict         = errors.New("moderation: state conflict")
	ErrIdempotencyConflict   = errors.New("moderation: idempotency conflict")
	ErrUnsupportedReason     = errors.New("moderation: unsupported reason")
	ErrUnsupportedLabel      = errors.New("moderation: unsupported label")
	ErrInvalidCursor         = errors.New("moderation: invalid cursor")
	ErrModerationUnavailable = errors.New("moderation: temporarily unavailable")
	ErrSubjectNotIndexed     = errors.New("moderation: subject never indexed")
	ErrResolverUnavailable   = errors.New("moderation: filter target resolver unavailable")
)
