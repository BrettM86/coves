//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// accountBanHidingProfile is the slice of actor.getProfile the hiding contract
// reads.
type accountBanHidingProfile struct {
	DID           string `json:"did"`
	AccountStatus string `json:"accountStatus"`
	BanNotice     *struct {
		Reason    string  `json:"reason"`
		ExpiresAt *string `json:"expiresAt"`
	} `json:"banNotice"`
}

// TestAccountBanHidingContract follows an instance account ban from a real
// admin OAuth session to what other viewers and the banned account read on
// the running AppView:
//
//	before → A's indexed post is in the community feed for V and A's directly
//	         written comment is in the thread with its content
//	ban    → for V the post is gone from the community feed, the comment is a
//	         moderator placeholder in getComments, and A's profile is
//	         "suspended"; A itself still lists the post in actor.getPosts and
//	         its own profile carries the ban notice with the ban's reason
//	unban  → V sees the post and the comment again and the profile is no
//	         longer suspended
func TestAccountBanHidingContract(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "abh")
	viewer := p.IndexedAccount(t, "abv")
	community := indexedCommunity(t, p, "abh", viewer.DID)
	post := indexedPost(t, p, community, account, "a post by the account the instance bans "+testkit.UniqueID(t))
	thread := indexedPost(t, p, community, viewer, "a thread the banned account comments on")
	accountView := p.AppView.As(p.AppView.SignIn(t, account))
	viewerView := p.AppView.As(p.AppView.SignIn(t, viewer))
	admin := testkit.ModerationAdmin(t, 1)
	adminView := p.AppView.As(p.AppView.SignIn(t, admin))

	commentText := "a comment by the account the instance bans " + testkit.UniqueID(t)
	rkey := testkit.TID()
	comment := commentURI(account.DID, rkey)
	account.PutRecord(t, commentCollection, rkey, commentRecord(thread, thread, commentText))

	communityFeed := func(client *testkit.AppView) ([]string, error) {
		var out struct {
			Feed []feedItemView `json:"feed"`
		}
		err := client.Query(context.Background(), "social.coves.communityFeed.getCommunity",
			url.Values{"community": {community.DID}, "sort": {"new"}, "limit": {"50"}}, &out)
		uris := make([]string, 0, len(out.Feed))
		for _, item := range out.Feed {
			uris = append(uris, item.Post.URI)
		}
		return uris, err
	}
	// commentNode reads A's comment in the thread as V; until the comment has
	// been seen once, an absent comment is still in flight, and afterwards it
	// must never leave the thread.
	seenComment := false
	commentNode := func() (threadNode, bool, error) {
		var out threadResponse
		if err := viewerView.Query(context.Background(), "social.coves.community.comment.getComments",
			url.Values{"post": {thread.URI}, "sort": {"new"}}, &out); err != nil {
			return threadNode{}, false, err
		}
		node, found := out.find(comment)
		if !found && seenComment {
			return threadNode{}, false, fmt.Errorf("the banned account's comment left the thread: %s", out.uris())
		}
		seenComment = seenComment || found
		return node, found, nil
	}
	profile := func(client *testkit.AppView) (accountBanHidingProfile, error) {
		var out accountBanHidingProfile
		err := client.Query(context.Background(), "social.coves.actor.getProfile", url.Values{"actor": {account.DID}}, &out)
		return out, err
	}
	visibleToViewer := func(description string) {
		t.Helper()
		p.Await(t, description+": V sees A's post in the community feed", func() (bool, error) {
			uris, err := communityFeed(viewerView)
			return slices.Contains(uris, post.URI), err
		}, withReadCadence())
		p.Await(t, description+": V sees A's comment with its content", func() (bool, error) {
			node, found, err := commentNode()
			if err != nil || !found {
				return false, err
			}
			return !node.Comment.IsDeleted && node.Comment.Record["content"] == commentText, nil
		}, withReadCadence())
		p.Await(t, description+": A's profile is not suspended for V", func() (bool, error) {
			view, err := profile(viewerView)
			return err == nil && view.DID == account.DID && view.AccountStatus == "", err
		}, withReadCadence())
	}

	visibleToViewer("before the ban")

	var banned accountBanMutation
	require.NoError(t, adminView.Procedure(t.Context(), banAccountMethod, map[string]any{
		"subject":         account.DID,
		"expectedVersion": "v0",
		"idempotencyKey":  testkit.UniqueID(t),
		"reason":          "social.coves.moderation.defs#reasonSpam",
	}, &banned), "the instance admin bans the account")
	require.Equal(t, "applied", banned.Outcome)

	p.Await(t, "while banned: A's post is absent from V's community feed", func() (bool, error) {
		uris, err := communityFeed(viewerView)
		return err == nil && !slices.Contains(uris, post.URI), err
	}, withReadCadence())
	p.Await(t, "while banned: A's comment is a moderator placeholder for V", func() (bool, error) {
		node, found, err := commentNode()
		if err != nil || !found {
			return false, err
		}
		return node.Comment.IsDeleted && node.Comment.Record == nil &&
			node.Comment.DeletionReason != nil && *node.Comment.DeletionReason == "moderator", nil
	}, withReadCadence())
	suspended, err := profile(viewerView)
	require.NoError(t, err)
	require.Equal(t, "suspended", suspended.AccountStatus, "V gets A's profile in the suspended state")
	require.Nil(t, suspended.BanNotice, "V never sees A's ban notice")

	var own struct {
		Feed []feedItemView `json:"feed"`
	}
	require.NoError(t, accountView.Query(t.Context(), "social.coves.actor.getPosts",
		url.Values{"actor": {account.DID}}, &own))
	ownURIs := make([]string, 0, len(own.Feed))
	for _, item := range own.Feed {
		ownURIs = append(ownURIs, item.Post.URI)
	}
	require.Equal(t, []string{post.URI}, ownURIs, "the banned account still lists its own post")
	ownProfile, err := profile(accountView)
	require.NoError(t, err)
	require.Empty(t, ownProfile.AccountStatus, "the banned account sees its own full profile")
	require.NotNil(t, ownProfile.BanNotice, "the banned account's own profile carries the ban notice")
	require.Equal(t, "social.coves.moderation.defs#reasonSpam", ownProfile.BanNotice.Reason)
	require.Nil(t, ownProfile.BanNotice.ExpiresAt, "a permanent ban's notice has no expiry")

	var unbanned accountBanMutation
	require.NoError(t, adminView.Procedure(t.Context(), unbanAccountMethod, map[string]any{
		"subject":         account.DID,
		"expectedVersion": banned.State.Version,
		"idempotencyKey":  testkit.UniqueID(t) + "-unban",
		"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
	}, &unbanned), "the instance admin lifts the ban")
	require.Equal(t, "applied", unbanned.Outcome)

	visibleToViewer("after the unban")
}
