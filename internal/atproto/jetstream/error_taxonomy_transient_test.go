//go:build integration

package jetstream

import (
	"context"
	"testing"

	"Coves/internal/db/postgres"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The transient half of the error taxonomy pinned in error_taxonomy_test.go.
// "Not found" here means "not indexed YET": proving these stay retryable
// requires a real database in which the dependency is genuinely absent, so
// they carry the integration tag while their permanent-rejection siblings —
// which fail before any repository access — stay in the unit tier.

func TestPostConsumer_CommunityNotFound_IsTransient(t *testing.T) {
	db := setupBridgedTestDB(t)
	defer func() { _ = db.Close() }()

	const ghostCommunity = "did:plc:jstaxghostcommunity"
	// Ensure the community really is absent.
	_, _ = db.Exec("DELETE FROM communities WHERE did = $1", ghostCommunity)

	c := NewPostEventConsumer(postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), newMockUserService(), db)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		ghostCommunity, "social.coves.community.post", "create", "p1",
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": ghostCommunity,
			"author":    "did:plc:someauthor",
			"createdAt": "2026-01-01T00:00:00Z",
		},
	))
	require.Error(t, err, "post for a not-yet-indexed community must fail")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"community-not-found is an ORDERING failure and must stay transient so the redrive can succeed")
	assert.Contains(t, err.Error(), "community not found")
}

func TestCommunityConsumer_SubscriptionCommunityNotFound_IsTransient(t *testing.T) {
	db := setupBridgedTestDB(t)
	defer func() { _ = db.Close() }()

	const (
		ghostCommunity = "did:plc:jstaxghostsubcomm"
		subscriber     = "did:plc:jstaxsubscriber"
	)
	_, _ = db.Exec("DELETE FROM community_subscriptions WHERE user_did = $1", subscriber)
	_, _ = db.Exec("DELETE FROM communities WHERE did = $1", ghostCommunity)

	c := NewCommunityEventConsumer(postgres.NewCommunityRepository(db), "did:web:test.local", true, nil)
	err := c.HandleEvent(context.Background(), taxonomyEvent(
		subscriber, "social.coves.community.subscription", "create", "s1",
		map[string]interface{}{
			"subject":   ghostCommunity,
			"createdAt": "2026-01-01T00:00:00Z",
		},
	))
	require.Error(t, err, "subscription to a not-yet-indexed community must fail")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"subscription community-not-found is an ORDERING failure and must stay transient so the redrive can succeed")
}
