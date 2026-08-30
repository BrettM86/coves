package jetstream

// coves:allow-public-host-file: this file tests the JETSTREAM_FEEDS spec parser, and the spec production actually runs names the public Bluesky Jetstream — asserting on any other string would test a topology we do not deploy. ParseFeeds and SubscribeURL are pure string functions: nothing here opens a socket.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFeeds_TwoFeeds_OrderPreserved(t *testing.T) {
	feeds, err := ParseFeeds("bsky=wss://jetstream2.us-east.bsky.network;self=ws://jetstream:6008")
	require.NoError(t, err)
	require.Len(t, feeds, 2)
	assert.Equal(t, Feed{Key: "bsky", BaseURL: "wss://jetstream2.us-east.bsky.network"}, feeds[0])
	assert.Equal(t, Feed{Key: "self", BaseURL: "ws://jetstream:6008"}, feeds[1])
}

func TestParseFeeds_SingleFeedWithWhitespaceAndTrailingSemicolon(t *testing.T) {
	feeds, err := ParseFeeds(" self = ws://localhost:6008 ; ") // coves:allow-host-literal: spec text under test; the padding around the URL is the point, so it stays inline.
	require.NoError(t, err)
	require.Len(t, feeds, 1)
	assert.Equal(t, Feed{Key: "self", BaseURL: "ws://localhost:6008"}, feeds[0]) // coves:allow-host-literal: the trimmed form the parser must produce from the padded input above.
}

func TestParseFeeds_Rejections(t *testing.T) {
	cases := map[string]string{
		"empty spec":              "",
		"missing equals":          "bsky wss://jetstream2.us-east.bsky.network",
		"missing key":             "=ws://jetstream:6008",
		"missing url":             "self=",
		"http scheme":             "self=http://jetstream:6008",
		"missing host":            "self=ws://",
		"query string in base":    "self=ws://jetstream:6008?wantedCollections=social.coves.feed.vote",
		"duplicate key":           "self=ws://a:1;self=ws://b:2",
		"uppercase key":           "Self=ws://jetstream:6008",
		"key with at sign":        "my@feed=ws://jetstream:6008",
		"only empty entries":      ";;",
		"whitespace inside a key": "my feed=ws://jetstream:6008",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFeeds(spec)
			assert.Error(t, err, "spec %q must be rejected", spec)
		})
	}
}

func TestSubscribeURL_AppendsSubscribeAndCollections(t *testing.T) {
	got, err := SubscribeURL("ws://jetstream:6008", []string{
		"social.coves.community.postv2",
	})
	require.NoError(t, err)
	assert.Equal(t, "ws://jetstream:6008/subscribe?wantedCollections=social.coves.community.postv2", got)
}

func TestSubscribeURL_MultipleCollectionsRepeatParameter(t *testing.T) {
	got, err := SubscribeURL("wss://jetstream2.us-east.bsky.network", []string{
		"social.coves.actor.profile",
		"social.coves.actor.block",
	})
	require.NoError(t, err)
	assert.Equal(t,
		"wss://jetstream2.us-east.bsky.network/subscribe?wantedCollections=social.coves.actor.profile&wantedCollections=social.coves.actor.block",
		got)
}

func TestSubscribeURL_ExistingSubscribePathNotDuplicated(t *testing.T) {
	got, err := SubscribeURL("ws://jetstream:6008/subscribe", []string{"social.coves.feed.vote"})
	require.NoError(t, err)
	assert.Equal(t, "ws://jetstream:6008/subscribe?wantedCollections=social.coves.feed.vote", got)
}

func TestFeedConsumerName_PrimaryFeedKeepsLegacyName(t *testing.T) {
	// The bare names key live production cursors; renaming them would orphan
	// the cursors and restart every consumer at the live tail.
	assert.Equal(t, "comments", FeedConsumerName(ConsumerComments, "bsky"))
	assert.Equal(t, "comments@self", FeedConsumerName(ConsumerComments, "self"))
}

func TestWantedCollections_CoversEveryCanonicalConsumer(t *testing.T) {
	for _, consumer := range []string{
		ConsumerUsers, ConsumerCommunities, ConsumerPosts,
		ConsumerAggregators, ConsumerVotes, ConsumerComments,
	} {
		collections, err := WantedCollections(consumer)
		require.NoError(t, err, "consumer %s must have wantedCollections", consumer)
		assert.NotEmpty(t, collections,
			"consumer %s has no wantedCollections; its per-feed URL would subscribe to the whole firehose", consumer)
	}
}

func TestWantedCollections_UnknownConsumerErrors(t *testing.T) {
	// A silent nil here would build a filterless subscribe URL — i.e. the
	// consumer would ingest the ENTIRE firehose. Unknown names must fail closed.
	collections, err := WantedCollections("no-such-consumer")
	assert.Error(t, err)
	assert.Nil(t, collections)
}

func TestWantedCollections_ReturnsACopy(t *testing.T) {
	first, err := WantedCollections(ConsumerPosts)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	first[0] = "mutated.collection"

	second, err := WantedCollections(ConsumerPosts)
	require.NoError(t, err)
	assert.Equal(t, "social.coves.community.postv2", second[0],
		"WantedCollections must return a copy; callers must not be able to mutate the canonical table")
}

func TestWantedAndConsumedCollections_RetireLegacyPost(t *testing.T) {
	wanted, err := WantedCollections(ConsumerPosts)
	require.NoError(t, err)
	consumed := ConsumedCollections()

	assert.NotContains(t, wanted, "social.coves.community.post")
	assert.NotContains(t, consumed, "social.coves.community.post")
	for _, collection := range []string{
		"social.coves.community.postv2",
		"social.coves.community.acceptance",
		"social.coves.community.removal",
	} {
		assert.Contains(t, wanted, collection)
		assert.Equal(t, []string{ConsumerPosts}, consumed[collection])
	}
}

func TestConsumedCollections_MatchesEveryConsumersFilters(t *testing.T) {
	consumed := ConsumedCollections()

	// Derived from the same table the subscribe URLs are, so the two can never
	// disagree: every collection any consumer wants is present, attributed to
	// that consumer.
	for _, consumer := range []string{
		ConsumerUsers, ConsumerCommunities, ConsumerPosts,
		ConsumerAggregators, ConsumerVotes, ConsumerComments,
	} {
		collections, err := WantedCollections(consumer)
		require.NoError(t, err)
		for _, collection := range collections {
			assert.Contains(t, consumed[collection], consumer,
				"collection %s is filtered for by consumer %s but the contract inventory does not say so",
				collection, consumer)
		}
	}

	total := 0
	for _, consumers := range consumed {
		total += len(consumers)
		assert.NotEmpty(t, consumers, "a collection in the inventory with no consumer is a bug in ConsumedCollections")
	}
	assert.Equal(t, total, countFilteredCollections(),
		"the inventory must account for every (consumer, collection) pair and no more")
}

func countFilteredCollections() int {
	n := 0
	for _, collections := range consumerWantedCollections {
		n += len(collections)
	}
	return n
}

func TestConsumedCollections_ReturnsACopy(t *testing.T) {
	first := ConsumedCollections()
	require.NotEmpty(t, first["social.coves.community.postv2"])
	first["social.coves.community.postv2"][0] = "mutated"
	delete(first, "social.coves.feed.vote")

	second := ConsumedCollections()
	assert.Equal(t, []string{ConsumerPosts}, second["social.coves.community.postv2"])
	assert.Equal(t, []string{ConsumerVotes}, second["social.coves.feed.vote"],
		"the contract inventory must not be mutable through a returned map")
}
