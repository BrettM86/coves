package timeline

import (
	"context"
	"errors"
	"time"

	"Coves/internal/core/posts"
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
