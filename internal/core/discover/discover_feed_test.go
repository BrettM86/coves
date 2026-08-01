//go:build integration

// The discover feed's behaviour is decided almost entirely by SQL: the ranking
// expression, the cursor encoding and the ORDER BY that the cursor has to agree
// with all live in the Postgres repository, so a test with a stubbed repository
// would assert nothing that can actually break. These tests therefore run
// against a real database, wiring the real repository to the real service and
// driving the real HTTP handler in process — the shortest path that still
// exercises every layer the ranking passes through.
//
// They are in an external test package because they import
// Coves/internal/db/postgres, which imports this package: in-package that is an
// import cycle.
package discover_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	discoverhandler "Coves/internal/api/handlers/discover"
	"Coves/internal/api/middleware"
	"Coves/internal/core/discover"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cursorSecret is the key the repository signs pagination cursors with. Its
// value is irrelevant to these tests as long as the same repository instance
// both mints and verifies the cursor, which is what the pagination cases do.
const cursorSecret = "test-cursor-secret"

// mockVoteService is an in-memory votes.Service holding pre-seeded viewer
// votes.
//
// Viewer state is hydrated from a per-session cache that production fills from
// the viewer's own repository over OAuth. These tests are about what the feed
// does with that cache once it is populated — whether the right vote lands on
// the right post, and whether an unauthenticated request gets none at all — so
// the cache is seeded directly rather than round-tripped through a PDS.
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
	return nil // Votes are pre-populated via AddVote, so there is nothing to fetch.
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

// A nil vote service and a nil Bluesky service are passed wherever a case does
// not exercise them: the handler reads both as "no viewer state and no bridged
// hydration available", which is the anonymous path.

// TestGetDiscover_ShowsAllCommunities proves the discover feed is
// community-agnostic: unlike the timeline, it draws on every indexed community
// rather than the viewer's subscriptions.
func TestGetDiscover_ShowsAllCommunities(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

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
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discover.DiscoverResponse
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

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("public-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	postURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Public post", 10, time.Now())

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "Discover should work without authentication")

	var response discover.DiscoverResponse
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

// TestGetDiscover_HotSort checks that hot ranking does not lose posts. The
// ordering itself is pinned by TestGetDiscover_HotSort_LogDampedRanking; what
// this case guards is that switching sort modes still returns the whole
// cross-community set.
func TestGetDiscover_HotSort(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	community1DID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	community2DID, err := fixtures.Community(ctx, db, fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err)

	post1URI := fixtures.Post(t, db, community1DID, "did:plc:alice", "Recent trending", 50, time.Now().Add(-1*time.Hour))
	post2URI := fixtures.Post(t, db, community2DID, "did:plc:bob", "Old popular", 100, time.Now().Add(-24*time.Hour))
	post3URI := fixtures.Post(t, db, community1DID, "did:plc:charlie", "Brand new", 5, time.Now().Add(-10*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discover.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

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
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("ranking-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	// A bridged Lemmy-style blowout: 782 votes, a day old (rank ≈ 0.058)
	blowoutURI := fixtures.Post(t, db, communityDID, "did:plc:lemmy", "Federated blowout", 782, time.Now().Add(-24*time.Hour))
	// A brand-new organic post with no votes yet (rank ≈ 0.33)
	freshURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Fresh organic post", 0, time.Now().Add(-5*time.Minute))
	// A six-hour-old post with no votes (rank ≈ 0.044)
	sixHourURI := fixtures.Post(t, db, communityDID, "did:plc:bob", "Six hour old post", 0, time.Now().Add(-6*time.Hour))
	// A brand-new but downvoted post (rank ≈ -0.26)
	downvotedURI := fixtures.Post(t, db, communityDID, "did:plc:carol", "Downvoted post", -5, time.Now().Add(-5*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var response discover.DiscoverResponse
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
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("hotpage-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	blowoutURI := fixtures.Post(t, db, communityDID, "did:plc:lemmy", "Federated blowout", 782, time.Now().Add(-24*time.Hour))
	freshURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Fresh organic post", 0, time.Now().Add(-5*time.Minute))
	sixHourURI := fixtures.Post(t, db, communityDID, "did:plc:bob", "Six hour old post", 0, time.Now().Add(-6*time.Hour))
	downvotedURI := fixtures.Post(t, db, communityDID, "did:plc:carol", "Downvoted post", -5, time.Now().Add(-5*time.Minute))

	expectedOrder := []string{freshURI, blowoutURI, sixHourURI, downvotedURI}

	// Paginate to exhaustion, one post per page (the database is a private
	// clone, so the feed contains exactly these four posts)
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

		var response discover.DiscoverResponse
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
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("future-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	// A 0-vote post claiming to be from 10 days in the future
	futureURI := fixtures.Post(t, db, communityDID, "did:plc:mallory", "Future dated post", 0, time.Now().Add(240*time.Hour))
	// A recent post with modest genuine engagement (rank ≈ 0.95 vs the clamped ≈ 0.35)
	votedURI := fixtures.Post(t, db, communityDID, "did:plc:alice", "Recent voted post", 50, time.Now().Add(-1*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	// Before the GREATEST age clamp this query 500'd with "a negative number
	// raised to a non-integer power yields a complex result"
	require.Equal(t, http.StatusOK, rec.Code, "Future-dated post must not break the hot feed")

	var response discover.DiscoverResponse
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

// TestGetDiscover_Pagination walks the chronological cursor: pages must be full
// and must not repeat a post already served.
func TestGetDiscover_Pagination(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("test-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		fixtures.Post(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), 10-i, time.Now().Add(-time.Duration(i)*time.Hour))
	}

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=2", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 discover.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	require.Len(t, page1.Feed, 2, "First page should have 2 posts")
	require.NotNil(t, page1.Cursor, "Should have cursor for next page")

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getDiscover?sort=new&limit=2&cursor=%s", *page1.Cursor), nil)
	rec = httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 discover.DiscoverResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	require.Len(t, page2.Feed, 2, "Second page should have 2 posts")

	assert.NotEqual(t, page1.Feed[0].Post.URI, page2.Feed[0].Post.URI, "Pages should not overlap")
}

// TestGetDiscover_LimitValidation checks the handler rejects an over-large page
// size rather than letting it reach the query: an unbounded limit is the
// cheapest denial-of-service against a public, unauthenticated endpoint.
func TestGetDiscover_LimitValidation(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, nil, nil)

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

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, mockVotes, nil)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)

	// Both context values are what OptionalAuth would have set: the handler
	// reads the DID to key the vote lookup and the session to populate the
	// cache, so a test that set only one would exercise a state the middleware
	// never produces.
	did, err := syntax.ParseDID(viewerDID)
	require.NoError(t, err)
	session := &oauthlib.ClientSessionData{
		AccountDID:  did,
		AccessToken: "test_token",
	}
	reqCtx := context.WithValue(req.Context(), middleware.UserDIDKey, viewerDID)
	reqCtx = context.WithValue(reqCtx, middleware.OAuthSessionKey, session)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discover.DiscoverResponse
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
// anonymous requests even when the vote cache holds a vote for that post. The
// vote in the cache belongs to a different account, and leaking it would tell
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

	discoverRepo := postgres.NewDiscoverRepository(db, cursorSecret)
	discoverService := discover.NewDiscoverService(discoverRepo)
	handler := discoverhandler.NewGetDiscoverHandler(discoverService, mockVotes, nil)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover?sort=new&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetDiscover(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response discover.DiscoverResponse
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
