package blueskypost

import (
	"errors"
	"time"
)

// Sentinel errors for typed error checking
var (
	// ErrCircuitOpen indicates the circuit breaker is open for a provider
	ErrCircuitOpen = errors.New("circuit breaker open")
)

// BlueskyPostResult represents the resolved data from a Bluesky post.
// This includes the post text, author information, engagement metrics,
// and media indicators (Phase 1 does not render media, only indicates presence).
type BlueskyPostResult struct {
	// CreatedAt is when the post was created
	CreatedAt time.Time `json:"createdAt"`

	// Author contains the post author's identity information
	Author *Author `json:"author"`

	// QuotedPost is a nested Bluesky post if this post quotes another post
	// Limited to 1 level of nesting in Phase 1
	QuotedPost *BlueskyPostResult `json:"quotedPost,omitempty"`

	// URI is the AT-URI of the post (e.g., at://did:plc:xxx/app.bsky.feed.post/abc123)
	URI string `json:"uri"`

	// CID is the content identifier for this version of the post
	CID string `json:"cid"`

	// Text is the post content (plain text)
	Text string `json:"text"`

	// Message provides a human-readable error message if Unavailable is true
	Message string `json:"message,omitempty"`

	// ReplyCount is the number of replies to this post
	ReplyCount int `json:"replyCount"`

	// RepostCount is the number of reposts of this post
	RepostCount int `json:"repostCount"`

	// LikeCount is the number of likes on this post
	LikeCount int `json:"likeCount"`

	// MediaCount is the number of images/videos in the post (Phase 1: count only, no rendering)
	MediaCount int `json:"mediaCount"`

	// HasMedia indicates if the post contains images or videos (Phase 1: indicator only)
	HasMedia bool `json:"hasMedia"`

	// Unavailable indicates the post could not be resolved (deleted, private, blocked, etc.)
	Unavailable bool `json:"unavailable"`
}

// Author represents a Bluesky post author's identity.
type Author struct {
	// DID is the decentralized identifier of the author
	DID string `json:"did"`

	// Handle is the user's handle (e.g., user.bsky.social)
	Handle string `json:"handle"`

	// DisplayName is the user's chosen display name (may be empty)
	DisplayName string `json:"displayName,omitempty"`

	// Avatar is the URL to the user's avatar image (may be empty)
	Avatar string `json:"avatar,omitempty"`
}
