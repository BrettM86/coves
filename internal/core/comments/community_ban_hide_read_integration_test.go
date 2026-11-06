//go:build integration

package comments_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	banHideVisibleText = "said before the ban and still visible"
	banHideTopText     = "banned top-level comment that must stay hidden"
	banHideReplyText   = "a reply that outlives its hidden parent"
	banHideThreadText  = "an ordinary top-level comment"
	banHideNestedText  = "banned nested reply that must stay hidden"
	banHidePendingText = "banned reply under a parent that is never indexed"
	banHideForgedText  = "banned reply naming a root in the banning community and a parent elsewhere"
)

// findBanHideNode returns the node for uri anywhere in the tree.
func findBanHideNode(nodes []*comments.ThreadViewComment, uri string) *comments.ThreadViewComment {
	for _, node := range nodes {
		if node.Comment.URI == uri {
			return node
		}
		if found := findBanHideNode(node.Replies, uri); found != nil {
			return found
		}
	}
	return nil
}

// requireBanHidePlaceholder asserts the node is a placeholder that reads as a
// moderator removal and serves no record and none of the comment's text.
func requireBanHidePlaceholder(t *testing.T, node *comments.ThreadViewComment, text string) {
	t.Helper()
	assert.True(t, node.Comment.IsDeleted, "a ban-hidden comment must be served as a placeholder")
	if assert.NotNil(t, node.Comment.DeletionReason, "a ban-hidden placeholder must say why it is hidden") {
		assert.Equal(t, comments.DeletionReasonModerator, *node.Comment.DeletionReason,
			"a ban-hidden placeholder must read like a moderator removal, not an author delete")
	}
	encoded, err := json.Marshal(node.Comment)
	require.NoError(t, err)
	var served map[string]any
	require.NoError(t, json.Unmarshal(encoded, &served))
	assert.Nil(t, served["record"], "a ban-hidden comment's record must not be served: %s", encoded)
	assert.NotContains(t, string(encoded), text, "a ban-hidden comment's text must not be served")
}

func requireBanHideContent(t *testing.T, node *comments.ThreadViewComment, text string) {
	t.Helper()
	require.NotNil(t, node)
	assert.False(t, node.Comment.IsDeleted)
	record, ok := node.Comment.Record.(*comments.CommentRecord)
	require.True(t, ok, "comment must serve its record, got %T", node.Comment.Record)
	assert.Equal(t, text, record.Content)
}

// TestCommunityBanHiddenCommentsReadPaths drives a ban and the banned author's
// comments through the real consumers, then reads them back through every
// comment read path. A's comment indexed before the ban stays visible; A's
// comments indexed after it are placeholders in the thread (top-level, nested
// and as a subtree anchor), keep their replies, and are absent from A's
// profile activity for other viewers.
func TestCommunityBanHiddenCommentsReadPaths(t *testing.T) {
	db := testkit.DB(t)
	ctx := t.Context()

	bannedName := testkit.UniqueIDWithPrefix(t, "bhbanned")
	bannedDID := fixtures.DID(bannedName)
	fixtures.User(t, db, bannedName+".test", bannedDID)
	otherName := testkit.UniqueIDWithPrefix(t, "bhother")
	otherDID := fixtures.DID(otherName)
	fixtures.User(t, db, otherName+".test", otherDID)
	communityName := testkit.UniqueIDWithPrefix(t, "bhcommunity")
	communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
	require.NoError(t, err)
	postURI := fixtures.Post(t, db, communityDID, otherDID, "a thread with a banned commenter", 0, time.Now())
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(ctx, postURI)
	require.NoError(t, err)

	bans := postgres.NewCommunityBanRepository(db)
	postConsumer := jetstream.NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), nil, db,
		jetstream.WithPostCommunityBans(bans),
	)
	commentConsumer := jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	commentService := setupCommentService(db)

	started := time.Now().Add(-10 * time.Minute)
	step := 0
	nextTime := func() time.Time {
		step++
		return started.Add(time.Duration(step) * time.Second)
	}
	// comment indexes a comment through the comment consumer; parentURI "" is a
	// top-level comment on the post.
	comment := func(authorDID, parentURI, text string) string {
		t.Helper()
		rkey := testkit.TID()
		event := consumerCommentEvent(authorDID, postURI, post.CID, rkey, "create", testkit.TID(), moderatedCommentCID, text, "", nextTime())
		if parentURI != "" {
			event.Commit.Record["reply"].(map[string]interface{})["parent"] = map[string]interface{}{
				"uri": parentURI, "cid": moderatedCommentCID,
			}
		}
		require.NoError(t, commentConsumer.HandleEvent(ctx, event))
		uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey
		indexed, err := postgres.NewCommentRepository(db).GetByURI(ctx, uri)
		require.NoError(t, err, "fixture: the comment must be indexed")
		require.Equal(t, text, indexed.Content)
		return uri
	}

	visibleURI := comment(bannedDID, "", banHideVisibleText)

	// The community bans A through the real ban consumer.
	banTime := nextTime()
	require.NoError(t, postConsumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Did: communityDID, Kind: "commit", TimeUS: banTime.UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: testkit.TID(), Operation: "create", Collection: communities.BanCollection,
			RKey: communities.BanRecordKey(bannedDID), CID: moderatedCommentCID,
			Record: map[string]interface{}{
				"$type":     communities.BanCollection,
				"subject":   bannedDID,
				"createdAt": banTime.UTC().Format(time.RFC3339),
			},
		},
	}))
	_, err = bans.GetCommunityBan(ctx, communityDID, bannedDID)
	require.NoError(t, err, "fixture: the ban must be indexed")

	hiddenTopURI := comment(bannedDID, "", banHideTopText)
	replyURI := comment(otherDID, hiddenTopURI, banHideReplyText)
	threadURI := comment(otherDID, "", banHideThreadText)
	hiddenNestedURI := comment(bannedDID, threadURI, banHideNestedText)
	// A's reply naming P as its root and a parent that is never indexed: its
	// community never resolves, so it stays pending_ban_check for good.
	pendingURI := comment(bannedDID, "at://"+bannedDID+"/"+moderation.CommentCollection+"/"+testkit.TID(), banHidePendingText)
	pendingRkey := strings.TrimPrefix(pendingURI, "at://"+bannedDID+"/"+moderation.CommentCollection+"/")
	var pendingReason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT hide_reason FROM comments WHERE uri = $1`, pendingURI).Scan(&pendingReason))
	require.Equal(t, "pending_ban_check", pendingReason, "fixture: the comment must stay pending")

	for name, viewer := range map[string]*string{"other viewer": &otherDID, "anonymous viewer": nil} {
		t.Run("thread for "+name, func(t *testing.T) {
			response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
				PostURI: postURI, ViewerDID: viewer, Sort: "new", Depth: 3, Limit: 50,
			})
			require.NoError(t, err)

			requireBanHideContent(t, findBanHideNode(response.Comments, visibleURI), banHideVisibleText)

			hiddenTop := findBanHideNode(response.Comments, hiddenTopURI)
			require.NotNil(t, hiddenTop, "the hidden top-level comment keeps its place in the thread")
			requireBanHidePlaceholder(t, hiddenTop, banHideTopText)
			reply := findBanHideNode(hiddenTop.Replies, replyURI)
			require.NotNil(t, reply, "the reply stays beneath its hidden parent")
			requireBanHideContent(t, reply, banHideReplyText)

			thread := findBanHideNode(response.Comments, threadURI)
			requireBanHideContent(t, thread, banHideThreadText)
			hiddenNested := findBanHideNode(thread.Replies, hiddenNestedURI)
			require.NotNil(t, hiddenNested, "the hidden nested reply keeps its place under its parent")
			requireBanHidePlaceholder(t, hiddenNested, banHideNestedText)
		})

		t.Run("subtree anchored at the hidden comment for "+name, func(t *testing.T) {
			response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
				PostURI:    postURI,
				ParentRkey: strings.TrimPrefix(hiddenTopURI, "at://"+bannedDID+"/"+moderation.CommentCollection+"/"),
				ViewerDID:  viewer, Sort: "new", Depth: 1, Limit: 50,
			})
			require.NoError(t, err)
			require.Len(t, response.Comments, 1)
			require.Equal(t, hiddenTopURI, response.Comments[0].Comment.URI)
			requireBanHidePlaceholder(t, response.Comments[0], banHideTopText)
			require.Len(t, response.Comments[0].Replies, 1)
			assert.Equal(t, replyURI, response.Comments[0].Replies[0].Comment.URI)
			requireBanHideContent(t, response.Comments[0].Replies[0], banHideReplyText)
		})

		t.Run("subtree anchored at a pending comment for "+name, func(t *testing.T) {
			response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
				PostURI: postURI, ParentRkey: pendingRkey, ViewerDID: viewer, Sort: "new", Depth: 1, Limit: 50,
			})
			require.NoError(t, err)
			require.Len(t, response.Comments, 1)
			require.Equal(t, pendingURI, response.Comments[0].Comment.URI)
			requireBanHidePlaceholder(t, response.Comments[0], banHidePendingText)
		})

		for _, community := range []string{"", communityDID} {
			t.Run("actor comments for "+name+" in community "+community, func(t *testing.T) {
				response, err := commentService.GetActorComments(ctx, &comments.GetActorCommentsRequest{
					ActorDID: bannedDID, Community: community, Limit: 50, ViewerDID: viewer,
				})
				require.NoError(t, err)
				listed := map[string]bool{}
				for _, view := range response.Comments {
					listed[view.URI] = true
				}
				assert.True(t, listed[visibleURI], "the comment from before the ban stays in A's activity")
				assert.False(t, listed[hiddenTopURI], "a ban-hidden comment is absent from A's activity for other viewers")
				assert.False(t, listed[hiddenNestedURI], "a ban-hidden reply is absent from A's activity for other viewers")
				assert.False(t, listed[pendingURI], "a pending comment is absent from A's activity for other viewers")
			})
		}
	}

	t.Run("the author still sees their pending comment", func(t *testing.T) {
		response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
			PostURI: postURI, ParentRkey: pendingRkey, ViewerDID: &bannedDID, Sort: "new", Depth: 1, Limit: 50,
		})
		require.NoError(t, err)
		require.Len(t, response.Comments, 1)
		requireBanHideContent(t, response.Comments[0], banHidePendingText)

		activity, err := commentService.GetActorComments(ctx, &comments.GetActorCommentsRequest{
			ActorDID: bannedDID, Limit: 50, ViewerDID: &bannedDID,
		})
		require.NoError(t, err)
		listed := map[string]bool{}
		for _, view := range activity.Comments {
			listed[view.URI] = true
		}
		assert.True(t, listed[pendingURI], "the author keeps their own pending comment in their activity")
	})
}

// TestCommunityBanForgedRootIntoBanningCommunity covers a banned author who
// names a post in the banning community C as the reply root and a post in an
// unbanned community D as the parent. The parent chain resolves to D, so no
// hide reason is recorded; the comment's content must still not be served
// under C to anyone but its author, through the permalink or C's activity.
func TestCommunityBanForgedRootIntoBanningCommunity(t *testing.T) {
	db := testkit.DB(t)
	ctx := t.Context()

	bannedName := testkit.UniqueIDWithPrefix(t, "bfbanned")
	bannedDID := fixtures.DID(bannedName)
	fixtures.User(t, db, bannedName+".test", bannedDID)
	otherName := testkit.UniqueIDWithPrefix(t, "bfother")
	otherDID := fixtures.DID(otherName)
	fixtures.User(t, db, otherName+".test", otherDID)
	banningName := testkit.UniqueIDWithPrefix(t, "bfbanning")
	banningDID, err := fixtures.Community(ctx, db, banningName, "owner"+banningName)
	require.NoError(t, err)
	unbannedName := testkit.UniqueIDWithPrefix(t, "bfunbanned")
	unbannedDID, err := fixtures.Community(ctx, db, unbannedName, "owner"+unbannedName)
	require.NoError(t, err)
	rootURI := fixtures.Post(t, db, banningDID, otherDID, "a thread in the banning community", 0, time.Now())
	root, err := postgres.NewPostRepository(db).GetRawIndexedRow(ctx, rootURI)
	require.NoError(t, err)
	parentURI := fixtures.Post(t, db, unbannedDID, otherDID, "a thread in an unbanned community", 0, time.Now())
	parent, err := postgres.NewPostRepository(db).GetRawIndexedRow(ctx, parentURI)
	require.NoError(t, err)

	bans := postgres.NewCommunityBanRepository(db)
	postConsumer := jetstream.NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), nil, db,
		jetstream.WithPostCommunityBans(bans),
	)
	commentConsumer := jetstream.NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	commentService := setupCommentService(db)

	banTime := time.Now().Add(-10 * time.Minute)
	require.NoError(t, postConsumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Did: banningDID, Kind: "commit", TimeUS: banTime.UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: testkit.TID(), Operation: "create", Collection: communities.BanCollection,
			RKey: communities.BanRecordKey(bannedDID), CID: moderatedCommentCID,
			Record: map[string]interface{}{
				"$type":     communities.BanCollection,
				"subject":   bannedDID,
				"createdAt": banTime.UTC().Format(time.RFC3339),
			},
		},
	}))
	_, err = bans.GetCommunityBan(ctx, banningDID, bannedDID)
	require.NoError(t, err, "fixture: the ban must be indexed")

	rkey := testkit.TID()
	event := consumerCommentEvent(bannedDID, rootURI, root.CID, rkey, "create", testkit.TID(), moderatedCommentCID,
		banHideForgedText, "", banTime.Add(time.Minute))
	event.Commit.Record["reply"].(map[string]interface{})["parent"] = map[string]interface{}{
		"uri": parentURI, "cid": parent.CID,
	}
	require.NoError(t, commentConsumer.HandleEvent(ctx, event))
	forgedURI := "at://" + bannedDID + "/" + moderation.CommentCollection + "/" + rkey
	indexed, err := postgres.NewCommentRepository(db).GetByURI(ctx, forgedURI)
	require.NoError(t, err, "fixture: the comment must be indexed")
	require.Nil(t, indexed.HideReason, "fixture: the parent chain resolves to the unbanned community, so no hide reason is recorded")

	activityListing := func(t *testing.T, viewer *string) map[string]bool {
		t.Helper()
		response, err := commentService.GetActorComments(ctx, &comments.GetActorCommentsRequest{
			ActorDID: bannedDID, Community: banningDID, Limit: 50, ViewerDID: viewer,
		})
		require.NoError(t, err)
		listed := map[string]bool{}
		for _, view := range response.Comments {
			listed[view.URI] = true
		}
		return listed
	}

	for name, viewer := range map[string]*string{"other viewer": &otherDID, "anonymous viewer": nil} {
		t.Run("permalink under the declared root for "+name, func(t *testing.T) {
			response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
				PostURI: rootURI, ParentRkey: rkey, ViewerDID: viewer, Sort: "new", Depth: 1, Limit: 50,
			})
			if err == nil {
				encoded, marshalErr := json.Marshal(response)
				require.NoError(t, marshalErr)
				assert.NotContains(t, string(encoded), banHideForgedText,
					"the comment's content must not be served under the banning community's post")
			}
			assert.True(t, errors.Is(err, comments.ErrParentNotFound),
				"the comment is not found under a root whose community its parent chain does not reach, got %v", err)
		})

		t.Run("activity in the banning community for "+name, func(t *testing.T) {
			assert.False(t, activityListing(t, viewer)[forgedURI],
				"the comment is absent from the banned author's activity in the banning community")
		})
	}

	t.Run("the author still sees their comment", func(t *testing.T) {
		response, err := commentService.GetComments(ctx, &comments.GetCommentsRequest{
			PostURI: rootURI, ParentRkey: rkey, ViewerDID: &bannedDID, Sort: "new", Depth: 1, Limit: 50,
		})
		require.NoError(t, err)
		require.Len(t, response.Comments, 1)
		requireBanHideContent(t, response.Comments[0], banHideForgedText)
		assert.True(t, activityListing(t, &bannedDID)[forgedURI],
			"the author keeps their own comment in their activity in the community they named")
	})
}
