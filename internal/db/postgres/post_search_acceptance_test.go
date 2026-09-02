//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchPosts_AcceptanceContract(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	communityA := visibilityCommunity(t, db, "srchA")
	communityB := visibilityCommunity(t, db, "srchB")
	author := "did:plc:srchacceptanceauthor"
	createTestUser(t, db, "srchacceptanceauthor.test", author)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	borrowChecker := seedVisibilityPost(t, db, communityA, author, "srchborrow", "Rust borrow checker tips", base.Add(4*time.Hour))
	pendingAsync := seedVisibilityPost(t, db, communityA, author, "srchpending", "Rust async pitfalls", base.Add(3*time.Hour))
	goGenerics := seedVisibilityPost(t, db, communityA, author, "srchgo", "Go generics", base.Add(2*time.Hour))
	embedded := seedVisibilityPost(t, db, communityB, author, "srchembedded", "Rust on embedded", base.Add(time.Hour))

	seedVisibilityAdmission(t, db, communityA, borrowChecker, posts.AdmissionStatusAccepted, "bafypostv2srchborrow", "")
	seedVisibilityAdmission(t, db, communityA, pendingAsync, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, communityA, goGenerics, posts.AdmissionStatusAccepted, "bafypostv2srchgo", "")
	seedVisibilityAdmission(t, db, communityB, embedded, posts.AdmissionStatusAccepted, "bafypostv2srchembedded", "")

	repo := NewCommunityFeedRepository(db, "test-secret")
	scopedFeed, scopedCursor, err := repo.SearchPosts(ctx, communityFeeds.SearchPostsRequest{
		Query:     "rust",
		Community: communityA,
		ViewerDID: "",
		Sort:      "relevance",
		Timeframe: "all",
		Limit:     50,
		Cursor:    nil,
	})
	require.NoError(t, err, "a valid community-scoped post search must not fail")
	assert.ElementsMatch(t, []string{borrowChecker}, feedURIs(scopedFeed),
		"a community-scoped search must return only matching accepted posts from that community. The pending Rust post is speech community A has not agreed to carry, the Go post is not a text match, and community B's Rust post crossing the scope would leak another community's content")
	assert.Nil(t, scopedCursor,
		"a one-result search below the limit must not emit a cursor; doing so tells clients there is another page when none exists")

	globalFeed, _, err := repo.SearchPosts(ctx, communityFeeds.SearchPostsRequest{
		Query:     "rust",
		Community: "",
		ViewerDID: "",
		Sort:      "relevance",
		Timeframe: "all",
		Limit:     50,
		Cursor:    nil,
	})
	require.NoError(t, err, "a valid cross-community post search must not fail")
	assert.ElementsMatch(t, []string{borrowChecker, embedded}, feedURIs(globalFeed),
		"a cross-community search must return matching accepted posts from every community and nothing else. Missing the embedded post means the empty community filter stayed scoped; surfacing the pending post publishes speech community A has not agreed to carry; surfacing Go generics ignores the text query")
}
