package discover

import (
	coreerrors "Coves/internal/core/errors"
	"Coves/internal/core/posts"
	"context"
	"errors"
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
	ViewerDID string  `json:"-"` // Optional: authenticated viewer's DID for block filtering
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

// GetPost returns the underlying PostView for viewer state enrichment
func (f *FeedViewPost) GetPost() *posts.PostView {
	return f.Post
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

// ValidationError is the shared validation error type. It is aliased rather
// than redefined so that one errors.As at the API boundary matches validation
// failures from every domain package, instead of each handler needing to know
// which domains it might hear from.
type ValidationError = coreerrors.ValidationError

// NewValidationError creates a new validation error
func NewValidationError(field, message string) error {
	return coreerrors.NewValidationError(field, message)
}

// IsValidationError checks if an error is a validation error.
//
// This unwraps, unlike the bare type assertion it replaces: a validation error
// that any layer wrapped with %w for context used to stop being recognised
// here, and fell through to a 500 instead of a 400.
func IsValidationError(err error) bool {
	return coreerrors.IsValidationError(err)
}
