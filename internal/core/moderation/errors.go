package moderation

import "errors"

// One sentinel per error code the moderation lexicons declare (docs/MODERATION.md, Errors),
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
	ErrOwnerMustTransfer     = errors.New("moderation: owner must transfer ownership")
	ErrCannotRemoveOwner     = errors.New("moderation: cannot remove the owner")
	ErrRoleNotFound          = errors.New("moderation: role not found")
	ErrInviteeHasRole        = errors.New("moderation: invitee already holds a role")
	ErrInviteeNotModerator   = errors.New("moderation: invitee is not a moderator")
	ErrInviteeBanned         = errors.New("moderation: invitee is banned from the community")
	ErrInvitationNotFound    = errors.New("moderation: invitation not found")
	ErrInvitationLapsed      = errors.New("moderation: invitation lapsed")
	ErrInvitationNotOpen     = errors.New("moderation: invitation is not open")
	ErrInvitationStale       = errors.New("moderation: invitation is stale")
	ErrAccountNotFound       = errors.New("moderation: account not found")
	ErrPostNotFound          = errors.New("moderation: post not found")
	ErrRateLimited           = errors.New("moderation: rate limited")
	ErrCommentNotFound       = errors.New("moderation: comment not found")
	ErrSubjectHoldsRole      = errors.New("moderation: subject holds a role in the community")
)
