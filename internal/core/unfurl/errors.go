package unfurl

import "errors"

var (
	// ErrNotFound is returned when an unfurl cache entry is not found or has expired
	ErrNotFound = errors.New("unfurl cache entry not found or expired")

	// ErrInvalidURL is returned when the provided URL is invalid
	ErrInvalidURL = errors.New("invalid URL")

	// ErrInvalidTTL is returned when the provided TTL is invalid (e.g., negative or zero)
	ErrInvalidTTL = errors.New("invalid TTL: must be positive")

	// ErrPageTooLarge is returned when a remote page delivers more than
	// maxUnfurlBodyBytes.
	//
	// IT IS A REFUSAL, NOT A TRUNCATION, and that distinction is this site's
	// alone among the fetch sites in this tree. Every other one discards the
	// body or treats it as opaque bytes; this one parses the body and returns
	// its CONTENT — og:title, og:description, og:image — into an UnfurlResult
	// that is cached for a day and served into a post. parseOpenGraph and
	// html.Parse are both error-tolerant, so half a document yields a result
	// that reads as whole. A caller must be told the difference.
	ErrPageTooLarge = errors.New("unfurl target exceeds the response size limit")
)
