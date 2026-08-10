//go:build integration

package posts_test

import (
	"context"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The production CommunityRepoFactory: how the engine gets a signing session on
// a community's repo, and — the part with teeth — how it decides it has no
// business having one.
//
// # WHY THE REFUSAL IS THE INTERESTING HALF
//
// This is the AppView's only answer to "may I write into this community's
// repository". Get it wrong in the permissive direction and the engine starts
// deciding about communities it does not host: it runs the policy, reaches a
// verdict, and then discovers at the PDS that it holds no keys — after the
// decision, and pointed at whatever host that community's record named.
//
// The tempting implementation is a hosted_by_did comparison, and it is the wrong
// one for a reason that is invisible from the column name. hosted_by_did is
// copied out of a community's own PROFILE RECORD as it is indexed from the
// firehose, so it is a claim made by whoever controls that repo. Anyone can
// publish a community profile naming this AppView as its host. Every fixture
// community in this suite has exactly that shape, which is what the second test
// below exploits.
//
// Credentials cannot be claimed: a stored refresh token exists only because this
// AppView provisioned the account itself, through
// social.coves.community.create. That is the honest question, and it is the only
// one the factory is allowed to ask.

func TestCommunityRepoFactory_OpensAHostedCommunitysRepo(t *testing.T) {
	t.Parallel()

	fixture := newPostFixture(t)
	factory := posts.NewCommunityRepoFactory(fixture.communityService)

	repo, err := factory(context.Background(), fixture.community.DID)
	require.NoError(t, err, "the AppView provisioned this community's account, so it holds its credentials")
	require.NotNil(t, repo)

	assert.Equal(t, fixture.community.DID, repo.DID(),
		"the repo's DID is the authority half of every record URI the writers produce; a client bound to the wrong repo "+
			"would mint acceptance URIs under a DID that never signed them")

	// An authenticated round trip, not merely a constructed struct. A factory
	// that returned a client with an empty or stale token would satisfy every
	// assertion above and then fail on the engine's first write — which is the
	// point at which a verdict has already been reached.
	commit, err := repo.GetLatestCommit(context.Background())
	require.NoError(t, err,
		"the returned client must be able to talk to the repo: an unauthenticated one fails later, after the engine has already decided")
	require.NotNil(t, commit)
}

func TestCommunityRepoFactory_RefusesACommunityItOnlyClaimsToHost(t *testing.T) {
	t.Parallel()

	fixture := newPostFixture(t)

	// A community indexed from the firehose: it names this instance in
	// hosted_by_did — the claim any repo on the network can make — and stores no
	// credentials, because nothing here ever provisioned it.
	label := testkit.UniqueIDWithPrefix(t, "claim")
	claimant, err := fixtures.Community(context.Background(), fixture.db, label, "owner"+label)
	require.NoError(t, err)

	var claimedHost string
	require.NoError(t, fixture.db.QueryRow(
		`SELECT hosted_by_did FROM communities WHERE did = $1`, claimant).Scan(&claimedHost))
	require.NotEmpty(t, claimedHost,
		"fixture: the community must CLAIM a host, or this proves nothing about which signal the factory trusts")

	repo, err := posts.NewCommunityRepoFactory(fixture.communityService)(context.Background(), claimant)

	require.Error(t, err,
		"a community whose credentials this AppView does not hold must be refused, whatever its profile record claims about who hosts it")
	assert.Nil(t, repo)

	// The SPELLING of the refusal is what the driver switches on. A permanent
	// skip tells it to stop offering this subject; a generic error reads as
	// transient, so every pass would re-list every remote community's posts,
	// forever, and the deferrals worth looking at would be buried underneath.
	assert.ErrorIs(t, err, posts.ErrCommunityNotHosted,
		"the refusal must be ErrCommunityNotHosted: not hosting is permanent, and an unclassified error would be retried until the heat death of the queue")
}

func TestCommunityRepoFactory_RefusesACommunityNobodyHasIndexed(t *testing.T) {
	t.Parallel()

	fixture := newPostFixture(t)

	repo, err := posts.NewCommunityRepoFactory(fixture.communityService)(
		context.Background(), "did:plc:aaaaaaaaaanevercommunity")

	require.Error(t, err, "a DID naming no indexed community cannot yield a repo client")
	assert.Nil(t, repo)
}
