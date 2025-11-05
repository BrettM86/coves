package comments

import (
	"time"
)

// Comment represents a comment in the AppView database
// Comments are indexed from the firehose after being written to user repositories
type Comment struct {
	ID            int64      `json:"id" db:"id"`
	URI           string     `json:"uri" db:"uri"`
	CID           string     `json:"cid" db:"cid"`
	RKey          string     `json:"rkey" db:"rkey"`
	CommenterDID  string     `json:"commenterDid" db:"commenter_did"`

	// Author info (hydrated from users table for view building)
	// Only populated by ListByParentWithHotRank, not persisted in comments table
	CommenterHandle string   `json:"commenterHandle,omitempty" db:"-"`

	// Threading (reply references)
	RootURI       string     `json:"rootUri" db:"root_uri"`
	RootCID       string     `json:"rootCid" db:"root_cid"`
	ParentURI     string     `json:"parentUri" db:"parent_uri"`
	ParentCID     string     `json:"parentCid" db:"parent_cid"`

	// Content
	Content       string     `json:"content" db:"content"`
	ContentFacets *string    `json:"contentFacets,omitempty" db:"content_facets"`
	Embed         *string    `json:"embed,omitempty" db:"embed"`
	ContentLabels *string    `json:"labels,omitempty" db:"content_labels"`
	Langs         []string   `json:"langs,omitempty" db:"langs"`

	// Timestamps
	CreatedAt     time.Time  `json:"createdAt" db:"created_at"`
	IndexedAt     time.Time  `json:"indexedAt" db:"indexed_at"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty" db:"deleted_at"`

	// Stats (denormalized for performance)
	UpvoteCount   int        `json:"upvoteCount" db:"upvote_count"`
	DownvoteCount int        `json:"downvoteCount" db:"downvote_count"`
	Score         int        `json:"score" db:"score"`
	ReplyCount    int        `json:"replyCount" db:"reply_count"`
}

// CommentRecord represents the atProto record structure indexed from Jetstream
// This is the data structure that gets stored in the user's repository
// Matches social.coves.feed.comment lexicon
type CommentRecord struct {
	Type    string                 `json:"$type"`
	Reply   ReplyRef               `json:"reply"`
	Content string                 `json:"content"`
	Facets  []interface{}          `json:"facets,omitempty"`
	Embed   map[string]interface{} `json:"embed,omitempty"`
	Langs   []string               `json:"langs,omitempty"`
	Labels  *SelfLabels            `json:"labels,omitempty"`
	CreatedAt string               `json:"createdAt"`
}

// ReplyRef represents the threading structure from the comment lexicon
// Root always points to the original post, parent points to the immediate parent
type ReplyRef struct {
	Root   StrongRef `json:"root"`
	Parent StrongRef `json:"parent"`
}

// StrongRef represents a strong reference to a record (URI + CID)
// Matches com.atproto.repo.strongRef
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// SelfLabels represents self-applied content labels per com.atproto.label.defs#selfLabels
// This is the structured format used in atProto for content warnings
type SelfLabels struct {
	Values []SelfLabel `json:"values"`
}

// SelfLabel represents a single label value per com.atproto.label.defs#selfLabel
// Neg is optional and negates the label when true
type SelfLabel struct {
	Val string `json:"val"`           // Required: label value (max 128 chars)
	Neg *bool  `json:"neg,omitempty"` // Optional: negates the label if true
}
