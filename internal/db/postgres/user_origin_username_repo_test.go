//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserRepo_GetDIDByOriginUsername pins the bridged-user pair lookup behind
// name@host actor identifiers: an exact (case-sensitive) username match wins;
// failing that, a unique case-insensitive match; more than one row at the
// deciding step is ErrAmbiguousUser and no row is ErrUserNotFound. Each fixture
// group sits on its own origin so the groups cannot match each other.
func TestUserRepo_GetDIDByOriginUsername(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(db)

	seed := []struct {
		did, handle, originUsername, origin string
	}{
		{"did:plc:originuseralice00000000", "alice-b.lemmy-world.ap.example.test", "Alice_B", "lemmy.world"},
		{"did:plc:originuserbob0000000000", "bob.lemmy-ml.ap.example.test", "bob", "lemmy.ml"},
		{"did:plc:originusercarolupper000", "carol.example-social.ap.example.test", "Carol", "example.social"},
		{"did:plc:originusercarollower000", "carol-2.example-social.ap.example.test", "carol", "example.social"},
		{"did:plc:originuserdavefirst0000", "dave.example-org.ap.example.test", "Dave", "example.org"},
		{"did:plc:originuserdavesecond000", "dave-2.example-org.ap.example.test", "Dave", "example.org"},
	}
	for _, s := range seed {
		_, err := repo.Create(ctx, &users.User{DID: s.did, Handle: s.handle, PDSURL: "https://pds.example.test"})
		require.NoError(t, err, "create %s", s.did)
		_, err = db.ExecContext(ctx,
			`UPDATE users SET origin = $2, origin_username = $3 WHERE did = $1`,
			s.did, s.origin, s.originUsername)
		require.NoError(t, err, "set origin pair for %s", s.did)
	}

	cases := []struct {
		name     string
		username string
		origin   string
		wantDID  string
		wantErr  error
	}{
		{"exact match", "Alice_B", "lemmy.world", "did:plc:originuseralice00000000", nil},
		{"unique case-insensitive match", "alice_b", "lemmy.world", "did:plc:originuseralice00000000", nil},
		{"username on another origin is not found", "bob", "lemmy.world", "", users.ErrUserNotFound},
		{"exact match beats case-insensitive sibling (upper)", "Carol", "example.social", "did:plc:originusercarolupper000", nil},
		{"exact match beats case-insensitive sibling (lower)", "carol", "example.social", "did:plc:originusercarollower000", nil},
		{"two case-insensitive matches are ambiguous", "CAROL", "example.social", "", users.ErrAmbiguousUser},
		{"two exact matches are ambiguous", "Dave", "example.org", "", users.ErrAmbiguousUser},
		{"two case-insensitive duplicates are ambiguous", "dave", "example.org", "", users.ErrAmbiguousUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			did, err := repo.GetDIDByOriginUsername(ctx, tc.username, tc.origin)
			if tc.wantErr != nil {
				require.Error(t, err, "GetDIDByOriginUsername(%q, %q) must fail", tc.username, tc.origin)
				assert.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, did)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDID, did)
		})
	}
}

// TestUserRepo_GetDIDByOriginUsername_QueryFailureIsNotNotFound pins that a
// failed query is returned as itself: a database the repository cannot reach
// must surface as a server error, never as ErrUserNotFound (a 404).
func TestUserRepo_GetDIDByOriginUsername_QueryFailureIsNotNotFound(t *testing.T) {
	t.Parallel()
	repo := postgres.NewUserRepository(testkit.DB(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	did, err := repo.GetDIDByOriginUsername(ctx, "Alice_B", "lemmy.world")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, users.ErrUserNotFound)
	assert.Empty(t, did)
}

// TestUserRepo_GetDIDByOriginUsername_CaseInsensitiveQueryFailureIsNotNotFound
// pins the same guarantee for the second (case-insensitive) query, which runs
// only after the exact query found nothing. This test's own database clone
// shadows lower(text) with a function that raises, and puts public ahead of
// pg_catalog on the pool's single connection, so only that second query fails.
// The exact query finds no row for "nobody"; the case-insensitive query then
// evaluates the shadowed lower() against the seeded row and raises.
func TestUserRepo_GetDIDByOriginUsername_CaseInsensitiveQueryFailureIsNotNotFound(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	repo := postgres.NewUserRepository(db)

	// A row on the same origin whose username is not an exact match, so the
	// case-insensitive query has a row to evaluate lower() against.
	const seedDID = "did:plc:originuserlowerfail00000"
	_, err := repo.Create(ctx, &users.User{DID: seedDID, Handle: "somebody.lemmy-world.ap.example.test", PDSURL: "https://pds.example.test"})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`UPDATE users SET origin = $2, origin_username = $3 WHERE did = $1`,
		seedDID, "lemmy.world", "Somebody")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		CREATE FUNCTION public.lower(text) RETURNS text LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected case-insensitive query failure'; END $$`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `SET search_path TO public, pg_catalog`)
	require.NoError(t, err)

	did, err := repo.GetDIDByOriginUsername(ctx, "nobody", "lemmy.world")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected case-insensitive query failure")
	assert.NotErrorIs(t, err, users.ErrUserNotFound)
	assert.Empty(t, did)
}
