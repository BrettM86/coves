//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The subscription domain's pipeline contract: the ingestion proof for
// social.coves.community.subscription.
//
// # A SUBSCRIPTION IS A RECORD IN THE SUBSCRIBER'S REPO, NAMING A COMMUNITY
//
// Like a vote and unlike a post, the record lives in the actor's own
// repository, and the consumer takes the subscriber's DID from the commit's
// repo rather than from the record. The record's only payload is `subject` —
// the community DID — plus a contentVisibility level. The XRPC procedures
// social.coves.community.subscribe/unsubscribe are just endpoints that create
// and delete these records; the collection is what the AppView indexes.
//
// # IT IS OBSERVED ON TWO ENDPOINTS, AND THAT IS DELIBERATE
//
// A subscription moves two numbers, maintained in two completely different
// ways, and asserting both is what makes this contract more than a round-trip:
//
//   - social.coves.community.get's subscriberCount is a STORED COLUMN.
//     SubscribeWithCount does `UPDATE communities SET subscriber_count =
//     subscriber_count + 1` in the same transaction as the row insert, and
//     UnsubscribeWithCount decrements it with a GREATEST(0, …) floor.
//   - social.coves.actor.getProfile's stats.communityCount is a LIVE COUNT(*)
//     over community_subscriptions (user_repo.go).
//
// A stored counter and a recomputed one can drift apart, and the stored one is
// the one that can be wrong: a missed increment, a double decrement, or an
// unsubscribe that removes the row without adjusting the column all leave the
// COUNT(*) correct and the column silently off. The floor at zero then hides
// the drift from anyone reading the number alone. Checking the two together is
// the cheapest possible detector for that whole class, and it costs one extra
// request.
//
// # THE FAN-OUT THIS CONTRACT CANNOT REACH, STATED PLAINLY
//
// The interesting thing a subscription DOES — a post from a subscribed
// community appearing in the subscriber's feed — is served by exactly one
// endpoint, social.coves.feed.getTimeline, and it is behind RequireAuth. §3.4b's
// standing limitation applies: nothing outside the browser OAuth callback mints
// a credential RequireAuth accepts, so T2 cannot call it at all. Every other
// public surface was checked before writing that sentence:
// social.coves.feed.getDiscover explicitly does not filter by subscription
// ("show ALL posts from ALL communities", discover_repo.go),
// communityFeed.getCommunity filters by community and never by subscriber, and
// community.list's ?subscribed=true filter 401s without a session.
//
// So the fan-out is covered at T1 (internal/core/timeline/timeline_feed_test.go,
// against the repo's own join) and becomes reachable here when the Phase-5
// test-only session mint lands. It is named here rather than quietly omitted, because
// "the subscription contract covers the timeline" would otherwise stay true in
// everyone's memory and false in the code — the same note journey_test.go makes
// about the step it had to substitute.
//
// # THE 401 MATRIX IS NOT REPEATED HERE
//
// social.coves.community.subscribe and .unsubscribe are the write endpoints for
// this collection, and both are already asserted to answer 401 to a
// session-less client in TestCommunityAPIContract's boundary matrix, which
// enumerates every NSID RegisterCommunityRoutes puts behind RequireAuth. Adding
// a second copy here would make that matrix's completeness harder to see, not
// easier — the value of listing them in one place is that a route added without
// middleware has exactly one test to escape.
const subscriptionCollection = "social.coves.community.subscription"

// subscriptionRecord builds a social.coves.community.subscription record in the
// shape internal/core/communities writes it (service.go's Subscribe), so the
// consumer parses exactly what production hands it.
//
// contentVisibility is the subscriber's preferred content level for this
// community, clamped by the consumer to 1-5 with a default of 3. The clamping
// is behavioural breadth and belongs at T1 (§3.4 rule 3); what a contract needs
// is a value the record carries through unchanged.
func subscriptionRecord(communityDID string, contentVisibility int) map[string]any {
	return map[string]any{
		"$type":             subscriptionCollection,
		"subject":           communityDID,
		"contentVisibility": contentVisibility,
		"createdAt":         time.Now().UTC().Format(time.RFC3339),
	}
}

// subscriptionCounts is the pair of numbers this contract watches: the
// community's stored subscriber count and the subscriber's recomputed community
// count, read from two different endpoints.
type subscriptionCounts struct {
	Subscribers int // social.coves.community.get → subscriberCount (stored column)
	Communities int // social.coves.actor.getProfile → stats.communityCount (COUNT(*))
}

// counts reads both numbers. Two requests, so the pair is very slightly
// non-atomic — which is not a problem for the assertions here, because every
// one of them is made after a wait has already settled one of the two.
func (p *pipeline) counts(ctx context.Context, communityDID, subscriberDID string) (subscriptionCounts, error) {
	community, err := p.Community(ctx, communityDID)
	if err != nil {
		return subscriptionCounts{}, err
	}
	var profile struct {
		Stats struct {
			CommunityCount int `json:"communityCount"`
		} `json:"stats"`
	}
	if err := p.AppView.Query(ctx, "social.coves.actor.getProfile",
		url.Values{"actor": {subscriberDID}}, &profile); err != nil {
		return subscriptionCounts{}, err
	}
	return subscriptionCounts{
		Subscribers: community.SubscriberCount,
		Communities: profile.Stats.CommunityCount,
	}, nil
}

// TestCommunitySubscriptionIngestion is the pipeline proof for subscriptions.
//
// coves:ingestion-contract social.coves.community.subscription
//
// Every record is written straight into the subscriber's own repo with the
// subscriber's session, and every observation is made through two serving
// endpoints at once (see the file's opening note):
//
//	subscribe        → both counts reach exactly one
//	duplicate rkey   → a second subscription record for the same community does NOT
//	                   double either count, and STAYS undoubled (Holds)
//	unsubscribe      → both counts return to zero, and STAY there (Holds, §3.4a)
//	stale unsubscribe→ deleting the superseded record does not drive the count negative
func TestCommunitySubscriptionIngestion(t *testing.T) {
	p := newPipeline(t)

	creator := p.IndexedAccount(t, "sc")
	subscriber := p.IndexedAccount(t, "ss")
	community := indexedCommunity(t, p, "sb", creator.DID)

	ctx := context.Background()

	// The community is fresh — provisioned and indexed by this test — so an
	// exact count is safe even on a kept stack where earlier runs have left
	// their own communities behind. That is the whole reason the fixture is
	// per-contract rather than shared.
	before, err := p.counts(ctx, community.DID, subscriber.DID)
	require.NoError(t, err)
	require.Equal(t, subscriptionCounts{}, before,
		"a newly indexed community has no subscribers and a fresh account subscribes to nothing; "+
			"non-zero here means the fixture is not as isolated as this contract assumes")

	awaitCounts := func(description string, want subscriptionCounts) {
		t.Helper()
		p.Await(t, description, func() (bool, error) {
			got, err := p.counts(context.Background(), community.DID, subscriber.DID)
			if err != nil {
				return false, err
			}
			return got == want, nil
		})
	}
	holdCounts := func(description string, want subscriptionCounts) {
		t.Helper()
		p.Holds(t, description, func() (bool, error) {
			got, err := p.counts(context.Background(), community.DID, subscriber.DID)
			if err != nil {
				return false, err
			}
			return got == want, nil
		})
	}

	one := subscriptionCounts{Subscribers: 1, Communities: 1}

	// ---- subscribe -----------------------------------------------------------
	first := testkit.TID()
	subscriber.PutRecord(t, subscriptionCollection, first, subscriptionRecord(community.DID, 4))
	awaitCounts("the directly-written subscription to reach both count surfaces via the consumers", one)

	// ---- a second subscription record, new rkey ------------------------------
	// The idempotency case that matters, and the one no test in the tree covered
	// before this: SubscribeWithCount's ON CONFLICT keys on (user_did,
	// community_did), NOT on the record URI, and it decides whether to increment
	// from the `xmax = 0` discriminator — "did this statement actually insert a
	// row". So a second record for the same community re-points the stored
	// record_uri at the newer record and does not touch the count.
	//
	// Why a client produces one at all: nothing stops a user subscribing from
	// two devices, and nothing in atProto makes an rkey a natural key. The
	// re-pointing is deliberate — it is what makes a redriven delete of the OLD
	// record a no-op instead of a silent unsubscribe — and the step after next
	// asserts that half.
	//
	// Holds rather than a single read, because "the count did not double" is a
	// claim about an event that has already been processed: an eventually-check
	// would pass in the window before the second increment landed.
	second := testkit.TID()
	subscriber.PutRecord(t, subscriptionCollection, second, subscriptionRecord(community.DID, 2))
	holdCounts("a second subscription record for the same community to leave both counts at one", one)

	// ---- unsubscribe ---------------------------------------------------------
	// The NEWER record, which is the one the stored row now points at. A delete
	// commit carries no record body, so the consumer finds the community by
	// looking the subscription up by URI — which is why deleting this one works
	// and deleting the older one (below) does not.
	subscriber.DeleteExistingRecord(t, subscriptionCollection, second)
	awaitCounts("the deleted subscription to leave both count surfaces", subscriptionCounts{})
	holdCounts("the unsubscribe to stay unsubscribed", subscriptionCounts{})

	// ---- the superseded record's delete is a no-op ---------------------------
	// `first` was superseded when `second` re-pointed the row, and the row is
	// gone entirely now. Its delete must not decrement anything. The GREATEST(0,
	// …) floor would hide a spurious decrement from zero, so this is asserted
	// where it can be seen: with a second, unrelated subscriber's live
	// subscription present, which a spurious decrement has something to take
	// away from.
	//
	// This is the redrive-safety property the consumer's own comment claims
	// ("the redriven delete of the old URI then finds no row here and is skipped
	// instead of tearing down the valid subscription"), reached from the outside.
	other := p.IndexedAccount(t, "so")
	other.PutRecord(t, subscriptionCollection, testkit.TID(), subscriptionRecord(community.DID, 3))
	p.Await(t, "a second subscriber, so a spurious decrement is visible", func() (bool, error) {
		view, err := p.Community(context.Background(), community.DID)
		if err != nil {
			return false, err
		}
		return view.SubscriberCount == 1, nil
	})

	subscriber.DeleteExistingRecord(t, subscriptionCollection, first)
	p.Holds(t, "deleting the superseded subscription record to leave the other subscriber alone",
		func() (bool, error) {
			view, err := p.Community(context.Background(), community.DID)
			if err != nil {
				return false, err
			}
			return view.SubscriberCount == 1, nil
		})

	// And the first subscriber is still at zero: the delete did not resurrect
	// anything either.
	final, err := p.counts(ctx, community.DID, subscriber.DID)
	require.NoError(t, err)
	require.Equal(t, subscriptionCounts{Subscribers: 1, Communities: 0}, final,
		"after unsubscribing, the community keeps the other subscriber's count and the "+
			"unsubscribed actor's own community count is zero")
}
