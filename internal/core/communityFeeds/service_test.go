// Shared fakes for the community feed service tests (SearchPosts). The
// repository fake appends each request to a call log and the tests assert on
// the log; the community service fake does the same for the identifier it was
// asked to resolve.
//
// This is an external test package and untagged: no database, no sockets,
// nothing out of process.
package communityFeeds_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
)

// recordingFeedRepo logs every request it is handed and answers with whatever
// the test seeded.
type recordingFeedRepo struct {
	// The embedded nil Repository satisfies the rest of the interface and
	// panics if reached: the search service uses SearchPosts only.
	communityFeeds.Repository

	mu sync.Mutex

	searchFeed   []*communityFeeds.FeedViewPost
	searchCursor *string
	searchErr    error

	searchRequests []communityFeeds.SearchPostsRequest
}

func (r *recordingFeedRepo) SearchPosts(_ context.Context, req communityFeeds.SearchPostsRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
	r.mu.Lock()
	r.searchRequests = append(r.searchRequests, req)
	r.mu.Unlock()
	if r.searchErr != nil {
		return nil, nil, r.searchErr
	}
	return r.searchFeed, r.searchCursor, nil
}

// onlySearch returns the single search request the repository received, or
// fails the test naming what it actually saw.
func (r *recordingFeedRepo) onlySearch(t *testing.T) communityFeeds.SearchPostsRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.searchRequests) != 1 {
		t.Fatalf("expected the repository to be asked to search exactly once, got %d call(s): %+v",
			len(r.searchRequests), r.searchRequests)
	}
	return r.searchRequests[0]
}

func (r *recordingFeedRepo) searchCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.searchRequests)
}

// recordingCommunityService is a communities.Service that answers only
// ResolveCommunityIdentifier. The embedded interface is nil, so any other call
// panics — the feed service is supposed to use exactly one method of it, and
// this is what keeps that true.
type recordingCommunityService struct {
	communities.Service

	mu sync.Mutex

	resolved string
	err      error

	asked []string
}

func (c *recordingCommunityService) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	c.mu.Lock()
	c.asked = append(c.asked, identifier)
	c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	return c.resolved, nil
}

func (c *recordingCommunityService) askedFor() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// theCommunityDID is what the identifier resolves to in the tests that get that
// far. It is deliberately unlike any identifier passed in, so "the repository
// received the resolved DID" cannot accidentally hold.
const theCommunityDID = "did:plc:resolvedcommunity000000"

// errRepositoryUnavailable stands in for anything the datastore can go wrong
// with. It is not one of the domain's typed errors, so "propagated" can be told
// apart from "mapped".
var errRepositoryUnavailable = errors.New("connection pool exhausted")

// newFeedService wires the service the way cmd/server does, over fakes.
func newFeedService(repo *recordingFeedRepo, communityService *recordingCommunityService) communityFeeds.Service {
	return communityFeeds.NewCommunityFeedService(repo, communityService)
}

// TestFeedViewPostExposesItsPost covers the accessor the searchPosts handler
// uses to hang viewer vote state on each row. A nil return would render every
// post as unvoted for a user who has voted.
func TestFeedViewPostExposesItsPost(t *testing.T) {
	t.Parallel()

	post := &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}
	item := &communityFeeds.FeedViewPost{Post: post}

	if item.GetPost() != post {
		t.Errorf("GetPost returned %v, want the wrapped post", item.GetPost())
	}
}
