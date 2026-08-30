//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPostV2HydrationFixture(
	t *testing.T,
	db *sql.DB,
	resolved *identity.Identity,
) pv2Fixture {
	t.Helper()
	f := newPV2Fixture(t, db)
	resolver := &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		pv2Other: resolved,
	}}
	userService := users.NewUserService(
		postgres.NewUserRepository(db), resolver, bridgedTestNativePDS, nil, "",
	)
	f.consumer = NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db),
		userService,
		db,
		WithAdmissions(f.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostIdentityResolver(resolver),
	)
	return f
}

func TestPostV2Hydration_UnknownAuthor_IndexesResolvedIdentity(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2HydrationFixture(t, db, &identity.Identity{
		DID: pv2Other, Handle: "resolved-author.test", PDSURL: bridgedTestNativePDS,
	})
	ctx := context.Background()
	const rkey = "pv2hydrateauthor"
	uri := pv2URI(pv2Other, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Other, "create", rkey, testkit.TID(), "bafyreipv2hydrateauthor", time.Now().UnixMicro(),
		pv2Record(pv2Community, "resolved author", "profile arrived through hydration"),
	)))

	var handle string
	require.NoError(t, db.QueryRow(`SELECT handle FROM users WHERE did = $1`, pv2Other).Scan(&handle))
	assert.Equal(t, "resolved-author.test", handle)

	views, err := postgres.NewPostRepository(db).GetViewsByURIs(ctx, []string{uri}, pv2Other)
	require.NoError(t, err)
	view := views[uri]
	require.NotNil(t, view)
	require.NotNil(t, view.Author)
	assert.Equal(t, pv2Other, view.Author.DID)
	assert.Equal(t, "resolved-author.test", view.Author.Handle)
}

func TestPostV2Hydration_MismatchedResolvedDID_DoesNotHydrate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newPostV2HydrationFixture(t, db, &identity.Identity{
		DID: pv2Author, Handle: invalidHandle, PDSURL: bridgedTestNativePDS,
	})
	ctx := context.Background()
	const rkey = "pv2hydratemismatch"
	uri := pv2URI(pv2Other, rkey)

	require.NoError(t, f.consumer.HandleEvent(ctx, pv2Event(
		pv2Other, "create", rkey, testkit.TID(), "bafyreipv2hydratemismatch", time.Now().UnixMicro(),
		pv2Record(pv2Community, "unverified author", "the post still indexes"),
	)))

	assert.Zero(t, countRows(t, db, `SELECT count(*) FROM users WHERE did = $1`, pv2Other))
	assert.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))

	views, err := postgres.NewPostRepository(db).GetViewsByURIs(ctx, []string{uri}, pv2Other)
	require.NoError(t, err)
	view := views[uri]
	require.NotNil(t, view)
	require.NotNil(t, view.Author)
	assert.Equal(t, pv2Other, view.Author.DID)
	assert.Equal(t, pv2Other, view.Author.Handle,
		"an unhydrated author falls back to its DID; handle.invalid must not be persisted")
}
