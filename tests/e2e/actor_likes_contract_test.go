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

const (
	actorGetLikesEndpoint = "social.coves.actor.getLikes"
	likedPostItemType     = "social.coves.actor.getLikes#likedPost"
	likedCommentItemType  = "social.coves.actor.getLikes#likedComment"
)

// actorLikesPage is the slice of social.coves.actor.getLikes the contract
// observes: each item names its union member in $type, carries a post, and a
// liked comment item also carries the comment.
type actorLikesPage struct {
	Feed []struct {
		Type string `json:"$type"`
		Post struct {
			URI    string           `json:"uri"`
			Viewer *viewerVoteState `json:"viewer"`
		} `json:"post"`
		Comment *struct {
			URI    string           `json:"uri"`
			Viewer *viewerVoteState `json:"viewer"`
		} `json:"comment"`
	} `json:"feed"`
	Cursor string `json:"cursor"`
}

// TestActorLikesContract: account A upvotes B's post, then B's nested reply
// (a reply to another comment), with vote records written straight to A's
// PDS. Once Jetstream has indexed both votes (the indexed upvote counts
// move), A, signed in, lists its likes with social.coves.actor.getLikes:
//   - filter omitted, and filter=all: both, newest like first, so the
//     reply's #likedComment item (with the thread's ROOT post) and then the
//     post's #likedPost item;
//   - filter=posts: only the post, as a #likedPost item with A's viewer vote
//     "up" and no comment;
//   - filter=comments: only the reply, as a #likedComment item with A's
//     viewer vote "up" and the thread's ROOT post.
//
// B, signed in, asking for A's likes is refused with 403 NotAllowed, and an
// anonymous caller is refused with 401.
func TestActorLikesContract(t *testing.T) {
	p := newPipeline(t)

	liker := p.IndexedAccount(t, "lka")
	author := p.IndexedAccount(t, "lkb")
	community := indexedCommunity(t, p, "lk", author.DID)
	post := indexedPost(t, p, community, author, "a post to like "+testkit.UniqueID(t))

	topRKey := testkit.TID()
	topRecord := author.PutRecord(t, commentCollection, topRKey, commentRecord(post, post, "a top-level comment"))
	top := strongRef{URI: commentURI(author.DID, topRKey), CID: topRecord.CID}
	replyRKey := testkit.TID()
	replyRecord := author.PutRecord(t, commentCollection, replyRKey, commentRecord(post, top, "a nested reply to like"))
	reply := strongRef{URI: commentURI(author.DID, replyRKey), CID: replyRecord.CID}

	postVote := liker.PutRecord(t, voteCollection, testkit.TID(), voteRecord(post, "up"))
	replyVote := liker.PutRecord(t, voteCollection, testkit.TID(), voteRecord(reply, "up"))
	p.Await(t, "A's upvote to reach the post's indexed upvote count", func() (bool, error) {
		views, err := p.Posts(context.Background(), post.URI)
		if err != nil {
			return false, err
		}
		if len(views) != 1 || views[0].NotFound {
			return false, errPostVanished(post.URI)
		}
		return views[0].Stats.Upvotes == 1, nil
	})
	p.Await(t, "A's upvote to reach the nested reply's indexed upvote count", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		node, ok := thread.find(reply.URI)
		if !ok {
			return false, nil
		}
		return node.Comment.Stats.Upvotes == 1, nil
	}, withReadCadence())

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	actor := url.Values{"actor": {liker.DID}}
	likerClient := p.AppView.As(p.AppView.SignIn(t, liker))

	type likedEntry struct{ Type, CommentURI, PostURI string }
	listEntries := func(query url.Values, what string) []likedEntry {
		t.Helper()
		var page actorLikesPage
		require.NoError(t, likerClient.Query(ctx, actorGetLikesEndpoint, query, &page), what)
		observed := make([]likedEntry, 0, len(page.Feed))
		for _, entry := range page.Feed {
			item := likedEntry{Type: entry.Type, PostURI: entry.Post.URI}
			if entry.Comment != nil {
				item.CommentURI = entry.Comment.URI
			}
			observed = append(observed, item)
		}
		return observed
	}
	bothNewestFirst := []likedEntry{
		{Type: likedCommentItemType, CommentURI: reply.URI, PostURI: post.URI},
		{Type: likedPostItemType, PostURI: post.URI},
	}
	require.Equal(t, bothNewestFirst, listEntries(actor, "A listing its likes with the filter omitted"),
		"with the filter omitted, A's likes hold the reply and then the post, newest like first")
	require.Equal(t, bothNewestFirst,
		listEntries(url.Values{"actor": {liker.DID}, "filter": {"all"}}, "A listing its likes with filter=all"),
		"with filter=all, A's likes hold the reply and then the post, newest like first")

	var postsOnly actorLikesPage
	require.NoError(t, likerClient.Query(ctx, actorGetLikesEndpoint,
		url.Values{"actor": {liker.DID}, "filter": {"posts"}}, &postsOnly), "A listing its own liked posts")
	require.Len(t, postsOnly.Feed, 1, "A's liked posts hold exactly the post A upvoted, not the comment")
	require.Equal(t, likedPostItemType, postsOnly.Feed[0].Type)
	require.Equal(t, post.URI, postsOnly.Feed[0].Post.URI)
	require.Nil(t, postsOnly.Feed[0].Comment, "a liked post item carries no comment")
	require.Equal(t, votedState("up", postVote.URI), observedViewerVote(postsOnly.Feed[0].Post.Viewer),
		"the liked post carries A's own upvote as viewer state")

	var comments actorLikesPage
	require.NoError(t, likerClient.Query(ctx, actorGetLikesEndpoint,
		url.Values{"actor": {liker.DID}, "filter": {"comments"}}, &comments), "A listing its own liked comments")
	require.Len(t, comments.Feed, 1, "A's liked comments hold exactly the reply A upvoted")
	item := comments.Feed[0]
	require.Equal(t, likedCommentItemType, item.Type)
	require.NotNil(t, item.Comment, "a liked comment item carries the comment")
	require.Equal(t, reply.URI, item.Comment.URI)
	require.Equal(t, votedState("up", replyVote.URI), observedViewerVote(item.Comment.Viewer),
		"the liked comment carries A's own upvote as viewer state")
	require.Equal(t, post.URI, item.Post.URI, "a liked nested reply carries its thread's root post, not its parent comment")

	err := p.AppView.As(p.AppView.SignIn(t, author)).Query(ctx, actorGetLikesEndpoint, actor, &actorLikesPage{})
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "NotAllowed", "B asking for A's likes")

	err = p.AppView.Query(ctx, actorGetLikesEndpoint, actor, &actorLikesPage{})
	_ = requireXRPCRefusal(t, err, http.StatusUnauthorized, "AuthenticationRequired", "an anonymous caller asking for A's likes")
}
