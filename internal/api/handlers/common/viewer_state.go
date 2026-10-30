package common

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
)

// FeedPostProvider is implemented by any feed post wrapper that contains a PostView.
// This allows the helper to work with different feed post types (discover, timeline, communityFeed).
type FeedPostProvider interface {
	GetPost() *posts.PostView
}

// PopulateViewerVoteState enriches feed posts with the authenticated user's vote state.
// This is a no-op if voteService is nil or the request is unauthenticated.
//
// Parameters:
//   - ctx: Request context for the vote lookup
//   - r: HTTP request (used to identify the viewer)
//   - voteService: Vote service that resolves the viewer's votes (may be nil)
//   - feedPosts: Posts to enrich with viewer state (must implement FeedPostProvider)
//
// Every post is resolved in one ViewerVotes call. A post the viewer voted on
// gets vote and voteUri; one without a vote gets neither. Vote fields are
// merged into an existing viewer state; other viewer fields are kept. If the
// lookup fails, the error is logged and no post gets vote state: a failed
// lookup must not render as "not voted".
func PopulateViewerVoteState[T FeedPostProvider](
	ctx context.Context,
	r *http.Request,
	voteService votes.Service,
	feedPosts []T,
) {
	if voteService == nil {
		return
	}

	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		return
	}

	subjectURIs := make([]string, 0, len(feedPosts))
	for _, feedPost := range feedPosts {
		if post := feedPost.GetPost(); post != nil {
			subjectURIs = append(subjectURIs, post.URI)
		}
	}
	if len(subjectURIs) == 0 {
		return
	}

	viewerVotes, err := voteService.ViewerVotes(ctx, userDID, subjectURIs)
	if err != nil {
		log.Printf("Warning: failed to resolve viewer votes for user %s (%d posts): %v",
			userDID, len(subjectURIs), err)
		return
	}

	// Populate viewer state on each post
	for _, feedPost := range feedPosts {
		if post := feedPost.GetPost(); post != nil {
			if vote, exists := viewerVotes[post.URI]; exists {
				if post.Viewer == nil {
					post.Viewer = &posts.ViewerState{}
				}
				post.Viewer.Vote = &vote.Direction
				post.Viewer.VoteURI = &vote.URI
			}
		}
	}
}

// PopulateCommunityViewerState enriches communities with the authenticated user's subscription state.
// This is a no-op if the request is unauthenticated.
func PopulateCommunityViewerState(
	ctx context.Context,
	r *http.Request,
	repo communities.Repository,
	communityList []*communities.Community,
) {
	if repo == nil || len(communityList) == 0 {
		return
	}

	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		return // Not authenticated, leave viewer state nil
	}

	// Collect community DIDs
	communityDIDs := make([]string, len(communityList))
	for i, c := range communityList {
		communityDIDs[i] = c.DID
	}

	// Batch query subscriptions
	subscribed, err := repo.GetSubscribedCommunityDIDs(ctx, userDID, communityDIDs)
	if err != nil {
		log.Printf("Warning: failed to get subscription state for user %s (%d communities): %v",
			userDID, len(communityDIDs), err)
		return
	}

	// Populate viewer state on each community
	for _, c := range communityList {
		isSubscribed := subscribed[c.DID]
		if c.Viewer == nil {
			c.Viewer = &communities.CommunityViewerState{}
		}
		c.Viewer.Subscribed = &isSubscribed
	}
}

// PopulateCommunityBlockState enriches one community with the authenticated
// viewer's aggregate-feed block preference. The get endpoint calls this for a
// single community; list endpoints deliberately do not fan out into one query
// per row and clients that need the full list use getBlockedCommunities.
//
// Unlike cosmetic viewer-state enrichment, a lookup failure is fatal for the
// single-community endpoint. Omitting blocked would make clients interpret an
// unknown value as false and offer a second block write for an already-blocked
// community.
func PopulateCommunityBlockState(
	ctx context.Context,
	r *http.Request,
	repo communities.Repository,
	community *communities.Community,
) error {
	if repo == nil || community == nil {
		return nil
	}

	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		return nil
	}

	blocked, err := repo.IsBlocked(ctx, userDID, community.DID)
	if err != nil {
		return fmt.Errorf("get community block state: %w", err)
	}

	if community.Viewer == nil {
		community.Viewer = &communities.CommunityViewerState{}
	}
	community.Viewer.Blocked = &blocked
	return nil
}
