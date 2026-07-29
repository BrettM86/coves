//go:build integration

package integration

import (
	"Coves/internal/api/handlers/discover"
	"Coves/internal/api/middleware"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	discoverCore "Coves/internal/core/discover"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockVoteService implements votes.Service for testing viewer vote state
type mockVoteService struct {
	cachedVotes map[string]*votes.CachedVote // userDID:subjectURI -> vote
}

func newMockVoteService() *mockVoteService {
	return &mockVoteService{
		cachedVotes: make(map[string]*votes.CachedVote),
	}
}

func (m *mockVoteService) AddVote(userDID, subjectURI, direction, voteURI string) {
	key := userDID + ":" + subjectURI
	m.cachedVotes[key] = &votes.CachedVote{
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

func (m *mockVoteService) EnsureCachePopulated(_ context.Context, _ *oauthlib.ClientSessionData) error {
	return nil // Mock always succeeds - votes pre-populated via AddVote
}

func (m *mockVoteService) GetViewerVote(userDID, subjectURI string) *votes.CachedVote {
	key := userDID + ":" + subjectURI
	return m.cachedVotes[key]
}

func (m *mockVoteService) GetViewerVotesForSubjects(userDID string, subjectURIs []string) map[string]*votes.CachedVote {
	result := make(map[string]*votes.CachedVote)
	for _, uri := range subjectURIs {
		key := userDID + ":" + uri
		if vote, exists := m.cachedVotes[key]; exists {
			result[uri] = vote
		}
	}
	return result
}

// TestGetDiscover_ShowsAllCommunities tests discover feed shows posts from ALL communities
func TestGetDiscover_ShowsAllCommunities(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil) // nil vote/bluesky services - tests don't need them

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create three communities
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err)

	community3DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("cooking-%d", testID), fmt.Sprintf("charlie-%d.test", testID))
	require.NoError(t, err)

	// Create posts in all three communities
	post1URI := createTestPost(t, db, community1DID, "did:plc:alice", "Gaming post", 50, time.Now().Add(-1*time.Hour))
	post2URI := createTestPost(t, db, community2DID, "did:plc:bob", "Tech post", 30, time.Now().Add(-2*time.Hour))
	post3URI := createTestPost(t, db, community3DID, "did:plc:charlie", "Cooking post", 100, time.Now().Add(-30*time.Minute))

	// Request discover feed (no auth required!)
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify all our posts are present (may include posts from other tests)
	uris := make(map[string]bool)
	for _, post := range response.Feed {
		uris[post.Post.URI] = true
	}
	assert.True(t, uris[post1URI], "Should contain gaming post")
	assert.True(t, uris[post2URI], "Should contain tech post")
	assert.True(t, uris[post3URI], "Should contain cooking post")

	// Verify newest post appears before older posts in the feed
	var post3Index, post1Index, post2Index int = -1, -1, -1
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
	if post3Index >= 0 && post1Index >= 0 && post2Index >= 0 {
		assert.Less(t, post3Index, post1Index, "Newest post (30min ago) should appear before 1hr old post")
		assert.Less(t, post1Index, post2Index, "1hr old post should appear before 2hr old post")
	}
}

// TestGetDiscover_NoAuthRequired tests discover feed works without authentication
func TestGetDiscover_NoAuthRequired(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil) // nil vote/bluesky services - tests don't need them

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create community and post
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("public-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	postURI := createTestPost(t, db, communityDID, "did:plc:alice", "Public post", 10, time.Now())

	// Request discover WITHOUT any authentication
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	// Note: No auth context set!
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Should succeed without auth
	assert.Equal(t, http.StatusOK, rec.Code, "Discover should work without authentication")

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify our post is present
	found := false
	for _, post := range response.Feed {
		if post.Post.URI == postURI {
			found = true
			break
		}
	}
	assert.True(t, found, "Should show post even without authentication")
}

// TestGetDiscover_HotSort tests hot sorting across all communities
func TestGetDiscover_HotSort(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil) // nil vote/bluesky services

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create communities
	community1DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("gaming-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	community2DID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("tech-%d", testID), fmt.Sprintf("bob-%d.test", testID))
	require.NoError(t, err)

	// Create posts with different scores/ages
	post1URI := createTestPost(t, db, community1DID, "did:plc:alice", "Recent trending", 50, time.Now().Add(-1*time.Hour))
	post2URI := createTestPost(t, db, community2DID, "did:plc:bob", "Old popular", 100, time.Now().Add(-24*time.Hour))
	post3URI := createTestPost(t, db, community1DID, "did:plc:charlie", "Brand new", 5, time.Now().Add(-10*time.Minute))

	// Request hot discover
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify all our posts are present
	uris := make(map[string]bool)
	for _, post := range response.Feed {
		uris[post.Post.URI] = true
	}
	assert.True(t, uris[post1URI], "Should contain recent trending post")
	assert.True(t, uris[post2URI], "Should contain old popular post")
	assert.True(t, uris[post3URI], "Should contain brand new post")
}

// TestGetDiscover_HotSort_LogDampedRanking pins the tuning of the log-damped
// hot rank: (SIGN(score)*LN(ABS(score)+1) + 1) / (age_hours + 2)^1.5
//
// The scenario mirrors the federation problem that motivated the formula: a
// bridged Lemmy post with a huge vote count from a much larger population must
// not bury fresh organic posts for days. At the same time, votes still matter —
// a day-old genuinely popular post should outrank a six-hour-old post nobody
// voted on.
func TestGetDiscover_HotSort_LogDampedRanking(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("ranking-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	// A bridged Lemmy-style blowout: 782 votes, a day old (rank ≈ 0.058)
	blowoutURI := createTestPost(t, db, communityDID, "did:plc:lemmy", "Federated blowout", 782, time.Now().Add(-24*time.Hour))
	// A brand-new organic post with no votes yet (rank ≈ 0.33)
	freshURI := createTestPost(t, db, communityDID, "did:plc:alice", "Fresh organic post", 0, time.Now().Add(-5*time.Minute))
	// A six-hour-old post with no votes (rank ≈ 0.044)
	sixHourURI := createTestPost(t, db, communityDID, "did:plc:bob", "Six hour old post", 0, time.Now().Add(-6*time.Hour))
	// A brand-new but downvoted post (rank ≈ -0.26)
	downvotedURI := createTestPost(t, db, communityDID, "did:plc:carol", "Downvoted post", -5, time.Now().Add(-5*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	position := make(map[string]int)
	for i, post := range response.Feed {
		position[post.Post.URI] = i
	}
	for _, uri := range []string{blowoutURI, freshURI, sixHourURI, downvotedURI} {
		require.Contains(t, position, uri, "All posts should appear in the hot feed")
	}

	assert.Less(t, position[freshURI], position[blowoutURI],
		"Fresh 0-vote post should outrank a day-old vote blowout")
	assert.Less(t, position[blowoutURI], position[sixHourURI],
		"Day-old high-vote post should still outrank a 6h-old 0-vote post")
	assert.Less(t, position[sixHourURI], position[downvotedURI],
		"Post at score -5 (negative rank) should rank below these positive-rank posts")
}

// TestGetDiscover_HotSort_PaginationCoversNegativeScores paginates the exact
// LogDampedRanking scenario one post at a time. This exercises the cursor-side
// hot-rank expression (rebuilt with a pinned timestamp) against the live ORDER
// BY across the positive-to-negative rank boundary: every post must appear
// exactly once, in rank order, with no skips or duplicates. A divergence
// between the live and cursor formulas fails this test.
func TestGetDiscover_HotSort_PaginationCoversNegativeScores(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("hotpage-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	blowoutURI := createTestPost(t, db, communityDID, "did:plc:lemmy", "Federated blowout", 782, time.Now().Add(-24*time.Hour))
	freshURI := createTestPost(t, db, communityDID, "did:plc:alice", "Fresh organic post", 0, time.Now().Add(-5*time.Minute))
	sixHourURI := createTestPost(t, db, communityDID, "did:plc:bob", "Six hour old post", 0, time.Now().Add(-6*time.Hour))
	downvotedURI := createTestPost(t, db, communityDID, "did:plc:carol", "Downvoted post", -5, time.Now().Add(-5*time.Minute))

	expectedOrder := []string{freshURI, blowoutURI, sixHourURI, downvotedURI}

	// Paginate to exhaustion, one post per page (setupTestDB wiped the posts
	// table, so the feed contains exactly these four posts)
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		url := "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=1"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		handler.HandleGetDiscover(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "page %d should succeed", page)

		var response discoverCore.DiscoverResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))

		for _, post := range response.Feed {
			seen = append(seen, post.Post.URI)
		}
		if response.Cursor == nil {
			break
		}
		cursor = *response.Cursor
	}

	assert.Equal(t, expectedOrder, seen,
		"Pagination should return every post exactly once, in hot-rank order")
}

// TestGetDiscover_HotSort_FutureDatedPost guards the feed against records
// asserting a future created_at (hostile or clock-skewed federated repos).
// The ingestion path clamps these to now, but this test inserts one directly
// to pin the SQL-level defence in hotRankSQL: GREATEST(age, 0) means a
// future-dated post ranks like a brand-new 0-vote post — it must not error
// the query (negative POWER base) and must not outrank a post with real votes.
func TestGetDiscover_HotSort_FutureDatedPost(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("future-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	// A 0-vote post claiming to be from 10 days in the future
	futureURI := createTestPost(t, db, communityDID, "did:plc:mallory", "Future dated post", 0, time.Now().Add(240*time.Hour))
	// A recent post with modest genuine engagement (rank ≈ 0.95 vs the clamped ≈ 0.35)
	votedURI := createTestPost(t, db, communityDID, "did:plc:alice", "Recent voted post", 50, time.Now().Add(-1*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Before the GREATEST age clamp this query 500'd with "a negative number
	// raised to a non-integer power yields a complex result"
	require.Equal(t, http.StatusOK, rec.Code, "Future-dated post must not break the hot feed")

	var response discoverCore.DiscoverResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))

	position := make(map[string]int)
	for i, post := range response.Feed {
		position[post.Post.URI] = i
	}
	require.Contains(t, position, futureURI)
	require.Contains(t, position, votedURI)

	assert.Less(t, position[votedURI], position[futureURI],
		"Future-dated 0-vote post should rank like a fresh post, not above genuinely engaged posts")
}

// TestGetDiscover_Pagination tests cursor-based pagination
func TestGetDiscover_Pagination(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create community
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("test-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	// Create 5 posts
	for i := 0; i < 5; i++ {
		createTestPost(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), 10-i, time.Now().Add(-time.Duration(i)*time.Hour))
	}

	// First page: limit 2
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=2", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	assert.Len(t, page1.Feed, 2, "First page should have 2 posts")
	assert.NotNil(t, page1.Cursor, "Should have cursor for next page")

	// Second page: use cursor
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getDiscover?sort=new&limit=2&cursor=%s", *page1.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	assert.Len(t, page2.Feed, 2, "Second page should have 2 posts")

	// Verify no overlap
	assert.NotEqual(t, page1.Feed[0].Post.URI, page2.Feed[0].Post.URI, "Pages should not overlap")
}

// TestGetDiscover_LimitValidation tests limit parameter validation
func TestGetDiscover_LimitValidation(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Setup services
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, nil, nil)

	t.Run("Limit exceeds maximum", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=100", nil)
		rec := httptest.NewRecorder()
		handler.HandleGetDiscover(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errorResp map[string]string
		err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errorResp["error"])
		assert.Contains(t, errorResp["message"], "limit")
	})
}

// TestGetDiscover_ViewerVoteState tests that authenticated users see their vote state on posts
func TestGetDiscover_ViewerVoteState(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create community and posts
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("votes-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	post1URI := createTestPost(t, db, communityDID, "did:plc:author1", "Post with upvote", 10, time.Now().Add(-1*time.Hour))
	post2URI := createTestPost(t, db, communityDID, "did:plc:author2", "Post with downvote", 5, time.Now().Add(-2*time.Hour))
	_ = createTestPost(t, db, communityDID, "did:plc:author3", "Post without vote", 3, time.Now().Add(-3*time.Hour))

	// Setup mock vote service with pre-populated votes
	viewerDID := "did:plc:viewer123"
	mockVotes := newMockVoteService()
	mockVotes.AddVote(viewerDID, post1URI, "up", "at://"+viewerDID+"/social.coves.vote/vote1")
	mockVotes.AddVote(viewerDID, post2URI, "down", "at://"+viewerDID+"/social.coves.vote/vote2")

	// Setup handler with mock vote service
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, mockVotes, nil)

	// Create request with authenticated user context
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)

	// Inject OAuth session into context (simulates OptionalAuth middleware)
	did, _ := syntax.ParseDID(viewerDID)
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}
	reqCtx := context.WithValue(req.Context(), middleware.UserDIDKey, viewerDID)
	reqCtx = context.WithValue(reqCtx, middleware.OAuthSessionKey, session)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Assertions
	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Find our test posts and verify vote state
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
			// Posts without votes should have nil Viewer or nil Vote
			if feedPost.Post.Viewer != nil && feedPost.Post.Viewer.Vote != nil {
				// This post has a vote from our viewer - it's not post3
				continue
			}
			foundPost3 = true
		}
	}

	assert.True(t, foundPost1, "Should find post1 with upvote")
	assert.True(t, foundPost2, "Should find post2 with downvote")
	assert.True(t, foundPost3, "Should find post3 without vote")
}

// TestGetDiscover_NoViewerStateWithoutAuth tests that unauthenticated users don't get viewer state
func TestGetDiscover_NoViewerStateWithoutAuth(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	testID := time.Now().UnixNano()

	// Create community and post
	communityDID, err := createFeedTestCommunity(db, ctx, fmt.Sprintf("noauth-%d", testID), fmt.Sprintf("alice-%d.test", testID))
	require.NoError(t, err)

	postURI := createTestPost(t, db, communityDID, "did:plc:author", "Some post", 10, time.Now())

	// Setup mock vote service with a vote (but request will be unauthenticated)
	mockVotes := newMockVoteService()
	mockVotes.AddVote("did:plc:someuser", postURI, "up", "at://did:plc:someuser/social.coves.vote/vote1")

	// Setup handler with mock vote service
	discoverRepo := postgres.NewDiscoverRepository(db, "test-cursor-secret")
	discoverService := discoverCore.NewDiscoverService(discoverRepo)
	handler := discover.NewGetDiscoverHandler(discoverService, mockVotes, nil)

	// Create request WITHOUT auth context
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Should succeed
	assert.Equal(t, http.StatusOK, rec.Code)

	var response discoverCore.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Find our post and verify NO viewer state (unauthenticated)
	for _, feedPost := range response.Feed {
		if feedPost.Post.URI == postURI {
			assert.Nil(t, feedPost.Post.Viewer, "Unauthenticated request should not have viewer state")
			return
		}
	}
	t.Fatal("Test post not found in response")
}
