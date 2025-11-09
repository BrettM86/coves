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
	Title         *string       `json:"title,omitempty"`
	Text          *string       `json:"text,omitempty"`
	Viewer        *ViewerState  `json:"viewer,omitempty"`
	Author        *AuthorView   `json:"author"`
	Stats         *PostStats    `json:"stats,omitempty"`
	Community     *CommunityRef `json:"community"`
	RKey          string        `json:"rkey"`
	CID           string        `json:"cid"`
	URI           string        `json:"uri"`
	TextFacets    []interface{} `json:"textFacets,omitempty"`
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
