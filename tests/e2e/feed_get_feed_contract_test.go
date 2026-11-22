//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The getFeed contract: social.coves.feed.getFeed serving the instance's
// built-in "hot" generator for scope home, end to end.
//
// # WHAT THE APPVIEW PUBLISHES AT BOOT
//
// The AppView describes each built-in generator with a
// social.coves.feed.generator record in the instance account's repo, written
// at startup. The record is read here straight from the PDS, as any atProto
// client would, so the assertion is about what the network can see rather than
// about anything the AppView remembers.
//
// # WHICH AT-URI IS THE FEED
//
// The feed is named at://<SERVER_DID>/social.coves.feed.generator/hot, built
// from the AppView's configured instance DID. In this stack SERVER_DID is
// unset, so the AppView runs on internal/config's default, which this package
// records once as communityInstanceDID. The PDS record's own at-uri carries the
// instance PDS account's did:plc and is a different string; that is expected,
// and it is why the record is looked up by handle and the feed by the instance
// DID.

// generatorCollection and hotGeneratorRKey are spelled out for the same reason
// communityProfileCollection is: they are wire identifiers the AppView and any
// client must independently agree on.
const (
	generatorCollection = "social.coves.feed.generator"
	hotGeneratorRKey    = "hot"
)

// hotFeedURI is the at-uri a client passes as getFeed's feed for the built-in
// hot generator.
func hotFeedURI() string {
	return "at://" + communityInstanceDID + "/" + generatorCollection + "/" + hotGeneratorRKey
}

// feedItem is the slice of a getFeed feed entry this contract observes.
type feedItem struct {
	Post postView `json:"post"`
}

// homeFeedPost reads the first page of getFeed scope=home for viewer, with
// feed set when non-empty, and returns the view of uri if the page serves it.
func homeFeedPost(ctx context.Context, viewer *testkit.AppView, feed, uri string) (postView, bool, error) {
	params := url.Values{"scope": {"home"}, "limit": {"50"}}
	if feed != "" {
		params.Set("feed", feed)
	}
	var page struct {
		Feed []feedItem `json:"feed"`
	}
	if err := viewer.Query(ctx, "social.coves.feed.getFeed", params, &page); err != nil {
		return postView{}, false, err
	}
	for _, item := range page.Feed {
		if item.Post.URI == uri {
			return item.Post, true, nil
		}
	}
	return postView{}, false, nil
}

// TestFeedGetFeedContract_HotBuiltinPublishedAndServedForHome proves both
// halves of the built-in hot feed: the generator record the AppView publishes
// in the instance account's repo, and getFeed scope=home serving a subscribed
// community's post, with the viewer's indexed upvote, whether the client names
// the hot feed or leaves feed out for the instance default.
func TestFeedGetFeedContract_HotBuiltinPublishedAndServedForHome(t *testing.T) {
	p := newPipeline(t)

	instanceHandle := os.Getenv("SERVER_HANDLE")
	require.NotEmpty(t, instanceHandle,
		"SERVER_HANDLE must be set (the runner reads it from .env.ci, as the AppView does); run 'make ci'")

	// The AppView writes the record at startup; the health check may answer
	// before the write lands, so the record is waited for rather than read once.
	var generator struct {
		URI   string `json:"uri"`
		Value struct {
			Kind   string   `json:"kind"`
			Scopes []string `json:"scopes"`
		} `json:"value"`
	}
	p.Await(t, "the AppView's hot generator record to exist in the instance account's repo", func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
		defer cancel()
		params := url.Values{"repo": {instanceHandle}, "collection": {generatorCollection}, "rkey": {hotGeneratorRKey}}
		return testkit.PendingIfNotFound(p.PDS.Anon.Query(ctx, "com.atproto.repo.getRecord", params, &generator))
	})
	require.True(t, strings.HasSuffix(generator.URI, "/"+generatorCollection+"/"+hotGeneratorRKey),
		"getRecord answered uri %q, which is not the hot generator record", generator.URI)
	require.Equal(t, "posts", generator.Value.Kind, "the hot generator record's kind")
	require.Equal(t, []string{"home", "community"}, generator.Value.Scopes, "the hot generator record's scopes")

	// Given a community, an author, and a subscriber signed in once.
	creator := p.IndexedAccount(t, "gfc")
	author := p.IndexedAccount(t, "gfa")
	subscriberAccount := p.IndexedAccount(t, "gfs")
	community := indexedCommunity(t, p, "gf", creator.DID)
	subscriber := p.AppView.As(p.AppView.SignIn(t, subscriberAccount))

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	// When the subscriber subscribes through the AppView.
	var subscribed struct {
		URI string `json:"uri"`
	}
	require.NoError(t, subscriber.Procedure(ctx, "social.coves.community.subscribe",
		map[string]string{"community": community.DID}, &subscribed),
		"social.coves.community.subscribe must accept %s's sealed session", subscriberAccount.Handle)
	p.Await(t, "the subscription to be indexed against the community", func() (bool, error) {
		view, err := p.Community(context.Background(), community.DID)
		if err != nil {
			return false, err
		}
		return view.SubscriberCount == 1, nil
	})

	// And the author posts, the community accepts it, and the subscriber
	// upvotes it with a record written straight to their own PDS repo.
	post := indexedPost(t, p, community, author, "a post for the hot feed "+testkit.UniqueID(t))
	upvote := subscriberAccount.PutRecord(t, voteCollection, testkit.TID(), voteRecord(post, "up"))
	p.Await(t, "the subscriber's upvote to be indexed on the post", func() (bool, error) {
		view, err := p.Post(context.Background(), post.URI)
		if err != nil {
			return false, err
		}
		return view.Stats.Upvotes == 1, nil
	})

	// Then getFeed home serves the post with the subscriber's vote, both with
	// the hot feed named and with feed omitted.
	for _, request := range []struct {
		name string
		feed string
	}{
		{name: "feed = the hot generator's at-uri", feed: hotFeedURI()},
		{name: "feed omitted (instance default)", feed: ""},
	} {
		var served postView
		p.Await(t, "getFeed home, "+request.name+", to serve the subscribed community's post", func() (bool, error) {
			readCtx, readCancel := context.WithTimeout(context.Background(), contractBudget)
			defer readCancel()
			view, found, err := homeFeedPost(readCtx, subscriber, request.feed, post.URI)
			if err != nil {
				return false, err
			}
			served = view
			return found, nil
		})
		require.Equal(t, votedState("up", upvote.URI), observedViewerVote(served.Viewer),
			"getFeed home, %s: the post must carry the subscriber's indexed upvote, with the URI the PDS committed it under",
			request.name)
	}
}
