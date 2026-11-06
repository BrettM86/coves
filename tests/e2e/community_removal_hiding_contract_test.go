//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityRemovalHidingContract follows an instance community removal
// from a real admin OAuth session to what a subscribed viewer and the post's
// author read on the running AppView:
//
//	before  → author A's indexed post is in subscriber V's timeline and V
//	          reads its thread
//	remove  → V's getComments for the post is 404 RootNotFound and V's
//	          timeline omits it; A's getComments still serves the post, with
//	          status "removed"
//	restore → V sees the post in the timeline and reads its thread again
func TestCommunityRemovalHidingContract(t *testing.T) {
	p := newPipeline(t)
	author := p.IndexedAccount(t, "crha")
	viewer := p.IndexedAccount(t, "crhv")
	community := indexedCommunity(t, p, "crh", viewer.DID)
	authorView := p.AppView.As(p.AppView.SignIn(t, author))
	viewerView := p.AppView.As(p.AppView.SignIn(t, viewer))
	admin := testkit.ModerationAdmin(t, 1)
	adminView := p.AppView.As(p.AppView.SignIn(t, admin))

	require.NoError(t, viewerView.Procedure(t.Context(), "social.coves.community.subscribe",
		map[string]string{"community": community.DID}, nil), "V subscribes to the community")
	p.Await(t, "V's subscription to be indexed against the community", func() (bool, error) {
		view, err := p.Community(context.Background(), community.DID)
		return err == nil && view.SubscriberCount == 1, err
	})
	post := indexedPost(t, p, community, author, "a post in a community the instance removes "+testkit.UniqueID(t))

	thread := func(client *testkit.AppView) error {
		return client.Query(context.Background(), "social.coves.community.comment.getComments",
			url.Values{"post": {post.URI}, "sort": {"new"}}, &threadResponse{})
	}
	visibleToViewer := func(description string) {
		t.Helper()
		p.Await(t, description+": V's timeline serves A's post", func() (bool, error) {
			uris, err := timelineURIs(context.Background(), viewerView)
			return slices.Contains(uris, post.URI), err
		}, withReadCadence())
		require.NoError(t, thread(viewerView), "%s: V reads the post's thread", description)
	}

	visibleToViewer("before the removal")

	var removed communityRemovalMutation
	require.NoError(t, adminView.Procedure(t.Context(), removeCommunityMethod, map[string]any{
		"subject":         community.DID,
		"expectedVersion": "v0",
		"idempotencyKey":  testkit.UniqueID(t),
		"reason":          "social.coves.moderation.defs#reasonSpam",
	}, &removed), "the instance admin removes the community")
	require.Equal(t, "applied", removed.Outcome)

	_ = requireXRPCRefusal(t, thread(viewerView), http.StatusNotFound, "RootNotFound",
		"V's getComments for a post in a removed community")
	uris, err := timelineURIs(t.Context(), viewerView)
	require.NoError(t, err, "V's timeline while the community is removed")
	require.NotContains(t, uris, post.URI, "V's timeline omits the post while its community is removed")

	var own struct {
		Post struct {
			URI    string `json:"uri"`
			Status string `json:"status"`
		} `json:"post"`
	}
	require.NoError(t, authorView.Query(t.Context(), "social.coves.community.comment.getComments",
		url.Values{"post": {post.URI}, "sort": {"new"}}, &own), "A reads the thread of its own post")
	require.Equal(t, post.URI, own.Post.URI)
	require.Equal(t, "removed", own.Post.Status, "A's own post in a removed community carries status removed")

	var restored communityRemovalMutation
	require.NoError(t, adminView.Procedure(t.Context(), restoreCommunityMethod, map[string]any{
		"subject":         community.DID,
		"expectedVersion": removed.State.Version,
		"idempotencyKey":  testkit.UniqueID(t) + "-restore",
		"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
	}, &restored), "the instance admin restores the community")
	require.Equal(t, "applied", restored.Outcome)

	visibleToViewer("after the restore")
}
