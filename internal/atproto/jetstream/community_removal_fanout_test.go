//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// While the instance has removed community C, a reply by Y to A's post P in C
// and an upvote by Y on P that Coves indexes from the firehose are stored, but
// notify A of nothing. The same reply and upvote on A's post L in a live
// community D notify A.

// communityRemovalFanoutScene is C with A's post P and the live D with A's
// post L, indexed by the post consumer, and the comment and vote consumers
// built as cmd/server builds them for this instance.
type communityRemovalFanoutScene struct {
	db               *sql.DB
	author, replier  string
	removedCommunity string
	removedPost      notificationGatePost
	livePost         notificationGatePost
	comments         *CommentEventConsumer
	votes            *VoteEventConsumer
	createdAt        string
}

func newCommunityRemovalFanoutScene(t *testing.T) communityRemovalFanoutScene {
	t.Helper()
	ctx := context.Background()
	db := testkit.DB(t)
	id := testkit.UniqueID(t)
	author, replier := "did:plc:"+id+"author", "did:plc:"+id+"replier"
	insertBridgedUserOnPDS(t, db, author, id+"author.test", bridgedTestNativePDS)
	insertBridgedUserOnPDS(t, db, replier, id+"replier.test", bridgedTestNativePDS)
	removedCommunity, liveCommunity := "did:plc:"+id+"removed", "did:plc:"+id+"live"
	insertBridgedCommunityOnPDS(t, db, removedCommunity, id+"removed.test", author, bridgedTestNativePDS)
	insertBridgedCommunityOnPDS(t, db, liveCommunity, id+"live.test", author, bridgedTestNativePDS)

	userService := newMockUserService()
	userService.users[author] = &users.User{DID: author, Handle: id + "author.test"}
	postConsumer := NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		userService, db, WithAdmissions(postgres.NewAdmissionRepository(db)),
	)
	indexPost := func(community string) notificationGatePost {
		key := testkit.TID()
		post := notificationGatePost{authorDID: author, uri: pv2URI(author, key), cid: "bafyreicommunityremovalfanoutpost"}
		require.NoError(t, postConsumer.HandleEvent(ctx, pv2Event(author, "create", key, testkit.TID(), post.cid,
			time.Now().UnixMicro(), pv2Record(community, "A post to reply to and upvote", "post body"))), "index A's post")
		require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, post.uri))
		return post
	}

	instance := fixtures.InstanceDID()
	notificationRepository := postgres.NewNotificationRepository(db, postgres.WithNotificationAccountRestrictionAuthority(instance))
	return communityRemovalFanoutScene{
		db: db, author: author, replier: replier, removedCommunity: removedCommunity,
		removedPost: indexPost(removedCommunity), livePost: indexPost(liveCommunity),
		comments: NewCommentEventConsumer(
			postgres.NewCommentRepository(db, postgres.WithAccountRestrictionAuthority(instance)), db,
			WithCommentNotifications(notificationRepository),
			WithCommentAccountBanAuthority(instance)),
		votes: NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db,
			WithVoteNotifications(notificationRepository),
			WithVoteAccountBanAuthority(instance)),
		createdAt: activatedCommentNotificationTime(t, db, ctx),
	}
}

// authorNotifications counts A's notifications that reference post.
func (s communityRemovalFanoutScene) authorNotifications(t *testing.T, post notificationGatePost) int {
	t.Helper()
	return countRows(t, s.db, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND (subject_uri = $2 OR root_post_uri = $2)`, s.author, post.uri)
}

func TestCommunityRemovalFanout_ReplyAndUpvoteNotifyNoOne(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"reply", "upvote"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s := newCommunityRemovalFanoutScene(t)
			communityremovalfixture.New(s.db).Remove(t, s.removedCommunity)

			deliver := func(post notificationGatePost) {
				t.Helper()
				if kind == "reply" {
					// Asserts the reply comment row is indexed.
					replyForNotificationGate(t, notificationGateFixture{db: s.db, commenterDID: s.replier}, s.comments, post, s.createdAt)
					return
				}
				// Asserts the vote row is indexed and active.
				deliverGroupVote(t, s.votes, s.replier, post.uri, "up", s.createdAt)
			}

			deliver(s.livePost)
			require.Equal(t, 1, s.authorNotifications(t, s.livePost),
				"control: Y's %s on A's post in the live community notifies A", kind)

			deliver(s.removedPost)
			assert.Equal(t, 0, s.authorNotifications(t, s.removedPost),
				"Y's %s on A's post in the removed community notifies A of nothing", kind)
		})
	}
}

// A comment K by Y whose parent is A's post P in the removed community C but
// whose commenter-chosen root is A's post L in the live community D belongs
// to C through its parent chain. While C is removed an upvote by A on K
// notifies Y of nothing; the same upvote on Y's comment under L notifies Y.
func TestCommunityRemovalFanout_UpvoteOnMismatchedRootCommentNotifiesNoOne(t *testing.T) {
	t.Parallel()
	s := newCommunityRemovalFanoutScene(t)
	comment := func(parent notificationGatePost) string {
		t.Helper()
		key := testkit.TID()
		uri := "at://" + s.replier + "/" + CommentCollection + "/" + key
		record := revCommentRecord("Y replies under the declared root L", s.livePost.uri, s.livePost.cid, parent.uri, parent.cid)
		record["createdAt"] = s.createdAt
		require.NoError(t, s.comments.HandleEvent(context.Background(), revCommitEvent(
			s.replier, CommentCollection, "create", key, testkit.TID(),
			"bafyreicommunityremovalmismatchedroot", time.Now().UnixMicro(), record,
		)), "index Y's comment")
		require.Equal(t, 1, countRows(t, s.db, `SELECT count(*) FROM comments WHERE uri = $1`, uri),
			"Y's comment must be indexed")
		return uri
	}
	mismatched := comment(s.removedPost)
	control := comment(s.livePost)
	communityremovalfixture.New(s.db).Remove(t, s.removedCommunity)

	deliverGroupVote(t, s.votes, s.author, control, "up", s.createdAt)
	require.Equal(t, 1, groupCount(t, s.db, s.replier, control),
		"control: A's upvote on Y's comment under the live community notifies Y")

	deliverGroupVote(t, s.votes, s.author, mismatched, "up", s.createdAt)
	assert.Equal(t, 0, groupCount(t, s.db, s.replier, mismatched),
		"A's upvote on Y's comment whose parent chain reaches the removed community notifies Y of nothing")
}
