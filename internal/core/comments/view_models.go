package comments

import (
	"Coves/internal/core/posts"
)

// CommentView represents the full view of a comment with all metadata
// Matches social.coves.feed.getComments#commentView lexicon
// Used in thread views and get endpoints
type CommentView struct {
	URI           string              `json:"uri"`
	CID           string              `json:"cid"`
	Author        *posts.AuthorView   `json:"author"`
	Record        interface{}         `json:"record"`                  // Original record verbatim
	Post          *CommentRef         `json:"post"`                    // Reference to parent post
	Parent        *CommentRef         `json:"parent,omitempty"`        // Parent comment if nested
	Content       string              `json:"content"`
	ContentFacets []interface{}       `json:"contentFacets,omitempty"`
	Embed         interface{}         `json:"embed,omitempty"`
	CreatedAt     string              `json:"createdAt"`               // RFC3339
	IndexedAt     string              `json:"indexedAt"`               // RFC3339
	Stats         *CommentStats       `json:"stats"`
	Viewer        *CommentViewerState `json:"viewer,omitempty"`
}

// ThreadViewComment represents a comment with its nested replies
// Matches social.coves.feed.getComments#threadViewComment lexicon
// Supports recursive threading for comment trees
type ThreadViewComment struct {
	Comment *CommentView         `json:"comment"`
	Replies []*ThreadViewComment `json:"replies,omitempty"` // Recursive nested replies
	HasMore bool                 `json:"hasMore,omitempty"` // Indicates more replies exist
}

// CommentRef is a minimal reference to a post or comment (URI + CID)
// Used for threading references (post and parent comment)
type CommentRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// CommentStats represents aggregated statistics for a comment
// Includes voting metrics and reply counts
type CommentStats struct {
	Upvotes    int `json:"upvotes"`
	Downvotes  int `json:"downvotes"`
	Score      int `json:"score"`
	ReplyCount int `json:"replyCount"`
}

// CommentViewerState represents the viewer's relationship with the comment
// Includes voting state and vote record reference
type CommentViewerState struct {
	Vote    *string `json:"vote,omitempty"`    // "up" or "down"
	VoteURI *string `json:"voteUri,omitempty"` // URI of the vote record
}

// GetCommentsResponse represents the response for fetching comments on a post
// Matches social.coves.feed.getComments lexicon output
// Includes the full comment thread tree and original post reference
type GetCommentsResponse struct {
	Comments []*ThreadViewComment `json:"comments"`
	Post     interface{}          `json:"post"`             // PostView from post handler
	Cursor   *string              `json:"cursor,omitempty"` // Pagination cursor
}
