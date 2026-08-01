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

// TestVoteCacheStaleMapSurvivesExpiryAndIsRevivedByTheNextVote pins a KNOWN
// DEFECT.
//
// Expiry does not drop a user's votes, only their deadline: c.votes[userDID]
// stays in the map. SetVote and RemoveVote then both write
// c.expiry[userDID] = now + ttl unconditionally, so the FIRST vote action after
// a lapse republishes the whole stale map as fresh for another full TTL,
// without anyone having re-read the repository.
//
// The service normally hides this, because CreateVote re-populates from the PDS
// whenever IsCached is false and the populate happens before the SetVote. The
// gap is the fallback: a populate that fails for any reason other than
// authorization is logged as a warning and the service continues down the
// direct-pagination path (service_impl.go findExistingVoteWithCache). If that
// second read succeeds — a transient failure, which is the only kind that
// behaves this way — the vote is written and SetVote revives the stale map.
//
// The consequence is not a stale render. The cache is what CreateVote's toggle
// decision is made from, so an entry that outlived the repository makes the
// service delete an rkey that is not there any more; the PDS answers a missing
// rkey with success, and the user's second tap — meant to cast a vote — is
// reported as having withdrawn one.
//
// Filed: ~/Code/claude-skills/issues/2026-07-30-vote-cache-expiry-revived-by-next-vote.md
//
// IF THIS TEST FAILED, the defect is FIXED: either expiry now drops the votes
// as well as the deadline, or SetVote/RemoveVote no longer extend the deadline
// of a lapsed user. Delete the pin and the KNOWN DEFECT note in cache.go, and
// assert the corrected behaviour instead.
func TestVoteCacheStaleMapSurvivesExpiryAndIsRevivedByTheNextVote(t *testing.T) {
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

	// THE PIN. Nothing re-read the repository successfully into the cache, yet
	// the user is cached again and the hour-old entry is being served.
	// Asserted as one tuple so that whichever half of the fix lands — sweeping
	// the votes on lapse, or refusing to extend a lapsed deadline — this is the
	// assertion that fails, and it names the issue when it does.
	revived := cache.GetVote(voterDID, oldPost)
	require.Equal(t,
		[]any{true, "3kgone"},
		[]any{cache.IsCached(voterDID), rkeyOfVote(revived)},
		"expected the lapsed user to be cached again and the hour-old entry to be served (pinning "+
			"known defect 2026-07-30-vote-cache-expiry-revived-by-next-vote: expiry drops the "+
			"deadline but never c.votes[userDID], and SetVote extends the deadline unconditionally, "+
			"so one vote after a lapse republishes the whole stale map for another full TTL. "+
			"IF THIS FAILED, the defect is FIXED — invert this step to expect {false, \"\"} and "+
			"close the issue)")
}

// rkeyOfVote renders a possibly-absent cache entry as a comparable value, so the
// pin above can assert presence and identity in one comparison.
func rkeyOfVote(vote *CachedVote) string {
	if vote == nil {
		return ""
	}
	return vote.RKey
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
