package identity

import "fmt"

// ErrNotFound is returned when an identity cannot be resolved
type ErrNotFound struct {
	Identifier string
	Reason     string
}

func (e *ErrNotFound) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("identity not found: %s (%s)", e.Identifier, e.Reason)
	}
	return fmt.Sprintf("identity not found: %s", e.Identifier)
}

// ErrInvalidIdentifier is returned for malformed handles or DIDs
type ErrInvalidIdentifier struct {
	Identifier string
	Reason     string
}

func (e *ErrInvalidIdentifier) Error() string {
	return fmt.Sprintf("invalid identifier %s: %s", e.Identifier, e.Reason)
}

// ErrCacheMiss is returned when an identifier is not in the cache
type ErrCacheMiss struct {
	Identifier string
}

func (e *ErrCacheMiss) Error() string {
	return fmt.Sprintf("cache miss: %s", e.Identifier)
}

// ErrResolutionFailed is returned when resolution fails for reasons other than not found
type ErrResolutionFailed struct {
	Identifier string
	Reason     string
}

func (e *ErrResolutionFailed) Error() string {
	return fmt.Sprintf("resolution failed for %s: %s", e.Identifier, e.Reason)
}
