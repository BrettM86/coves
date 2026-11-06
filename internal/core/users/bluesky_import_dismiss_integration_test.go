//go:build integration

package users_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDismissBlueskyProfileImport_SetsTheTimestampOnce drives the real service
// over the real repository and reads the column directly: the claim is about
// the row, not about what a method reports.
func TestDismissBlueskyProfileImport_SetsTheTimestampOnce(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, testPDSURL(), nil, "")

	seedUser := func(t *testing.T, prefix string) string {
		t.Helper()
		did := "did:plc:" + testkit.UniqueIDWithPrefix(t, prefix)
		handle := testkit.UniqueIDWithPrefix(t, prefix) + ".test.coves.dev"
		_, err := userRepo.Create(ctx, &users.User{DID: did, Handle: handle, PDSURL: testPDSURL()})
		require.NoError(t, err, "seeding the user row")
		return did
	}

	t.Run("fresh row is set, and a second call keeps the first value", func(t *testing.T) {
		did := seedUser(t, "dismissfresh")
		bystanderDID := seedUser(t, "dismissbystander")

		require.NoError(t, userService.DismissBlueskyProfileImport(ctx, did))
		first := readDismissedAt(t, db, did)
		require.True(t, first.Valid, "the first dismissal must set bluesky_profile_import_dismissed_at")
		assert.False(t, readDismissedAt(t, db, bystanderDID).Valid,
			"dismissing one user set bluesky_profile_import_dismissed_at on another user")

		require.NoError(t, userService.DismissBlueskyProfileImport(ctx, did))
		second := readDismissedAt(t, db, did)
		require.True(t, second.Valid)
		assert.True(t, first.Time.Equal(second.Time),
			"a second dismissal changed the timestamp from %v to %v", first.Time, second.Time)
	})

	t.Run("an existing historical dismissal is left exactly as it was", func(t *testing.T) {
		did := seedUser(t, "dismissold")
		historical := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
		_, err := db.ExecContext(ctx,
			`UPDATE users SET bluesky_profile_import_dismissed_at = $1 WHERE did = $2`, historical, did)
		require.NoError(t, err, "seeding the historical dismissal")

		require.NoError(t, userService.DismissBlueskyProfileImport(ctx, did))

		got := readDismissedAt(t, db, did)
		require.True(t, got.Valid)
		assert.True(t, historical.Equal(got.Time),
			"the dismissal overwrote %v with %v", historical, got.Time)
	})

	t.Run("an unknown DID is ErrUserNotFound", func(t *testing.T) {
		did := "did:plc:" + testkit.UniqueIDWithPrefix(t, "dismissunknown")

		err := userService.DismissBlueskyProfileImport(ctx, did)
		assert.True(t, errors.Is(err, users.ErrUserNotFound),
			"an unknown DID returned %v, want users.ErrUserNotFound", err)
	})
}

// TestGetBlueskyProfileImportDismissedAt_ReadsTheColumn drives the real service
// over the real repository; the expected values come from the column itself or
// from a literal written to it.
func TestGetBlueskyProfileImportDismissedAt_ReadsTheColumn(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()

	userRepo := postgres.NewUserRepository(db)
	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, testPDSURL(), nil, "")

	seedUser := func(t *testing.T, prefix string) string {
		t.Helper()
		did := "did:plc:" + testkit.UniqueIDWithPrefix(t, prefix)
		handle := testkit.UniqueIDWithPrefix(t, prefix) + ".test.coves.dev"
		_, err := userRepo.Create(ctx, &users.User{DID: did, Handle: handle, PDSURL: testPDSURL()})
		require.NoError(t, err, "seeding the user row")
		return did
	}

	t.Run("a fresh row has no dismissal", func(t *testing.T) {
		did := seedUser(t, "readfresh")

		got, err := userService.GetBlueskyProfileImportDismissedAt(ctx, did)
		require.NoError(t, err)
		assert.Nil(t, got, "a never-dismissed user reported a dismissal at %v", got)
	})

	t.Run("after a dismissal it returns the stored timestamp", func(t *testing.T) {
		did := seedUser(t, "readdismissed")
		require.NoError(t, userService.DismissBlueskyProfileImport(ctx, did))
		stored := readDismissedAt(t, db, did)
		require.True(t, stored.Valid, "the dismissal did not set the column")

		got, err := userService.GetBlueskyProfileImportDismissedAt(ctx, did)
		require.NoError(t, err)
		require.NotNil(t, got, "a dismissed user reported no dismissal")
		assert.True(t, stored.Time.Equal(*got),
			"read %v, but the column holds %v", *got, stored.Time)
	})

	t.Run("a historical timestamp is returned exactly", func(t *testing.T) {
		did := seedUser(t, "readhistorical")
		historical := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
		_, err := db.ExecContext(ctx,
			`UPDATE users SET bluesky_profile_import_dismissed_at = $1 WHERE did = $2`, historical, did)
		require.NoError(t, err, "seeding the historical dismissal")

		got, err := userService.GetBlueskyProfileImportDismissedAt(ctx, did)
		require.NoError(t, err)
		require.NotNil(t, got, "a historically dismissed user reported no dismissal")
		assert.True(t, historical.Equal(*got), "read %v, want %v", *got, historical)
	})

	t.Run("an unknown DID is ErrUserNotFound", func(t *testing.T) {
		did := "did:plc:" + testkit.UniqueIDWithPrefix(t, "readunknown")

		got, err := userService.GetBlueskyProfileImportDismissedAt(ctx, did)
		assert.Nil(t, got)
		assert.True(t, errors.Is(err, users.ErrUserNotFound),
			"an unknown DID returned %v, want users.ErrUserNotFound", err)
	})
}

func readDismissedAt(t *testing.T, db *sql.DB, did string) sql.NullTime {
	t.Helper()
	var dismissedAt sql.NullTime
	err := db.QueryRowContext(context.Background(),
		`SELECT bluesky_profile_import_dismissed_at FROM users WHERE did = $1`, did).Scan(&dismissedAt)
	require.NoError(t, err, "reading bluesky_profile_import_dismissed_at")
	return dismissedAt
}
