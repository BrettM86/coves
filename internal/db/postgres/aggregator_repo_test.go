//go:build integration

package postgres

import (
	"Coves/internal/core/aggregators"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The aggregator index and the two database triggers that keep its stats
// honest.
//
// Three tables cooperate here and only one of them is written by application
// code. `aggregators` and `aggregator_authorizations` are indexed from the
// firehose, `aggregator_posts` is AppView-only rate-limit bookkeeping, and
// `aggregators.communities_using` / `aggregators.posts_created` are maintained
// by triggers in migration 012 rather than by any Go statement. A repo test is
// therefore the only place those triggers exist at all: mocking the repository
// mocks them away, and reading the migration proves nothing about the schema
// the template database actually carries.
//
// These assertions used to live in tests/integration/aggregator_test.go.

const (
	aggregatorServiceCollection       = "social.coves.aggregator.service"
	aggregatorAuthorizationCollection = "social.coves.aggregator.authorization"
)

// indexAggregator writes a minimal service declaration and returns its DID.
//
// Minimal on purpose: a test asserting on a field sets that field itself, so
// the fixture can never be the reason an assertion passes.
func indexAggregator(t *testing.T, repo aggregators.Repository, displayName string) string {
	t.Helper()

	did := "did:plc:" + testkit.UniqueID(t)
	require.NoError(t, repo.CreateAggregator(context.Background(), &aggregators.Aggregator{
		DID:         did,
		DisplayName: displayName,
		CreatedAt:   time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   "at://" + did + "/" + aggregatorServiceCollection + "/self",
		RecordCID:   "bafyservice",
	}))
	return did
}

// indexAuthorizingCommunity creates the community row
// aggregator_authorizations' foreign key requires, and returns its DID.
func indexAuthorizingCommunity(t *testing.T, db *sql.DB) string {
	t.Helper()

	id := testkit.UniqueID(t)
	did := "did:plc:" + id
	createTestCommunity(t, db, did, "c-"+id+".coves.social", did)
	return did
}

// enabledAuthorization builds the record a community publishes to grant an
// aggregator posting rights. The record key is unique per call because
// record_uri carries a UNIQUE constraint, and one community may authorize
// several aggregators.
func enabledAuthorization(t *testing.T, aggregatorDID, communityDID string) *aggregators.Authorization {
	t.Helper()

	return &aggregators.Authorization{
		AggregatorDID: aggregatorDID,
		CommunityDID:  communityDID,
		Enabled:       true,
		CreatedBy:     communityDID,
		CreatedAt:     time.Now(),
		IndexedAt:     time.Now(),
		RecordURI:     "at://" + communityDID + "/" + aggregatorAuthorizationCollection + "/" + testkit.UniqueID(t),
		RecordCID:     "bafyauthorization",
	}
}

func TestAggregatorRepo_CreateRoundTripsTheServiceDeclaration(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	did := "did:plc:" + testkit.UniqueID(t)
	// A JSON Schema, because that is what the column holds: a community's
	// per-aggregator config is validated against it before the authorization is
	// accepted (aggregators.validateConfig). Stored as JSONB, so what comes back
	// is Postgres' reserialization rather than these bytes.
	schema := []byte(`{"type":"object","properties":{"feedUrl":{"type":"string"},"updateInterval":{"type":"number","minimum":5}},"required":["feedUrl"]}`)

	// createdAt belongs to the RECORD and indexedAt to the AppView. Setting them
	// an hour apart is what proves the column takes the declaration's timestamp
	// instead of the DEFAULT NOW() the migration also offers.
	declaredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
		DID:           did,
		DisplayName:   "RSS Feed Aggregator",
		Description:   "Aggregates content from RSS feeds",
		AvatarURL:     "https://cdn.example.com/avatar.png",
		ConfigSchema:  schema,
		MaintainerDID: "did:plc:maintainer",
		SourceURL:     "https://github.com/example/rss-aggregator",
		CreatedAt:     declaredAt,
		IndexedAt:     time.Now(),
		RecordURI:     "at://" + did + "/" + aggregatorServiceCollection + "/self",
		RecordCID:     "bafyservice",
	}))

	indexed, err := repo.GetAggregator(ctx, did)
	require.NoError(t, err)

	assert.Equal(t, did, indexed.DID)
	assert.Equal(t, "RSS Feed Aggregator", indexed.DisplayName)
	assert.Equal(t, "Aggregates content from RSS feeds", indexed.Description)
	assert.Equal(t, "https://cdn.example.com/avatar.png", indexed.AvatarURL)
	assert.Equal(t, "did:plc:maintainer", indexed.MaintainerDID)
	assert.Equal(t, "https://github.com/example/rss-aggregator", indexed.SourceURL)
	assert.Equal(t, "at://"+did+"/"+aggregatorServiceCollection+"/self", indexed.RecordURI)
	assert.Equal(t, "bafyservice", indexed.RecordCID)
	assert.JSONEq(t, string(schema), string(indexed.ConfigSchema),
		"the config schema must come back parseable, or no community can validate a config against it")
	assert.True(t, indexed.CreatedAt.Equal(declaredAt),
		"createdAt = %v, want the declaration's %v", indexed.CreatedAt, declaredAt)

	// Both stats are trigger-maintained from here on; nothing has happened yet.
	assert.Equal(t, 0, indexed.CommunitiesUsing)
	assert.Equal(t, 0, indexed.PostsCreated)
}

// A service declaration is a single record at rkey `self`, so every update to
// it arrives as another create — the firehose replays, and an aggregator that
// edits its display name emits the same URI again.
func TestAggregatorRepo_CreateUpsertsTheDeclarationWithoutLosingStats(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "Original Name")
	communityDID := indexAuthorizingCommunity(t, db)
	require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, communityDID)))

	require.NoError(t, repo.CreateAggregator(ctx, &aggregators.Aggregator{
		DID:         aggregatorDID,
		DisplayName: "Updated Name",
		CreatedAt:   time.Now(),
		IndexedAt:   time.Now(),
		RecordURI:   "at://" + aggregatorDID + "/" + aggregatorServiceCollection + "/self",
		RecordCID:   "bafyservice2",
	}))

	indexed, err := repo.GetAggregator(ctx, aggregatorDID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", indexed.DisplayName)
	assert.Equal(t, "bafyservice2", indexed.RecordCID)

	// The upsert lists its columns explicitly and the two counters are not among
	// them. If they ever are, a redeclaration would zero stats the triggers own,
	// and an aggregator could reset its own usage numbers by republishing.
	assert.Equal(t, 1, indexed.CommunitiesUsing,
		"re-indexing a declaration must not clobber the trigger-maintained stats")
}

// IsAggregator is on the post-creation critical path: it is what decides
// whether an author is held to the aggregator authorization rules or to the
// membership rules.
func TestAggregatorRepo_IsAggregator(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "Declared")

	declared, err := repo.IsAggregator(ctx, aggregatorDID)
	require.NoError(t, err)
	assert.True(t, declared)

	// A human. The answer must be false rather than an error, because every post
	// a person writes takes this path.
	human, err := repo.IsAggregator(ctx, "did:plc:"+testkit.UniqueID(t))
	require.NoError(t, err)
	assert.False(t, human)
}

func TestAggregatorRepo_CreateAuthorizationRoundTrips(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "RSS Feed Aggregator")
	communityDID := indexAuthorizingCommunity(t, db)

	granted := enabledAuthorization(t, aggregatorDID, communityDID)
	granted.Config = []byte(`{"feedUrl":"https://example.com/feed.xml","updateInterval":15}`)
	require.NoError(t, repo.CreateAuthorization(ctx, granted))

	// The repository assigns the row id on the way in; a caller that needs to
	// address the row afterwards has nothing else to go on.
	assert.NotZero(t, granted.ID)

	indexed, err := repo.GetAuthorization(ctx, aggregatorDID, communityDID)
	require.NoError(t, err)

	assert.Equal(t, aggregatorDID, indexed.AggregatorDID)
	assert.Equal(t, communityDID, indexed.CommunityDID)
	assert.True(t, indexed.Enabled)
	assert.Equal(t, communityDID, indexed.CreatedBy)
	assert.Equal(t, granted.RecordURI, indexed.RecordURI)
	assert.JSONEq(t, string(granted.Config), string(indexed.Config),
		"the config is what the aggregator reads to know what this community wants")
	assert.Nil(t, indexed.DisabledAt, "an enabled authorization has never been disabled")
}

// A community may hold at most one authorization per aggregator, enforced by a
// UNIQUE constraint the repository resolves with ON CONFLICT. That is what
// makes replayed firehose events safe: the second delivery of a record must
// overwrite the first rather than fail or duplicate.
func TestAggregatorRepo_CreateAuthorizationUpsertsPerCommunity(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "RSS Feed Aggregator")
	communityDID := indexAuthorizingCommunity(t, db)

	require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, communityDID)))

	// The community published an update turning the authorization off. It
	// arrives as another create carrying a new record CID.
	revoked := enabledAuthorization(t, aggregatorDID, communityDID)
	revoked.Enabled = false
	revoked.RecordCID = "bafyauthorization2"
	require.NoError(t, repo.CreateAuthorization(ctx, revoked))

	indexed, err := repo.GetAuthorization(ctx, aggregatorDID, communityDID)
	require.NoError(t, err)
	assert.False(t, indexed.Enabled, "the later record must win")
	assert.Equal(t, "bafyauthorization2", indexed.RecordCID)

	authorizations, err := repo.ListAuthorizationsForCommunity(ctx, communityDID, false, 10, 0)
	require.NoError(t, err)
	assert.Len(t, authorizations, 1, "the upsert must overwrite the row, not add one beside it")
}

// IsAuthorized is the check that stands between an aggregator and a community's
// repository, and it is consulted on every aggregator post.
func TestAggregatorRepo_IsAuthorized(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "RSS Feed Aggregator")

	enabledCommunity := indexAuthorizingCommunity(t, db)
	require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, enabledCommunity)))

	disabledCommunity := indexAuthorizingCommunity(t, db)
	disabled := enabledAuthorization(t, aggregatorDID, disabledCommunity)
	disabled.Enabled = false
	require.NoError(t, repo.CreateAuthorization(ctx, disabled))

	for _, tc := range []struct {
		name         string
		communityDID string
		want         bool
	}{
		{name: "enabled authorization", communityDID: enabledCommunity, want: true},
		// A disabled row is not an absent row, and the difference is the whole
		// point of the enabled column: revoking access must not require the
		// community to delete the record and lose its audit trail.
		{name: "disabled authorization", communityDID: disabledCommunity, want: false},
		{name: "no authorization at all", communityDID: "did:plc:" + testkit.UniqueID(t), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authorized, err := repo.IsAuthorized(ctx, aggregatorDID, tc.communityDID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, authorized)
		})
	}
}

// Rate limiting is a rolling window, so the count has to be scoped by time and
// by community — an aggregator's quota in one community must not be spent by
// its posts in another.
func TestAggregatorRepo_CountsRecentPostsForRateLimiting(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "RSS Feed Aggregator")
	communityDID := indexAuthorizingCommunity(t, db)
	otherCommunityDID := indexAuthorizingCommunity(t, db)

	require.NoError(t, repo.RecordAggregatorPost(ctx, aggregatorDID, communityDID,
		"at://"+communityDID+"/social.coves.community.post/first", "bafypost1"))
	require.NoError(t, repo.RecordAggregatorPost(ctx, aggregatorDID, communityDID,
		"at://"+communityDID+"/social.coves.community.post/second", "bafypost2"))
	require.NoError(t, repo.RecordAggregatorPost(ctx, aggregatorDID, otherCommunityDID,
		"at://"+otherCommunityDID+"/social.coves.community.post/elsewhere", "bafypost3"))

	count, err := repo.CountRecentPosts(ctx, aggregatorDID, communityDID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, count, "the third post was in another community and must not count here")

	// The window boundary, from the other side. A query that ignored `since`
	// would agree with the assertion above and still let the limit reset never
	// happen, which is the failure that would put an aggregator in permanent
	// timeout after its first busy hour.
	future, err := repo.CountRecentPosts(ctx, aggregatorDID, communityDID, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, future, "posts older than the window must fall out of the count")
}

// communities_using is a cached count on the aggregator row, recomputed by a
// trigger on every authorization write. Nothing in Go maintains it, and the
// detailed getServices view serves it straight to clients.
func TestAggregatorRepo_CommunitiesUsingTracksEnabledAuthorizations(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "Widely Used Aggregator")

	communityDIDs := make([]string, 3)
	for i := range communityDIDs {
		communityDIDs[i] = indexAuthorizingCommunity(t, db)
		require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, communityDIDs[i])))
	}

	indexed, err := repo.GetAggregator(ctx, aggregatorDID)
	require.NoError(t, err)
	require.Equal(t, 3, indexed.CommunitiesUsing)

	// Disabling counts down. The trigger recomputes rather than increments, so
	// this is the assertion that separates a working recount from one that only
	// ever fires on INSERT — and without it a community that revokes access
	// stays in the aggregator's advertised reach forever.
	revoked := enabledAuthorization(t, aggregatorDID, communityDIDs[0])
	revoked.Enabled = false
	require.NoError(t, repo.CreateAuthorization(ctx, revoked))

	indexed, err = repo.GetAggregator(ctx, aggregatorDID)
	require.NoError(t, err)
	assert.Equal(t, 2, indexed.CommunitiesUsing, "a disabled authorization is not a community using the aggregator")

	// And deleting the record — the shape a firehose delete arrives in — counts
	// down too, through the trigger's third branch.
	require.NoError(t, repo.DeleteAuthorization(ctx, aggregatorDID, communityDIDs[1]))

	indexed, err = repo.GetAggregator(ctx, aggregatorDID)
	require.NoError(t, err)
	assert.Equal(t, 1, indexed.CommunitiesUsing)
}

func TestAggregatorRepo_PostsCreatedCountsEveryTrackedPostOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "Prolific Aggregator")
	communityDID := indexAuthorizingCommunity(t, db)

	const tracked = 5
	for i := 0; i < tracked; i++ {
		uri := "at://" + communityDID + "/social.coves.community.post/" + testkit.UniqueID(t)
		require.NoError(t, repo.RecordAggregatorPost(ctx, aggregatorDID, communityDID, uri, "bafypost"))
	}

	indexed, err := repo.GetAggregator(ctx, aggregatorDID)
	require.NoError(t, err)

	// Exactly five, not at least five. This counter is an increment rather than
	// a recount, so the two ways it can break are both overcounts — a trigger
	// firing per statement as well as per row, or a second trigger created by a
	// re-run migration — and a >= assertion is blind to both. The database is a
	// per-test clone, so nothing else can have contributed.
	assert.Equal(t, tracked, indexed.PostsCreated)
}

// disabledAt/disabledBy are the audit trail for a revocation: who turned the
// aggregator off and when. They are nullable, and a nullable timestamp that
// silently reads back as the zero time is indistinguishable from 0001-01-01
// having been recorded on purpose.
func TestAggregatorRepo_AuthorizationRoundTripsTheDisableAuditTrail(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	aggregatorDID := indexAggregator(t, repo, "RSS Feed Aggregator")
	moderatorDID := "did:plc:" + testkit.UniqueID(t)

	disabledCommunity := indexAuthorizingCommunity(t, db)
	disabledAt := time.Now().UTC().Truncate(time.Microsecond)
	revoked := enabledAuthorization(t, aggregatorDID, disabledCommunity)
	revoked.Enabled = false
	revoked.DisabledBy = moderatorDID
	revoked.DisabledAt = &disabledAt
	require.NoError(t, repo.CreateAuthorization(ctx, revoked))

	indexed, err := repo.GetAuthorization(ctx, aggregatorDID, disabledCommunity)
	require.NoError(t, err)
	require.NotNil(t, indexed.DisabledAt)
	assert.True(t, indexed.DisabledAt.Equal(disabledAt),
		"disabledAt = %v, want %v", *indexed.DisabledAt, disabledAt)
	assert.Equal(t, moderatorDID, indexed.DisabledBy)

	// An enabled authorization leaves both columns NULL, which must survive as a
	// nil pointer rather than as a zero time.
	enabledCommunity := indexAuthorizingCommunity(t, db)
	require.NoError(t, repo.CreateAuthorization(ctx, enabledAuthorization(t, aggregatorDID, enabledCommunity)))

	indexed, err = repo.GetAuthorization(ctx, aggregatorDID, enabledCommunity)
	require.NoError(t, err)
	assert.Nil(t, indexed.DisabledAt)
	assert.Empty(t, indexed.DisabledBy)
}

// An authorization record is keyed by (aggregator_did, community_did) in the
// table but by its AT-URI on the wire, and the two disagree the moment a
// community UPDATES an existing record to name a different aggregatorDid.
// Before this test the upsert alone hit record_uri's UNIQUE constraint — an
// error no redrive could clear, classified transient, so the consumer stalled
// 4.2s in-line and ten more times on the redriver — while the OLD aggregator
// kept the row and stayed authorized (docs/CONSUMER_TRUST_AUDIT.md §1.3).
func TestAggregatorRepo_CreateAuthorizationRetargetsTheRecordURI(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	ctx := context.Background()

	firstAggregator := indexAggregator(t, repo, "First Aggregator")
	secondAggregator := indexAggregator(t, repo, "Second Aggregator")
	communityDID := indexAuthorizingCommunity(t, db)

	original := enabledAuthorization(t, firstAggregator, communityDID)
	require.NoError(t, repo.CreateAuthorization(ctx, original))

	// Same record URI (same rkey in the community's repo), rewritten to name
	// the second aggregator.
	retargeted := enabledAuthorization(t, secondAggregator, communityDID)
	retargeted.RecordURI = original.RecordURI
	retargeted.RecordCID = "bafyretargeted"
	require.NoError(t, repo.CreateAuthorization(ctx, retargeted),
		"the rewritten record must index; the record_uri UNIQUE constraint must not be reachable from a legitimate update")

	_, err := repo.GetAuthorization(ctx, firstAggregator, communityDID)
	assert.ErrorIs(t, err, aggregators.ErrAuthorizationNotFound,
		"the aggregator the record no longer names must lose its authorization")

	current, err := repo.GetAuthorization(ctx, secondAggregator, communityDID)
	require.NoError(t, err)
	assert.Equal(t, original.RecordURI, current.RecordURI)
	assert.Equal(t, "bafyretargeted", current.RecordCID)

	authorizations, err := repo.ListAuthorizationsForCommunity(ctx, communityDID, false, 10, 0)
	require.NoError(t, err)
	assert.Len(t, authorizations, 1, "one record, one authorization")
}

// The other half of the same site: an authorization whose community this
// AppView has not indexed must surface as a NOT-FOUND the consumer can
// classify, not as a raw foreign-key error it has to treat as infrastructure.
func TestAggregatorRepo_CreateAuthorizationForUnknownCommunityIsNotFound(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	repo := NewAggregatorRepository(db, credentialciphertest.Fixed())
	aggregatorDID := indexAggregator(t, repo, "Orphan Aggregator")

	err := repo.CreateAuthorization(context.Background(),
		enabledAuthorization(t, aggregatorDID, "did:plc:"+testkit.UniqueID(t)))
	require.Error(t, err)
	assert.ErrorIs(t, err, aggregators.ErrCommunityNotFound)
	assert.True(t, aggregators.IsNotFound(err))
}
