package communityFeeds

import (
	"Coves/internal/core/posts"
	"time"
)

// GetCommunityFeedRequest represents input for fetching a community feed
// Matches social.coves.communityFeed.getCommunity lexicon input
// Alpha: Basic sorting only (hot, top, new) - no post type filtering
type GetCommunityFeedRequest struct {
	Cursor    *string `json:"cursor,omitempty"`
	Community string  `json:"community"`
	Sort      string  `json:"sort"`
	Timeframe string  `json:"timeframe"`
	Limit     int     `json:"limit"`
}

// FeedResponse represents paginated feed output
// Matches social.coves.communityFeed.getCommunity lexicon output
type FeedResponse struct {
	Cursor *string         `json:"cursor,omitempty"`
	Feed   []*FeedViewPost `json:"feed"`
}

// FeedViewPost wraps a post with additional feed context
// Matches social.coves.communityFeed.getTimeline#feedViewPost
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
// Can be reasonRepost or reasonPin
type FeedReason struct {
	Repost *ReasonRepost `json:"-"`
	Pin    *ReasonPin    `json:"-"`
	Type   string        `json:"$type"`
}

// ReasonRepost indicates post was reposted/shared
type ReasonRepost struct {
	By        *posts.AuthorView `json:"by"`
	IndexedAt time.Time         `json:"indexedAt"`
}

// ReasonPin indicates post is pinned by community
type ReasonPin struct {
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
