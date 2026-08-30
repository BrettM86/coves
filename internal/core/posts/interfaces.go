package posts

import (
	"context"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// Service constructor accepts optional blobs.Service and unfurl.Service for embed enhancement.
// When unfurlService is provided, external embeds will be automatically enriched with metadata.
// When blobService is provided, thumbnails from unfurled URLs will be uploaded as blobs.

// Service defines the business logic interface for posts
// Coordinates between Repository, community service, and PDS
type Service interface {
	// CreatePost writes a new post into the AUTHOR's repository and, when this
	// AppView hosts the community, settles the community's admission
	// synchronously (docs/PRD_AUTHOR_OWNED_POSTS.md §4.2).
	//
	// Flow: Validate -> Admission -> Write postv2 to the author's repo at the
	// deterministic rkey -> seed the admission row -> local fast-path acceptance
	// -> return URI/CID/status. AppView indexing still happens asynchronously
	// via the Jetstream consumer; the fast path only means the community's
	// answer does not wait for it.
	//
	// THE SESSION IS THE AUTHOR'S OWN, and it is an argument rather than a
	// context value because it is the credential the record gets signed under.
	// It may be nil for a non-interactive author — an aggregator posting on its
	// stored tokens — in which case the service resolves those and answers
	// ErrNoAuthorCredentials if there are none.
	CreatePost(ctx context.Context, session *oauth.ClientSessionData, req CreatePostRequest) (*CreatePostResponse, error)

	// UpdatePost edits an existing post in place, in the author's repository.
	//
	// The record's community and createdAt are PRESERVED from the standing
	// record rather than taken from the request: the first is immutable by
	// lexicon, and the second is what every feed orders by, so re-stamping it
	// would jump an edited post back to the top of every sort.
	//
	// The edit is guarded by the standing record's CID, so an edit racing
	// another edit is ErrConcurrentModification rather than a silent overwrite.
	// The community's ADMISSION LEDGER is untouched: an edit is not a
	// submission, so it consumes no quota and cannot be refused as a duplicate
	// of the post it is editing. Whether the edited content is still acceptable
	// is the acceptance engine's question, asked when the edit reaches the
	// firehose (§5.5).
	UpdatePost(ctx context.Context, session *oauth.ClientSessionData, req UpdatePostRequest) (*UpdatePostResponse, error)

	// GetAuthorPosts retrieves posts authored by a specific user for their profile page
	// Supports filtering by post type (with/without replies, media only) and community
	// Returns paginated feed with cursor
	GetAuthorPosts(ctx context.Context, req GetAuthorPostsRequest) (*GetAuthorPostsResponse, error)

	// GetPosts batch-fetches post views by AT-URI for feed hydration and permalink
	// (cold-load) rendering. Implements social.coves.community.post.get.
	// URIs must be canonical DID-based AT-URIs; malformed or handle-based URIs are
	// rejected with a validation error (handles are mutable and would break on rename).
	// Results are returned in the same order as req.URIs; valid URIs whose post is
	// missing or deleted come back as NotFoundPost markers. When req.ViewerDID is set
	// and a BlockChecker is wired, posts authored by users the viewer has blocked are
	// returned as BlockedPost markers instead of full views (matching feed/timeline).
	GetPosts(ctx context.Context, req GetPostsRequest) ([]*PostResult, error)

	// DeletePost removes a post record from the repository that holds it.
	// SECURITY: Only the post author can delete their own posts.
	//
	// BOTH post collections are supported, and they authorize differently
	// because they live in different repos. A postv2 URI names the AUTHOR's
	// repo, so authorization is the URI's authority against the session DID —
	// a local check, decided before anything is fetched. A deprecated
	// community.post URI names the COMMUNITY's repo, where the delete goes out
	// on the community's credentials, so the record's `author` field has to be
	// read back and compared. The second path exists until task 8's
	// re-materialization retires the collection.
	DeletePost(ctx context.Context, session *oauth.ClientSessionData, req DeletePostRequest) error

	// Future methods (Beta):
	// ListCommunityPosts(ctx context.Context, communityDID string, limit, offset int) ([]*Post, error)
}

// Repository defines the data access interface for posts
// Used by Jetstream consumer to index posts from firehose
type Repository interface {
	// Create inserts a new post into the AppView database
	// Called by Jetstream consumer after post is created on PDS
	Create(ctx context.Context, post *Post) error

	// ────────────────────────────────────────────────────────────────────────
	// DANGER — GetRawIndexedRow IS NOT A DISPLAY READ. It applies NEITHER the
	// admission visibility predicate NOR `deleted_at IS NULL`, so it returns
	// the full title and content of a post that is pending, rejected, removed
	// by a moderator, or soft-deleted by its own author.
	//
	// It is named for what it is — the raw indexed row — precisely so that it
	// cannot be reached for by accident, because misuse is SILENT: it selects
	// bare columns with no join a compiler could miss and no error a test would
	// see, just a hidden post's content on the wire.
	//
	// If you are hydrating anything a reader will SEE, use one of:
	//   • GetViewsByURIs(ctx, uris, viewerDID)  — batch, hydrated, gated
	//   • VisibleHeaderView(ctx, uri, viewerDID) — single post, hydrated, gated
	//     (on the concrete postgres repo; the comment service requires it
	//     through comments.PostReader, so the binding is a compile error to omit)
	//
	// Legitimate callers are the ones that must see a row REGARDLESS of who may
	// look at it: the admission decider (it decides visibility, so it cannot
	// depend on it) and the post.get removal path (a tombstone is emitted for a
	// post the predicate has already hidden). Both check what they need
	// themselves — the removal path re-checks deleted_at explicitly.
	// ────────────────────────────────────────────────────────────────────────
	GetRawIndexedRow(ctx context.Context, uri string) (*Post, error)

	// GetRawIndexedRowsByURIs is the batched GetRawIndexedRow: same ungated raw
	// rows, one round trip. THE SAME DANGER APPLIES — read the banner above
	// before calling it. URIs with no indexed row are absent from the map.
	//
	// It exists because the post.get removal path needs the community and
	// soft-delete state of every absent URI in a caller-supplied batch, and
	// looping GetRawIndexedRow there put an N+1 on a public endpoint whose URI
	// list the caller controls.
	GetRawIndexedRowsByURIs(ctx context.Context, uris []string) (map[string]*Post, error)

	// GetViewsByURIs retrieves full post views (with author + community joins) for a
	// set of canonical DID-based AT-URIs. Returns a map keyed by URI; missing or
	// soft-deleted posts are simply absent from the map. Backs social.coves.community.post.get.
	//
	// viewerDID scopes the admission visibility gate and is REQUIRED: pass the
	// authenticated viewer's DID, or "" for an anonymous read. "" is the
	// fail-closed value — accepted content only — so an implementation that
	// ignores this argument narrows nothing and widens nothing for the public,
	// while an author passed here reaches their own pending/rejected/removed
	// posts, the same author-self-view contract actor.getPosts and the feeds
	// honor (PRD §6.2).
	GetViewsByURIs(ctx context.Context, uris []string, viewerDID string) (map[string]*PostView, error)

	// GetByAuthor retrieves posts authored by a specific user
	// Supports filtering by post type and community
	// Returns posts, cursor for pagination, and error
	GetByAuthor(ctx context.Context, req GetAuthorPostsRequest) ([]*PostView, *string, error)

	// SoftDelete marks a post as deleted in the AppView database
	// Called by the Jetstream postv2 consumer and postService.deleteCommunityPost
	// Idempotent: Returns success if post already deleted
	SoftDelete(ctx context.Context, uri string) error

	// Future methods (Beta):
	// Update(ctx context.Context, post *Post) error
	// List(ctx context.Context, communityDID string, limit, offset int) ([]*Post, int, error)
}

// BlockChecker reports which of the given author DIDs the viewer (blockerDID) has
// blocked. It is the narrow slice of the user-block store that GetPosts needs to enforce
// viewer block visibility, and is satisfied by the userblocks repository's AreBlocked.
// Optional: when nil, block enforcement is skipped (e.g. minimal setups and unit tests).
type BlockChecker interface {
	// AreBlocked returns a map of blockedDID -> true for each DID in blockedDIDs that
	// blockerDID has blocked. DIDs that are not blocked are absent from the map.
	AreBlocked(ctx context.Context, blockerDID string, blockedDIDs []string) (map[string]bool, error)
}
