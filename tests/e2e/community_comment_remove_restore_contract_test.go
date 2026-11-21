//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityCommentRemoveRestoreContract is the production wiring of a
// community moderator removing and restoring a comment. The owner creates a
// community through community.create (the AppView hosts it and keeps its PDS
// credentials; its password is never reset, so the AppView must write as the
// community), invites a second account as moderator, and the invitee accepts.
// An author's post is accepted and the author comments under it. Then, as the
// moderator:
//
//	removeComment  → the community's repo holds a social.coves.community.removal
//	                 for the comment with code spam and the moderator's reason,
//	                 and getComments serves the comment as a moderator-removed
//	                 placeholder attributed to the community
//	restoreComment → the removal record is gone and getComments serves the
//	                 comment with its content
//
// After the firehose copies of both commits are consumed (ordered by a
// sentinel profile update written after them), the public log serves exactly
// one apply-removal and one retract-removal for the comment, each naming the
// moderator.
func TestCommunityCommentRemoveRestoreContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "crro")
	moderator := p.IndexedAccount(t, "crrm")
	author := p.IndexedAccount(t, "crra")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	authorClient := p.AppView.As(p.AppView.SignIn(t, author))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "crr"), "description": "a community whose moderator removes comments",
	}, &created), "the owner creates a community through the AppView, which hosts it")
	require.NotEmpty(t, created.DID)
	communityDID := created.DID

	// The owner invites the moderator, who accepts.
	var rosterState struct {
		Version int64 `json:"version"`
	}
	require.NoError(t, ownerClient.Query(ctx, "social.coves.moderation.getCommunityState",
		url.Values{"subject": {communityDID}, "community": {communityDID}}, &rosterState))
	var invited struct {
		Invitation struct {
			ID string `json:"id"`
		} `json:"invitation"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.inviteModerator", map[string]any{
		"community": communityDID, "subject": moderator.DID,
		"expectedVersion": rosterState.Version, "idempotencyKey": testkit.UniqueID(t),
	}, &invited), "the owner invites the moderator")
	require.NotEmpty(t, invited.Invitation.ID)
	var inbox struct {
		Invitations []struct {
			Invitation struct {
				ID string `json:"id"`
			} `json:"invitation"`
			RosterVersion int64 `json:"rosterVersion"`
		} `json:"invitations"`
	}
	require.NoError(t, moderatorClient.Query(ctx, "social.coves.community.listViewerInvitations", url.Values{}, &inbox))
	require.Len(t, inbox.Invitations, 1, "the moderator's inbox holds the invitation")
	require.Equal(t, invited.Invitation.ID, inbox.Invitations[0].Invitation.ID)
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.acceptInvitation", map[string]any{
		"invitation": invited.Invitation.ID, "expectedVersion": inbox.Invitations[0].RosterVersion,
		"idempotencyKey": testkit.UniqueID(t),
	}, nil), "the moderator accepts the invitation")

	// The author's post is accepted and the author comments under it.
	var post strongRef
	require.NoError(t, authorClient.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": communityDID,
		"title":     "a post whose comment a moderator will remove " + testkit.UniqueID(t),
		"content":   "a post in a hosted community",
	}, &post), "the author posts in the hosted community")
	require.NotEmpty(t, post.URI)
	require.NotEmpty(t, post.CID)
	awaitStatus(t, p, post.URI, communityDID, "accepted", "the hosted community to accept the author's post")

	const commentText = "a comment the moderator will take down"
	commentRKey := testkit.TID()
	commentAt := commentURI(author.DID, commentRKey)
	author.PutRecord(t, commentCollection, commentRKey, commentRecord(post, post, commentText))
	recordKey := subjectRkey(commentAt)
	removalURI := "at://" + communityDID + "/" + removalCollection + "/" + recordKey

	commentNode := func() (threadNode, bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return threadNode{}, false, err
		}
		node, ok := thread.find(commentAt)
		return node, ok, nil
	}
	p.Await(t, "the author's comment to be served", func() (bool, error) {
		node, ok, err := commentNode()
		if err != nil || !ok {
			return false, err
		}
		return node.Comment.Record["content"] == commentText, nil
	}, withReadCadence())

	type commentActionResponse struct {
		URI            string `json:"uri"`
		Version        int64  `json:"version"`
		ActionID       string `json:"actionId"`
		AlreadyApplied bool   `json:"alreadyApplied"`
	}
	const removalReason = "Phishing link in a comment"

	// The moderator removes the comment.
	var removed commentActionResponse
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.removeComment", map[string]any{
		"subject": commentAt, "code": "spam", "reason": removalReason,
		"expectedVersion": 0, "idempotencyKey": testkit.UniqueID(t),
	}, &removed), "the moderator removes the comment")
	require.Equal(t, removalURI, removed.URI, "removeComment answers with the removal record's URI")
	require.False(t, removed.AlreadyApplied)
	require.NotEmpty(t, removed.ActionID, "removeComment answers with the action it wrote")

	removalRecord, err := readCommunityRecord(author, communityDID, removalCollection, recordKey)
	require.NoError(t, err)
	require.True(t, removalRecord.Present, "the community's repo holds the removal record once removeComment returns")
	uri, _ := recordSubject(removalRecord.Value)
	require.Equal(t, commentAt, uri, "the removal names the comment")
	require.Equal(t, "spam", removalRecord.Value["code"])
	require.Equal(t, removalReason, removalRecord.Value["reason"])

	p.FreshReadQuota(t, "community-comment-remove")
	node, ok, err := commentNode()
	require.NoError(t, err)
	require.True(t, ok, "the removed comment is still in the thread")
	require.True(t, node.Comment.IsDeleted, "the comment is served as a placeholder once removeComment returns")
	require.NotNil(t, node.Comment.DeletionReason)
	require.Equal(t, "moderator", *node.Comment.DeletionReason)
	require.Nil(t, node.Comment.Record, "a removed comment's record is not served")
	require.NotNil(t, node.Comment.Moderation)
	require.Equal(t, "removed", node.Comment.Moderation.State)
	require.Len(t, node.Comment.Moderation.Sources, 1, "the removal is attributed to exactly one source")
	source := node.Comment.Moderation.Sources[0]
	require.Equal(t, communityDID, source.AuthorityDID)
	require.Equal(t, "community", source.Scope.Kind)
	require.Equal(t, communityDID, source.Scope.CommunityDID)

	// The moderator restores it.
	var restored commentActionResponse
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.restoreComment", map[string]any{
		"subject": commentAt, "expectedVersion": removed.Version, "idempotencyKey": testkit.UniqueID(t),
	}, &restored), "the moderator restores the comment")
	require.Equal(t, removalURI, restored.URI, "restoreComment answers with the removal record's URI")
	require.False(t, restored.AlreadyApplied)

	removalRecord, err = readCommunityRecord(author, communityDID, removalCollection, recordKey)
	require.NoError(t, err)
	require.False(t, removalRecord.Present, "the removal record is gone once restoreComment returns")

	p.FreshReadQuota(t, "community-comment-restore")
	node, ok, err = commentNode()
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, node.Comment.IsDeleted, "the comment is served again once restoreComment returns")
	require.Equal(t, commentText, node.Comment.Record["content"], "with its content")
	require.Nil(t, node.Comment.Moderation, "no removal is served")

	// The firehose copies of both commits: a profile update written after
	// them reaches community.get only through the firehose, in repo order.
	sentinel := "sentinel after the comment restore " + testkit.UniqueID(t)
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.update", map[string]any{
		"communityDid": communityDID, "description": sentinel,
	}, nil))
	p.Await(t, "the sentinel profile update to reach community.get", func() (bool, error) {
		community, err := p.Community(context.Background(), communityDID)
		if err != nil {
			return false, err
		}
		return community.Description == sentinel, nil
	})

	var log map[string]any
	require.NoError(t, p.AppView.Query(ctx, listActionsMethod, url.Values{
		"community": {communityDID}, "subject": {commentAt},
	}, &log))
	byKind := map[string][]map[string]any{}
	for _, action := range requireModlogActions(t, log) {
		kind := fmt.Sprint(action["action"])
		byKind[kind] = append(byKind[kind], action)
	}
	require.Len(t, byKind["apply-removal"], 1, "the public log serves exactly one apply-removal for the comment: %v", log)
	require.Len(t, byKind["retract-removal"], 1, "the public log serves exactly one retract-removal for the comment: %v", log)
	require.Len(t, byKind, 2, "no other action is logged for the comment: %v", log)
	for _, kind := range []string{"apply-removal", "retract-removal"} {
		require.Equalf(t, map[string]any{"did": moderator.DID}, byKind[kind][0]["actor"], "the %s action names the moderator", kind)
	}
}
