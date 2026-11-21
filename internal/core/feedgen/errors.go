package feedgen

import "errors"

var (
	// ErrUnknownFeed means the requested feed at-uri is not a registered generator.
	ErrUnknownFeed = errors.New("unknown feed")

	// ErrUnsupportedFeedScope means the feed cannot be used in the requested
	// scope, or the scope has no supported default feed.
	ErrUnsupportedFeedScope = errors.New("unsupported feed scope")

	// ErrInvalidCursor means the cursor is malformed, tampered with, or was
	// issued for a different feed, scope, community, or viewer.
	ErrInvalidCursor = errors.New("invalid cursor")
)
