//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityViewerActionsContract is the production wiring of the
// community moderation actions a viewer may take. The owner creates a hosted
// community, invites a moderator who accepts, and an author with no role
// posts. The moderator's community feed lists remove and ban-author on the
// post, a signed-in account with no role sees no community actions, and once
// the moderator removes the post its post.get removal tombstone offers
// restore.
func TestCommunityViewerActionsContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "cvao")
	moderator := p.IndexedAccount(t, "cvam")
	author := p.IndexedAccount(t, "cvaa")
	stranger := p.IndexedAccount(t, "cvas")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	authorClient := p.AppView.As(p.AppView.SignIn(t, author))
	strangerClient := p.AppView.As(p.AppView.SignIn(t, stranger))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "cva"), "description": "a community whose moderator sees its actions",
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

	// The author, who holds no role, posts and the post is accepted.
	var post struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	require.NoError(t, authorClient.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": communityDID,
		"title":     "a post a moderator may act on " + testkit.UniqueID(t),
		"content":   "a post in a hosted community",
	}, &post), "the author posts in the hosted community")
	require.NotEmpty(t, post.URI)
	awaitStatus(t, p, post.URI, communityDID, "accepted", "the hosted community to accept the author's post")

	feedPost := func(client *testkit.AppView) (map[string]any, error) {
		var response struct {
			Feed []struct {
				Post map[string]any `json:"post"`
			} `json:"feed"`
		}
		if err := client.Query(context.Background(), "social.coves.communityFeed.getCommunity",
			url.Values{"community": {communityDID}, "sort": {"new"}}, &response); err != nil {
			return nil, err
		}
		for _, entry := range response.Feed {
			if entry.Post["uri"] == post.URI {
				return entry.Post, nil
			}
		}
		return nil, nil
	}
	var moderatorFeedPost map[string]any
	p.Await(t, "the accepted post to reach the community feed", func() (bool, error) {
		found, err := feedPost(moderatorClient)
		moderatorFeedPost = found
		return found != nil, err
	})
	moderatorViewer, _ := moderatorFeedPost["viewer"].(map[string]any)
	require.Equal(t, []any{"remove", "ban-author"}, moderatorViewer["communityModeration"],
		"the moderator may remove the post and ban its author")

	strangerFeedPost, err := feedPost(strangerClient)
	require.NoError(t, err)
	require.NotNil(t, strangerFeedPost, "the signed-in account with no role reads the post")
	if strangerViewer, ok := strangerFeedPost["viewer"].(map[string]any); ok {
		require.NotContains(t, strangerViewer, "communityModeration", "an account with no role gets no community actions")
	}

	// The moderator removes the post; its removal tombstone offers restore.
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.removePost", map[string]any{
		"subject": post.URI, "code": "spam", "reason": "Selling followers",
		"expectedVersion": communityVersion(moderatorClient, post.URI), "idempotencyKey": testkit.UniqueID(t),
	}, nil), "the moderator removes the post")
	var response struct {
		Posts []map[string]any `json:"posts"`
	}
	require.NoError(t, moderatorClient.Query(ctx, "social.coves.community.post.get",
		url.Values{"uris": {post.URI}}, &response))
	require.Len(t, response.Posts, 1, "post.get returns one union member for one URI")
	tombstone := response.Posts[0]
	require.Equal(t, post.URI, tombstone["uri"])
	require.Equal(t, true, tombstone["removed"], "the moderator reads the #removedPost tombstone")
	require.Equal(t, map[string]any{"communityModeration": []any{"restore"}}, tombstone["viewer"],
		"the moderator may restore the removed post")
}
