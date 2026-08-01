//go:build integration

package postgres_test

import (
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthorProfileHydration verifies that post views hydrate the author's display
// name and avatar from the users table across every read path that shares
// postViewSelectColumns/scanPostView: batch get-by-URI, author feed, and the
// community feed (which the timeline and discover feeds share their scanner with).
//
// Regression test for the bug where feeds and post views only hydrated the
// community avatar and author cards were always bare even for fully indexed users.
func TestAuthorProfileHydration(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	testID := testkit.UniqueID(t)

	communityDID, err := fixtures.Community(ctx, db, fmt.Sprintf("avhydr-%s", testID), fmt.Sprintf("owner-%s.test", testID))
	require.NoError(t, err)

	authorDID := fmt.Sprintf("did:plc:avhydr%s", testID)
	postURI := fixtures.Post(t, db, communityDID, authorDID, "Author hydration post", 1, time.Now().Add(-1*time.Hour))

	// Give the author an indexed profile (as the profile firehose consumer would)
	const avatarCID = "bafkreiauthoravatar"
	const displayName = "Hydrated Author"
	_, err = db.ExecContext(ctx,
		`UPDATE users SET display_name = $1, avatar_cid = $2 WHERE did = $3`,
		displayName, avatarCID, authorDID)
	require.NoError(t, err)

	assertAuthorHydrated := func(t *testing.T, author *posts.AuthorView, path string) {
		t.Helper()
		require.NotNil(t, author, "%s: author view missing", path)
		assert.Equal(t, authorDID, author.DID, path)
		require.NotNil(t, author.DisplayName, "%s: author display name not hydrated", path)
		assert.Equal(t, displayName, *author.DisplayName, path)
		require.NotNil(t, author.Avatar, "%s: author avatar not hydrated", path)
		assert.Contains(t, *author.Avatar, avatarCID, "%s: avatar URL must reference the avatar CID", path)
		// Image proxy is disabled in the test env, so the URL is the direct PDS
		// getBlob form with the DID query-escaped. Pinning the author's DID here
		// catches regressions that hydrate the community's DID/PDS instead.
		assert.Contains(t, *author.Avatar, url.QueryEscape(authorDID), "%s: avatar URL must reference the author's own DID", path)
	}

	postRepo := postgres.NewPostRepository(db)

	t.Run("GetViewsByURIs", func(t *testing.T) {
		views, err := postRepo.GetViewsByURIs(ctx, []string{postURI})
		require.NoError(t, err)
		require.Contains(t, views, postURI)
		assertAuthorHydrated(t, views[postURI].Author, "GetViewsByURIs")
	})

	t.Run("GetByAuthor", func(t *testing.T) {
		views, _, err := postRepo.GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: authorDID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, views, 1)
		assertAuthorHydrated(t, views[0].Author, "GetByAuthor")
	})

	t.Run("CommunityFeed", func(t *testing.T) {
		feedRepo := postgres.NewCommunityFeedRepository(db, "test-cursor-secret")
		for _, sort := range []string{"new", "hot"} {
			feed, _, err := feedRepo.GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
				Community: communityDID,
				Sort:      sort,
				Limit:     10,
			})
			require.NoError(t, err, "sort=%s", sort)
			require.Len(t, feed, 1, "sort=%s", sort)
			assertAuthorHydrated(t, feed[0].Post.Author, "CommunityFeed sort="+sort)
		}
	})

	t.Run("AuthorWithoutProfileStaysBare", func(t *testing.T) {
		bareDID := fmt.Sprintf("did:plc:bare%s", testID)
		bareURI := fixtures.Post(t, db, communityDID, bareDID, "Bare author post", 1, time.Now())

		views, err := postRepo.GetViewsByURIs(ctx, []string{bareURI})
		require.NoError(t, err)
		require.Contains(t, views, bareURI)
		author := views[bareURI].Author
		require.NotNil(t, author)
		assert.Nil(t, author.DisplayName, "user without profile must not get a fabricated display name")
		assert.Nil(t, author.Avatar, "user without profile must not get a fabricated avatar URL")
	})
}
