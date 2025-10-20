package posts

import (
	"time"
)

// Post represents a post in the AppView database
// Posts are indexed from the firehose after being written to community repositories
type Post struct {
	CreatedAt     time.Time  `json:"createdAt" db:"created_at"`
	IndexedAt     time.Time  `json:"indexedAt" db:"indexed_at"`
	EditedAt      *time.Time `json:"editedAt,omitempty" db:"edited_at"`
	Embed         *string    `json:"embed,omitempty" db:"embed"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty" db:"deleted_at"`
	ContentLabels *string    `json:"contentLabels,omitempty" db:"content_labels"`
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
// Matches social.coves.post.create lexicon input schema
type CreatePostRequest struct {
	OriginalAuthor interface{}            `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}            `json:"federatedFrom,omitempty"`
	Location       interface{}            `json:"location,omitempty"`
	Title          *string                `json:"title,omitempty"`
	Content        *string                `json:"content,omitempty"`
	Embed          map[string]interface{} `json:"embed,omitempty"`
	Community      string                 `json:"community"`
	AuthorDID      string                 `json:"authorDid"`
	Facets         []interface{}          `json:"facets,omitempty"`
	ContentLabels  []string               `json:"contentLabels,omitempty"`
}

// CreatePostResponse represents the response from creating a post
// Matches social.coves.post.create lexicon output schema
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
	Type           string                 `json:"$type"`
	Community      string                 `json:"community"`
	Author         string                 `json:"author"`
	CreatedAt      string                 `json:"createdAt"`
	Facets         []interface{}          `json:"facets,omitempty"`
	ContentLabels  []string               `json:"contentLabels,omitempty"`
}
