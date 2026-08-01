//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What subscribing and blocking actually write to a user's repo.
//
// These are the halves of tests/integration/community_e2e_test.go's
// Subscribe/Unsubscribe/Block/Unblock subtests that nothing else covered. Those
// subtests each did three things at once: called the XRPC endpoint, fetched the
// resulting record from the PDS, and hand-fed a synthetic event to a consumer.
// Two of the three now have better homes — handler behaviour is
// internal/api/handlers/community's subscribe_test.go and block_test.go, and
// consumer behaviour is internal/atproto/jetstream's community_consumer_test.go
// and community_consumer_block_test.go, both of which cover more cases than the
// deleted file did. What was left, and is here, is the write-forward itself: the record
// the service puts in the user's repo.
//
// # WHY THE COLLECTION NAME IS THE POINT
//
// The deleted file shouted about this in capitals at four separate call sites
// ("CRITICAL: Use correct collection name (record type, not XRPC endpoint)"),
// which is a comment doing a test's job. social.coves.community.subscribe is a
// PROCEDURE — an HTTP endpoint — and social.coves.community.subscription is the
// RECORD TYPE it creates; writing the procedure NSID as a collection produces a
// record no consumer is subscribed to, so the write succeeds, the client sees
// 200, and the subscription silently never indexes. Nothing downstream can
// detect it: the consumer tests feed themselves correctly-named events. Only a
// test that reads the repo back can, which is this one.

// writeForwardFixture is the service under test, wired the way production wires
// it except for the PDS client factory: password auth instead of OAuth/DPoP, so
// a test can hold a session without an authorization-code flow.
type writeForwardFixture struct {
	service   communities.Service
	repo      communities.Repository
	user      *testkit.Account
	session   *oauth.ClientSessionData
	community *communities.Community
}

func newWriteForwardFixture(t *testing.T) *writeForwardFixture {
	t.Helper()

	service, repo, pdsServer := newCommunityService(t)
	user := pdsServer.CreateAccount(t, testkit.WithHandlePrefix("wf"))
	did, err := syntax.ParseDID(user.DID)
	require.NoError(t, err)

	// The community is seeded straight into the index rather than provisioned
	// through the service: subscribing and blocking read the community row and
	// write to the USER's repo, so the community's own PDS account is not part
	// of what is under test here, and provisioning one would add an account
	// creation to every case.
	name := testkit.UniqueIDWithPrefix(t, "wfc")
	community, err := repo.Create(context.Background(), &communities.Community{
		DID:          "did:plc:" + name,
		Handle:       "c-" + name + "." + instanceDomain,
		Name:         name,
		OwnerDID:     "did:plc:" + name,
		CreatedByDID: user.DID,
		HostedByDID:  instanceDID,
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	return &writeForwardFixture{
		service: service,
		repo:    repo,
		user:    user,
		session: &oauth.ClientSessionData{
			AccountDID:  did,
			SessionID:   "write-forward-test",
			HostURL:     pdsServer.URL(),
			AccessToken: user.AccessToken,
		},
		community: community,
	}
}

// getRecordErr asks the PDS for a record and returns only the error, so a test
// can assert a record's ABSENCE — Account.GetRecord fails the test on a missing
// record, which is the right default and the wrong tool here.
func getRecordErr(ctx context.Context, account *testkit.Account, collection, rkey string) error {
	return account.XRPC().Query(ctx, "com.atproto.repo.getRecord", url.Values{
		"repo":       {account.DID},
		"collection": {collection},
		"rkey":       {rkey},
	}, nil)
}

// rkeyOf returns the record key an AT-URI ends with.
func rkeyOf(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := syntax.ParseATURI(uri)
	require.NoErrorf(t, err, "the service returned an unparseable record URI %q", uri)
	rkey := parsed.RecordKey().String()
	require.NotEmptyf(t, rkey, "the record URI %q has no record key", uri)
	return rkey
}

func TestService_SubscribeWritesASubscriptionRecord(t *testing.T) {
	t.Parallel()

	f := newWriteForwardFixture(t)
	ctx := context.Background()

	subscription, err := f.service.SubscribeToCommunity(ctx, f.session, f.community.DID, 5)
	require.NoError(t, err)
	require.Equal(t, f.community.DID, subscription.CommunityDID)
	require.Equal(t, f.user.DID, subscription.UserDID)
	require.Equal(t, 5, subscription.ContentVisibility)

	// The record lives in the SUBSCRIBER's repo, not the community's: that is
	// what makes a subscription portable with the user rather than a row the
	// community owns.
	assert.Equal(t,
		"at://"+f.user.DID+"/social.coves.community.subscription/"+rkeyOf(t, subscription.RecordURI),
		subscription.RecordURI)

	record := f.user.GetRecord(t, "social.coves.community.subscription", rkeyOf(t, subscription.RecordURI))
	assert.Equal(t, "social.coves.community.subscription", record.Value["$type"])
	assert.Equal(t, f.community.DID, record.Value["subject"],
		"atProto convention: the community is referenced by a subject field")
	// JSON numbers decode as float64, which is also how the consumer receives
	// them from Jetstream.
	assert.EqualValues(t, 5, record.Value["contentVisibility"])
	assert.NotEmpty(t, record.Value["createdAt"])
}

func TestService_SubscribeClampsContentVisibility(t *testing.T) {
	t.Parallel()

	// The service's clamp is not the consumer's. Out-of-range here means "the
	// client sent nonsense, use the default", so 0 and 9 both become 3;
	// extractContentVisibility in the community consumer clamps INTO the range
	// instead, so a 9 that reaches it from the firehose becomes 5. Both are
	// defensible and they disagree, which is worth having written down: a
	// client that sends 9 gets 3 in its record, and a federated record carrying
	// 9 indexes as 5.
	for _, tc := range []struct {
		name      string
		requested int
		stored    int
	}{
		{name: "zero means unset", requested: 0, stored: 3},
		{name: "negative", requested: -1, stored: 3},
		{name: "above the range", requested: 9, stored: 3},
		{name: "in range", requested: 1, stored: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newWriteForwardFixture(t)
			subscription, err := f.service.SubscribeToCommunity(
				context.Background(), f.session, f.community.DID, tc.requested)
			require.NoError(t, err)

			assert.Equal(t, tc.stored, subscription.ContentVisibility)
			record := f.user.GetRecord(t, "social.coves.community.subscription", rkeyOf(t, subscription.RecordURI))
			assert.EqualValues(t, tc.stored, record.Value["contentVisibility"],
				"the clamped value must be what lands in the repo, not only what is returned")
		})
	}
}

func TestService_UnsubscribeDeletesTheSubscriptionRecord(t *testing.T) {
	t.Parallel()

	f := newWriteForwardFixture(t)
	ctx := context.Background()

	subscription, err := f.service.SubscribeToCommunity(ctx, f.session, f.community.DID, 3)
	require.NoError(t, err)
	rkey := rkeyOf(t, subscription.RecordURI)

	// Unsubscribe finds the record key through the AppView's index, so the
	// subscription has to be indexed first — which the firehose would have done
	// by now in production, and which this test does directly because the
	// consumer is not what is under test.
	_, err = f.repo.SubscribeWithCount(ctx, subscription)
	require.NoError(t, err)

	require.NoError(t, f.service.UnsubscribeFromCommunity(ctx, f.session, f.community.DID))

	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, f.user, "social.coves.community.subscription", rkey)),
		"the subscription record is still in the user's repo after unsubscribing")
}

func TestService_BlockWritesABlockRecord(t *testing.T) {
	t.Parallel()

	f := newWriteForwardFixture(t)
	ctx := context.Background()

	block, err := f.service.BlockCommunity(ctx, f.session, f.community.DID)
	require.NoError(t, err)
	require.Equal(t, f.community.DID, block.CommunityDID)
	require.Equal(t, f.user.DID, block.UserDID)

	rkey := rkeyOf(t, block.RecordURI)
	assert.Equal(t, "at://"+f.user.DID+"/social.coves.community.block/"+rkey, block.RecordURI)

	record := f.user.GetRecord(t, "social.coves.community.block", rkey)
	assert.Equal(t, "social.coves.community.block", record.Value["$type"])
	assert.Equal(t, f.community.DID, record.Value["subject"])
	assert.NotEmpty(t, record.Value["createdAt"])
}

func TestService_UnblockDeletesTheBlockRecord(t *testing.T) {
	t.Parallel()

	f := newWriteForwardFixture(t)
	ctx := context.Background()

	block, err := f.service.BlockCommunity(ctx, f.session, f.community.DID)
	require.NoError(t, err)
	rkey := rkeyOf(t, block.RecordURI)

	// As with unsubscribe, the record key comes from the index.
	_, err = f.repo.BlockCommunity(ctx, block)
	require.NoError(t, err)

	require.NoError(t, f.service.UnblockCommunity(ctx, f.session, f.community.DID))
	assert.True(t, testkit.IsNotFound(getRecordErr(ctx, f.user, "social.coves.community.block", rkey)),
		"the block record is still in the user's repo after unblocking")
}

func TestService_WriteForwardResolvesEveryIdentifierForm(t *testing.T) {
	t.Parallel()

	// Subscribing by handle must reach the same repo write as subscribing by
	// DID. The resolution itself is covered exhaustively at the service level
	// elsewhere; what this adds is that the write path uses the RESOLVED did as
	// the record's subject, rather than storing whatever string the client sent
	// — a subject carrying a handle would be a record no consumer can join to a
	// community.
	f := newWriteForwardFixture(t)
	ctx := context.Background()

	subscription, err := f.service.SubscribeToCommunity(ctx, f.session, f.community.Handle, 3)
	require.NoError(t, err)

	record := f.user.GetRecord(t, "social.coves.community.subscription", rkeyOf(t, subscription.RecordURI))
	assert.Equal(t, f.community.DID, record.Value["subject"],
		"a subscription created by handle must still record the community's DID")
}
