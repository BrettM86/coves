package timeline

import (
	coreerrors "Coves/internal/core/errors"
	"Coves/internal/core/posts"
	"context"
	"errors"
	"time"
)

// Repository defines timeline data access interface
type Repository interface {
	GetTimeline(ctx context.Context, req GetTimelineRequest) ([]*FeedViewPost, *string, error)
}

// Service defines timeline business logic interface
type Service interface {
	GetTimeline(ctx context.Context, req GetTimelineRequest) (*TimelineResponse, error)
}

// GetTimelineRequest represents input for fetching a user's timeline
// Matches social.coves.timeline.getTimeline lexicon input
type GetTimelineRequest struct {
	Cursor    *string `json:"cursor,omitempty"`
	UserDID   string  `json:"-"` // Extracted from auth, not from query params
	Sort      string  `json:"sort"`
	Timeframe string  `json:"timeframe"`
	Limit     int     `json:"limit"`
}

// TimelineResponse represents paginated timeline output
// Matches social.coves.timeline.getTimeline lexicon output
type TimelineResponse struct {
	Cursor *string         `json:"cursor,omitempty"`
	Feed   []*FeedViewPost `json:"feed"`
}

// FeedViewPost wraps a post with additional feed context
// Matches social.coves.timeline.getTimeline#feedViewPost
type FeedViewPost struct {
	Post   *posts.PostView `json:"post"`
	Reason *FeedReason     `json:"reason,omitempty"` // Why this post is in feed
	Reply  *ReplyRef       `json:"reply,omitempty"`  // Reply context
}

// GetPost returns the underlying PostView for viewer state enrichment
func (f *FeedViewPost) GetPost() *posts.PostView {
	return f.Post
}

// FeedReason is a union type for feed context
// Future: Can be reasonRepost or reasonCommunity
type FeedReason struct {
	Repost    *ReasonRepost    `json:"-"`
	Community *ReasonCommunity `json:"-"`
	Type      string           `json:"$type"`
}

// ReasonRepost indicates post was reposted/shared
type ReasonRepost struct {
	By        *posts.AuthorView `json:"by"`
	IndexedAt time.Time         `json:"indexedAt"`
}

// ReasonCommunity indicates which community this post is from
// Useful when timeline shows posts from multiple communities
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
	ErrUnauthorized  = errors.New("unauthorized")
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
