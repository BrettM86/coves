package pds

import "errors"

// Typed errors for PDS operations.
// These allow services to use errors.Is() for reliable error detection
// instead of fragile string matching.
var (
	// ErrUnauthorized indicates the request failed due to invalid or expired credentials (HTTP 401).
	ErrUnauthorized = errors.New("unauthorized")

	// ErrForbidden indicates the request was rejected due to insufficient permissions (HTTP 403).
	ErrForbidden = errors.New("forbidden")

	// ErrNotFound indicates the requested resource does not exist (HTTP 404).
	ErrNotFound = errors.New("not found")

	// ErrBadRequest indicates the request was malformed or invalid (HTTP 400).
	ErrBadRequest = errors.New("bad request")

	// ErrConflict indicates a conflict occurred, such as a record being modified by another operation (HTTP 409).
	ErrConflict = errors.New("conflict")

	// ErrRateLimited indicates the request was rejected due to rate limiting (HTTP 429).
	ErrRateLimited = errors.New("rate limited")

	// ErrPayloadTooLarge indicates the request payload exceeds PDS limits (HTTP 413).
	ErrPayloadTooLarge = errors.New("payload too large")

	// ErrSwapConflict indicates an optimistic-concurrency guard lost: the
	// swapRecord CID or the swapCommit CID the request named is not the one the
	// repo is at, so another writer got there first.
	//
	// It is NOT ErrConflict, and the difference is not cosmetic. A PDS answers
	// a failed swap with HTTP 400 and `"error": "InvalidSwap"` — verified
	// against a live PDS, not inferred from the lexicon, which documents 409 —
	// so the status code alone maps it onto ErrBadRequest, indistinguishable
	// from a malformed record. A lost race is the one 400 that must be RETRIED
	// (re-read, re-shape, write again) rather than reported, so it needs its
	// own sentinel.
	ErrSwapConflict = errors.New("swap conflict")

	// ErrNoCommit indicates a single-record write that the PDS accepted without
	// producing a commit: the record it was asked to write was byte-identical
	// to the one already standing at that key, so there was nothing to commit.
	//
	// VERIFIED AGAINST A LIVE PDS: putRecord answers a no-op write with HTTP
	// 200 carrying uri and cid but NO `commit` object, and it does so BEFORE
	// the swapRecord guard is evaluated — a create-only put (swapRecord null)
	// of an identical record is a 200 no-op, while the same put of DIFFERENT
	// bytes is InvalidSwap.
	//
	// It is an error rather than a zero-valued success because the commit rev
	// is exactly what most callers are about to persist as an ordering
	// watermark, and a fabricated one is worse than a failure. It is its OWN
	// sentinel rather than a generic malformed-body report because for a
	// GUARDED CREATE it is not a failure at all: it means the record already
	// exists and is identical, which is precisely what a retry after a lost
	// response should be told.
	ErrNoCommit = errors.New("write produced no commit")

	// ErrServerError indicates the PDS failed to process a well-formed request
	// (HTTP 5xx). It is separated from the generic wrap because it is the one
	// remote failure class that is worth retrying unchanged: applyWrites
	// answers a delete of a missing record, or a create of an existing one,
	// with a 500, and a caller that cannot tell that from a transport failure
	// cannot decide whether to re-read and re-shape its batch.
	ErrServerError = errors.New("server error")

	// ErrSessionExpired indicates a stored OAuth session could not be resumed:
	// the refresh token expired, the session was revoked on the PDS, or the
	// DPoP key no longer matches. Unlike ErrUnauthorized this is detected
	// locally, before any request reaches the PDS, so it carries no HTTP
	// status — but it has the same remedy, and a client that is not told to
	// re-authenticate will retry forever.
	ErrSessionExpired = errors.New("oauth session expired")
)

// IsAuthError returns true if the error is an authentication/authorization error.
//
// It deliberately spans both 401 and 403, so it must NOT be used to pick an
// HTTP status: 401 means "sign in again" while 403 means "your session lacks
// the scope for this", and collapsing them leaves clients unable to tell a
// dead session from a permissions gap. Use IsReauthRequired for that decision,
// or match the individual sentinels.
func IsAuthError(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrSessionExpired)
}

// IsReauthRequired reports whether the user's session is no longer usable and
// the client must start a new sign-in. This is the check that should drive a
// 401 response. ErrForbidden is excluded: re-authenticating with the same
// scopes would fail the same way.
func IsReauthRequired(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrSessionExpired)
}

// IsConflictError returns true if the error indicates a conflict (e.g., duplicate record).
func IsConflictError(err error) bool {
	return errors.Is(err, ErrConflict)
}
