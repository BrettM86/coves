package comments

import (
	"Coves/internal/core/posts"
)

// CommentView represents the full view of a comment with all metadata
// Matches social.coves.community.comment.getComments#commentView lexicon
// Used in thread views and get endpoints
// For deleted comments, IsDeleted=true and content-related fields are empty/nil
type CommentView struct {
	Embed          interface{}         `json:"embed,omitempty"`
	Record         interface{}         `json:"record"`
	Viewer         *CommentViewerState `json:"viewer,omitempty"`
	Author         *posts.AuthorView   `json:"author"`
	Post           *CommentRef         `json:"post"`
	Parent         *CommentRef         `json:"parent,omitempty"`
	Stats          *CommentStats       `json:"stats"`
	Content        string              `json:"content"`
	CreatedAt      string              `json:"createdAt"`
	IndexedAt      string              `json:"indexedAt"`
	URI            string              `json:"uri"`
	CID            string              `json:"cid"`
	IsDeleted      bool                `json:"isDeleted,omitempty"`
	DeletionReason *string             `json:"deletionReason,omitempty"`
	DeletedAt      *string             `json:"deletedAt,omitempty"`
}

// ThreadViewComment represents a comment with its nested replies
// Matches social.coves.community.comment.getComments#threadViewComment lexicon
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
	Post     interface{}          `json:"post"`
	Cursor   *string              `json:"cursor,omitempty"`
	Comments []*ThreadViewComment `json:"comments"`
}

// GetActorCommentsRequest defines the parameters for fetching a user's comments
// Used by social.coves.actor.getComments endpoint
type GetActorCommentsRequest struct {
	ActorDID  string  // Required: DID of the commenter
	Community string  // Optional: filter to comments in a specific community (handle or DID)
	Limit     int     // Max comments to return (1-100, default 50)
	Cursor    *string // Pagination cursor from previous response
	ViewerDID *string // Optional: DID of the viewer for populating viewer state
}

// GetActorCommentsResponse represents the response for fetching a user's comments
// Matches social.coves.actor.getComments lexicon output
type GetActorCommentsResponse struct {
	Comments []*CommentView `json:"comments"`
	Cursor   *string        `json:"cursor,omitempty"`
}
