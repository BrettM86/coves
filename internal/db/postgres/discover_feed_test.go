//go:build integration

// The discover feed's new and top sorts (getDiscover is an alias of getFeed
// scope all) are decided by SQL: the scope-all query of the new and top
// generators and the cursor they emit. These tests therefore run against a real
// database, served as production registers the endpoint (RegisterFeedRoutes
// over the production built-ins, postgres.FeedAliasServer) and driven in
// process. Discover Hot's ranking tests stay in internal/core/discover. The
// file is in the external test package because it seeds rows with
// Coves/tests/fixtures.
package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockVoteService is an in-memory votes.Service holding pre-seeded viewer
// votes.
//
// Production resolves viewer votes from the AppView index overlaid by recent
// writes. These tests are about what the feed does with the resolved votes —
// whether the right vote lands on the right post, and whether an
// unauthenticated request gets none at all — so the votes are seeded directly.
type mockVoteService struct {
	viewerVotes map[string]votes.ViewerVote // viewerDID:subjectURI -> vote
}

func newMockVoteService() *mockVoteService {
	return &mockVoteService{
		viewerVotes: make(map[string]votes.ViewerVote),
	}
}

func (m *mockVoteService) AddVote(viewerDID, subjectURI, direction, voteURI string) {
	key := viewerDID + ":" + subjectURI
	m.viewerVotes[key] = votes.ViewerVote{
		Direction: direction,
		URI:       voteURI,
	}
}

func (m *mockVoteService) CreateVote(_ context.Context, _ *oauthlib.ClientSessionData, _ votes.CreateVoteRequest) (*votes.CreateVoteResponse, error) {
	return &votes.CreateVoteResponse{}, nil
}

func (m *mockVoteService) DeleteVote(_ context.Context, _ *oauthlib.ClientSessionData, _ votes.DeleteVoteRequest) error {
	return nil
}

func (m *mockVoteService) ViewerVotes(_ context.Context, viewerDID string, subjectURIs []string) (map[string]votes.ViewerVote, error) {
	result := make(map[string]votes.ViewerVote)
	for _, uri := range subjectURIs {
		key := viewerDID + ":" + uri
		if vote, exists := m.viewerVotes[key]; exists {
			result[uri] = vote
		}
	}
	return result, nil
}

// The real vote service is wired wherever a case does not exercise votes, and
// no Bluesky service anywhere: no case here has a bridged embed.

// TestGetDiscover_ShowsAllCommunities proves the discover feed is
// community-agnostic: unlike the timeline, it draws on every indexed community
// rather than the viewer's subscriptions.
func TestGetDiscover_ShowsAllCommunities(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	server := postgres.NewFeedAliasServer(db, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	community1DID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	community2DID, err := fixtures.Community(ctx, db, fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err)

	community3DID, err := fixtures.Community(ctx, db, fmt.Sprintf("cooking-%s", testID), fmt.Sprintf("charlie-%s.test", testID))
	require.NoError(t, err)

	post1URI := fixtures.Post(t, db, community1DID, "did:plc:alice", "Gaming post", 50, time.Now().Add(-1*time.Hour))
	post2URI := fixtures.Post(t, db, community2DID, "did:plc:bob", "Tech post", 30, time.Now().Add(-2*time.Hour))
	post3URI := fixtures.Post(t, db, community3DID, "did:plc:charlie", "Cooking post", 100, time.Now().Add(-30*time.Minute))

	// No authentication: discover is the logged-out landing feed.
	rec := server.Serve(postgres.GetDiscoverPath, "sort=new&limit=50", "")

	assert.Equal(t, http.StatusOK, rec.Code)

	var response postgres.FeedAliasPage
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	uris := make(map[string]bool)
	for _, post := range response.Feed {
		uris[post.Post.URI] = true
	}
	assert.True(t, uris[post1URI], "Should contain gaming post")
	assert.True(t, uris[post2URI], "Should contain tech post")
	assert.True(t, uris[post3URI], "Should contain cooking post")

	// Under "new", the three posts must come back in reverse chronological
	// order regardless of which community they belong to.
	post3Index, post1Index, post2Index := -1, -1, -1
	for i, post := range response.Feed {
		switch post.Post.URI {
		case post3URI:
			post3Index = i
		case post1URI:
			post1Index = i
		case post2URI:
			post2Index = i
		}
	}
	require.NotEqual(t, -1, post3Index)
	require.NotEqual(t, -1, post1Index)
	require.NotEqual(t, -1, post2Index)
	assert.Less(t, post3Index, post1Index, "Newest post (30min ago) should appear before 1hr old post")
	assert.Less(t, post1Index, post2Index, "1hr old post should appear before 2hr old post")
}

// TestGetDiscover_NoAuthRequired pins the endpoint as anonymous-readable. It is
// the only feed a signed-out visitor can see, so a regression that started
// demanding a session would empty the front page.
func TestGetDiscover_NoAuthRequired(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	server := postgres.NewFeedAliasServer(db, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("public-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	postURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Public post", 10, time.Now())

	rec := server.Serve(postgres.GetDiscoverPath, "sort=new&limit=50", "")

	assert.Equal(t, http.StatusOK, rec.Code, "Discover should work without authentication")

	var response postgres.FeedAliasPage
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	found := false
	for _, post := range response.Feed {
		if post.Post.URI == postURI {
			found = true
			break
		}
	}
	assert.True(t, found, "Should show post even without authentication")
}

// TestGetDiscover_Pagination walks the chronological cursor: pages must be full
// and must not repeat a post already served.
func TestGetDiscover_Pagination(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	server := postgres.NewFeedAliasServer(db, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("test-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		fixtures.Post(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), 10-i, time.Now().Add(-time.Duration(i)*time.Hour))
	}

	rec := server.Serve(postgres.GetDiscoverPath, "sort=new&limit=2", "")

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 postgres.FeedAliasPage
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	require.Len(t, page1.Feed, 2, "First page should have 2 posts")
	require.NotNil(t, page1.Cursor, "Should have cursor for next page")

	rec = server.Serve(postgres.GetDiscoverPath, fmt.Sprintf("sort=new&limit=2&cursor=%s", *page1.Cursor), "")

	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 postgres.FeedAliasPage
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	require.Len(t, page2.Feed, 2, "Second page should have 2 posts")

	assert.NotEqual(t, page1.Feed[0].Post.URI, page2.Feed[0].Post.URI, "Pages should not overlap")
}

// TestGetDiscover_LimitValidation checks the endpoint rejects an over-large page
// size rather than letting it reach the query: an unbounded limit is the
// cheapest denial-of-service against a public, unauthenticated endpoint.
func TestGetDiscover_LimitValidation(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	server := postgres.NewFeedAliasServer(db, nil)

	t.Run("Limit exceeds maximum", func(t *testing.T) {
		rec := server.Serve(postgres.GetDiscoverPath, "sort=new&limit=100", "")

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errorResp map[string]string
		err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errorResp["error"])
		assert.Contains(t, errorResp["message"], "limit")
	})
}

// TestGetDiscover_ViewerVoteState proves each post carries the viewer's OWN
// vote and only that vote. Cross-wiring here would show a reader somebody
// else's upvote, or let a re-tap toggle a vote the viewer never cast.
func TestGetDiscover_ViewerVoteState(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("votes-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	post1URI := fixtures.Post(t, db, communityDID, "did:plc:author1", "Post with upvote", 10, time.Now().Add(-1*time.Hour))
	post2URI := fixtures.Post(t, db, communityDID, "did:plc:author2", "Post with downvote", 5, time.Now().Add(-2*time.Hour))
	_ = fixtures.Post(t, db, communityDID, "did:plc:author3", "Post without vote", 3, time.Now().Add(-3*time.Hour))

	viewerDID := "did:plc:viewer123"
	mockVotes := newMockVoteService()
	mockVotes.AddVote(viewerDID, post1URI, "up", "at://"+viewerDID+"/social.coves.vote/vote1")
	mockVotes.AddVote(viewerDID, post2URI, "down", "at://"+viewerDID+"/social.coves.vote/vote2")

	server := postgres.NewFeedAliasServer(db, mockVotes)

	// The viewer is signed in by bearer token, so OptionalAuth sets the DID and
	// session a real request carries; the vote lookup is keyed by the DID.
	rec := server.Serve(postgres.GetDiscoverPath, "sort=new&limit=50", viewerDID)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response postgres.FeedAliasPage
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	var foundPost1, foundPost2, foundPost3 bool
	for _, feedPost := range response.Feed {
		switch feedPost.Post.URI {
		case post1URI:
			foundPost1 = true
			require.NotNil(t, feedPost.Post.Viewer, "Post1 should have viewer state")
			require.NotNil(t, feedPost.Post.Viewer.Vote, "Post1 should have vote direction")
			assert.Equal(t, "up", *feedPost.Post.Viewer.Vote, "Post1 should show upvote")
			require.NotNil(t, feedPost.Post.Viewer.VoteURI, "Post1 should have vote URI")
			assert.Contains(t, *feedPost.Post.Viewer.VoteURI, "vote1", "Post1 should have correct vote URI")

		case post2URI:
			foundPost2 = true
			require.NotNil(t, feedPost.Post.Viewer, "Post2 should have viewer state")
			require.NotNil(t, feedPost.Post.Viewer.Vote, "Post2 should have vote direction")
			assert.Equal(t, "down", *feedPost.Post.Viewer.Vote, "Post2 should show downvote")
			require.NotNil(t, feedPost.Post.Viewer.VoteURI, "Post2 should have vote URI")

		default:
			// The third post must come back with no vote of its own.
			if feedPost.Post.Viewer != nil && feedPost.Post.Viewer.Vote != nil {
				continue
			}
			foundPost3 = true
		}
	}

	assert.True(t, foundPost1, "Should find post1 with upvote")
	assert.True(t, foundPost2, "Should find post2 with downvote")
	assert.True(t, foundPost3, "Should find post3 without vote")
}

// TestGetDiscover_NoViewerStateWithoutAuth proves viewer state is withheld from
// anonymous requests even when the vote service holds a vote for that post. The
// vote belongs to a different account, and leaking it would tell
// every visitor how a named user voted.
func TestGetDiscover_NoViewerStateWithoutAuth(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("noauth-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	postURI := fixtures.Post(t, db, communityDID, "did:plc:author", "Some post", 10, time.Now())

	mockVotes := newMockVoteService()
	mockVotes.AddVote("did:plc:someuser", postURI, "up", "at://did:plc:someuser/social.coves.vote/vote1")

	server := postgres.NewFeedAliasServer(db, mockVotes)

	rec := server.Serve(postgres.GetDiscoverPath, "sort=new&limit=50", "")

	assert.Equal(t, http.StatusOK, rec.Code)

	var response postgres.FeedAliasPage
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	for _, feedPost := range response.Feed {
		if feedPost.Post.URI == postURI {
			assert.Nil(t, feedPost.Post.Viewer, "Unauthenticated request should not have viewer state")
			return
		}
	}
	t.Fatal("Test post not found in response")
}
