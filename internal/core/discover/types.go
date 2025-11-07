package discover

import (
	"context"
	"errors"

	"Coves/internal/core/posts"
)

// Repository defines discover data access interface
type Repository interface {
	GetDiscover(ctx context.Context, req GetDiscoverRequest) ([]*FeedViewPost, *string, error)
}

// Service defines discover business logic interface
type Service interface {
	GetDiscover(ctx context.Context, req GetDiscoverRequest) (*DiscoverResponse, error)
}

// GetDiscoverRequest represents input for fetching the discover feed
// Matches social.coves.feed.getDiscover lexicon input
type GetDiscoverRequest struct {
	Cursor    *string `json:"cursor,omitempty"`
	Sort      string  `json:"sort"`
	Timeframe string  `json:"timeframe"`
	Limit     int     `json:"limit"`
}

// DiscoverResponse represents paginated discover feed output
// Matches social.coves.feed.getDiscover lexicon output
type DiscoverResponse struct {
	Cursor *string         `json:"cursor,omitempty"`
	Feed   []*FeedViewPost `json:"feed"`
}

// FeedViewPost wraps a post with additional feed context
type FeedViewPost struct {
	Post   *posts.PostView `json:"post"`
	Reason *FeedReason     `json:"reason,omitempty"`
	Reply  *ReplyRef       `json:"reply,omitempty"`
}

// FeedReason is a union type for feed context
type FeedReason struct {
	Repost    *ReasonRepost    `json:"-"`
	Community *ReasonCommunity `json:"-"`
	Type      string           `json:"$type"`
}

// ReasonRepost indicates post was reposted/shared
type ReasonRepost struct {
	By        *posts.AuthorView `json:"by"`
	IndexedAt string            `json:"indexedAt"`
}

// ReasonCommunity indicates which community this post is from
type ReasonCommunity struct {
	Community *posts.CommunityRef `json:"community"`
}

// ReplyRef contains context about post replies
type ReplyRef struct {
	Root   *PostRef `json:"root"`
	Parent *PostRef `json:"parent"`
}

// PostRef is a minimal reference to a post (URI + CID)
type PostRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// Errors
var (
	ErrInvalidCursor = errors.New("invalid cursor")
)

// ValidationError represents a validation error with field context
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return &ValidationError{
		Field:   field,
		Message: message,
	}
}

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	_, ok := err.(*ValidationError)
	return ok
}
