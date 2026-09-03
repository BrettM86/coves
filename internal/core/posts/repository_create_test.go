//go:build integration

package posts_test

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"fmt"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The contract posts.Repository.Create owes its callers.
//
// Two things, and both are domain invariants rather than SQL details. Create
// fills in the fields the database owns — the surrogate id and indexed_at,
// which the caller cannot know and every later update needs. And a post URI is
// indexed AT MOST ONCE: the firehose redelivers on every reconnect, so the
// consumer hands the same record to Create repeatedly, and the second attempt
// has to come back as a recognisable "already indexed" rather than as a
// duplicate row or a raw constraint violation the consumer would treat as a
// transport failure and retry forever.
//
// This is asserted against the real Postgres implementation because the
// invariant IS the unique index; a fake repository asserting it would only be
// asserting its own map.
func TestPostRepository_CreateIndexesEachPostOnce(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	pdsURL := testkit.Endpoints().PDS.BaseURL

	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	postRepo := postgres.NewPostRepository(db)

	// A post row references both, so both have to exist before one can be
	// inserted at all.
	authorDID := fixtures.DID("postauthor2")
	_, err := userRepo.Create(ctx, &users.User{
		DID:    authorDID,
		Handle: "postauthor2.test",
		PDSURL: pdsURL,
	})
	require.NoError(t, err, "creating the post's author")

	communityDID := fixtures.DID("testcommunity2")
	_, err = communityRepo.Create(ctx, &communities.Community{
		DID:          communityDID,
		Handle:       fmt.Sprintf("c-testcommunity2.%s", instanceDomain),
		Name:         "testcommunity2",
		Visibility:   "public",
		CreatedByDID: authorDID,
		HostedByDID:  instanceDID,
		PDSURL:       pdsURL,
	})
	require.NoError(t, err, "creating the community the posts belong to")

	// postAt builds a post record in the community's repo, which is where post
	// records live.
	postAt := func(rkey, cid, content string) *posts.Post {
		return &posts.Post{
			URI:          fmt.Sprintf("at://%s/social.coves.community.post/%s", communityDID, rkey),
			CID:          cid,
			RKey:         rkey,
			AuthorDID:    authorDID,
			CommunityDID: communityDID,
			Content:      &content,
		}
	}

	t.Run("A new post comes back with the fields the database owns", func(t *testing.T) {
		title := "Test Title"
		post := postAt("test123", "bafy2test123", "Test post content")
		post.Title = &title

		require.NoError(t, postRepo.Create(ctx, post))
		assert.NotZero(t, post.ID, "Create should write back the row's id")
		assert.NotZero(t, post.IndexedAt, "Create should write back when the row was indexed")
	})

	t.Run("Re-indexing the same URI is refused", func(t *testing.T) {
		first := postAt("duplicate", "bafy2duplicate1", "Duplicate post")
		require.NoError(t, postRepo.Create(ctx, first))

		// A different CID at the same URI: a redelivered record, or a genuinely
		// changed one. Either way it is not a second post, and Create is not the
		// call that updates it.
		second := postAt("duplicate", "bafy2duplicate2", "Duplicate post")

		err := postRepo.Create(ctx, second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already indexed",
			"the conflict should be reported as a recognisable duplicate, not as a raw constraint error")
	})
}
