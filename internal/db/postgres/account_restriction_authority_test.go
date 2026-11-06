//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foreignAccountRemovalAuthority is an instance other than this one
// (fixtures.InstanceDID).
const foreignAccountRemovalAuthority = "did:web:foreign-instance.test"

// seedForeignAccountRemoval records an active permanent instance-scope removal
// of accountDID's account subject in the shape BanAccount writes, but made by
// another instance's authority. It is not this instance's ban.
func seedForeignAccountRemoval(t *testing.T, db *sql.DB, accountDID string) {
	t.Helper()
	actionID := "foreign-account-removal-" + testkit.UniqueID(t)
	_, err := db.ExecContext(context.Background(), `INSERT INTO moderation_actions
		(id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, subject_kind, action, reason, origin, created_at)
		VALUES ($1, 'did:plc:foreignremovaladmin', $2, 'instance', $3, NULL, 'account', 'remove',
		        'social.coves.moderation.defs#reasonSpam', 'local', NOW())`,
		actionID, foreignAccountRemovalAuthority, "at://"+accountDID)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO moderation_decisions
		(authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)`,
		foreignAccountRemovalAuthority, "at://"+accountDID, actionID)
	require.NoError(t, err)
}

// TestAccountRestriction_ForeignAuthorityRemovalRestrictsNothing: another
// instance's active removal of A's account subject is not this instance's
// account ban, so no read path hides A's posts, comments or the notifications
// that reference them.
func TestAccountRestriction_ForeignAuthorityRemovalRestrictsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUnreadCountFixture(t)
	author := "did:plc:foreignremoved" + testkit.UniqueID(t)
	post := seedVisibilityPost(t, f.db, f.community, author, testkit.TID(), "a post by A", f.sortAt)
	seedVisibilityAdmission(t, f.db, f.community, post, posts.AdmissionStatusAccepted, "", "")
	comment := seedActorComment(t, f.db, author, f.root, testkit.TID(), f.sortAt)
	f.listNotificationAt(t, f.recipient, author, "mention", post, "", f.root, f.sortAt.Add(time.Second))
	f.listNotificationAt(t, f.recipient, author, "postReply", comment, f.root, f.root, f.sortAt.Add(2*time.Second))
	seedForeignAccountRemoval(t, f.db, author)
	viewer := f.recipient

	postRepo := NewPostRepository(f.db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	authorPosts, _, err := postRepo.GetByAuthor(ctx, posts.GetAuthorPostsRequest{ActorDID: author, ViewerDID: viewer, Limit: 50})
	require.NoError(t, err)
	var authorPostURIs []string
	for _, view := range authorPosts {
		authorPostURIs = append(authorPostURIs, view.URI)
	}
	assert.Equal(t, []string{post}, authorPostURIs, "actor.getPosts lists A's post")

	feed, _, err := NewCommunityFeedRepository(f.db, "foreign-removal-secret", WithAccountRestrictionAuthority(fixtures.InstanceDID())).
		GetCommunityFeed(ctx, communityFeeds.GetCommunityFeedRequest{
			Community: f.community, ViewerDID: viewer, Sort: "new", Timeframe: "all", Limit: 50,
		})
	require.NoError(t, err)
	var feedURIs []string
	for _, item := range feed {
		feedURIs = append(feedURIs, item.Post.URI)
	}
	assert.Contains(t, feedURIs, post, "the community feed lists A's post")

	community, err := NewCommunityRepository(f.db, credentialciphertest.Fixed(), WithAccountRestrictionAuthority(fixtures.InstanceDID())).
		GetByDID(ctx, f.community)
	require.NoError(t, err)
	assert.Equal(t, 2, community.PostCount, "the community's post count includes A's post")

	commentRepo := NewCommentRepository(f.db, WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	for _, sort := range []string{"hot", "new"} {
		replies, _, err := commentRepo.ListByParentWithHotRank(ctx, f.root, sort, "all", 50, nil, viewer)
		require.NoError(t, err)
		require.Len(t, replies, 1, "%s replies list A's comment", sort)
		assert.False(t, replies[0].CommenterRestricted, "%s replies do not read A as restricted", sort)
	}
	batch, err := commentRepo.ListByParentsBatch(ctx, []string{f.root}, "new", 10, viewer)
	require.NoError(t, err)
	require.Len(t, batch[f.root], 1, "the batched replies list A's comment")
	assert.False(t, batch[f.root][0].CommenterRestricted, "the batched replies do not read A as restricted")
	byRkey, err := commentRepo.GetByRootAndRkey(ctx, f.root, comment[len(comment)-13:], viewer)
	require.NoError(t, err)
	assert.False(t, byRkey.CommenterRestricted, "a permalinked comment does not read A as restricted")
	listed, _, err := commentRepo.ListByCommenterWithCursor(ctx, comments.ListByCommenterRequest{CommenterDID: author, ViewerDID: viewer, Limit: 50})
	require.NoError(t, err)
	assert.Len(t, listed, 1, "actor.getComments lists A's comment")

	page, err := NewNotificationRepository(f.db, WithNotificationAccountRestrictionAuthority(fixtures.InstanceDID())).(notifications.ReadRepository).List(ctx, f.recipient, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Notifications, 2, "both notifications stay listed")
	for _, listedNotification := range page.Notifications {
		assert.Equal(t, notifications.ReferenceLive, listedNotification.Record.State,
			"the notification whose record is %s reads it live", listedNotification.RecordURI)
	}
}
