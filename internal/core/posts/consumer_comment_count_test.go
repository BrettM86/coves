//go:build integration

package posts_test

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// A post's comment_count when the firehose delivers the comments first.
//
// # WHY THIS IS A POST TEST
//
// comment_count is a column on posts, and only the post consumer can get it
// right. Nothing orders the firehose: a comment and its parent postv2 live in
// their respective authors' repos, which can commit independently, so the
// AppView routinely learns about a reply before it learns about the thing
// replied to. The comment consumer cannot increment a row that does not exist
// yet, so it writes the comment and moves on; the post
// consumer, when the post finally arrives, has to COUNT the comments already
// indexed against that URI instead of inserting a zero
// (internal/atproto/jetstream/post_consumer.go). That reconciliation is the
// behaviour under test, and getting it wrong is invisible — the post indexes
// fine, it just reports "0 comments" under a thread that has three.
//
// # WHY IT NEEDS A DATABASE
//
// The reconciliation IS a SQL statement: a counting subquery in the insert. A
// consumer test with a fake repository would be asserting that the test's own
// fake counts, so both consumers here run against the real repositories on a
// real schema, and the assertions read the column back.
//
// The consumers are driven by calling HandleEvent directly rather than by
// dialling a websocket. What is under test is the ordering the events arrive
// in, and constructing that ordering by hand is both exact and instant, where
// provoking it through a live Jetstream would be neither. The socket itself is
// covered at the pipeline tier (docs/TEST_ARCHITECTURE.md §3.4).
func TestPostConsumer_ReconcilesCommentCountWhenCommentsArriveFirst(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()

	postRepo := postgres.NewPostRepository(db)
	commentRepo := postgres.NewCommentRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	userRepo := postgres.NewUserRepository(db)
	// A nil identity resolver is deliberate: the post consumer only asks the user
	// service about authors it has not seen, and every author here is seeded
	// below, so no lookup should ever reach the network. A resolution attempt
	// would fail this test rather than slow it down, which is the useful failure.
	userService := users.NewUserService(userRepo, nil, testkit.Endpoints().PDS.BaseURL, nil, "")

	commentConsumer := jetstream.NewCommentEventConsumer(commentRepo, db)
	postConsumer := jetstream.NewPostEventConsumer(
		postRepo, communityRepo, userService, db,
		jetstream.WithAdmissions(postgres.NewAdmissionRepository(db)),
	)

	// Both rows exist before any event is handled. The post consumer refuses a
	// post whose community it has never indexed (there is no admission subject
	// otherwise) and needs the author indexed to hang the post off, so seeding
	// them is a precondition of the scenario rather than part of it.
	testUser := fixtures.User(t, db, "reconcile.test", fixtures.DID("reconcile"))
	testCommunity, err := fixtures.Community(ctx, db, "reconcile-community", "owner.test")
	require.NoError(t, err, "seeding the community the posts belong to")

	// commentOnPost hands the comment consumer a reply to postURI and returns the
	// URI the comment was indexed under. The reply's root and parent are both the
	// post: these are top-level comments, which is the shape the post's own
	// counter counts.
	//
	// Unlike a post, a comment lives in its AUTHOR's repo, so its URI is built
	// from the commenter's DID.
	commentOnPost := func(t *testing.T, rev, cid, content, postURI, postCID string) string {
		t.Helper()
		rkey := testkit.TID()
		err := commentConsumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        rev,
				Operation:  "create",
				Collection: "social.coves.community.comment",
				RKey:       rkey,
				CID:        cid,
				Record: map[string]interface{}{
					"$type":   "social.coves.community.comment",
					"content": content,
					"reply": map[string]interface{}{
						"root":   map[string]interface{}{"uri": postURI, "cid": postCID},
						"parent": map[string]interface{}{"uri": postURI, "cid": postCID},
					},
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		})
		require.NoError(t, err, "indexing a comment on %s", postURI)
		return fmt.Sprintf("at://%s/social.coves.community.comment/%s", testUser.DID, rkey)
	}

	// postEvent builds the commit the author's repo emits for a postv2 record.
	// The event DID is the author and the community remains in the record body.
	postEvent := func(rev, rkey, cid, title string) *jetstream.JetstreamEvent {
		return &jetstream.JetstreamEvent{
			Did:  testUser.DID,
			Kind: "commit",
			Commit: &jetstream.CommitEvent{
				Rev:        rev,
				Operation:  "create",
				Collection: posts.PostV2Collection,
				RKey:       rkey,
				CID:        cid,
				Record: map[string]interface{}{
					"$type":     posts.PostV2Collection,
					"community": testCommunity,
					"title":     title,
					"content":   "indexed out of order on purpose",
					"createdAt": time.Now().Format(time.RFC3339),
				},
			},
		}
	}

	postURIFor := func(rkey string) string {
		return fmt.Sprintf("at://%s/%s/%s", testUser.DID, posts.PostV2Collection, rkey)
	}

	t.Run("Single comment arrives before post", func(t *testing.T) {
		postRkey := testkit.TID()
		postURI := postURIFor(postRkey)

		commentURI := commentOnPost(t, "comment-rev", "bafycomment", "Comment arriving before parent post!", postURI, "bafypost")

		// The comment is indexed against a parent that does not exist. It must
		// still be readable, and still point at the post: a consumer that
		// dropped the orphan would leave nothing for the post to reconcile
		// against, and the count would come out right for the wrong reason.
		comment, err := commentRepo.GetByURI(ctx, commentURI)
		require.NoError(t, err, "the orphaned comment should be indexed")
		require.Equal(t, postURI, comment.ParentURI)

		require.NoError(t, postConsumer.HandleEvent(ctx, postEvent("post-rev", postRkey, "bafypost", "Post arriving after comment")))

		post, err := postRepo.GetRawIndexedRow(ctx, postURI)
		require.NoError(t, err, "the post should be indexed")
		require.Equal(t, 1, post.CommentCount,
			"the post consumer should have counted the comment that arrived before it")

		// Read the column directly as well. post.CommentCount could in principle
		// be computed on the read path; the point of the fix is that the stored
		// value is right, because that is what the feed queries select.
		var stored int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT comment_count FROM posts WHERE uri = $1", postURI).Scan(&stored))
		require.Equal(t, 1, stored)
	})

	t.Run("Several comments arrive before post", func(t *testing.T) {
		postRkey := testkit.TID()
		postURI := postURIFor(postRkey)

		for i := 1; i <= 3; i++ {
			commentOnPost(t, fmt.Sprintf("comment-%d-rev", i), fmt.Sprintf("bafycomment%d", i),
				fmt.Sprintf("Comment %d before post", i), postURI, "bafypost2")
		}

		require.NoError(t, postConsumer.HandleEvent(ctx, postEvent("post2-rev", postRkey, "bafypost2", "Post with 3 pre-existing comments")))

		post, err := postRepo.GetRawIndexedRow(ctx, postURI)
		require.NoError(t, err)
		require.Equal(t, 3, post.CommentCount,
			"reconciliation counts every pre-existing comment, not just the first")
	})

	t.Run("Comments before and after the post both count", func(t *testing.T) {
		postRkey := testkit.TID()
		postURI := postURIFor(postRkey)

		for i := 1; i <= 2; i++ {
			commentOnPost(t, fmt.Sprintf("before-%d-rev", i), fmt.Sprintf("bafybefore%d", i),
				fmt.Sprintf("Before comment %d", i), postURI, "bafypost3")
		}

		require.NoError(t, postConsumer.HandleEvent(ctx, postEvent("post3-rev", postRkey, "bafypost3", "Post with before and after comments")))

		post, err := postRepo.GetRawIndexedRow(ctx, postURI)
		require.NoError(t, err)
		require.Equal(t, 2, post.CommentCount)

		// The increment path and the reconciliation path have to agree: a
		// reconciled count that a later increment then overwrites is the same
		// bug seen from the other end.
		commentOnPost(t, "after-rev", "bafyafter", "Comment after post exists", postURI, "bafypost3")

		post, err = postRepo.GetRawIndexedRow(ctx, postURI)
		require.NoError(t, err)
		require.Equal(t, 3, post.CommentCount,
			"a comment arriving after the post should increment the reconciled count")
	})

	t.Run("Replaying the post event preserves the reconciled count", func(t *testing.T) {
		postRkey := testkit.TID()
		postURI := postURIFor(postRkey)

		commentOnPost(t, "idem-comment-rev", "bafyidemcomment", "Comment for idempotent test", postURI, "bafyidempost")

		event := postEvent("idem-post-rev", postRkey, "bafyidempost", "Idempotent test post")
		require.NoError(t, postConsumer.HandleEvent(ctx, event))

		post, err := postRepo.GetRawIndexedRow(ctx, postURI)
		require.NoError(t, err)
		require.Equal(t, 1, post.CommentCount)

		// Jetstream redelivers on reconnect, so every consumer sees its own
		// events again. A replay that re-inserted the post would reset the count
		// it had just reconciled.
		require.NoError(t, postConsumer.HandleEvent(ctx, event), "a replayed post event should be a no-op, not an error")

		post, err = postRepo.GetRawIndexedRow(ctx, postURI)
		require.NoError(t, err)
		require.Equal(t, 1, post.CommentCount,
			"replaying the post event must not reset comment_count")
	})
}
