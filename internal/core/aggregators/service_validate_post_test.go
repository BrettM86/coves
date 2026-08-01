//go:build integration

package aggregators_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ValidateAggregatorPost is the gate every aggregator post passes through
// before anything is written to a community's repository (internal/core/posts
// service.go step 5). It answers two questions in one call — is this aggregator
// still authorized here, and has it spent its hourly quota — and the caller
// turns the difference into a 403 or a 429.
//
// It is tested against the real repository rather than a fake because both
// answers are database reads whose correctness is the point: the quota is a
// COUNT over a rolling window, and a fake would be asserting that the test's own
// arithmetic matches the test's own expectations.
//
// These assertions used to live in tests/integration/aggregator_test.go.

// postValidation is an aggregator that a community has authorized, which is the
// state every case here starts from or deviates from.
type postValidation struct {
	service       aggregators.Service
	repo          aggregators.Repository
	aggregatorDID string
	communityDID  string
}

// newPostValidation indexes an aggregator, a community, and an enabled
// authorization joining them.
//
// The service's community collaborator is nil: ValidateAggregatorPost never
// consults it, and passing a real one would suggest the community's own state
// takes part in this decision when the only thing that does is the
// authorization record.
func newPostValidation(t *testing.T) *postValidation {
	t.Helper()

	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewAggregatorRepository(db)

	aggregatorDID := "did:plc:" + testkit.UniqueID(t)
	require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "RSS Feed Aggregator",
		CreatedAt:   time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   "at://" + aggregatorDID + "/social.coves.aggregator.service/self",
		RecordCID:   "bafyservice",
	}))

	name := testkit.UniqueID(t)
	communityDID := "did:plc:" + name
	_, err := postgres.NewCommunityRepository(db).Create(ctx, &communities.Community{
		DID:         communityDID,
		Handle:      "c-" + name + ".coves.social",
		Name:        name,
		OwnerDID:    communityDID,
		HostedByDID: "did:web:coves.social",
		Visibility:  "public",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	})
	require.NoError(t, err)

	require.NoError(t, repo.CreateAuthorization(ctx, &aggregators.Authorization{
		AggregatorDID: aggregatorDID,
		CommunityDID:  communityDID,
		Enabled:       true,
		CreatedBy:     communityDID,
		CreatedAt:     time.Now(),
		IndexedAt:     time.Now(),
		RecordURI:     "at://" + communityDID + "/social.coves.aggregator.authorization/" + testkit.UniqueID(t),
		RecordCID:     "bafyauthorization",
	}))

	return &postValidation{
		service:       aggregators.NewAggregatorService(repo, nil),
		repo:          repo,
		aggregatorDID: aggregatorDID,
		communityDID:  communityDID,
	}
}

// recordPosts fills the aggregator's hourly quota with n posts.
func (v *postValidation) recordPosts(t *testing.T, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		uri := "at://" + v.communityDID + "/social.coves.community.post/" + testkit.UniqueID(t)
		require.NoError(t, v.repo.RecordAggregatorPost(context.Background(),
			v.aggregatorDID, v.communityDID, uri, "bafypost"))
	}
}

func TestService_ValidateAggregatorPostAcceptsAnAuthorizedAggregator(t *testing.T) {
	t.Parallel()

	v := newPostValidation(t)
	assert.NoError(t, v.service.ValidateAggregatorPost(context.Background(), v.aggregatorDID, v.communityDID))
}

func TestService_ValidateAggregatorPostRefusesAnAggregatorTheCommunityNeverAuthorized(t *testing.T) {
	t.Parallel()

	v := newPostValidation(t)

	// A declared aggregator is not an authorized one. Nothing about being
	// registered with the instance grants reach into a community's repository —
	// only that community's own authorization record does.
	stranger := "did:plc:" + testkit.UniqueID(t)

	err := v.service.ValidateAggregatorPost(context.Background(), stranger, v.communityDID)
	require.Error(t, err)
	assert.True(t, aggregators.IsUnauthorized(err),
		"the caller maps this to a 403, so the classification is the contract; got: %v", err)
}

// The quota is ten posts per community per rolling hour
// (aggregators.RateLimitMaxPosts), and the boundary is the interesting part: the
// tenth post must be allowed and the eleventh refused. An off-by-one here is
// invisible to any test that only checks "enough posts eventually fail".
func TestService_ValidateAggregatorPostEnforcesTheHourlyQuota(t *testing.T) {
	t.Parallel()

	v := newPostValidation(t)
	ctx := context.Background()

	v.recordPosts(t, aggregators.RateLimitMaxPosts-1)
	assert.NoError(t, v.service.ValidateAggregatorPost(ctx, v.aggregatorDID, v.communityDID),
		"the last post inside the quota must still be allowed")

	v.recordPosts(t, 1)
	err := v.service.ValidateAggregatorPost(ctx, v.aggregatorDID, v.communityDID)
	require.Error(t, err)
	assert.True(t, aggregators.IsRateLimited(err),
		"the caller maps this to a 429, which is what tells a well-behaved aggregator to back off; got: %v", err)
}
