package discover

import (
	"errors"

	"Coves/internal/core/feedgen"
)

// Repository is the Discover Hot store: the discover-hot feed generator and
// the cleanup of its expired ranking state.
type Repository interface {
	feedgen.HydratedPostGenerator
	DiscoverHotStateCleaner
}

// Errors
var (
	ErrInvalidCursor       = errors.New("invalid cursor")
	ErrDiscoverUnavailable = errors.New("discover unavailable")
)
