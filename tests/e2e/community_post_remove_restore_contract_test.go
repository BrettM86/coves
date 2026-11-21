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

// TestCommunityPostRemoveRestoreContract is the production wiring of a
// community moderator removing and restoring a post. The owner creates a
// community through community.create (the AppView hosts it and keeps its PDS
// credentials; its password is never reset, so the AppView must write as the
// community), invites a second account as moderator, and the invitee accepts.
// An author's post is accepted. Then, as the moderator:
//
//	removePost  → the community's repo holds a social.coves.community.removal
//	              for the post with code spam and the moderator's reason, and
//	              no acceptance; the author's post.get view is removed and
//	              carries the community's code and reason
//	restorePost → the removal record is gone and an acceptance pinning the
//	              post's CID stands; the author's view is accepted again
//
// After the firehose copies of both commits are consumed (ordered by a
// sentinel profile update written after them), the public log serves exactly
// one remove and one restore action for the post, each naming the moderator,
// and the post's community version is the one the restore returned.
func TestCommunityPostRemoveRestoreContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "prro")
	moderator := p.IndexedAccount(t, "prrm")
	author := p.IndexedAccount(t, "prra")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	authorClient := p.AppView.As(p.AppView.SignIn(t, author))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "prr"), "description": "a community whose moderator removes posts",
	}, &created), "the owner creates a community through the AppView, which hosts it")
	require.NotEmpty(t, created.DID)
	communityDID := created.DID

	communityVersion := func(client *testkit.AppView, subject string) int64 {
		t.Helper()
		var state struct {
			Version int64 `json:"version"`
		}
		require.NoError(t, client.Query(ctx, "social.coves.moderation.getCommunityState",
			url.Values{"subject": {subject}, "community": {communityDID}}, &state))
		return state.Version
	}

	// The owner invites the moderator, who accepts.
	var invited struct {
		Invitation struct {
			ID string `json:"id"`
		} `json:"invitation"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.inviteModerator", map[string]any{
		"community": communityDID, "subject": moderator.DID,
		"expectedVersion": communityVersion(ownerClient, communityDID), "idempotencyKey": testkit.UniqueID(t),
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

	// The author's post is accepted.
	var post struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	require.NoError(t, authorClient.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": communityDID,
		"title":     "a post a moderator will remove " + testkit.UniqueID(t),
		"content":   "a post in a hosted community",
	}, &post), "the author posts in the hosted community")
	require.NotEmpty(t, post.URI)
	require.NotEmpty(t, post.CID)
	recordKey := subjectRkey(post.URI)
	removalURI := "at://" + communityDID + "/" + removalCollection + "/" + recordKey
	acceptanceURI := "at://" + communityDID + "/" + acceptanceCollection + "/" + recordKey
	awaitStatus(t, p, post.URI, communityDID, "accepted", "the hosted community to accept the author's post")
	p.Await(t, "the community's repo to hold the acceptance pinning the post", func() (bool, error) {
		acceptance, err := readCommunityRecord(author, communityDID, acceptanceCollection, recordKey)
		if err != nil || !acceptance.Present {
			return false, err
		}
		uri, cid := recordSubject(acceptance.Value)
		return uri == post.URI && cid == post.CID, nil
	})

	authorView := func() map[string]any {
		t.Helper()
		var response struct {
			Posts []map[string]any `json:"posts"`
		}
		require.NoError(t, authorClient.Query(ctx, "social.coves.community.post.get",
			url.Values{"uris": {post.URI}}, &response))
		require.Len(t, response.Posts, 1, "post.get returns one union member for one URI")
		return response.Posts[0]
	}
	p.Await(t, "the author's post to be indexed", func() (bool, error) {
		var response struct {
			Posts []map[string]any `json:"posts"`
		}
		if err := authorClient.Query(context.Background(), "social.coves.community.post.get",
			url.Values{"uris": {post.URI}}, &response); err != nil {
			return false, err
		}
		return len(response.Posts) == 1 && response.Posts[0]["uri"] == post.URI &&
			response.Posts[0]["cid"] == post.CID && response.Posts[0]["record"] != nil, nil
	})
	type postActionResponse struct {
		URI            string `json:"uri"`
		Version        int64  `json:"version"`
		ActionID       string `json:"actionId"`
		AlreadyApplied bool   `json:"alreadyApplied"`
	}
	const removalReason = "Selling followers in the comments"

	// The moderator removes the post.
	var removed postActionResponse
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.removePost", map[string]any{
		"subject": post.URI, "code": "spam", "reason": removalReason,
		"expectedVersion": communityVersion(moderatorClient, post.URI), "idempotencyKey": testkit.UniqueID(t),
	}, &removed), "the moderator removes the post")
	require.Equal(t, removalURI, removed.URI, "removePost answers with the removal record's URI")
	require.False(t, removed.AlreadyApplied)

	removalRecord, err := readCommunityRecord(author, communityDID, removalCollection, recordKey)
	require.NoError(t, err)
	require.True(t, removalRecord.Present, "the community's repo holds the removal record once removePost returns")
	uri, cid := recordSubject(removalRecord.Value)
	require.Equal(t, post.URI, uri, "the removal names the post")
	require.Equal(t, post.CID, cid, "at its current CID")
	require.Equal(t, "spam", removalRecord.Value["code"])
	require.Equal(t, removalReason, removalRecord.Value["reason"])
	acceptance, err := readCommunityRecord(author, communityDID, acceptanceCollection, recordKey)
	require.NoError(t, err)
	require.False(t, acceptance.Present, "the acceptance is deleted in the same commit")

	view := authorView()
	require.Equal(t, post.URI, view["uri"], "the author still reads their removed post")
	require.Equal(t, "removed", view["status"], "the author's view is removed")
	require.Equal(t, map[string]any{"code": "spam", "reason": removalReason}, view["communityRemoval"],
		"the author's view carries the community's code and reason")

	// The moderator restores it.
	var restored postActionResponse
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.restorePost", map[string]any{
		"subject": post.URI, "expectedVersion": communityVersion(moderatorClient, post.URI),
		"idempotencyKey": testkit.UniqueID(t),
	}, &restored), "the moderator restores the post")
	require.Equal(t, acceptanceURI, restored.URI, "restorePost answers with the acceptance record's URI")
	require.False(t, restored.AlreadyApplied)

	removalRecord, err = readCommunityRecord(author, communityDID, removalCollection, recordKey)
	require.NoError(t, err)
	require.False(t, removalRecord.Present, "the removal record is gone once restorePost returns")
	acceptance, err = readCommunityRecord(author, communityDID, acceptanceCollection, recordKey)
	require.NoError(t, err)
	require.True(t, acceptance.Present, "an acceptance stands again")
	uri, cid = recordSubject(acceptance.Value)
	require.Equal(t, post.URI, uri)
	require.Equal(t, post.CID, cid, "pinned to the post's current CID")
	view = authorView()
	require.Equal(t, "accepted", view["status"], "the author's view is accepted again")
	require.Nil(t, view["communityRemoval"], "no community removal is served")

	// The firehose copies of both commits: a profile update written after
	// them reaches community.get only through the firehose, in repo order.
	sentinel := "sentinel after the restore " + testkit.UniqueID(t)
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
	require.Equal(t, restored.Version, communityVersion(moderatorClient, post.URI),
		"the firehose copies of the local commits change the post's community version no further")
	require.Equal(t, "accepted", authorView()["status"], "the firehose copies leave the post accepted")

	var log map[string]any
	require.NoError(t, p.AppView.Query(ctx, listActionsMethod, url.Values{
		"community": {communityDID}, "subject": {post.URI},
	}, &log))
	byKind := map[string][]map[string]any{}
	for _, action := range requireModlogActions(t, log) {
		kind := fmt.Sprint(action["action"])
		byKind[kind] = append(byKind[kind], action)
	}
	require.Len(t, byKind["remove"], 1, "the public log serves exactly one remove action for the post: %v", log)
	require.Len(t, byKind["restore"], 1, "the public log serves exactly one restore action for the post: %v", log)
	require.Len(t, byKind, 2, "no other action is logged for the post: %v", log)
	for _, kind := range []string{"remove", "restore"} {
		require.Equalf(t, map[string]any{"did": moderator.DID}, byKind[kind][0]["actor"], "the %s action names the moderator", kind)
	}
}
