//go:build integration

package discover_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	discoverhandler "Coves/internal/api/handlers/discover"
	"Coves/internal/core/discover"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDiscover_HotSort_SnapshotFreezesContinuation(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repository := postgres.NewDiscoverRepository(db, "snapshot-cursor-secret")
	service := discover.NewDiscoverService(repository)
	handler := discoverhandler.NewGetDiscoverHandler(service, nil, nil)

	ctx := context.Background()
	testID := testkit.UniqueID(t)
	createdAt := time.Now().Add(-4 * time.Hour)
	scores := []int{100, 50, 20, 5, 0}
	expected := make([]string, len(scores))
	for i, score := range scores {
		communityDID, err := fixtures.Community(ctx, db,
			fmt.Sprintf("snapshot-%d-%s", i, testID),
			fmt.Sprintf("snapshot-%d-%s.test", i, testID))
		require.NoError(t, err)
		expected[i] = fixtures.Post(t, db, communityDID, "did:plc:snapshot-author",
			fmt.Sprintf("Snapshot candidate %d", i), score, createdAt)
	}

	requestPage := func(cursor string) discover.DiscoverResponse {
		t.Helper()
		requestURL := "/xrpc/social.coves.feed.getDiscover?sort=hot&limit=2"
		if cursor != "" {
			requestURL += "&cursor=" + url.QueryEscape(cursor)
		}
		req := httptest.NewRequest(http.MethodGet, requestURL, nil)
		rec := httptest.NewRecorder()
		handler.HandleGetDiscover(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "Discover Hot response: %s", rec.Body.String())

		var response discover.DiscoverResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		return response
	}
	feedURIs := func(response discover.DiscoverResponse) []string {
		uris := make([]string, len(response.Feed))
		for i, item := range response.Feed {
			uris[i] = item.Post.URI
		}
		return uris
	}

	pageOne := requestPage("")
	require.Equal(t, expected[:2], feedURIs(pageOne))
	require.NotNil(t, pageOne.Cursor)

	changedScores := []int{-500_000, 500_000, -1_000_000, 40, 30}
	for i, score := range changedScores {
		upvotes, downvotes := score, 0
		if score < 0 {
			upvotes, downvotes = 0, -score
		}
		_, err := db.ExecContext(ctx, `
			UPDATE posts
			SET score = $1, upvote_count = $2, downvote_count = $3
			WHERE uri = $4
		`, score, upvotes, downvotes, expected[i])
		require.NoError(t, err)
	}

	newCommunityDID, err := fixtures.Community(ctx, db,
		fmt.Sprintf("snapshot-new-%s", testID),
		fmt.Sprintf("snapshot-new-%s.test", testID))
	require.NoError(t, err)
	newTopURI := fixtures.Post(t, db, newCommunityDID, "did:plc:new-snapshot-author",
		"New live leader", 2_000_000, time.Now())

	_, err = db.ExecContext(ctx, `DELETE FROM posts WHERE uri = $1`, expected[1])
	require.NoError(t, err)

	pageTwo := requestPage(*pageOne.Cursor)
	assert.Equal(t, expected[2:4], feedURIs(pageTwo),
		"continuation must use frozen ranks and must not look up the deleted page-one anchor")

	returned := append([]string{}, feedURIs(pageOne)...)
	returned = append(returned, feedURIs(pageTwo)...)
	cursor := pageTwo.Cursor
	for page := 0; cursor != nil && page < len(expected); page++ {
		response := requestPage(*cursor)
		returned = append(returned, feedURIs(response)...)
		cursor = response.Cursor
	}

	assert.NotContains(t, returned, newTopURI, "posts created after page one must not enter its snapshot")
	assert.Equal(t, expected, returned, "the original snapshot candidates must be returned once in frozen order")
	unique := make(map[string]struct{}, len(returned))
	for _, uri := range returned {
		unique[uri] = struct{}{}
	}
	assert.Len(t, unique, len(expected), "snapshot continuation must not lose or duplicate candidates")
}
