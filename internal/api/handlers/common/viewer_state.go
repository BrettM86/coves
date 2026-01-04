package common

import (
	"Coves/internal/api/middleware"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/votes"
	"context"
	"log"
	"net/http"
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
//   - ctx: Request context for PDS calls
//   - r: HTTP request (used to extract OAuth session)
//   - voteService: Vote service for cache lookup (may be nil)
//   - feedPosts: Posts to enrich with viewer state (must implement FeedPostProvider)
//
// The function logs but does not fail on errors - viewer state is optional enrichment.
func PopulateViewerVoteState[T FeedPostProvider](
	ctx context.Context,
	r *http.Request,
	voteService votes.Service,
	feedPosts []T,
) {
	if voteService == nil {
		return
	}

	session := middleware.GetOAuthSession(r)
	if session == nil {
		return
	}

	userDID := middleware.GetUserDID(r)

	// Ensure vote cache is populated from PDS
	if err := voteService.EnsureCachePopulated(ctx, session); err != nil {
		log.Printf("Warning: failed to populate vote cache: %v", err)
		return
	}

	// Collect post URIs to batch lookup
	postURIs := make([]string, 0, len(feedPosts))
	for _, feedPost := range feedPosts {
		if post := feedPost.GetPost(); post != nil {
			postURIs = append(postURIs, post.URI)
		}
	}

	// Get viewer votes for all posts
	viewerVotes := voteService.GetViewerVotesForSubjects(userDID, postURIs)

	// Populate viewer state on each post
	for _, feedPost := range feedPosts {
		if post := feedPost.GetPost(); post != nil {
			if vote, exists := viewerVotes[post.URI]; exists {
				post.Viewer = &posts.ViewerState{
					Vote:    &vote.Direction,
					VoteURI: &vote.URI,
				}
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
		c.Viewer = &communities.CommunityViewerState{
			Subscribed: &isSubscribed,
		}
	}
}
