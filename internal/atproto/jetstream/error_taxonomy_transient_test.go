//go:build integration

package jetstream

import (
	"context"
	"testing"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

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
	t.Parallel()
	db := testkit.DB(t)

	// The clone starts empty, so the community is absent by construction.
	const ghostCommunity = "did:plc:jstaxghostcommunity"

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

// The post consumer's OTHER ordering gate, and the one with a genuine chance of
// firing in production: BigSky preserves commit order within a repo, not across
// repos, so a post in a community's repo can reach the AppView before the
// author's own social.coves.actor.signup does.
//
// Its classification is the whole point. An unknown author is refused — the
// consumer will not index a post for a DID it has never seen — but refused
// TRANSIENTLY, so the event dead-letters with redrive attempts remaining and
// succeeds on replay once the author arrives. Wrapping it as ErrPermanentEvent
// would look like a tightening of the same security check and would instead
// discard every post that merely arrived early.
//
// The second half is what makes that claim more than a spelling assertion: the
// identical event, replayed after the author is indexed, is accepted. That is
// the redrive the transient classification promises.
func TestPostConsumer_AuthorNotFound_IsTransientAndSucceedsOnReplay(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	const (
		community  = "did:plc:jstaxauthorgatecomm"
		lateAuthor = "did:plc:jstaxlateauthor"
	)
	insertBridgedUser(t, db, "did:plc:jstaxgateowner", "gateowner.test")
	insertBridgedCommunity(t, db, community, "gatecommunity.test", "did:plc:jstaxgateowner")

	us := newMockUserService()
	c := NewPostEventConsumer(postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), us, db)

	event := taxonomyEvent(
		community, "social.coves.community.post", "create", "authorgate",
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": community,
			"author":    lateAuthor,
			"title":     "arrived before its author",
			"createdAt": "2026-01-01T00:00:00Z",
		},
	)

	err := c.HandleEvent(context.Background(), event)
	require.Error(t, err, "a post whose author has never been seen must not be indexed")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"author-not-found is an ORDERING failure and must stay transient so the redrive can succeed")
	assert.Contains(t, err.Error(), "author not found")

	uri := "at://" + community + "/social.coves.community.post/authorgate"
	var rows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE uri = $1`, uri).Scan(&rows))
	require.Equal(t, 0, rows, "the rejected post must not have been indexed")

	// The author signs up; the dead-lettered event is redriven. Both halves of
	// "indexed" are needed and they are not the same thing: the consumer asks
	// the user service, and the posts table's fk_author constraint asks the
	// users table — an author the service knows but the database does not still
	// fails, one layer further down.
	insertBridgedUser(t, db, lateAuthor, "lateauthor.test")
	us.users[lateAuthor] = &users.User{DID: lateAuthor, Handle: "lateauthor.test"}
	require.NoError(t, c.HandleEvent(context.Background(), event),
		"the same event must be accepted once the author is indexed — that is what 'transient' buys")

	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE uri = $1`, uri).Scan(&rows))
	assert.Equal(t, 1, rows, "the redriven post must be indexed exactly once")
}

func TestCommunityConsumer_SubscriptionCommunityNotFound_IsTransient(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	const (
		ghostCommunity = "did:plc:jstaxghostsubcomm"
		subscriber     = "did:plc:jstaxsubscriber"
	)

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
