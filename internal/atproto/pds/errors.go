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
)

// IsAuthError returns true if the error is an authentication/authorization error.
// This is a convenience function for checking if re-authentication might help.
func IsAuthError(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden)
}
