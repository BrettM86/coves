package votes_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

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

func TestCreateVote_FailedDirectionChangePreservesOriginalVote(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		svc := newService(t, fake, cache)
		ctx := context.Background()
		up, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		<-time.After(time.Microsecond) // Distinct TIDs under the virtual clock.
		oldKey := fake.creates()[0]
		fake.createErr = errors.New("pds refused the create")
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.Error(t, err)
		require.Len(t, fake.records, 1, "a rejected replacement must leave the original PDS vote intact")
		require.Equal(t, "up", fake.records[oldKey].Direction)
		require.False(t, cache.IsCached(testVoterDID), "an ambiguous refusal must discard the snapshot")
		listed := fake.listCalls
		fake.createErr = nil
		down, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.NoError(t, err)
		require.NotEmpty(t, down.URI)
		require.NotEqual(t, up.URI, down.URI)
		require.Greater(t, fake.listCalls, listed, "retry must reload authoritative state")
		require.Len(t, fake.records, 1)
		require.Equal(t, "down", cache.GetVote(testVoterDID, testSubject).Direction)
	})
}

func TestCreateVote_DirectionChangeReconcilesLostResponse(t *testing.T) {
	t.Parallel()
	fake := newFakePDS(t, testVoterDID)
	cache := votes.NewVoteCache(longTTL, nil)
	svc := newService(t, fake, cache)
	_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.batchErr = context.DeadlineExceeded
	fake.commitBeforeError = true
	fake.afterBatch = cancel
	down, err := svc.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
	require.NoError(t, err, "a replacement found on the PDS committed despite the lost response")
	require.Positive(t, fake.getCalls)
	require.NotEmpty(t, down.CID)
	cached := cache.GetVote(testVoterDID, testSubject)
	require.NotNil(t, cached)
	require.Equal(t, down.URI, cached.URI)
	require.Equal(t, "down", cached.Direction)
	require.Len(t, fake.records, 1)
}

func TestCreateVote_DirectionChangeUncertainReconciliationInvalidatesCache(t *testing.T) {
	t.Parallel()
	for _, mismatch := range []string{"unavailable", "subject", "direction", "uri", "empty cid", "subject cid", "type", "created at"} {
		t.Run(mismatch, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fake := newFakePDS(t, testVoterDID)
				cache := votes.NewVoteCache(longTTL, nil)
				svc := newService(t, fake, cache)
				_, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
				require.NoError(t, err)
				<-time.After(time.Microsecond) // Distinct TIDs under the virtual clock.
				fake.batchErr = context.DeadlineExceeded
				fake.commitBeforeError = true
				switch mismatch {
				case "unavailable":
					fake.getErr = errors.New("PDS read unavailable")
				case "subject":
					fake.getTransform = func(r *pds.RecordResponse) {
						r.Value["subject"] = map[string]any{"uri": "at://did:plc:other/social.coves.community.post/other", "cid": testSubjectCI}
					}
				case "direction":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["direction"] = "up" }
				case "uri":
					fake.getTransform = func(r *pds.RecordResponse) { r.URI = "at://" + testVoterDID + "/social.coves.feed.vote/another" }
				case "empty cid":
					fake.getTransform = func(r *pds.RecordResponse) { r.CID = "" }
				case "subject cid":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["subject"].(map[string]any)["cid"] = "different" }
				case "type":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["$type"] = "social.coves.feed.other" }
				case "created at":
					fake.getTransform = func(r *pds.RecordResponse) { r.Value["createdAt"] = "different" }

				}
				_, err = svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
				require.Error(t, err, "uncertain or mismatched state must not be reported as success")
				require.False(t, cache.IsCached(testVoterDID), "unknown PDS state must invalidate the complete cache")
				listed := fake.listCalls
				fake.batchErr = nil
				fake.getErr = nil
				fake.getTransform = nil
				response, err := svc.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
				require.NoError(t, err)
				require.Greater(t, fake.listCalls, listed, "the next tap must reload authoritative PDS state")
				require.Empty(t, response.URI, "the committed downvote must toggle off")
				require.Empty(t, fake.records)
			})
		})
	}
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

		fake.batchErr = pds.ErrForbidden
		_, err = svc.CreateVote(ctx, voter(t, testVoterDID),
			votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.ErrorIs(t, err, votes.ErrNotAuthorized)
		require.Equal(t, 1, fake.batchCalls)
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
			func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) {
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
			func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) {
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

	t.Run("an unknown user has unavailable vote state", func(t *testing.T) {
		t.Parallel()
		svc := populated(t)

		got := svc.GetViewerVotesForSubjects(cacheOtherDID, []string{testSubject})
		require.Nil(t, got, "unavailable cache must not be mistaken for confirmed absence")
		require.Empty(t, got)
	})

	t.Run("a service with no cache has unavailable vote state", func(t *testing.T) {
		t.Parallel()
		svc := newService(t, newFakePDS(t, testVoterDID), nil)

		assert.Nil(t, svc.GetViewerVote(testVoterDID, testSubject))
		got := svc.GetViewerVotesForSubjects(testVoterDID, []string{testSubject})
		require.Nil(t, got)
		assert.Empty(t, got)
	})

	t.Run("an expired cache reports unavailable state", func(t *testing.T) {
		t.Parallel()
		cache := votes.NewVoteCache(expiredTTL, nil)
		cache.SetVotesForUser(testVoterDID, map[string]*votes.CachedVote{
			testSubject: {Direction: "up", RKey: "3kaaa"},
		})
		svc := newService(t, newFakePDS(t, testVoterDID), cache)

		// Neither lookup populates — they are the read side, and the write side
		// (EnsureCachePopulated) is what a handler is expected to have called.
		// A lapsed cache must not be interpreted as confirmed absence: callers
		// can preserve indexed state instead of clearing the selected arrow.
		assert.Nil(t, svc.GetViewerVote(testVoterDID, testSubject))
		assert.Nil(t, svc.GetViewerVotesForSubjects(testVoterDID, []string{testSubject}))
	})
}

func TestViewerVoteLookups_ConfirmedAbsenceIsAvailable(t *testing.T) {
	cache := votes.NewVoteCache(longTTL, nil)
	cache.SetVotesForUser(testVoterDID, map[string]*votes.CachedVote{})
	service := newService(t, newFakePDS(t, testVoterDID), cache)
	result := service.GetViewerVotesForSubjects(testVoterDID, []string{testSubject})
	require.NotNil(t, result, "a populated cache with no votes confirms absence")
	require.Empty(t, result)
	cache.Invalidate(testVoterDID)
	require.Nil(t, service.GetViewerVotesForSubjects(testVoterDID, []string{testSubject}), "invalidated state is unknown")
}

func TestViewerVoteLookups_ReturnsIndependentSnapshot(t *testing.T) {
	cache := votes.NewVoteCache(longTTL, nil)
	cache.SetVotesForUser(testVoterDID, map[string]*votes.CachedVote{testSubject: {Direction: "up", URI: "original"}})
	service := newService(t, newFakePDS(t, testVoterDID), cache)
	snapshot := service.GetViewerVotesForSubjects(testVoterDID, []string{testSubject})
	snapshot[testSubject].Direction = "down"
	require.Equal(t, "up", service.GetViewerVotesForSubjects(testVoterDID, []string{testSubject})[testSubject].Direction, "rendering a snapshot must not mutate the cache")
	cache.RemoveVote(testVoterDID, testSubject)
	require.Equal(t, "original", snapshot[testSubject].URI)
}
