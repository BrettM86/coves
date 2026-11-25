package comments

import (
	"Coves/internal/core/posts"
)

// CommentView represents the full view of a comment with all metadata
// Matches social.coves.community.comment.defs#commentView lexicon
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
	CreatedAt      string              `json:"createdAt"`
	IndexedAt      string              `json:"indexedAt"`
	URI            string              `json:"uri"`
	CID            string              `json:"cid"`
	IsDeleted      bool                `json:"isDeleted,omitempty"`
	DeletionReason *string             `json:"deletionReason,omitempty"`
	DeletedAt      *string             `json:"deletedAt,omitempty"`
	Moderation     *ModerationView     `json:"moderation,omitempty"`
	// CommunityDID is the community the comment's thread belongs to. Not serialized.
	CommunityDID string `json:"-"`
}

// ThreadViewComment represents a comment with its nested replies
// Matches social.coves.community.comment.defs#threadViewComment lexicon
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
// Includes voting state, vote record reference, and the moderation actions the
// viewer may take
type CommentViewerState struct {
	Vote    *string `json:"vote,omitempty"`    // "up" or "down"
	VoteURI *string `json:"voteUri,omitempty"` // URI of the vote record
	// Moderation lists the moderation actions the viewer may take on the
	// comment, as modlog action tokens. Omitted when there are none.
	Moderation []string `json:"moderation,omitempty"`
	// CommunityModeration lists the community moderation actions the viewer
	// may take on the comment. Omitted when there are none.
	CommunityModeration []string `json:"communityModeration,omitempty"`
}

// IsEmpty reports whether the viewer state carries no field, so the viewer
// object can be omitted from the comment view.
func (s *CommentViewerState) IsEmpty() bool {
	return s.Vote == nil && s.VoteURI == nil && len(s.Moderation) == 0 && len(s.CommunityModeration) == 0
}

// GetCommentsResponse represents the response for fetching comments on a post
// Matches social.coves.community.comment.getComments lexicon output
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

// ModerationView is a comment's public removal state
// (social.coves.moderation.defs#moderationView).
type ModerationView struct {
	State   string                 `json:"state"`
	Sources []ModerationSourceView `json:"sources,omitempty"`
}

// ModerationSourceView attributes a removal (social.coves.moderation.defs#sourceView).
type ModerationSourceView struct {
	AuthorityDID string              `json:"authorityDid"`
	Scope        ModerationScopeView `json:"scope"`
}

// ModerationScopeView is a removal's scope (social.coves.moderation.defs#scopeView).
type ModerationScopeView struct {
	Kind         string `json:"kind"`
	CommunityDID string `json:"communityDid,omitempty"`
}

// LikesFilter selects what social.coves.actor.getLikes lists.
type LikesFilter string

const (
	// LikesFilterAll lists liked posts and liked comments together.
	LikesFilterAll LikesFilter = "all"
	// LikesFilterPosts lists liked posts only.
	LikesFilterPosts LikesFilter = "posts"
	// LikesFilterComments lists liked comments only.
	LikesFilterComments LikesFilter = "comments"
)

// GetActorLikesRequest asks for one page of what an actor upvoted
// (social.coves.actor.getLikes).
type GetActorLikesRequest struct {
	ActorDID  string      // Resolved DID of the actor whose upvotes are listed
	ViewerDID string      // Authenticated viewer's DID
	Filter    LikesFilter // LikesFilterAll, LikesFilterPosts or LikesFilterComments
	Limit     int         // Number of items to return (1-100, default 50)
	Cursor    *string     // Pagination cursor
}

// ActorLikesResponse is one page of liked items. Feed is never nil.
type ActorLikesResponse struct {
	Feed   []*LikedItem
	Cursor *string
}

// LikedItem is a liked post (Post only) or a liked comment (Comment, with
// Post its thread's root post).
type LikedItem struct {
	Post    *posts.PostView
	Comment *CommentView
}

// GetPost returns the item's post: the liked post, or a liked comment's root
// post.
func (i *LikedItem) GetPost() *posts.PostView { return i.Post }

// LikedSubjectKind is what a liked subject is.
type LikedSubjectKind string

const (
	LikedSubjectPost    LikedSubjectKind = "post"
	LikedSubjectComment LikedSubjectKind = "comment"
)

// LikedSubjectRef is one row of a likes page: the liked subject, its kind,
// and for a comment the root post of its thread and the community its parent
// chain reaches ("" for a post, or while the chain does not resolve).
type LikedSubjectRef struct {
	URI          string
	Kind         LikedSubjectKind
	RootURI      string
	CommunityDID string
}
