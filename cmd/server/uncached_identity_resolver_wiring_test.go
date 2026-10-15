package main

import (
	"database/sql"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/oauth"
	"Coves/internal/config"

	"github.com/stretchr/testify/require"
)

// unusedUncachedIdentityResolver satisfies identity.UncachedResolver without
// any implementation: building the consumers must not resolve anything, and a
// call would panic on the nil embedded interface rather than reach a network.
type unusedUncachedIdentityResolver struct{ identity.UncachedResolver }

// unusedIdentityResolver is the cached-resolver counterpart, present only
// because the community consumer refuses to be built without one.
type unusedIdentityResolver struct{ identity.Resolver }

// Without this test, deleting either WithUserUncachedIdentityResolver or
// WithCommunityUncachedIdentityResolver from registerFeedConsumers leaves every
// test green while production answers identity events and origin display
// fields from a cached identity.
func TestRegisterFeedConsumers_UserAndCommunityConsumersGetTheUncachedIdentityResolver(t *testing.T) {
	a := &application{
		cfg:                      &config.Config{},
		identityResolver:         unusedIdentityResolver{},
		uncachedIdentityResolver: unusedUncachedIdentityResolver{},
		oauthStore:               oauth.NewMobileAwareStoreWrapper(nil),
	}

	consumers, err := a.registerFeedConsumers()
	require.NoError(t, err)

	var userConsumer *jetstream.UserEventConsumer
	var communityConsumer *jetstream.CommunityEventConsumer
	for _, consumer := range consumers {
		switch consumer.name {
		case jetstream.ConsumerUsers:
			userConsumer = consumer.handler.(*jetstream.UserEventConsumer)
		case jetstream.ConsumerCommunities:
			communityConsumer = consumer.handler.(*jetstream.CommunityEventConsumer)
		}
	}
	require.NotNil(t, userConsumer, "the user consumer must be registered")
	require.NotNil(t, communityConsumer, "the community consumer must be registered")

	require.True(t, userConsumer.HasUncachedIdentityResolver(),
		"the user consumer must refresh identity events through the uncached resolver")
	require.True(t, communityConsumer.HasUncachedIdentityResolver(),
		"the community consumer must verify origin display fields through the uncached resolver")
}

// Without this test, dropping WithUserIdentityRefreshLock or
// WithCommunityIdentityRefreshLock from registerFeedConsumers leaves every
// unit test green while production refuses
// every identity refresh (the consumer fails closed without a lock), or, were
// the fail-closed check ever removed, lets two processes interleave refreshes of
// one DID and store the older handle.
//
// The pools never dial: sql.Open only validates its arguments, and building the
// consumers must not touch the database. The DSN points at a closed loopback
// port so a stray dial fails locally instead of reaching any host.
func TestRegisterFeedConsumers_UserConsumerGetsTheIdentityRefreshLock(t *testing.T) {
	openUndialedPool := func() *sql.DB {
		pool, err := sql.Open("postgres", "host=127.0.0.1 port=1 dbname=wiring_test sslmode=disable connect_timeout=1")
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })
		return pool
	}
	a := &application{
		cfg:                      &config.Config{},
		db:                       openUndialedPool(),
		sessionCoordinationDB:    openUndialedPool(),
		identityRefreshLockDB:    openUndialedPool(),
		identityResolver:         unusedIdentityResolver{},
		uncachedIdentityResolver: unusedUncachedIdentityResolver{},
		oauthStore:               oauth.NewMobileAwareStoreWrapper(nil),
	}

	consumers, err := a.registerFeedConsumers()
	require.NoError(t, err)

	var userConsumer *jetstream.UserEventConsumer
	var communityConsumer *jetstream.CommunityEventConsumer
	for _, consumer := range consumers {
		switch consumer.name {
		case jetstream.ConsumerUsers:
			userConsumer = consumer.handler.(*jetstream.UserEventConsumer)
		case jetstream.ConsumerCommunities:
			communityConsumer = consumer.handler.(*jetstream.CommunityEventConsumer)
		}
	}
	require.NotNil(t, userConsumer, "the user consumer must be registered")
	require.NotNil(t, communityConsumer, "the community consumer must be registered")

	require.True(t, userConsumer.HasIdentityRefreshLock(),
		"the user consumer must serialize identity refreshes of one DID through the Postgres refresh lock")
	require.True(t, communityConsumer.HasIdentityRefreshLock(),
		"the community consumer must serialize identity refreshes of one DID through the Postgres refresh lock")
}
