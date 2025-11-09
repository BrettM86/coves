package unfurl

import "errors"

var (
	// ErrNotFound is returned when an unfurl cache entry is not found or has expired
	ErrNotFound = errors.New("unfurl cache entry not found or expired")

	// ErrInvalidURL is returned when the provided URL is invalid
	ErrInvalidURL = errors.New("invalid URL")

	// ErrInvalidTTL is returned when the provided TTL is invalid (e.g., negative or zero)
	ErrInvalidTTL = errors.New("invalid TTL: must be positive")
)
