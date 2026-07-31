package votes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"
)

// Where the vote service gets its answer to "has this user already voted on
// this?", and what it does when it cannot get one.
//
// service_impl_test.go covers the DECISION — create, replace, withdraw — with
// the cache switched off, so the decision is made from a direct read of the
// repo. This file covers the read itself: the cache in front of it, the
// pagination behind it, the fallback between them, and the error taxonomy that
// decides whether a failure is worth retrying. Those are separate concerns and
// the same fake serves both.
//
// The distinction that runs through the whole file is transient versus
// terminal. A vote is a write to somebody else's repository over a token that
// can expire mid-request, so "the PDS said no" has to be classified before it
// is acted on: an authorization failure means stop, anything else means the
// slow path may still work. Getting that backwards is not a lost vote, it is a
// user being told to sign in again because a PDS was briefly busy.

func TestFindExistingVote_ServesFromCacheWithoutReadingTheRepoTwice(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	cache := votes.NewVoteCache(longTTL, nil)
	svc := newService(t, fake, cache)
	ctx := context.Background()

	_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	afterFirst := fake.listCalls
	require.Positive(t, afterFirst, "the first vote must read the repo: the cache starts empty and a "+
		"toggle decision made from an empty cache treats every vote as a first vote")

	// The same tap again. The answer is in the cache now, and going back to the
	// PDS for it would put a full repo pagination on the path of every vote.
	_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	assert.Equal(t, afterFirst, fake.listCalls,
		"the second vote re-read the voter's repo. The cache exists to make the toggle decision O(1); "+
			"if it is not consulted, every arrow tap paginates a repo that grows without bound")
	assert.Empty(t, fake.records, "and the decision itself was still right: the vote was withdrawn")
}

func TestFindExistingVote_CacheIsUpdatedInPlaceByEveryOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a created vote is cached", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		svc := newService(t, fake, cache)

		resp, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		cached := cache.GetVote(testVoterDID, testSubject)
		require.NotNil(t, cached, "a vote the service just wrote must be visible to the viewer-state "+
			"lookups that render the arrow, or the client shows an un-pressed arrow on a vote that exists")
		assert.Equal(t, "up", cached.Direction)
		assert.Equal(t, resp.URI, cached.URI)
		assert.NotEmpty(t, cached.RKey, "without the rkey the next tap cannot withdraw this vote")
	})

	t.Run("a withdrawn vote is uncached", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		svc := newService(t, fake, cache)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		assert.Nil(t, cache.GetVote(testVoterDID, testSubject),
			"a cache still holding a withdrawn vote makes the NEXT tap try to delete an rkey that is "+
				"gone, and the user's attempt to vote reads as another withdrawal")
	})

	t.Run("a direction change leaves the new direction cached under the new rkey", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		svc := newService(t, fake, cache)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		down, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.NoError(t, err)

		cached := cache.GetVote(testVoterDID, testSubject)
		require.NotNil(t, cached)
		assert.Equal(t, "down", cached.Direction)
		assert.Equal(t, down.URI, cached.URI,
			"the cache must name the record that now exists; pointing at the deleted one turns the "+
				"next withdrawal into a delete of nothing")
	})

	t.Run("DeleteVote uncaches too", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		svc := newService(t, fake, cache)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		require.NoError(t, svc.DeleteVote(ctx, voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: subject()}))

		assert.Nil(t, cache.GetVote(testVoterDID, testSubject))
	})
}

// TestCreateVote_FailedDirectionChangeStrandsTheCache pins a KNOWN DEFECT that
// was filed on 2026-07-23 and had no test until now.
//
// A direction change is delete-old then create-new, and the cache is only
// corrected after the create succeeds. If the create fails in between — a PDS
// error, an expired token, a dropped connection — the record is gone from the
// repository and the cache still holds it, with the old direction and the old
// rkey. The toggle-off path does not have this problem: it removes the entry
// immediately after its delete.
//
// The second half of this test is the part that makes it worth having. A
// stranded entry is not merely stale: the user's next tap on the same arrow is
// answered from it, the service deletes an rkey the PDS no longer has, the PDS
// reports success for a missing rkey, and the user is told they withdrew a vote
// they were trying to cast. They now have no vote at all and no error to
// explain it.
//
// Filed: ~/Code/claude-skills/issues/2026-07-23-vote-cache-desync-direction-switch.md
//
// IF THIS TEST FAILED, the defect is FIXED — the direction-change path now
// removes the cache entry alongside its delete. Delete the pin and assert
// instead that the follow-up tap creates a vote rather than reporting a
// withdrawal.
func TestCreateVote_FailedDirectionChangeStrandsTheCache(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	cache := votes.NewVoteCache(longTTL, nil)
	svc := newService(t, fake, cache)
	ctx := context.Background()

	_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	strandedRKey := fake.creates()[0]

	// The switch to "down": the delete lands, the create does not.
	fake.createErr = errors.New("pds refused the create")
	_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
	require.Error(t, err)
	require.Empty(t, fake.records, "the old record really is gone from the repository")

	stranded := cache.GetVote(testVoterDID, testSubject)
	strandedIn := ""
	if stranded != nil {
		strandedIn = stranded.RKey
	}
	require.Equal(t, strandedRKey, strandedIn,
		"expected the cache to still name the deleted record (pinning known defect "+
			"2026-07-23-vote-cache-desync-direction-switch: the direction-change branch deletes the "+
			"old record without removing the cache entry, and only corrects the cache after the "+
			"create succeeds. IF THIS FAILED, the defect is FIXED — invert this step to expect an "+
			"empty cache and close the issue)")

	// The user taps "up" again. There is no vote anywhere, so this should cast
	// one; instead the stale entry makes it read as a withdrawal.
	fake.createErr = nil
	response, err := svc.CreateVote(ctx, voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	require.Empty(t, response.URI,
		"expected the follow-up tap to be reported as a withdrawal of a vote that does not exist "+
			"(pinning known defect 2026-07-23-vote-cache-desync-direction-switch. IF THIS FAILED, "+
			"the defect is FIXED — the tap now creates a vote; assert the returned URI here, drop "+
			"the rest of this pin and close the issue)")
	require.Equal(t, []string{strandedRKey, strandedRKey}, fake.deletes(),
		"the service deleted the same already-absent rkey a second time; a real PDS answers a missing "+
			"rkey with success, which is why nothing surfaces")
	require.Empty(t, fake.records, "and the user ends up with no vote at all")
}

func TestFindExistingVote_FallsBackToPaginationWhenTheCacheCannotBePopulated(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	// One existing "up" vote, written directly into the repo so the cache has
	// something to miss.
	fake.records["3kexisting"] = votes.VoteRecord{
		Type:      "social.coves.feed.vote",
		Direction: "up",
		Subject:   votes.StrongRef{URI: testSubject, CID: testSubjectCI},
		CreatedAt: "2026-07-30T00:00:00Z",
	}

	// A cache whose entries are dead the moment they are written: every
	// populate "succeeds" and IsCached still answers false afterwards, which is
	// exactly the shape of the branch under test — the service must not assume
	// a successful populate means a usable cache.
	svc := newService(t, fake, votes.NewVoteCache(expiredTTL, nil))

	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	require.Equal(t, []string{"3kexisting"}, fake.deletes(),
		"the service missed the existing vote and treated this as a first vote. With an unusable cache "+
			"it must fall through to reading the repo directly, or every re-tap writes a second vote "+
			"record for the same subject")
	require.Empty(t, fake.creates(), "the tap was a withdrawal, not a new vote")
}

func TestFindExistingVote_PaginatesPastTheFirstPage(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	fake.pageSize = 2
	subjects := seedVotes(fake, 5)

	// No cache at all: this is the direct-pagination path, which is what the
	// service uses whenever the cache is unavailable.
	svc := newService(t, fake, nil)

	// The last-seeded vote is on the final page. A reader that stops after one
	// page never sees it.
	last := subjects[len(subjects)-1]
	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: votes.StrongRef{URI: last, CID: "bafyreilast"}, Direction: "up"})
	require.NoError(t, err)

	require.Len(t, fake.deletes(), 1,
		"the existing vote is on the last page of the repo and the service did not find it. A vote repo "+
			"passes 100 records for any regular user, so this is the common case, not the edge")
	require.Empty(t, fake.creates(),
		"finding the vote means withdrawing it; creating instead leaves two vote records for one subject")
}

func TestFindExistingVote_IgnoresRecordsItCannotRead(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	fake.rawRecords = []pds.RecordEntry{
		{URI: "at://" + testVoterDID + "/social.coves.feed.vote/3kbad1",
			Value: map[string]any{"direction": "up", "subject": "not-an-object"}},
		{URI: "at://" + testVoterDID + "/social.coves.feed.vote/3kbad2",
			Value: map[string]any{"direction": "up", "subject": map[string]any{"uri": 42}}},
		{URI: "short-uri",
			Value: map[string]any{"direction": "up", "subject": map[string]any{"uri": testSubject}}},
	}
	svc := newService(t, fake, nil)

	// The third raw record DOES name the subject under test, but its URI has
	// too few segments to yield an rkey. Acting on it would mean issuing a
	// delete with an empty record key.
	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
		votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)

	require.Empty(t, fake.deletes(),
		"the service tried to delete a record whose URI it could not parse an rkey out of — which "+
			"deletes nothing at best and the wrong key at worst")
	require.Len(t, fake.creates(), 1, "with no readable existing vote, this is a first vote")
}

func TestVoteService_ErrorTaxonomy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("an expired session on the create path is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		fake.createErr = pds.ErrSessionExpired
		svc := newService(t, fake, nil)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized,
			"the handler maps this to a 401 so the client can refresh and retry; a generic error would "+
				"be a 500 and the client would give up on a vote that a new token would have accepted")
	})

	t.Run("an expired session on the toggle-off path is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)
		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		fake.deleteErr = pds.ErrUnauthorized
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized)
	})

	t.Run("an expired session on a direction change is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)
		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		fake.deleteErr = pds.ErrForbidden
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized)
		require.Len(t, fake.creates(), 1,
			"a direction change whose delete was refused must not create the replacement: the repo would "+
				"then hold both directions for one subject")
	})

	t.Run("an expired session on the delete path is an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)
		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		fake.deleteErr = pds.ErrSessionExpired
		require.ErrorIs(t, svc.DeleteVote(ctx, voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: subject()}), votes.ErrNotAuthorized)
	})

	t.Run("an authorization failure while reading is not retried on the slow path", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		fake.listErr = pds.ErrUnauthorized
		svc := newService(t, fake, votes.NewVoteCache(longTTL, nil))

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.Error(t, err)
		assert.Equal(t, 1, fake.listCalls,
			"the populate failed on authorization and the service went back to the same PDS with the "+
				"same dead token. The fallback exists for transient failures; retrying a 401 just "+
				"doubles the latency of a request that cannot succeed")
		assert.Empty(t, fake.ops, "and nothing was written on the way to failing")
	})

	t.Run("a read failure is reported rather than treated as no vote", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		fake.listErr = errors.New("pds is down")
		svc := newService(t, fake, nil)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.Error(t, err, "an unreadable repo must not be reported as an empty one: the service would "+
			"write a duplicate vote record for a subject the user has already voted on")
		require.Empty(t, fake.creates())
	})

	t.Run("a PDS client that cannot be built fails the request", func(t *testing.T) {
		t.Parallel()
		svc := votes.NewServiceWithPDSFactory(nil, nil, nil,
			func(context.Context, *oauth.ClientSessionData) (pds.Client, error) {
				return nil, errors.New("dpop key rotated")
			})

		_, createErr := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorContains(t, createErr, "dpop key rotated",
			"the reason the session could not be used must survive into the error, or every auth "+
				"misconfiguration looks identical in the logs")

		deleteErr := svc.DeleteVote(ctx, voter(t, testVoterDID), votes.DeleteVoteRequest{Subject: subject()})
		require.ErrorContains(t, deleteErr, "dpop key rotated")
	})

	t.Run("the production constructor refuses to act without an OAuth client", func(t *testing.T) {
		t.Parallel()
		// NewService is what cmd/server calls. With no OAuth client wired it
		// cannot build a DPoP session for anybody, and the failure has to be an
		// error rather than a nil-pointer panic taking the process down on the
		// first vote after a bad deploy.
		svc := votes.NewService(nil, nil, nil, nil, nil)

		_, err := svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.ErrorContains(t, err, "OAuth client not configured")
	})
}

func TestDeleteVote_RejectsMalformedSubjects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		req  votes.DeleteVoteRequest
	}{
		{"empty subject URI", votes.DeleteVoteRequest{Subject: votes.StrongRef{CID: testSubjectCI}}},
		{"subject URI that is not an AT-URI", votes.DeleteVoteRequest{
			Subject: votes.StrongRef{URI: "https://example.com/post/1", CID: testSubjectCI}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			svc := newService(t, fake, nil)

			err := svc.DeleteVote(context.Background(), voter(t, testVoterDID), tc.req)
			require.ErrorIs(t, err, votes.ErrInvalidSubject)
			require.Zero(t, fake.listCalls,
				"a subject the service cannot parse must be rejected before the repo is read: a "+
					"pagination over every vote a user has ever cast is not the way to answer a 400")
		})
	}

	// Unlike CreateVote, a withdrawal does not need a CID — it names a record
	// that already exists rather than making a strong reference to a subject.
	t.Run("a missing CID is accepted", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)
		_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)

		require.NoError(t, svc.DeleteVote(context.Background(), voter(t, testVoterDID),
			votes.DeleteVoteRequest{Subject: votes.StrongRef{URI: testSubject}}))
	})
}

func TestEnsureCachePopulated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("fetches the voter's repo once", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		subjects := seedVotes(fake, 2)
		cache := votes.NewVoteCache(longTTL, nil)
		svc := newService(t, fake, cache)

		require.NoError(t, svc.EnsureCachePopulated(ctx, voter(t, testVoterDID)))
		require.Equal(t, 1, fake.listCalls)
		require.NotNil(t, cache.GetVote(testVoterDID, subjects[0]))

		// The point of the method is that a request handler can call it
		// unconditionally at the top of a read and pay for it at most once.
		require.NoError(t, svc.EnsureCachePopulated(ctx, voter(t, testVoterDID)))
		assert.Equal(t, 1, fake.listCalls,
			"a second call re-read the repo. Handlers call this per request, so a cache check that does "+
				"not short-circuit turns every feed render into a full vote pagination")
	})

	t.Run("is a no-op when the service has no cache", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		svc := newService(t, fake, nil)

		require.NoError(t, svc.EnsureCachePopulated(ctx, voter(t, testVoterDID)),
			"a service configured without a cache must not fail the requests that try to warm it")
		assert.Zero(t, fake.listCalls)
	})

	t.Run("reports a failed fetch", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, testVoterDID)
		fake.listErr = errors.New("pds is down")
		svc := newService(t, fake, votes.NewVoteCache(longTTL, nil))

		require.Error(t, svc.EnsureCachePopulated(ctx, voter(t, testVoterDID)))
	})

	t.Run("reports a session it cannot build a client for", func(t *testing.T) {
		t.Parallel()
		svc := votes.NewServiceWithPDSFactory(nil, votes.NewVoteCache(longTTL, nil), nil,
			func(context.Context, *oauth.ClientSessionData) (pds.Client, error) {
				return nil, errors.New("session store unreachable")
			})

		require.ErrorContains(t, svc.EnsureCachePopulated(ctx, voter(t, testVoterDID)),
			"session store unreachable")
	})
}

func TestViewerVoteLookups(t *testing.T) {
	t.Parallel()
	other := "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kother"
	unvoted := "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kunvoted"

	populated := func(t *testing.T) votes.Service {
		t.Helper()
		cache := votes.NewVoteCache(longTTL, nil)
		cache.SetVotesForUser(testVoterDID, map[string]*votes.CachedVote{
			testSubject: {Direction: "up", RKey: "3kaaa"},
			other:       {Direction: "down", RKey: "3kbbb"},
		})
		return newService(t, newFakePDS(t, testVoterDID), cache)
	}

	t.Run("one subject", func(t *testing.T) {
		t.Parallel()
		svc := populated(t)

		require.Equal(t, "up", svc.GetViewerVote(testVoterDID, testSubject).Direction)
		assert.Nil(t, svc.GetViewerVote(testVoterDID, unvoted),
			"a subject with no vote must answer nil, not a zero-valued vote: the renderer keys the "+
				"pressed arrow off the direction string and \"\" is neither up nor down")
		assert.Nil(t, svc.GetViewerVote(cacheOtherDID, testSubject),
			"one user's vote must never be served as another's viewer state")
	})

	t.Run("many subjects", func(t *testing.T) {
		t.Parallel()
		svc := populated(t)

		got := svc.GetViewerVotesForSubjects(testVoterDID, []string{testSubject, unvoted, other})
		require.Len(t, got, 2,
			"the batch lookup must return entries only for subjects actually voted on: a map with a "+
				"nil under every unvoted key makes the caller's `if vote, ok :=` say yes to everything")
		assert.Equal(t, "up", got[testSubject].Direction)
		assert.Equal(t, "down", got[other].Direction)
		assert.NotContains(t, got, unvoted)
	})

	t.Run("an unknown user is empty rather than nil", func(t *testing.T) {
		t.Parallel()
		svc := populated(t)

		got := svc.GetViewerVotesForSubjects(cacheOtherDID, []string{testSubject})
		require.NotNil(t, got, "callers range over this map without a nil check")
		require.Empty(t, got)
	})

	t.Run("a service with no cache answers empty rather than panicking", func(t *testing.T) {
		t.Parallel()
		svc := newService(t, newFakePDS(t, testVoterDID), nil)

		assert.Nil(t, svc.GetViewerVote(testVoterDID, testSubject))
		got := svc.GetViewerVotesForSubjects(testVoterDID, []string{testSubject})
		require.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("an expired cache reports no votes at all", func(t *testing.T) {
		t.Parallel()
		cache := votes.NewVoteCache(expiredTTL, nil)
		cache.SetVotesForUser(testVoterDID, map[string]*votes.CachedVote{
			testSubject: {Direction: "up", RKey: "3kaaa"},
		})
		svc := newService(t, newFakePDS(t, testVoterDID), cache)

		// Neither lookup populates — they are the read side, and the write side
		// (EnsureCachePopulated) is what a handler is expected to have called.
		// So a lapsed cache renders every arrow un-pressed rather than raising:
		// worth knowing, because it is silent.
		assert.Nil(t, svc.GetViewerVote(testVoterDID, testSubject))
		assert.Empty(t, svc.GetViewerVotesForSubjects(testVoterDID, []string{testSubject}))
	})
}
