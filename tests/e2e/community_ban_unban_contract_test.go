//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityBanUnbanContract is the production wiring of a community
// moderator banning and unbanning an account. The owner creates a community
// through community.create (the AppView hosts it and keeps its PDS
// credentials; its password is never reset, so the AppView must write as the
// community), invites a second account as moderator, and the invitee accepts.
// Then, as the moderator:
//
//	banAccount   → the community's repo holds a social.coves.moderation.ban
//	               on the account at the record key derived from its DID,
//	               with the moderator's reason; the account's post.create in
//	               the community answers 403 Banned and its community.get
//	               viewer state carries the reason; listBans lists the ban
//	unbanAccount → the ban record is gone, listBans is empty and the account
//	               posts again
func TestCommunityBanUnbanContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "cbuo")
	moderator := p.IndexedAccount(t, "cbum")
	banned := p.IndexedAccount(t, "cbub")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	bannedClient := p.AppView.As(p.AppView.SignIn(t, banned))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "cbu"), "description": "a community whose moderator bans an account",
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

	rkey := banRecordKey(banned.DID)
	banURI := "at://" + communityDID + "/" + banCollection + "/" + rkey
	type banActionResponse struct {
		URI            string `json:"uri"`
		Version        int64  `json:"version"`
		ActionID       string `json:"actionId"`
		AlreadyApplied bool   `json:"alreadyApplied"`
	}
	type listedBans struct {
		Bans []struct {
			Subject string `json:"subject"`
			URI     string `json:"uri"`
			Reason  string `json:"reason"`
			Active  bool   `json:"active"`
		} `json:"bans"`
	}
	listBans := func() listedBans {
		t.Helper()
		var listed listedBans
		require.NoError(t, moderatorClient.Query(ctx, "social.coves.community.listBans",
			url.Values{"community": {communityDID}}, &listed), "the moderator lists the community's bans")
		return listed
	}
	const banReason = "Posting invite spam in every thread"
	require.EqualValues(t, 0, communityVersion(moderatorClient, banned.DID), "the account's community version starts at zero")

	// The moderator bans the account.
	var banResult banActionResponse
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.banAccount", map[string]any{
		"community": communityDID, "subject": banned.DID, "reason": banReason,
		"expectedVersion": 0, "idempotencyKey": testkit.UniqueID(t),
	}, &banResult), "the moderator bans the account")
	require.Equal(t, banURI, banResult.URI, "banAccount answers with the ban record's URI")
	require.EqualValues(t, 1, banResult.Version, "the account's community version advanced once")
	require.NotEmpty(t, banResult.ActionID, "banAccount names its action")
	require.False(t, banResult.AlreadyApplied)

	banRecord, err := readCommunityRecord(banned, communityDID, banCollection, rkey)
	require.NoError(t, err)
	require.True(t, banRecord.Present, "the community's repo holds the ban record once banAccount returns")
	require.Equal(t, banCollection, banRecord.Value["$type"])
	require.Equal(t, banned.DID, banRecord.Value["subject"], "the ban names the account")
	require.Equal(t, banReason, banRecord.Value["reason"], "with the moderator's reason")
	require.NotContains(t, banRecord.Value, "expiresAt", "a ban written without an expiry is permanent")

	err = submitPost(bannedClient, communityDID, "a post from a banned account "+testkit.UniqueID(t))
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Banned", "the banned account's post.create")

	var community map[string]any
	require.NoError(t, bannedClient.Query(ctx, "social.coves.community.get",
		url.Values{"community": {communityDID}}, &community))
	viewer, ok := community["viewer"].(map[string]any)
	require.Truef(t, ok, "community.get as the banned account serves a viewer object: %v", community["viewer"])
	require.Equal(t, true, viewer["banned"], "the banned account's viewer state is banned")
	require.Equal(t, banURI, viewer["banUri"])
	require.Equal(t, banReason, viewer["banReason"], "the banned account sees the reason")

	listed := listBans()
	require.Len(t, listed.Bans, 1, "listBans lists the one ban")
	require.Equal(t, banned.DID, listed.Bans[0].Subject, "the listed ban names the account")
	require.Equal(t, banURI, listed.Bans[0].URI)
	require.Equal(t, banReason, listed.Bans[0].Reason)
	require.True(t, listed.Bans[0].Active, "a permanent ban is in force")

	// The moderator lifts the ban.
	var unbanResult banActionResponse
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.unbanAccount", map[string]any{
		"community": communityDID, "subject": banned.DID,
		"expectedVersion": 1, "idempotencyKey": testkit.UniqueID(t),
	}, &unbanResult), "the moderator unbans the account")
	require.Equal(t, banURI, unbanResult.URI, "unbanAccount answers with the ban record's URI")
	require.EqualValues(t, 2, unbanResult.Version, "the account's community version advanced once more")
	require.False(t, unbanResult.AlreadyApplied)

	banRecord, err = readCommunityRecord(banned, communityDID, banCollection, rkey)
	require.NoError(t, err)
	require.False(t, banRecord.Present, "the ban record is gone once unbanAccount returns")
	require.Empty(t, listBans().Bans, "listBans no longer lists the lifted ban")
	require.NoError(t, submitPost(bannedClient, communityDID, "a post after the ban was lifted "+testkit.UniqueID(t)),
		"post.create is accepted once the ban is lifted")
}
