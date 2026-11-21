//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityModeratorInvitationLifecycle is the production wiring of a
// moderator invitation (B20): the owner of a community the AppView hosts
// invites a second account by handle, the invitee finds the invitation in its
// own inbox and accepts it, the AppView writes the moderator record to the
// community's repo and indexes it, and the firehose copy of that record changes
// nothing further. The owner then removes the moderator; the record leaves the
// repo, the firehose delete changes nothing further, and the public log serves
// one remove-role action naming the owner and the role it removed.
//
// Before the removal, the moderator removes an author's accepted post through
// community.removePost. The community log (listActions filtered by community
// only) then serves exactly one remove action for the post naming the
// moderator and exactly one remove-role action for the moderator naming the
// owner with role "moderator".
//
// The community is created through community.create and its password is never
// reset: the AppView must keep its own credentials to write as the community.
//
// "Changes nothing further" is observed after an ordered sentinel: the owner
// updates the community's description through the AppView, whose index learns
// of it only from the firehose. The profile commit follows the role commit in
// the community's repo, so once the new description is served the role
// commit's copy has been consumed.
func TestCommunityModeratorInvitationLifecycle(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "ivo")
	invitee := p.IndexedAccount(t, "ivb")
	author := p.IndexedAccount(t, "iva")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "iv"), "description": "a community that invites moderators",
	}, &created), "the owner creates a community through the AppView, which hosts it and writes the owner record")
	require.NotEmpty(t, created.DID)
	communityDID := created.DID

	rosterVersion := func() int64 {
		t.Helper()
		var state struct {
			Kind    string `json:"kind"`
			Version int64  `json:"version"`
		}
		require.NoError(t, ownerClient.Query(ctx, "social.coves.moderation.getCommunityState",
			url.Values{"subject": {communityDID}, "community": {communityDID}}, &state))
		require.Equal(t, "roster", state.Kind)
		return state.Version
	}
	moderatorRecordsFor := func(subjectDID string) []map[string]any {
		t.Helper()
		var listing struct {
			Records []struct {
				URI   string         `json:"uri"`
				Value map[string]any `json:"value"`
			} `json:"records"`
		}
		require.NoError(t, p.PDS.Anon.Query(ctx, "com.atproto.repo.listRecords", url.Values{
			"repo": {communityDID}, "collection": {moderatorCollection}, "limit": {"100"},
		}, &listing))
		var matched []map[string]any
		for _, record := range listing.Records {
			if record.Value["subject"] == subjectDID {
				matched = append(matched, record.Value)
			}
		}
		return matched
	}
	moderatorListed := func(subjectDID string) bool {
		t.Helper()
		var response struct {
			Moderators []struct {
				Subject string `json:"subject"`
				Role    string `json:"role"`
			} `json:"moderators"`
		}
		require.NoError(t, p.AppView.Query(ctx, "social.coves.community.getModerators",
			url.Values{"community": {communityDID}}, &response))
		for _, moderator := range response.Moderators {
			if moderator.Subject == subjectDID {
				require.Equal(t, "moderator", moderator.Role)
				return true
			}
		}
		return false
	}
	// awaitSentinel writes a profile update through the AppView and waits for
	// the firehose to bring it back, which orders it after every earlier commit
	// to the community's repo.
	awaitSentinel := func(description string) {
		t.Helper()
		require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.update", map[string]any{
			"communityDid": communityDID, "description": description,
		}, nil))
		p.Await(t, "the sentinel profile update to reach community.get", func() (bool, error) {
			view, err := p.Community(context.Background(), communityDID)
			if err != nil {
				return false, err
			}
			return view.Description == description, nil
		})
	}

	// Invite by handle.
	var invited struct {
		Invitation struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Role    string `json:"role"`
		} `json:"invitation"`
		Version int64 `json:"version"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.inviteModerator", map[string]any{
		"community": communityDID, "subject": invitee.Handle,
		"expectedVersion": rosterVersion(), "idempotencyKey": testkit.UniqueID(t),
	}, &invited), "the owner invites the second account by its handle")
	require.NotEmpty(t, invited.Invitation.ID)
	require.Equal(t, invitee.DID, invited.Invitation.Subject, "the handle resolves to the invitee's DID")
	require.Equal(t, "moderator", invited.Invitation.Role)

	// The invitee reads its own inbox and accepts.
	inviteeClient := p.AppView.As(p.AppView.SignIn(t, invitee))
	var inbox struct {
		Invitations []struct {
			Invitation struct {
				ID        string `json:"id"`
				Community string `json:"community"`
				Role      string `json:"role"`
			} `json:"invitation"`
			RosterVersion int64 `json:"rosterVersion"`
		} `json:"invitations"`
	}
	require.NoError(t, inviteeClient.Query(ctx, "social.coves.community.listViewerInvitations", url.Values{}, &inbox))
	require.Len(t, inbox.Invitations, 1, "the invitee's inbox holds the one pending invitation")
	pending := inbox.Invitations[0]
	require.Equal(t, invited.Invitation.ID, pending.Invitation.ID)
	require.Equal(t, communityDID, pending.Invitation.Community)
	require.Equal(t, "moderator", pending.Invitation.Role)

	var accepted struct {
		Community string `json:"community"`
		Subject   string `json:"subject"`
		Version   int64  `json:"version"`
	}
	require.NoError(t, inviteeClient.Procedure(ctx, "social.coves.community.acceptInvitation", map[string]any{
		"invitation": pending.Invitation.ID, "expectedVersion": pending.RosterVersion,
		"idempotencyKey": testkit.UniqueID(t),
	}, &accepted), "the invitee accepts with the roster version its inbox served")
	require.Equal(t, communityDID, accepted.Community)
	require.Equal(t, invitee.DID, accepted.Subject)

	records := moderatorRecordsFor(invitee.DID)
	require.Len(t, records, 1, "the community's repo holds the invitee's role record")
	require.Equal(t, moderatorCollection, records[0]["$type"])
	require.Equal(t, "moderator", records[0]["role"])
	require.True(t, moderatorListed(invitee.DID), "getModerators lists the invitee once the accept returns")

	awaitSentinel("sentinel after the accept " + testkit.UniqueID(t))
	require.True(t, moderatorListed(invitee.DID), "the firehose copy keeps the invitee listed")
	require.Equal(t, accepted.Version, rosterVersion(),
		"the firehose copy of the accepted record must not change the index a second time")

	// The moderator removes an author's accepted post.
	authorClient := p.AppView.As(p.AppView.SignIn(t, author))
	var post struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	require.NoError(t, authorClient.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": communityDID,
		"title":     "a post the moderator will remove " + testkit.UniqueID(t),
		"content":   "a post in a community with a moderator",
	}, &post), "the author posts in the hosted community")
	require.NotEmpty(t, post.URI)
	awaitStatus(t, p, post.URI, communityDID, "accepted", "the hosted community to accept the author's post")
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
	var postState struct {
		Version int64 `json:"version"`
	}
	require.NoError(t, inviteeClient.Query(ctx, "social.coves.moderation.getCommunityState",
		url.Values{"subject": {post.URI}, "community": {communityDID}}, &postState))
	require.NoError(t, inviteeClient.Procedure(ctx, "social.coves.community.removePost", map[string]any{
		"subject": post.URI, "code": "spam", "reason": "Selling followers in the comments",
		"expectedVersion": postState.Version, "idempotencyKey": testkit.UniqueID(t),
	}, nil), "the moderator removes the post")

	// The owner removes the moderator.
	var removed struct {
		Community string `json:"community"`
		Subject   string `json:"subject"`
		Version   int64  `json:"version"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.removeModerator", map[string]any{
		"community": communityDID, "subject": invitee.DID,
		"expectedVersion": rosterVersion(), "idempotencyKey": testkit.UniqueID(t),
	}, &removed))
	require.Equal(t, invitee.DID, removed.Subject)
	require.Empty(t, moderatorRecordsFor(invitee.DID), "the role record is gone from the community's repo")
	require.False(t, moderatorListed(invitee.DID), "getModerators no longer lists the removed moderator")

	awaitSentinel("sentinel after the removal " + testkit.UniqueID(t))
	require.False(t, moderatorListed(invitee.DID))
	require.Equal(t, removed.Version, rosterVersion(),
		"the firehose delete of the removed record must not change the index a second time")

	// The sentinel above follows the removal commit and the role delete in
	// the community's repo, so both firehose copies have been consumed.
	var log map[string]any
	require.NoError(t, p.AppView.Query(ctx, listActionsMethod, url.Values{
		"community": {communityDID},
	}, &log))
	requireNoPrivateModlogKeys(t, log)
	var postRemovals, roleRemovals []map[string]any
	for _, action := range requireModlogActions(t, log) {
		subject, _ := action["subject"].(map[string]any)
		switch {
		case action["action"] == "remove" && subject["uri"] == post.URI:
			postRemovals = append(postRemovals, action)
		case action["action"] == "remove-role" && subject["uri"] == "at://"+invitee.DID:
			roleRemovals = append(roleRemovals, action)
		}
	}
	require.Len(t, postRemovals, 1, "the community log serves exactly one remove action for the post: %v", log)
	require.Equal(t, map[string]any{"did": invitee.DID}, postRemovals[0]["actor"], "the post removal names the moderator")
	require.Len(t, roleRemovals, 1, "the community log serves exactly one remove-role action for the moderator: %v", log)
	require.Equal(t, map[string]any{"did": owner.DID}, roleRemovals[0]["actor"], "the role removal names the owner")
	require.Equal(t, "moderator", roleRemovals[0]["role"], "the role removal names the role it removed")
}
