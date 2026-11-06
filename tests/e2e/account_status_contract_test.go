//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestAccountStatusTakedownContract follows a PDS-host takedown from the PDS
// admin API through relay and Jetstream to what another viewer reads on the
// running AppView:
//
//	before   → V sees A's indexed post in the community feed
//	takedown → A's PDS takes A's repo down; once the #account event reaches
//	           the AppView, A's post is gone from V's community feed and A's
//	           profile reads as suspended to V, with no display name or avatar
//	restore  → the PDS reverses the takedown; V sees A's post again
func TestAccountStatusTakedownContract(t *testing.T) {
	p := newPipeline(t)
	account := p.IndexedAccount(t, "ast")
	viewer := p.IndexedAccount(t, "asv")
	community := indexedCommunity(t, p, "ast", viewer.DID)
	post := indexedPost(t, p, community, account, "a post by the account its PDS takes down "+testkit.UniqueID(t))
	viewerView := p.AppView.As(p.AppView.SignIn(t, viewer))
	displayName := "taken down " + testkit.UniqueID(t)
	account.PutRecord(t, profileCollection, "self", map[string]any{
		"$type": profileCollection, "displayName": displayName,
	})
	awaitDisplayName(t, p, account.DID, displayName, "A's profile display name to be indexed")

	feedContainsPost := func() (bool, error) {
		var out struct {
			Feed []feedItemView `json:"feed"`
		}
		err := viewerView.Query(context.Background(), "social.coves.communityFeed.getCommunity",
			url.Values{"community": {community.DID}, "sort": {"new"}, "limit": {"50"}}, &out)
		if err != nil {
			return false, err
		}
		for _, item := range out.Feed {
			if item.Post.URI == post.URI {
				return true, nil
			}
		}
		return false, nil
	}

	p.Await(t, "before the takedown: V sees A's post in the community feed",
		feedContainsPost, withReadCadence())

	p.PDS.TakeDownRepo(t, account.DID, "e2e-takedown-"+testkit.UniqueID(t))

	p.Await(t, "while taken down: A's post is absent from V's community feed", func() (bool, error) {
		present, err := feedContainsPost()
		return err == nil && !present, err
	}, withReadCadence())

	var takenDownProfile struct {
		DID           string  `json:"did"`
		AccountStatus string  `json:"accountStatus"`
		DisplayName   *string `json:"displayName"`
		Avatar        *string `json:"avatar"`
	}
	err := viewerView.Query(context.Background(), "social.coves.actor.getProfile",
		url.Values{"actor": {account.DID}}, &takenDownProfile)
	require.NoError(t, err)
	require.Equal(t, account.DID, takenDownProfile.DID)
	require.Equal(t, "suspended", takenDownProfile.AccountStatus,
		"while taken down: A's profile reads as suspended to V")
	require.Nil(t, takenDownProfile.DisplayName, "while taken down: V is served no display name for A")
	require.Nil(t, takenDownProfile.Avatar, "while taken down: V is served no avatar for A")

	p.PDS.RestoreRepo(t, account.DID)

	p.Await(t, "after the takedown is reversed: V sees A's post in the community feed again",
		feedContainsPost, withReadCadence())
}
