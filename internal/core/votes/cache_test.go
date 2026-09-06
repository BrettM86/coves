package votes_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"
)

// The vote cache: what the AppView believes about a user's votes, and where
// that belief comes from.
//
// The cache is not an optimisation bolted onto a correct answer — it IS the
// answer. voteService.findExistingVoteWithCache consults it before deciding
// whether a tap on an arrow creates, replaces or withdraws a record, so a wrong
// cache entry does not make the product slow, it makes it write the wrong thing
// to the user's repository. That is why this file asserts the cache's own
// behaviour rather than only observing it through the service.
//
// # ON EXPIRY, AND WHY THERE ARE NO SLEEPS
//
// VoteCache has no injectable clock: every entry's deadline is computed from
// time.Now() at write time. Tests that need an entry to be visible use a TTL
// far longer than any test run; tests that need one to be invisible construct
// the cache with a TTL already in the past, so the deadline is behind us the
// instant it is written. Nothing waits. The one property that needs an entry to
// be live and then dead — see cache_expiry_internal_test.go — cannot be
// expressed from outside the package at all and is asserted in-package by
// moving the stored deadline, which is the clock as far as this type is
// concerned.
//
// Deliberately not asserted here: the exact instant of expiry. IsCached uses
// Before and GetVotesForUser uses After, so at the deadline itself the two
// disagree for as long as the wall clock stays on that nanosecond. No caller
// can observe that, and a test of it would be a test of scheduling.

const (
	// longTTL outlives any test run, so an entry written under it is live.
	longTTL = time.Hour
	// expiredTTL puts every entry's deadline in the past at the moment it is
	// written. It is how this file says "some time has passed" without passing
	// any.
	expiredTTL = -time.Hour

	cacheUserDID  = "did:plc:cccccccccccccccccccccccc"
	cacheOtherDID = "did:plc:dddddddddddddddddddddddd"
)

func cachedVote(direction, rkey string) *votes.CachedVote {
	return &votes.CachedVote{
		Direction: direction,
		URI:       "at://" + cacheUserDID + "/social.coves.feed.vote/" + rkey,
		RKey:      rkey,
	}
}

func TestVoteCache_EmptyCacheKnowsNothing(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(longTTL, nil)

	assert.False(t, cache.IsCached(cacheUserDID),
		"a user nobody has fetched must report as uncached, or the service will skip the fetch and "+
			"decide the toggle from an empty map — every vote would look like a first vote")
	assert.Nil(t, cache.GetVotesForUser(cacheUserDID))
	assert.Nil(t, cache.GetVote(cacheUserDID, testSubject))
}

func TestVoteCache_StoresAndServesAUsersVotes(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(longTTL, nil)

	cache.SetVotesForUser(cacheUserDID, map[string]*votes.CachedVote{
		testSubject: cachedVote("up", "3kaaa"),
	})

	require.True(t, cache.IsCached(cacheUserDID))
	got := cache.GetVote(cacheUserDID, testSubject)
	require.NotNil(t, got)
	assert.Equal(t, "up", got.Direction)
	assert.Equal(t, "3kaaa", got.RKey,
		"the rkey is the only thing the service can delete a vote by; a cache entry without one "+
			"turns a withdrawal into a no-op")

	assert.Nil(t, cache.GetVote(cacheUserDID, "at://did:plc:x/social.coves.community.post/never"),
		"a subject the user has not voted on must be absent, not zero-valued")
	assert.False(t, cache.IsCached(cacheOtherDID),
		"caching one user's votes must not make another user look cached: the second user would then "+
			"be answered from an empty map without anyone fetching their repo")
}

func TestVoteCache_SetVoteDoesNotEstablishCompleteness(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(longTTL, nil)

	cache.SetVote(cacheUserDID, testSubject, cachedVote("down", "3kbbb"))

	require.False(t, cache.IsCached(cacheUserDID))
	require.Nil(t, cache.GetVotesForUser(cacheUserDID))
}

func TestVoteCache_RemoveVoteDropsOnlyThatSubject(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(longTTL, nil)
	other := "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kother"

	cache.SetVotesForUser(cacheUserDID, map[string]*votes.CachedVote{
		testSubject: cachedVote("up", "3kaaa"),
		other:       cachedVote("up", "3kccc"),
	})
	cache.RemoveVote(cacheUserDID, testSubject)

	assert.Nil(t, cache.GetVote(cacheUserDID, testSubject))
	assert.NotNil(t, cache.GetVote(cacheUserDID, other),
		"withdrawing a vote on one post must leave the user's other votes alone")
	assert.True(t, cache.IsCached(cacheUserDID),
		"removing the last-touched vote must not un-cache the user, or the next lookup pays a full "+
			"repo walk for a repo we just described")
}

func TestVoteCache_RemoveVoteOnAnUnknownUserIsHarmless(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(longTTL, nil)

	cache.RemoveVote(cacheUserDID, testSubject)

	// The guard inside RemoveVote exists so this does not create a map. If it
	// did, the user would be reported as cached — with no votes in it — and
	// every subsequent vote of theirs would be treated as a first vote.
	assert.False(t, cache.IsCached(cacheUserDID),
		"removing a vote for a user who was never fetched must not make them look cached")
	assert.Nil(t, cache.GetVotesForUser(cacheUserDID))
}

func TestVoteCache_InvalidateForgetsTheUserEntirely(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(longTTL, nil)

	cache.SetVotesForUser(cacheUserDID, map[string]*votes.CachedVote{
		testSubject: cachedVote("up", "3kaaa"),
	})
	cache.SetVotesForUser(cacheOtherDID, map[string]*votes.CachedVote{
		testSubject: cachedVote("down", "3kddd"),
	})

	cache.Invalidate(cacheUserDID)

	assert.False(t, cache.IsCached(cacheUserDID))
	assert.Nil(t, cache.GetVotesForUser(cacheUserDID))

	cache.SetVote(cacheUserDID, testSubject, cachedVote("up", "3keee"))
	assert.Nil(t, cache.GetVotesForUser(cacheUserDID),
		"a single write after invalidation cannot establish a complete cache")

	assert.True(t, cache.IsCached(cacheOtherDID),
		"invalidating one user must not evict another")
}

func TestVoteCache_ExpiredEntriesAreInvisible(t *testing.T) {
	t.Parallel()
	cache := votes.NewVoteCache(expiredTTL, nil)

	cache.SetVotesForUser(cacheUserDID, map[string]*votes.CachedVote{
		testSubject: cachedVote("up", "3kaaa"),
	})

	assert.False(t, cache.IsCached(cacheUserDID),
		"an expired user must report as uncached, which is what makes the service re-fetch their repo")
	assert.Nil(t, cache.GetVotesForUser(cacheUserDID))
	assert.Nil(t, cache.GetVote(cacheUserDID, testSubject),
		"a stale entry must not be served: the user may have voted from another client since, and the "+
			"service would delete an rkey that no longer exists")
}

func TestVoteCache_PopulateFromPDS(t *testing.T) {
	t.Parallel()

	t.Run("reads every page of the voter's repo", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, cacheUserDID)
		fake.pageSize = 2
		subjects := seedVotes(fake, 5)

		cache := votes.NewVoteCache(longTTL, nil)
		require.NoError(t, cache.FetchAndCacheFromPDS(context.Background(), fake))

		require.Len(t, cache.GetVotesForUser(cacheUserDID), len(subjects),
			"the cache stopped before the end of the repo. Everything past the first page would then "+
				"read as 'not voted', and the next tap on one of those posts would write a duplicate "+
				"vote record")
		require.Greater(t, fake.listCalls, 1, "a five-record repo read two at a time is more than one call")
		for _, subjectURI := range subjects {
			require.NotNilf(t, cache.GetVote(cacheUserDID, subjectURI), "subject %s is missing", subjectURI)
		}
	})

	t.Run("keys the cache by the voter the PDS client speaks for", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, cacheUserDID)
		seedVotes(fake, 1)

		cache := votes.NewVoteCache(longTTL, nil)
		require.NoError(t, cache.FetchAndCacheFromPDS(context.Background(), fake))

		assert.True(t, cache.IsCached(cacheUserDID))
		assert.False(t, cache.IsCached(cacheOtherDID),
			"the cache took its key from somewhere other than the authenticated client's DID; one "+
				"user's votes would be served as another's")
	})

	t.Run("records the rkey each vote can be deleted by", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, cacheUserDID)
		subjects := seedVotes(fake, 1)

		cache := votes.NewVoteCache(longTTL, nil)
		require.NoError(t, cache.FetchAndCacheFromPDS(context.Background(), fake))

		got := cache.GetVote(cacheUserDID, subjects[0])
		require.NotNil(t, got)
		assert.NotEmpty(t, got.RKey, "an entry with no rkey cannot be withdrawn")
		assert.Equal(t, got.URI, "at://"+cacheUserDID+"/social.coves.feed.vote/"+got.RKey,
			"the rkey must be the last segment of the record's own URI, not a re-derivation")
	})

	t.Run("skips records that are not votes it can act on", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, cacheUserDID)
		good := seedVotes(fake, 1)[0]
		fake.rawRecords = []pds.RecordEntry{
			{ // subject is not an object
				URI:   "at://" + cacheUserDID + "/social.coves.feed.vote/3kbad1",
				Value: map[string]any{"direction": "up", "subject": "at://somewhere"},
			},
			{ // subject.uri is not a string
				URI:   "at://" + cacheUserDID + "/social.coves.feed.vote/3kbad2",
				Value: map[string]any{"direction": "up", "subject": map[string]any{"uri": 42}},
			},
			{ // subject.uri is empty
				URI:   "at://" + cacheUserDID + "/social.coves.feed.vote/3kbad3",
				Value: map[string]any{"direction": "up", "subject": map[string]any{"uri": ""}},
			},
			{ // no direction: nothing to render and nothing to compare a re-tap against
				URI:   "at://" + cacheUserDID + "/social.coves.feed.vote/3kbad4",
				Value: map[string]any{"subject": map[string]any{"uri": "at://did:plc:x/c/1"}},
			},
			{ // no subject at all
				URI:   "at://" + cacheUserDID + "/social.coves.feed.vote/3kbad5",
				Value: map[string]any{"direction": "down"},
			},
		}

		cache := votes.NewVoteCache(longTTL, nil)
		require.NoError(t, cache.FetchAndCacheFromPDS(context.Background(), fake),
			"one unreadable record in a repo must not cost the user their whole vote state: the repo "+
				"is user-writable and a third-party client can put anything in it")

		all := cache.GetVotesForUser(cacheUserDID)
		require.Len(t, all, 1, "only the well-formed vote belongs in the cache")
		require.NotNil(t, all[good])
	})

	t.Run("reports an expired session as an authorization failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, cacheUserDID)
		fake.listErr = pds.ErrSessionExpired

		cache := votes.NewVoteCache(longTTL, nil)
		err := cache.FetchAndCacheFromPDS(context.Background(), fake)

		require.ErrorIs(t, err, votes.ErrNotAuthorized,
			"the service only skips its slow fallback path for an authorization failure, because that "+
				"is the one failure a second attempt cannot fix; a generic error here would send it "+
				"straight back to the same PDS with the same dead token")
		assert.False(t, cache.IsCached(cacheUserDID), "a failed fetch must not leave the user marked cached")
	})

	t.Run("reports any other PDS failure without caching an empty repo", func(t *testing.T) {
		t.Parallel()
		fake := newFakePDS(t, cacheUserDID)
		fake.listErr = errors.New("pds is down")

		cache := votes.NewVoteCache(longTTL, nil)
		err := cache.FetchAndCacheFromPDS(context.Background(), fake)

		require.Error(t, err)
		assert.NotErrorIs(t, err, votes.ErrNotAuthorized,
			"a PDS outage is not an authorization failure, and mapping it to one would surface as a "+
				"401 telling the user to sign in again")
		assert.False(t, cache.IsCached(cacheUserDID),
			"caching 'no votes' after a failed read would make every one of this user's existing votes "+
				"invisible for a full TTL, and their next tap would write a duplicate")
	})
}

// seedVotes puts n distinct votes in the fake repo and returns their subject
// URIs, in the order they were written.
func seedVotes(fake *fakePDS, n int) []string {
	subjects := make([]string, 0, n)
	for i := 0; i < n; i++ {
		subjectURI := "at://did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/social.coves.community.post/3kseed" + string(rune('a'+i))
		rkey := "3ktid" + string(rune('a'+i))
		fake.records[rkey] = votes.VoteRecord{
			Type:      "social.coves.feed.vote",
			Direction: "up",
			Subject:   votes.StrongRef{URI: subjectURI, CID: "bafyseed" + string(rune('a'+i))},
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		subjects = append(subjects, subjectURI)
	}
	return subjects
}
