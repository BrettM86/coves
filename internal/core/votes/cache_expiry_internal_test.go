package votes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
)

// What happens to a vote cache after its TTL passes — and what happens to it
// after that.
//
// # WHY THIS ONE FILE IS IN-PACKAGE
//
// Every other test of this type lives in votes_test, because the cache's
// contract is a contract with its callers. This property is not expressible
// there: it needs an entry to be LIVE and then DEAD, and VoteCache computes
// every deadline from time.Now() with no clock to inject. Constructing the
// cache with a negative TTL — the trick the external file uses — makes entries
// dead from birth, which cannot show what a still-present-but-stale map does.
//
// So this file moves the stored deadline instead. For a type whose entire
// notion of time is that map, the deadline IS the clock, and rewinding it is
// the same test with no sleep in it.

// expire moves a user's cached deadline into the past, standing in for the TTL
// elapsing. The votes themselves are left exactly where they are, because that
// is what really happens: nothing sweeps the map when its deadline passes.
func expire(t *testing.T, c *VoteCache, userDID string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.expiry[userDID]
	require.Truef(t, exists, "cannot expire %s: nothing has cached it", userDID)
	c.expiry[userDID] = time.Now().Add(-time.Minute)
}

// A successful fallback write must not certify the stale cache as complete.
func TestVoteCacheFallbackWriteDoesNotReviveExpiredVotes(t *testing.T) {
	t.Parallel()

	const (
		voterDID = "did:plc:eeeeeeeeeeeeeeeeeeeeeeee"
		oldPost  = "at://did:plc:ffffffffffffffffffffffff/social.coves.community.post/3kold"
		newPost  = "at://did:plc:ffffffffffffffffffffffff/social.coves.community.post/3knew"
	)

	cache := NewVoteCache(time.Hour, nil)
	cache.SetVotesForUser(voterDID, map[string]*CachedVote{
		oldPost: {Direction: "up", URI: "at://" + voterDID + "/social.coves.feed.vote/3kgone", RKey: "3kgone"},
	})

	// An hour goes by. The user's vote on oldPost may have been withdrawn from
	// another client in that time — the TTL exists precisely because we cannot
	// know.
	expire(t, cache, voterDID)
	require.False(t, cache.IsCached(voterDID), "the lapse is what makes the service re-read the repo")
	require.Nil(t, cache.GetVote(voterDID, oldPost), "and a lapsed entry is not served while it is lapsed")

	// The user votes on a different post. The populate attempt hits a blip; the
	// pagination fallback right behind it succeeds, so the vote is written.
	repo := &blippingPDS{did: voterDID, failFirstList: true}
	service := NewServiceWithPDSFactory(nil, cache, nil,
		func(context.Context, *oauth.ClientSessionData) (pds.Client, error) { return repo, nil })

	did, err := syntax.ParseDID(voterDID)
	require.NoError(t, err)
	_, err = service.CreateVote(context.Background(), &oauth.ClientSessionData{AccountDID: did},
		CreateVoteRequest{Subject: StrongRef{URI: newPost, CID: "bafyreinewpost"}, Direction: "up"})
	require.NoError(t, err, "the fallback read succeeded, so the vote is written — this is the normal "+
		"recovery path and it must keep working")
	require.Equal(t, 2, repo.listCalls, "the fallback read is what makes this reachable; without it the "+
		"whole request fails and nothing is revived")

	require.False(t, cache.IsCached(voterDID))
	require.Nil(t, cache.GetVote(voterDID, oldPost))
	require.Nil(t, cache.GetVotesForUser(voterDID))
}

func TestVoteCacheExpiredAccessDiscardsMap(t *testing.T) {
	for _, action := range []string{"get all", "get subjects", "is cached", "set", "remove"} {
		t.Run(action, func(t *testing.T) {
			cache := NewVoteCache(time.Hour, nil)
			cache.SetVotesForUser("viewer", map[string]*CachedVote{"old": {Direction: "up"}})
			expire(t, cache, "viewer")
			switch action {
			case "get all":
				require.Nil(t, cache.GetVotesForUser("viewer"))
			case "get subjects":
				require.Nil(t, cache.getVotesForSubjects("viewer", []string{"old"}))
			case "is cached":
				require.False(t, cache.IsCached("viewer"))
			case "set":
				cache.SetVote("viewer", "new", &CachedVote{Direction: "down"})
			case "remove":
				cache.RemoveVote("viewer", "old")
			}
			require.NotContains(t, cache.votes, "viewer")
			require.NotContains(t, cache.expiry, "viewer")
		})
	}
}

// blippingPDS answers the first ListRecords with a transient failure and every
// later one normally. It is the narrowest thing that reaches the fallback
// branch: the cache populate and the direct pagination call the same method, so
// a fake that always fails fails both and the request never gets far enough to
// write anything.
type blippingPDS struct {
	did           string
	listCalls     int
	failFirstList bool
}

func (p *blippingPDS) ListRecords(context.Context, string, int, string) (*pds.ListRecordsResponse, error) {
	p.listCalls++
	if p.failFirstList && p.listCalls == 1 {
		return nil, errors.New("connection reset by peer")
	}
	return &pds.ListRecordsResponse{}, nil
}

func (p *blippingPDS) CreateRecord(_ context.Context, collection, rkey string, _ any) (string, string, error) {
	return "at://" + p.did + "/" + collection + "/" + rkey, "bafycid" + rkey, nil
}

func (p *blippingPDS) DeleteRecord(context.Context, string, string) error { return nil }
func (p *blippingPDS) DID() string                                        { return p.did }
func (p *blippingPDS) HostURL() string                                    { return "https://pds.invalid" }

func (p *blippingPDS) GetRecord(context.Context, string, string) (*pds.RecordResponse, error) {
	panic("the vote service does not call GetRecord")
}

func (p *blippingPDS) PutRecord(context.Context, string, string, any, string) (string, string, error) {
	panic("the vote service does not call PutRecord")
}

func (p *blippingPDS) UploadBlob(context.Context, []byte, string) (*blobs.BlobRef, error) {
	panic("the vote service does not upload blobs")
}
