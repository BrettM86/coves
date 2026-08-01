//go:build integration

// The timeline is the personalised feed: it serves only the communities the
// requesting account subscribes to. That fan-out is a join in the Postgres
// repository, and the exclusion it performs is a privacy boundary as much as a
// product one — a post from an unsubscribed community appearing here is the
// same class of defect as a leaked row.
//
// These tests are the ONLY coverage of that behaviour anywhere in the suite.
// The endpoint requires authentication, and the pipeline tier cannot
// authenticate a write or a read (docs/TEST_ARCHITECTURE.md §3.4b), so the
// personalised path is unreachable from T2 by construction. Everything here has
// to stay.
//
// They run against a real database with the real repository, service and HTTP
// handler wired together, because a stubbed repository would assert nothing
// about the join that does the actual filtering. They are in an external test
// package because they import Coves/internal/db/postgres, which imports this
// package: in-package that is an import cycle.
package timeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	timelinehandler "Coves/internal/api/handlers/timeline"
	"Coves/internal/api/middleware"
	"Coves/internal/core/timeline"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cursorSecret is the key the repository signs pagination cursors with. Its
// value is irrelevant as long as the same repository instance both mints and
// verifies the cursor, which is what the pagination cases do.
const cursorSecret = "test-cursor-secret"

// Subscriptions are inserted with content_visibility 3, the most permissive
// setting. There is no fixture for them because the visibility ladder is not
// what these tests are about: they need the subscription to exist and to hide
// nothing, so that any post missing from the feed is a fan-out defect rather
// than a filter doing its job.

// TestGetTimeline_Basic proves the core promise of the endpoint: posts from
// subscribed communities appear, posts from an unsubscribed community do not,
// and the "new" sort is reverse chronological across the community boundary.
func TestGetTimeline_Basic(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	userDID := fmt.Sprintf("did:plc:user-%s", testID)
	fixtures.User(t, db, fmt.Sprintf("testuser-%s.test", testID), userDID)

	community1DID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	community2DID, err := fixtures.Community(ctx, db, fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err)

	// A third community the user is deliberately NOT subscribed to.
	community3DID, err := fixtures.Community(ctx, db, fmt.Sprintf("cooking-%s", testID), fmt.Sprintf("charlie-%s.test", testID))
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3), ($1, $3, 3)
	`, userDID, community1DID, community2DID)
	require.NoError(t, err)

	post1URI := fixtures.Post(t, db, community1DID, "did:plc:alice", "Gaming post 1", 50, time.Now().Add(-1*time.Hour))
	post2URI := fixtures.Post(t, db, community2DID, "did:plc:bob", "Tech post 1", 30, time.Now().Add(-2*time.Hour))
	post3URI := fixtures.Post(t, db, community3DID, "did:plc:charlie", "Cooking post (should not appear)", 100, time.Now().Add(-30*time.Minute))
	post4URI := fixtures.Post(t, db, community1DID, "did:plc:alice", "Gaming post 2", 20, time.Now().Add(-3*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response timeline.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	require.Len(t, response.Feed, 3, "Timeline should show posts from subscribed communities only")

	uris := []string{response.Feed[0].Post.URI, response.Feed[1].Post.URI, response.Feed[2].Post.URI}
	assert.Contains(t, uris, post1URI, "Should contain gaming post 1")
	assert.Contains(t, uris, post2URI, "Should contain tech post 1")
	assert.Contains(t, uris, post4URI, "Should contain gaming post 2")
	assert.NotContains(t, uris, post3URI, "Should NOT contain post from unsubscribed community")

	assert.Equal(t, post1URI, response.Feed[0].Post.URI, "Newest post should be first")
	assert.Equal(t, post2URI, response.Feed[1].Post.URI, "Second newest post")
	assert.Equal(t, post4URI, response.Feed[2].Post.URI, "Oldest post should be last")

	// The Record field is the lexicon-shaped view of the post. Clients render
	// from it, so an omitted $type or a missing author is a schema break even
	// though the row itself is fine.
	for i, feedPost := range response.Feed {
		assert.NotNil(t, feedPost.Post.Record, "Post %d should have Record field", i)
		record, ok := feedPost.Post.Record.(map[string]interface{})
		require.True(t, ok, "Record should be a map")
		assert.Equal(t, "social.coves.community.post", record["$type"], "Record should have correct $type")
		assert.NotEmpty(t, record["community"], "Record should have community")
		assert.NotEmpty(t, record["author"], "Record should have author")
		assert.NotEmpty(t, record["createdAt"], "Record should have createdAt")
	}
}

// TestGetTimeline_HotSort checks hot ranking over a multi-community timeline
// keeps every subscribed post and attaches the community each one came from.
func TestGetTimeline_HotSort(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	userDID := fmt.Sprintf("did:plc:user-%s", testID)
	fixtures.User(t, db, fmt.Sprintf("testuser-%s.test", testID), userDID)

	community1DID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	community2DID, err := fixtures.Community(ctx, db, fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3), ($1, $3, 3)
	`, userDID, community1DID, community2DID)
	require.NoError(t, err)

	// Three points on the recency/score curve: recent and popular, old and
	// popular, brand new and unnoticed.
	fixtures.Post(t, db, community1DID, "did:plc:alice", "Recent trending gaming", 50, time.Now().Add(-1*time.Hour))
	fixtures.Post(t, db, community2DID, "did:plc:bob", "Old popular tech", 100, time.Now().Add(-24*time.Hour))
	fixtures.Post(t, db, community1DID, "did:plc:charlie", "Brand new gaming", 5, time.Now().Add(-10*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=hot&limit=10", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response timeline.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Len(t, response.Feed, 3, "Timeline should show all posts from subscribed communities")

	for _, feedPost := range response.Feed {
		require.NotNil(t, feedPost.Post.Community, "Post should have community context")
		assert.Contains(t, []string{community1DID, community2DID}, feedPost.Post.Community.DID)
	}
}

// TestGetTimeline_Pagination walks the chronological cursor and checks the
// pages do not overlap.
func TestGetTimeline_Pagination(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	userDID := fmt.Sprintf("did:plc:user-%s", testID)
	fixtures.User(t, db, fmt.Sprintf("testuser-%s.test", testID), userDID)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3)
	`, userDID, communityDID)
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		fixtures.Post(t, db, communityDID, "did:plc:alice", fmt.Sprintf("Post %d", i), 10-i, time.Now().Add(-time.Duration(i)*time.Hour))
	}

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=2", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page1 timeline.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page1)
	require.NoError(t, err)

	require.Len(t, page1.Feed, 2, "First page should have 2 posts")
	require.NotNil(t, page1.Cursor, "Should have cursor for next page")

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getTimeline?sort=new&limit=2&cursor=%s", *page1.Cursor), nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec = httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var page2 timeline.TimelineResponse
	err = json.Unmarshal(rec.Body.Bytes(), &page2)
	require.NoError(t, err)

	require.Len(t, page2.Feed, 2, "Second page should have 2 posts")
	assert.NotNil(t, page2.Cursor, "Should have cursor for next page")

	assert.NotEqual(t, page1.Feed[0].Post.URI, page2.Feed[0].Post.URI, "Pages should not overlap")
	assert.NotEqual(t, page1.Feed[1].Post.URI, page2.Feed[1].Post.URI, "Pages should not overlap")
}

// TestGetTimeline_EmptyWhenNoSubscriptions pins the new-account state: an
// account with no subscriptions gets an empty feed and no cursor, rather than
// falling back to a global feed.
func TestGetTimeline_EmptyWhenNoSubscriptions(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	testID := testkit.UniqueID(t)
	userDID := fmt.Sprintf("did:plc:user-%s", testID)
	fixtures.User(t, db, fmt.Sprintf("testuser-%s.test", testID), userDID)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var response timeline.TimelineResponse
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Empty(t, response.Feed, "Timeline should be empty when user has no subscriptions")
	assert.Nil(t, response.Cursor, "Should not have cursor when no results")
}

// TestGetTimeline_Unauthorized is the auth boundary for the personalised feed:
// with no session there is no "whose timeline" to answer, so the handler must
// refuse rather than serve somebody a default.
func TestGetTimeline_Unauthorized(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=10", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetTimeline(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	var errorResp map[string]string
	err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
	require.NoError(t, err)

	assert.Equal(t, "AuthenticationRequired", errorResp["error"])
}

// TestGetTimeline_LimitValidation checks an over-large page size is rejected at
// the handler rather than reaching the query.
func TestGetTimeline_LimitValidation(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	testID := testkit.UniqueID(t)
	userDID := fmt.Sprintf("did:plc:user-%s", testID)
	fixtures.User(t, db, fmt.Sprintf("testuser-%s.test", testID), userDID)

	t.Run("Limit exceeds maximum", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=100", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		var errorResp map[string]string
		err := json.Unmarshal(rec.Body.Bytes(), &errorResp)
		require.NoError(t, err)

		assert.Equal(t, "InvalidRequest", errorResp["error"])
		assert.Contains(t, errorResp["message"], "limit")
	})
}

// TestGetTimeline_MultiCommunity is the full fan-out scenario: four
// communities, three subscribed, eight posts spread across them, and every sort
// mode plus pagination driven over the same fixture set.
//
// The single unsubscribed community carries the highest-scoring and
// second-newest post in the whole database. That is deliberate: it means the
// exclusion has to hold under "new" (where it would sort near the top), under
// "hot" (where recency and score both favour it) and under "top" (where its
// score alone would put it first). A filter applied in only one of the three
// query paths passes one subtest and fails the others.
func TestGetTimeline_MultiCommunity(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	timelineRepo := postgres.NewTimelineRepository(db, cursorSecret)
	timelineService := timeline.NewTimelineService(timelineRepo)
	handler := timelinehandler.NewGetTimelineHandler(timelineService, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	userDID := fmt.Sprintf("did:plc:user-%s", testID)
	fixtures.User(t, db, fmt.Sprintf("testuser-%s.test", testID), userDID)

	community1DID, err := fixtures.Community(ctx, db, fmt.Sprintf("gaming-%s", testID), fmt.Sprintf("alice-%s.test", testID))
	require.NoError(t, err, "Failed to create gaming community")

	community2DID, err := fixtures.Community(ctx, db, fmt.Sprintf("tech-%s", testID), fmt.Sprintf("bob-%s.test", testID))
	require.NoError(t, err, "Failed to create tech community")

	community3DID, err := fixtures.Community(ctx, db, fmt.Sprintf("music-%s", testID), fmt.Sprintf("charlie-%s.test", testID))
	require.NoError(t, err, "Failed to create music community")

	community4DID, err := fixtures.Community(ctx, db, fmt.Sprintf("cooking-%s", testID), fmt.Sprintf("dave-%s.test", testID))
	require.NoError(t, err, "Failed to create cooking community (unsubscribed)")

	_, err = db.ExecContext(ctx, `
		INSERT INTO community_subscriptions (user_did, community_did, content_visibility)
		VALUES ($1, $2, 3), ($1, $3, 3), ($1, $4, 3)
	`, userDID, community1DID, community2DID, community3DID)
	require.NoError(t, err, "Failed to create subscriptions")

	gamingPost1 := fixtures.Post(t, db, community1DID, "did:plc:gamer1", "Epic gaming moment", 100, time.Now().Add(-2*time.Hour))
	gamingPost2 := fixtures.Post(t, db, community1DID, "did:plc:gamer2", "New game release", 75, time.Now().Add(-30*time.Minute))

	techPost1 := fixtures.Post(t, db, community2DID, "did:plc:dev1", "Golang best practices", 150, time.Now().Add(-4*time.Hour))
	techPost2 := fixtures.Post(t, db, community2DID, "did:plc:dev2", "atProto deep dive", 200, time.Now().Add(-1*time.Hour))
	techPost3 := fixtures.Post(t, db, community2DID, "did:plc:dev3", "Docker tips", 50, time.Now().Add(-15*time.Minute))

	musicPost1 := fixtures.Post(t, db, community3DID, "did:plc:artist1", "Album review", 80, time.Now().Add(-3*time.Hour))
	musicPost2 := fixtures.Post(t, db, community3DID, "did:plc:artist2", "Live concert tonight", 120, time.Now().Add(-10*time.Minute))

	// Newest-but-one and highest-scoring post in the database, in the one
	// community the user did not subscribe to.
	cookingPost := fixtures.Post(t, db, community4DID, "did:plc:chef1", "Best pizza recipe", 500, time.Now().Add(-5*time.Minute))

	t.Run("NEW sort - chronological across all subscribed communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timeline.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		require.Len(t, response.Feed, 7, "Timeline should show 7 posts from 3 subscribed communities")

		expectedOrder := []string{
			musicPost2,  // 10 minutes ago
			techPost3,   // 15 minutes ago
			gamingPost2, // 30 minutes ago
			techPost2,   // 1 hour ago
			gamingPost1, // 2 hours ago
			musicPost1,  // 3 hours ago
			techPost1,   // 4 hours ago
		}

		for i, expectedURI := range expectedOrder {
			assert.Equal(t, expectedURI, response.Feed[i].Post.URI,
				"Post %d should be %s in chronological order", i, expectedURI)
		}

		communityCountsByDID := make(map[string]int)
		for _, feedPost := range response.Feed {
			assert.NotEqual(t, cookingPost, feedPost.Post.URI,
				"Cooking post from unsubscribed community should NOT appear")
			require.NotNil(t, feedPost.Post.Community, "Post should have community context")
			communityCountsByDID[feedPost.Post.Community.DID]++
		}

		assert.Equal(t, 2, communityCountsByDID[community1DID], "Should have 2 gaming posts")
		assert.Equal(t, 3, communityCountsByDID[community2DID], "Should have 3 tech posts")
		assert.Equal(t, 2, communityCountsByDID[community3DID], "Should have 2 music posts")
		assert.Equal(t, 0, communityCountsByDID[community4DID], "Should have 0 cooking posts")
	})

	t.Run("HOT sort - recency and score across communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=hot&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timeline.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		require.Len(t, response.Feed, 7, "Timeline should show 7 posts from 3 subscribed communities")

		// The exact hot ordering is pinned by the discover package's ranking
		// tests; here it is enough that the top slot goes to one of the three
		// recent, well-scored posts rather than to the four-hour-old one.
		topPostURIs := []string{musicPost2, techPost2, gamingPost2}
		assert.Contains(t, topPostURIs, response.Feed[0].Post.URI,
			"Top post should be one of the recent high-scoring posts")

		for _, feedPost := range response.Feed {
			require.NotNil(t, feedPost.Post.Community, "Post should have community context")
			assert.Contains(t, []string{community1DID, community2DID, community3DID},
				feedPost.Post.Community.DID,
				"All posts should be from subscribed communities")
			assert.NotEqual(t, cookingPost, feedPost.Post.URI, "Cooking post should NOT appear")
		}
	})

	t.Run("TOP sort - highest scores across all communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=top&timeframe=all&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timeline.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		require.Len(t, response.Feed, 7, "Timeline should show 7 posts from 3 subscribed communities")

		assert.Equal(t, techPost2, response.Feed[0].Post.URI, "Top post should be techPost2 (score 200)")
		assert.Equal(t, techPost1, response.Feed[1].Post.URI, "Second post should be techPost1 (score 150)")
		assert.Equal(t, musicPost2, response.Feed[2].Post.URI, "Third post should be musicPost2 (score 120)")

		for i := 0; i < len(response.Feed)-1; i++ {
			currentScore := response.Feed[i].Post.Stats.Score
			nextScore := response.Feed[i+1].Post.Stats.Score
			assert.GreaterOrEqual(t, currentScore, nextScore,
				"Scores should be in descending order (post %d score=%d, post %d score=%d)",
				i, currentScore, i+1, nextScore)
		}

		// The excluded post outscores every post in the feed, so this is the
		// sort mode where a missing subscription filter would be loudest.
		for _, feedPost := range response.Feed {
			assert.NotEqual(t, cookingPost, feedPost.Post.URI,
				"Cooking post should NOT appear even with the highest score")
		}
	})

	t.Run("TOP sort with day timeframe - filters across communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=top&timeframe=day&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timeline.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		// Every fixture post is younger than a day, so the timeframe filter
		// must not drop any of them.
		require.Len(t, response.Feed, 7, "All posts are within last day")

		dayAgo := time.Now().Add(-24 * time.Hour)
		for _, feedPost := range response.Feed {
			assert.True(t, feedPost.Post.IndexedAt.After(dayAgo),
				"Post should be within last 24 hours")
		}
	})

	t.Run("Pagination across multiple communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=3", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var page1 timeline.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &page1)
		require.NoError(t, err)

		require.Len(t, page1.Feed, 3, "First page should have 3 posts")
		require.NotNil(t, page1.Cursor, "Should have cursor for next page")

		req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getTimeline?sort=new&limit=3&cursor=%s", *page1.Cursor), nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec = httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var page2 timeline.TimelineResponse
		err = json.Unmarshal(rec.Body.Bytes(), &page2)
		require.NoError(t, err)

		require.Len(t, page2.Feed, 3, "Second page should have 3 posts")
		require.NotNil(t, page2.Cursor, "Should have cursor for third page")

		page1URIs := make(map[string]bool)
		for _, p := range page1.Feed {
			page1URIs[p.Post.URI] = true
		}
		for _, p := range page2.Feed {
			assert.False(t, page1URIs[p.Post.URI], "Pages should not overlap")
		}

		req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/xrpc/social.coves.feed.getTimeline?sort=new&limit=3&cursor=%s", *page2.Cursor), nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec = httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var page3 timeline.TimelineResponse
		err = json.Unmarshal(rec.Body.Bytes(), &page3)
		require.NoError(t, err)

		// Seven posts over pages of three: the last page is the remainder and
		// must terminate the walk rather than hand back another cursor.
		assert.Len(t, page3.Feed, 1, "Third page should have 1 remaining post")
		assert.Nil(t, page3.Cursor, "Should not have cursor on last page")
	})

	t.Run("Record schema compliance across communities", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getTimeline?sort=new&limit=20", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), userDID))
		rec := httptest.NewRecorder()
		handler.HandleGetTimeline(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var response timeline.TimelineResponse
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		require.NoError(t, err)

		for i, feedPost := range response.Feed {
			assert.NotNil(t, feedPost.Post.Record, "Post %d should have Record field", i)

			record, ok := feedPost.Post.Record.(map[string]interface{})
			require.True(t, ok, "Record should be a map")

			assert.Equal(t, "social.coves.community.post", record["$type"],
				"Record should have correct $type")
			assert.NotEmpty(t, record["community"], "Record should have community")
			assert.NotEmpty(t, record["author"], "Record should have author")
			assert.NotEmpty(t, record["createdAt"], "Record should have createdAt")

			require.NotNil(t, feedPost.Post.Community, "Post should have community reference")
			assert.NotEmpty(t, feedPost.Post.Community.DID, "Community should have DID")
			assert.NotEmpty(t, feedPost.Post.Community.Handle, "Community should have handle")
			assert.NotEmpty(t, feedPost.Post.Community.Name, "Community should have name")

			assert.Contains(t, []string{community1DID, community2DID, community3DID},
				feedPost.Post.Community.DID,
				"Post should be from one of the subscribed communities")
		}
	})
}
