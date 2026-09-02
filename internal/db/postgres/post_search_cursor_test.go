//go:build integration

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPosts_RelevanceCursorWalksRankTiesWithoutDuplicatesOrGaps(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, expected := seedSearchCursorPosts(t, db, "srchcurwalk")
	repo := NewCommunityFeedRepository(db, "test-secret")

	page1, cursor1, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", nil))
	require.NoError(t, err, "fetching relevance page 1")
	require.Len(t, page1, 3, "page 1 must contain the requested three search results")
	require.NotNil(t, cursor1,
		"page 1 must carry a cursor because four matching rows remain; losing the rank tiebreak position here makes later pages unreachable")

	page2, cursor2, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", cursor1))
	require.NoError(t, err, "fetching relevance page 2")
	require.Len(t, page2, 3, "page 2 must contain the next three search results")
	require.NotNil(t, cursor2,
		"page 2 must carry a cursor because one matching row remains; a nil cursor silently truncates the result set")

	page3, cursor3, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", cursor2))
	require.NoError(t, err, "fetching relevance page 3")
	require.Len(t, page3, 1, "page 3 must contain the final search result")
	assert.Nil(t, cursor3, "the final partial page must not claim another page exists")

	got := append(feedURIs(page1), feedURIs(page2)...)
	got = append(got, feedURIs(page3)...)
	assert.Equal(t, expected, got,
		"a relevance cursor must carry the created_at and URI tiebreak columns. Repeating or skipping either position loses rows whenever ranks tie, which is the common case for short titles")
}

func TestSearchPosts_TamperedCursorIsRejected(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, _ := seedSearchCursorPosts(t, db, "srchcurtamper")
	repo := NewCommunityFeedRepository(db, "test-secret")

	page1, cursor, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", nil))
	require.NoError(t, err, "fetching the page whose cursor will be tampered with")
	require.Len(t, page1, 3, "the tamper fixture must produce a full first page")
	require.NotNil(t, cursor, "a full first page with more matches must mint the cursor under test")

	decoded, err := base64.StdEncoding.DecodeString(*cursor)
	require.NoError(t, err, "the repository must mint a base64 cursor before its payload can be tampered with")
	signatureSeparator := bytes.LastIndex(decoded, []byte("::"))
	require.Greater(t, signatureSeparator, 0, "the signed cursor must contain a non-empty payload before its signature")
	decoded[0] ^= 1
	tampered := base64.StdEncoding.EncodeToString(decoded)

	feed, _, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", &tampered))
	assert.True(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
		"changing one payload byte must invalidate the HMAC and return communityFeeds.ErrInvalidCursor; accepting it lets callers choose arbitrary page positions")
	assert.Empty(t, feed, "an invalid signed cursor must not return any search results")

	notBase64 := "not-base64%%"
	feed, _, err = repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", &notBase64))
	assert.True(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
		"a non-base64 cursor must return communityFeeds.ErrInvalidCursor rather than being ignored or leaking a decoder error")
	assert.Empty(t, feed, "a malformed cursor must not fall back to the first search page")
}

func TestSearchPosts_CursorIsBoundToTheQuery(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, _ := seedSearchCursorPosts(t, db, "srchcurquery")
	repo := NewCommunityFeedRepository(db, "test-secret")

	page1, cursor, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", nil))
	require.NoError(t, err, "fetching the tungsten page whose cursor will be replayed")
	require.Len(t, page1, 3, "the query-binding fixture must produce a full first page")
	require.NotNil(t, cursor, "a full first page with more tungsten matches must mint the cursor under test")

	_, _, err = repo.SearchPosts(ctx, searchCursorRequest(community, "filament", "relevance", cursor))
	assert.True(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
		"a cursor is a position in one result set; replaying a tungsten cursor against a filament query must return communityFeeds.ErrInvalidCursor or pagination can silently skip or repeat results")
}

func TestSearchPosts_CursorIsBoundToTheSort(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, _ := seedSearchCursorPosts(t, db, "srchcursort")
	repo := NewCommunityFeedRepository(db, "test-secret")

	page1, _, err := repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", nil))
	require.NoError(t, err, "fetching a post from the first search page")
	require.NotEmpty(t, page1, "the sort-binding fixture must return a post to position the foreign cursor")
	require.NotNil(t, page1[0].Post, "the first search result must carry its hydrated post")

	newSortCursor := newFeedRepoBase(db, "test-secret").buildCursor(page1[0].Post, "new", 0, time.Now())
	_, _, err = repo.SearchPosts(ctx, searchCursorRequest(community, "tungsten", "relevance", &newSortCursor))
	assert.True(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
		"a correctly signed cursor for a different ordering is still the wrong position. Accepting a new-sort cursor for relevance corrupts pagination without any error")
}

func seedSearchCursorPosts(t *testing.T, db *sql.DB, label string) (string, []string) {
	t.Helper()

	community := visibilityCommunity(t, db, label)
	author := "did:plc:" + label + "author"
	createTestUser(t, db, label+"author.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expected := make([]string, 0, 7)
	for index := 0; index < 7; index++ {
		rkey := fmt.Sprintf("%s%02d", label, index)
		createdAt := base.Add(time.Duration(7-index) * time.Hour)
		uri := seedVisibilityPost(t, db, community, author, rkey, "Tungsten filament notes", createdAt)
		seedVisibilityAdmission(t, db, community, uri, posts.AdmissionStatusAccepted, "bafypostv2"+rkey, "")
		expected = append(expected, uri)
	}
	return community, expected
}

func searchCursorRequest(communityDID, query, sort string, cursor *string) communityFeeds.SearchPostsRequest {
	return communityFeeds.SearchPostsRequest{
		Query:     query,
		Community: communityDID,
		ViewerDID: "",
		Sort:      sort,
		Timeframe: "all",
		Limit:     3,
		Cursor:    cursor,
	}
}

func TestSearchPosts_CursorIsBoundToItsResultSet(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	communityA, _ := seedSearchCursorPosts(t, db, "srchcurbounda")
	communityB, _ := seedSearchCursorPosts(t, db, "srchcurboundb")
	repo := NewCommunityFeedRepository(db, "test-secret")

	dimensions := []struct {
		name          string
		change        func(*communityFeeds.SearchPostsRequest)
		mustBeInvalid func(string) bool
	}{
		{
			name: "query",
			change: func(req *communityFeeds.SearchPostsRequest) {
				req.Query = "filament"
			},
			mustBeInvalid: func(string) bool { return true },
		},
		{
			name: "community_scoped_to_other",
			change: func(req *communityFeeds.SearchPostsRequest) {
				req.Community = communityB
			},
			mustBeInvalid: func(string) bool { return true },
		},
		{
			name: "community_scoped_to_unscoped",
			change: func(req *communityFeeds.SearchPostsRequest) {
				req.Community = ""
			},
			mustBeInvalid: func(string) bool { return true },
		},
		{
			name: "sort",
			change: func(req *communityFeeds.SearchPostsRequest) {
				if req.Sort == "new" {
					req.Sort = "top"
				} else {
					req.Sort = "new"
				}
			},
			mustBeInvalid: func(string) bool { return true },
		},
		{
			name: "timeframe",
			change: func(req *communityFeeds.SearchPostsRequest) {
				req.Timeframe = "year"
			},
			mustBeInvalid: func(sort string) bool { return sort == "top" },
		},
	}

	for _, sort := range []string{"new", "top", "relevance"} {
		for _, dimension := range dimensions {
			t.Run(sort+"/"+dimension.name, func(t *testing.T) {
				pageOneRequest := searchCursorRequest(communityA, "tungsten", sort, nil)
				pageOne, cursor, err := repo.SearchPosts(ctx, pageOneRequest)
				require.NoErrorf(t, err, "minting the %s cursor for the %s replay", sort, dimension.name)
				require.Lenf(t, pageOne, pageOneRequest.Limit,
					"the %s/%s fixture must fill page 1 so its cursor has a meaningful position", sort, dimension.name)
				require.NotNilf(t, cursor,
					"the %s/%s fixture has more matches than the limit and must mint a page-1 cursor", sort, dimension.name)

				identicalRequest := pageOneRequest
				identicalRequest.Cursor = cursor
				pageTwo, _, err := repo.SearchPosts(ctx, identicalRequest)
				require.NoErrorf(t, err,
					"a %s cursor replayed with identical parameters must return page 2 before testing the %s binding", sort, dimension.name)
				require.NotEmptyf(t, pageTwo,
					"a %s cursor replayed with identical parameters returned an empty page even though matches remain", sort)
				pageOneURIs := feedURIs(pageOne)
				for _, uri := range feedURIs(pageTwo) {
					require.NotContainsf(t, pageOneURIs, uri,
						"an identical %s cursor replay repeated %s from page 1; the positive control must prove the cursor advances", sort, uri)
				}

				changedRequest := pageOneRequest
				changedRequest.Cursor = cursor
				dimension.change(&changedRequest)
				changedPage, _, err := repo.SearchPosts(ctx, changedRequest)
				if dimension.mustBeInvalid(sort) {
					assert.Truef(t, errors.Is(err, communityFeeds.ErrInvalidCursor),
						"a %s cursor is a position in the complete {query, community, sort, timeframe} result set; changing %s must return communityFeeds.ErrInvalidCursor, got err=%v and page=%v",
						sort, dimension.name, err, feedURIs(changedPage))
					return
				}

				require.NoErrorf(t, err,
					"changing timeframe under %s must not reject the cursor because timeframe does not alter that sort's result set", sort)
				require.NotEmptyf(t, changedPage,
					"changing the ignored timeframe under %s must still return page 2", sort)
				assert.Equalf(t, feedURIs(pageTwo), feedURIs(changedPage),
					"timeframe is ignored under %s, so changing it must preserve the cursor's exact second page", sort)
			})
		}
	}
}

func TestSearchPosts_RelevanceCursorWalksRankAndCreatedAtTiesByURI(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	community, expected := seedSearchCursorFullTiePosts(t, db, "srchcurfulltie")
	repo := NewCommunityFeedRepository(db, "test-secret")

	pageOne, cursorOne, err := repo.SearchPosts(ctx, searchCursorRequest(community, "osmium", "relevance", nil))
	require.NoError(t, err, "fetching page 1 from the relevance full-tie fixture")
	require.Len(t, pageOne, 3, "page 1 must contain the requested three tied results")
	require.NotNil(t, cursorOne, "page 1 must carry a cursor while four tied results remain")

	pageTwo, cursorTwo, err := repo.SearchPosts(ctx, searchCursorRequest(community, "osmium", "relevance", cursorOne))
	require.NoError(t, err, "fetching page 2 from the relevance full-tie fixture")
	require.Len(t, pageTwo, 3, "page 2 must contain the next three tied results")
	require.NotNil(t, cursorTwo, "page 2 must carry a cursor while one tied result remains")

	pageThree, cursorThree, err := repo.SearchPosts(ctx, searchCursorRequest(community, "osmium", "relevance", cursorTwo))
	require.NoError(t, err, "fetching page 3 from the relevance full-tie fixture")
	require.Len(t, pageThree, 1, "page 3 must contain the final tied result")
	assert.Nil(t, cursorThree, "the final partial page must not advertise a fourth page")

	assert.Equal(t, expected[:3], feedURIs(pageOne),
		"when rank and created_at both tie, page 1 must use p.uri DESC as the final ordering key")
	assert.Equal(t, expected[3:6], feedURIs(pageTwo),
		"when rank and created_at both tie, page 2 must continue in p.uri DESC order without repeating page 1")
	assert.Equal(t, expected[6:], feedURIs(pageThree),
		"the final URI in descending tie-break order must remain reachable on page 3")
	walked := append(feedURIs(pageOne), feedURIs(pageTwo)...)
	walked = append(walked, feedURIs(pageThree)...)
	assert.Equal(t, expected, walked,
		"a relevance cursor must walk all seven rank-and-created_at ties exactly once; any duplicate or gap means the URI tie-break position was lost")
}

func seedSearchCursorFullTiePosts(t *testing.T, db *sql.DB, label string) (string, []string) {
	t.Helper()

	community := visibilityCommunity(t, db, label)
	author := "did:plc:" + label + "author"
	createTestUser(t, db, label+"author.test", author)
	sharedCreatedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expectedDescending := make([]string, 0, 7)
	for index := 0; index < 7; index++ {
		rkey := fmt.Sprintf("%s%02d", label, index)
		uri := seedVisibilityPost(t, db, community, author, rkey, "Identical osmium title", sharedCreatedAt)
		seedVisibilityAdmission(t, db, community, uri, posts.AdmissionStatusAccepted, "bafypostv2"+rkey, "")
		expectedDescending = append([]string{uri}, expectedDescending...)
	}
	return community, expectedDescending
}
