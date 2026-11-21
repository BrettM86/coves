// Package feedgen holds the feed generator registry, the built-in generator
// record sync, and the getFeed service.
package feedgen

import (
	"context"

	"Coves/internal/core/posts"
)

// Kind is what a feed generator orders: posts or comments.
type Kind string

const (
	KindPosts    Kind = "posts"
	KindComments Kind = "comments"
)

// Scope names, as in social.coves.feed.defs#scope.
const (
	ScopeAll       = "all"
	ScopeHome      = "home"
	ScopeCommunity = "community"
	ScopeThread    = "thread"
)

// GeneratorCollection is the NSID of feed generator declaration records.
const GeneratorCollection = "social.coves.feed.generator"

// Scope is where a feed is being read. CommunityDID is set only for the
// community scope.
type Scope struct {
	Name         string
	CommunityDID string
}

// SkeletonRequest asks a generator for one page of post URIs. Cursor is the
// generator's own (inner) cursor; "" requests the first page.
type SkeletonRequest struct {
	ViewerDID string
	Scope     Scope
	Limit     int
	Cursor    string
	Timeframe string
}

// SkeletonItem is one post URI a generator selected.
type SkeletonItem struct {
	URI         string
	FeedContext string
}

// SkeletonPage is one page of a generator's skeleton. Cursor "" means the
// generator has nothing further.
type SkeletonPage struct {
	Items  []SkeletonItem
	Cursor string
	ReqID  string
}

// PostGenerator produces post skeletons.
type PostGenerator interface {
	Skeleton(ctx context.Context, req SkeletonRequest) (SkeletonPage, error)
}

// HydratedPostPage is one page a generator hydrated itself. Cursor is the
// generator's own (inner) cursor; "" means the generator has nothing further.
type HydratedPostPage struct {
	Feed   []*FeedViewPost
	Cursor string
	ReqID  string
}

// HydratedPostGenerator produces pages of already hydrated posts, for
// generators whose selection and hydration cannot be split.
type HydratedPostGenerator interface {
	HydratedPage(ctx context.Context, req SkeletonRequest) (HydratedPostPage, error)
}

// HydrateRequest asks for the post views of URIs as seen by a viewer in a
// scope.
type HydrateRequest struct {
	ViewerDID string
	Scope     Scope
	URIs      []string
}

// PostHydrator turns post URIs into post views, in input order, dropping
// posts the viewer may not see.
type PostHydrator interface {
	HydratePosts(ctx context.Context, req HydrateRequest) ([]*posts.PostView, error)
}

// CursorSigner signs cursor payloads and verifies signed cursors.
type CursorSigner interface {
	Sign(payload string) string
	Verify(cursor string) (string, error)
}

// FeedViewPost is one item of a getFeed response.
type FeedViewPost struct {
	Post        *posts.PostView `json:"post"`
	FeedContext string          `json:"feedContext,omitempty"`
}

// GetPost returns the item's post view.
func (f *FeedViewPost) GetPost() *posts.PostView {
	if f == nil {
		return nil
	}
	return f.Post
}

// GetFeedRequest is a getFeed call. Feed "" selects the instance default for
// the scope; Cursor is the envelope cursor a previous response returned.
type GetFeedRequest struct {
	ViewerDID string
	Scope     Scope
	Feed      string
	Limit     int
	Cursor    string
	Timeframe string
}

// GetFeedResponse is one page of a getFeed call.
type GetFeedResponse struct {
	Feed   []*FeedViewPost `json:"feed"`
	Cursor string          `json:"cursor,omitempty"`
	ReqID  string          `json:"reqId,omitempty"`
}

// Service serves getFeed.
type Service interface {
	GetFeed(ctx context.Context, req GetFeedRequest) (*GetFeedResponse, error)
}

// CommentSkeletonRequest asks a comment generator for the ordered direct
// replies of each parent. Cursor is set only when Parents has one entry.
type CommentSkeletonRequest struct {
	ViewerDID      string
	Parents        []string
	LimitPerParent int
	Timeframe      string
	Cursor         string
}

// CommentReplies is one parent's ordered reply URIs. Cursor "" means the
// parent has no further replies to page.
type CommentReplies struct {
	Parent string
	URIs   []string
	Cursor string
}

// CommentSkeletonPage holds one CommentReplies per requested parent, in
// request order.
type CommentSkeletonPage struct {
	Replies []CommentReplies
}

// CommentGenerator orders the direct replies of several parents per call.
type CommentGenerator interface {
	CommentSkeleton(ctx context.Context, req CommentSkeletonRequest) (CommentSkeletonPage, error)
}
