package posts

import (
	"time"
)

// SelfLabels represents self-applied content labels per com.atproto.label.defs#selfLabels
// This is the structured format used in atProto for content warnings
type SelfLabels struct {
	Values []SelfLabel `json:"values"`
}

// SelfLabel represents a single label value per com.atproto.label.defs#selfLabel
// Neg is optional and negates the label when true
type SelfLabel struct {
	Neg *bool  `json:"neg,omitempty"`
	Val string `json:"val"`
}

// Post represents a post in the AppView database
// Posts are indexed from the firehose after being written to community repositories
type Post struct {
	CreatedAt     time.Time  `json:"createdAt" db:"created_at"`
	IndexedAt     time.Time  `json:"indexedAt" db:"indexed_at"`
	EditedAt      *time.Time `json:"editedAt,omitempty" db:"edited_at"`
	Embed         *string    `json:"embed,omitempty" db:"embed"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty" db:"deleted_at"`
	ContentLabels *string    `json:"labels,omitempty" db:"content_labels"`
	Title         *string    `json:"title,omitempty" db:"title"`
	Content       *string    `json:"content,omitempty" db:"content"`
	ContentFacets *string    `json:"contentFacets,omitempty" db:"content_facets"`
	CID           string     `json:"cid" db:"cid"`
	CommunityDID  string     `json:"communityDid" db:"community_did"`
	RKey          string     `json:"rkey" db:"rkey"`
	URI           string     `json:"uri" db:"uri"`
	AuthorDID     string     `json:"authorDid" db:"author_did"`
	ID            int64      `json:"id" db:"id"`
	UpvoteCount   int        `json:"upvoteCount" db:"upvote_count"`
	DownvoteCount int        `json:"downvoteCount" db:"downvote_count"`
	Score         int        `json:"score" db:"score"`
	CommentCount  int        `json:"commentCount" db:"comment_count"`
}

// CreatePostRequest represents input for creating a new post
// Matches social.coves.community.post.create lexicon input schema
type CreatePostRequest struct {
	OriginalAuthor interface{}            `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}            `json:"federatedFrom,omitempty"`
	Location       interface{}            `json:"location,omitempty"`
	Title          *string                `json:"title,omitempty"`
	Content        *string                `json:"content,omitempty"`
	Embed          map[string]interface{} `json:"embed,omitempty"`
	ThumbnailURL   *string                `json:"thumbnailUrl,omitempty"`
	Labels         *SelfLabels            `json:"labels,omitempty"`
	Community      string                 `json:"community"`
	AuthorDID      string                 `json:"authorDid"`
	Facets         []interface{}          `json:"facets,omitempty"`
}

// CreatePostResponse represents the response from creating a post
// Matches social.coves.community.post.create lexicon output schema
type CreatePostResponse struct {
	URI string `json:"uri"` // AT-URI of created post
	CID string `json:"cid"` // CID of created post
}

// DeletePostRequest represents input for deleting a post
// Matches social.coves.community.post.delete lexicon input schema
type DeletePostRequest struct {
	URI string `json:"uri"` // AT-URI of the post to delete
}

// PostRecord represents the actual atProto record structure written to PDS
// This is the data structure that gets stored in the community's repository
type PostRecord struct {
	OriginalAuthor interface{}            `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}            `json:"federatedFrom,omitempty"`
	Location       interface{}            `json:"location,omitempty"`
	Title          *string                `json:"title,omitempty"`
	Content        *string                `json:"content,omitempty"`
	Embed          map[string]interface{} `json:"embed,omitempty"`
	Labels         *SelfLabels            `json:"labels,omitempty"`
	Type           string                 `json:"$type"`
	Community      string                 `json:"community"`
	Author         string                 `json:"author"`
	CreatedAt      string                 `json:"createdAt"`
	Facets         []interface{}          `json:"facets,omitempty"`
}

// PostView represents the full view of a post with all metadata
// Matches social.coves.community.post.get#postView lexicon
// Used in feeds and get endpoints
type PostView struct {
	IndexedAt     time.Time     `json:"indexedAt"`
	CreatedAt     time.Time     `json:"createdAt"`
	Record        interface{}   `json:"record,omitempty"`
	Embed         interface{}   `json:"embed,omitempty"`
	Language      *string       `json:"language,omitempty"`
	EditedAt      *time.Time    `json:"editedAt,omitempty"`
	Viewer        *ViewerState  `json:"viewer,omitempty"`
	Author        *AuthorView   `json:"author"`
	Stats         *PostStats    `json:"stats,omitempty"`
	Community     *CommunityRef `json:"community"`
	RKey          string        `json:"rkey"`
	CID           string        `json:"cid"`
	URI           string        `json:"uri"`
	UpvoteCount   int           `json:"-"`
	DownvoteCount int           `json:"-"`
	Score         int           `json:"-"`
	CommentCount  int           `json:"-"`
}

// AuthorView represents author information in post views
type AuthorView struct {
	DisplayName *string `json:"displayName,omitempty"`
	Avatar      *string `json:"avatar,omitempty"`
	Reputation  *int    `json:"reputation,omitempty"`
	DID         string  `json:"did"`
	Handle      string  `json:"handle"`
}

// CommunityRef represents minimal community info in post views
type CommunityRef struct {
	Avatar *string `json:"avatar,omitempty"`
	DID    string  `json:"did"`
	Handle string  `json:"handle"`
	Name   string  `json:"name"`
	PDSURL string  `json:"-"` // Not exposed to API, used for blob URL transformation
}

// PostStats represents aggregated statistics
type PostStats struct {
	TagCounts    map[string]int `json:"tagCounts,omitempty"`
	Upvotes      int            `json:"upvotes"`
	Downvotes    int            `json:"downvotes"`
	Score        int            `json:"score"`
	CommentCount int            `json:"commentCount"`
	ShareCount   int            `json:"shareCount,omitempty"`
}

// ViewerState represents the viewer's relationship with the post
type ViewerState struct {
	Vote     *string  `json:"vote,omitempty"`
	VoteURI  *string  `json:"voteUri,omitempty"`
	SavedURI *string  `json:"savedUri,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Saved    bool     `json:"saved"`
}

// Filter constants for GetAuthorPosts
const (
	FilterPostsWithReplies = "posts_with_replies"
	FilterPostsNoReplies   = "posts_no_replies"
	FilterPostsWithMedia   = "posts_with_media"
)

// GetAuthorPostsRequest represents input for fetching author's posts
// Matches social.coves.actor.getPosts lexicon input
type GetAuthorPostsRequest struct {
	ActorDID  string  // Resolved DID from actor param (handle or DID)
	Filter    string  // FilterPostsWithReplies, FilterPostsNoReplies, FilterPostsWithMedia
	Community string  // Optional community DID filter
	Limit     int     // Number of posts to return (1-100, default 50)
	Cursor    *string // Pagination cursor
	ViewerDID string  // Viewer's DID for enriching viewer state
}

// GetAuthorPostsResponse represents author posts response
// Matches social.coves.actor.getPosts lexicon output
type GetAuthorPostsResponse struct {
	Feed   []*FeedViewPost `json:"feed"`
	Cursor *string         `json:"cursor,omitempty"`
}

// FeedViewPost matches social.coves.feed.defs#feedViewPost
// Wraps a post with optional context about why it appears in a feed
type FeedViewPost struct {
	Post   *PostView   `json:"post"`
	Reason *FeedReason `json:"reason,omitempty"` // Context for why post appears in feed
	Reply  *ReplyRef   `json:"reply,omitempty"`  // Reply context if post is a reply
}

// GetPost returns the underlying PostView for viewer state enrichment
func (f *FeedViewPost) GetPost() *PostView {
	return f.Post
}

// FeedReason represents the reason a post appears in a feed
// Matches social.coves.feed.defs union type for feed context
type FeedReason struct {
	Type   string        `json:"$type"`
	Repost *ReasonRepost `json:"repost,omitempty"`
	Pin    *ReasonPin    `json:"pin,omitempty"`
}

// ReasonRepost indicates the post was reposted by another user
type ReasonRepost struct {
	By        *AuthorView `json:"by"`
	IndexedAt string      `json:"indexedAt"`
}

// ReasonPin indicates the post is pinned by the community
type ReasonPin struct {
	Community *CommunityRef `json:"community"`
}

// ReplyRef contains context about post replies
// Matches social.coves.feed.defs#replyRef
type ReplyRef struct {
	Root   *PostRef `json:"root"`
	Parent *PostRef `json:"parent"`
}

// PostRef is a minimal reference to a post (URI + CID)
// Matches social.coves.feed.defs#postRef
type PostRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}
