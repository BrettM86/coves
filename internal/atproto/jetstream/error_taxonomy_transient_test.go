//go:build integration

package jetstream

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"testing"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The unresolved-reference half of the error taxonomy pinned in error_taxonomy_test.go.
// "Not found" here means "not indexed YET": proving these stay retryable
// requires a real database in which the dependency is genuinely absent, so
// they carry the integration tag while their permanent-rejection siblings —
// which fail before any repository access — stay in the unit tier.

func TestCommunityConsumer_SubscriptionCommunityNotFound_IsUnresolved(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	const (
		ghostCommunity = "did:plc:jstaxghostsubcomm"
		subscriber     = "did:plc:jstaxsubscriber"
	)

	c := NewCommunityEventConsumer(postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), "did:web:test.local", true, nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		subscriber, "social.coves.community.subscription", "create", "s1",
		map[string]interface{}{
			"subject":   ghostCommunity,
			"createdAt": "2026-01-01T00:00:00Z",
		},
	))
	require.Error(t, err, "subscription to a not-yet-indexed community must fail")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"subscription community-not-found is an ORDERING failure and must stay redrivable")
	assert.ErrorIs(t, err, ErrUnresolvedReference,
		"…and as an UNRESOLVED REFERENCE: a subscriber naming a made-up DID must not buy in-line retries on the communities lane")
}

func TestPostV2Consumer_NonDIDCommunity_IsPermanent(t *testing.T) {
	// The postv2 path is gated on a wired admissions store before it parses
	// anything, so this one needs the store even though the gate under test
	// fires before the first repository access.
	t.Parallel()
	db := testkit.DB(t)
	c := NewPostEventConsumer(postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), newMockUserService(), db,
		WithAdmissions(postgres.NewAdmissionRepository(db)))
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		"did:plc:someauthor", PostV2Collection, "create", "p1",
		map[string]interface{}{
			"$type":     PostV2Collection,
			"community": "not a did at all",
			"createdAt": "2026-01-01T00:00:00Z",
		},
	))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentEvent,
		"a postv2 community that is not DID-shaped can never be indexed and must not reach the "+
			"unknown-community branch, whose redrive budget exists for communities that have merely not arrived")
	assert.NotErrorIs(t, err, ErrUnresolvedReference)
}
