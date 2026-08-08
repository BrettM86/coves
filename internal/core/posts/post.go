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

	// Bridge-asserted origin-platform vote aggregates for federated/bridged content.
	// Populated from the record's bridgedStats field; kept separate from native votes.
	// BridgedStatsAsOf is nil when no bridgedStats have ever been applied.
	BridgedUpvoteCount   int        `json:"bridgedUpvoteCount" db:"bridged_upvote_count"`
	BridgedDownvoteCount int        `json:"bridgedDownvoteCount" db:"bridged_downvote_count"`
	BridgedStatsAsOf     *time.Time `json:"bridgedStatsAsOf,omitempty" db:"bridged_stats_as_of"`
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

	// Status is the community's decision as of this response: PostStatusAccepted
	// when the local fast path settled it synchronously, PostStatusPending when
	// the community still owes a decision (§4.2 steps 4 and 5).
	//
	// IT IS NOT AN ERROR CHANNEL. Pending is a SUCCESS: the author's record
	// exists and is theirs whatever the community decides, and the acceptance
	// this AppView failed to write is retried idempotently by the firehose
	// engine. A client that treated pending as a failure and resubmitted would
	// be answered with its own post's URI, because the rkey is deterministic —
	// but it would also show its author an error over a post that was written.
	//
	// Omitted when empty so pre-flip clients, which have never seen the field,
	// decode a response identical to the one they used to get.
	Status string `json:"status,omitempty"`
}

// UpdatePostRequest represents input for editing an existing post.
//
// It carries the post's URI and the mutable content fields ONLY. There is
// deliberately no community field: the postv2 lexicon calls `community`
// immutable — retargeting a post means writing a new post record, and consumers
// discard an update event that changes it — so an edit that could express a
// retarget would be an edit whose only possible outcome is being ignored by
// every reader.
type UpdatePostRequest struct {
	Title   *string                `json:"title,omitempty"`
	Content *string                `json:"content,omitempty"`
	Embed   map[string]interface{} `json:"embed,omitempty"`
	Labels  *SelfLabels            `json:"labels,omitempty"`

	// Community is accepted so that a client which sends it can be REFUSED
	// rather than silently obeyed-in-part. See the type comment: it is not a
	// field an edit may change, and a request naming a different community is a
	// validation error, not a partially applied update.
	Community string `json:"community,omitempty"`

	URI    string        `json:"uri"`
	Facets []interface{} `json:"facets,omitempty"`
	Langs  []string      `json:"langs,omitempty"`
	Tags   []string      `json:"tags,omitempty"`
}

// UpdatePostResponse is the edited record's identity: the same URI it always
// had, and the NEW CID the edit committed.
type UpdatePostResponse struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// DeletePostRequest represents input for deleting a post
// Matches social.coves.community.post.delete lexicon input schema
type DeletePostRequest struct {
	URI string `json:"uri"` // AT-URI of the post to delete
}

// GetPostsRequest represents input for batch-fetching post views by AT-URI.
// Matches social.coves.community.post.get parameters (plus viewer context).
// URIs must be canonical DID-based AT-URIs (at://<community-did>/.../<rkey>);
// handle-based authorities are rejected (handles are mutable and would break on rename).
type GetPostsRequest struct {
	URIs      []string // 1..25 canonical DID-based post AT-URIs to hydrate
	ViewerDID string   // Optional viewer DID (from OptionalAuth) for viewer state
}

// NotFoundPost is a union member of the social.coves.community.post.get output,
// emitted when a requested URI cannot be resolved (deleted, never indexed, or an
// unresolvable/invalid authority). Matches social.coves.community.post.get#notFoundPost.
type NotFoundPost struct {
	URI      string `json:"uri"`
	NotFound bool   `json:"notFound"` // Always true (const per lexicon); discriminates the union on the wire
}

// BlockedAuthor is the minimal author info carried on a BlockedPost.
// Matches social.coves.community.post.get#blockedAuthor.
type BlockedAuthor struct {
	DID string `json:"did"`
}

// BlockedPost is a union member of the social.coves.community.post.get output, emitted
// when a found post is hidden from the viewer because the viewer has blocked its author.
// This keeps get-by-URI (permalink/cold-load) consistent with feed/timeline block
// filtering. Matches social.coves.community.post.get#blockedPost.
//
// blockedBy is always "author" today: community blocks are not enforced on any read path
// yet, and moderator removals already surface as notFoundPost (soft-deleted rows are
// absent from the repo fetch).
//
// KNOWN DEFECT, not a design choice: community blocks are indexed and then read by nobody
// (issue 2026-07-29-community-blocks-indexed-but-never-enforced). When that is fixed this
// is the second place it surfaces — the union member already has room for a "community"
// value — so do not "tidy" this comment into a claim that author is the only case there
// can be.
type BlockedPost struct {
	URI       string         `json:"uri"`
	Blocked   bool           `json:"blocked"` // Always true (const per lexicon); discriminates the union on the wire
	BlockedBy string         `json:"blockedBy,omitempty"`
	Author    *BlockedAuthor `json:"author,omitempty"`
}

// PostResult is one ordered element of a GetPosts response. Exactly one of Post,
// Blocked, or NotFound is set: Post when the post was found and visible to the viewer,
// Blocked when the viewer has blocked the author, NotFound when the URI could not be
// resolved. Construct results via the result helpers so the const discriminators
// (notFound/blocked == true) cannot be left unset.
type PostResult struct {
	Post     *PostView
	Blocked  *BlockedPost
	NotFound *NotFoundPost
}

// foundResult builds a found (postView) union member.
func foundResult(view *PostView) *PostResult {
	return &PostResult{Post: view}
}

// notFoundResult builds a notFoundPost union member with its const discriminator set.
func notFoundResult(uri string) *PostResult {
	return &PostResult{NotFound: &NotFoundPost{URI: uri, NotFound: true}}
}

// blockedByAuthorResult builds a blockedPost union member (blockedBy "author") with its
// const discriminator set.
func blockedByAuthorResult(uri, authorDID string) *PostResult {
	return &PostResult{Blocked: &BlockedPost{
		URI:       uri,
		Blocked:   true,
		BlockedBy: "author",
		Author:    &BlockedAuthor{DID: authorDID},
	}}
}

// GetPost returns the underlying PostView (nil for blocked/not-found results),
// satisfying the viewer-state enrichment helper's FeedPostProvider interface. Blocked
// and not-found results carry no PostView, so they are skipped by viewer enrichment
// and embed transforms.
func (r *PostResult) GetPost() *PostView {
	return r.Post
}

// Member returns the single populated union member for wire encoding and true, or
// (nil, false) when the result is NOT in the valid exactly-one-member state — either
// empty (no member set) or, defensively, more than one member set. The latter would be
// an assembly bug; reporting it (rather than silently picking one by priority) is the
// whole point of this guard, so a mis-assembled result surfaces as an internal error
// instead of emitting a null or ambiguous array entry that violates the lexicon's union
// (postView | blockedPost | notFoundPost).
func (r *PostResult) Member() (interface{}, bool) {
	var member interface{}
	count := 0
	if r.Post != nil {
		member = r.Post
		count++
	}
	if r.Blocked != nil {
		member = r.Blocked
		count++
	}
	if r.NotFound != nil {
		member = r.NotFound
		count++
	}
	if count != 1 {
		return nil, false
	}
	return member, true
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

	// PDSURL is the author's PDS, not exposed to the API — the mirror of
	// CommunityRef.PDSURL beside it, and needed for the same reason: a postv2
	// post's blobs live in the AUTHOR's repository, so building their URLs
	// means knowing which server holds them.
	//
	// The repository query has always SELECTed this column (it hydrates the
	// author's avatar) and always dropped it here. Carrying it is what lets the
	// blob transform pick an owner per record instead of assuming every post's
	// media is the community's.
	PDSURL string `json:"-"`
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
	FilterPostsWithReplies = "posts-with-replies"
	FilterPostsNoReplies   = "posts-no-replies"
	FilterPostsWithMedia   = "posts-with-media"
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
