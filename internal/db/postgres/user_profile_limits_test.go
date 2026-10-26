//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"

	"Coves/internal/core/users"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// The users profile columns are bounded by the lexicon's byte caps (640 for
// displayName, 2560 for description). The lexicon's grapheme caps cannot be
// expressed in Postgres, so a lexicon-valid value of multi-code-point
// graphemes (flags, ZWJ emoji) must store intact rather than trip a
// code-point CHECK.
func TestUserRepo_UpdateProfile_StoresLexiconValidMultiCodePointText(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewUserRepository(db)
	ctx := context.Background()

	id := testkit.UniqueID(t)
	did := "did:plc:limits" + id
	_, err := repo.Create(ctx, &users.User{DID: did, Handle: "limits" + id + ".test", PDSURL: "https://test.pds"})
	require.NoError(t, err)

	displayName := strings.Repeat("🇺🇸", 64)  // 64 graphemes, 128 code points, 512 bytes
	description := strings.Repeat("🇺🇸", 256) // 256 graphemes, 512 code points, 2048 bytes
	updated, err := repo.UpdateProfile(ctx, did, users.UpdateProfileInput{
		DisplayName: &displayName,
		Description: &description,
	})
	require.NoError(t, err)
	require.Equal(t, displayName, updated.DisplayName)
	require.Equal(t, description, updated.Description)
}

func TestUsersProfileColumns_RejectValuesOverTheByteCaps(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewUserRepository(db)
	ctx := context.Background()

	id := testkit.UniqueID(t)
	did := "did:plc:bytecap" + id
	_, err := repo.Create(ctx, &users.User{DID: did, Handle: "bytecap" + id + ".test", PDSURL: "https://test.pds"})
	require.NoError(t, err)

	tests := []struct {
		column     string
		value      string
		constraint string
	}{
		{column: "display_name", value: strings.Repeat("a", 641), constraint: "users_display_name_check"},
		{column: "description", value: strings.Repeat("a", 2561), constraint: "users_description_check"},
	}
	for _, tt := range tests {
		_, err := db.ExecContext(ctx, `UPDATE users SET `+tt.column+` = $1 WHERE did = $2`, tt.value, did)
		var pqErr *pq.Error
		require.ErrorAs(t, err, &pqErr, "%s over its byte cap must be rejected", tt.column)
		require.Equal(t, tt.constraint, pqErr.Constraint)
	}
}

// Migration 060 renames bio to description and widens both profile CHECKs to
// the lexicon's byte caps. Down restores the old code-point CHECKs, so it
// truncates values that only the wider CHECKs allowed.
func TestMigration060_PreservesProfileTextAcrossDownAndUp(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewUserRepository(db)
	ctx := context.Background()

	id := testkit.UniqueID(t)
	keptDID, longDID := "did:plc:kept"+id, "did:plc:long"+id
	for _, did := range []string{keptDID, longDID} {
		_, err := repo.Create(ctx, &users.User{DID: did, Handle: did[len("did:plc:"):] + ".test", PDSURL: "https://test.pds"})
		require.NoError(t, err)
	}
	_, err := db.ExecContext(ctx, `UPDATE users SET description = 'kept', display_name = 'Kept' WHERE did = $1`, keptDID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE users SET description = $1, display_name = $2 WHERE did = $3`,
		strings.Repeat("🇺🇸", 256), strings.Repeat("🇺🇸", 64), longDID)
	require.NoError(t, err)

	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	require.EqualValues(t, 61, testkit.MigrateDownOne(t, db, 61))
	require.EqualValues(t, 60, testkit.MigrateDownOne(t, db, 60))

	var bio, displayName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT bio, display_name FROM users WHERE did = $1`, keptDID).Scan(&bio, &displayName))
	require.Equal(t, "kept", bio)
	require.Equal(t, "Kept", displayName)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT bio, display_name FROM users WHERE did = $1`, longDID).Scan(&bio, &displayName))
	require.Equal(t, strings.Repeat("🇺🇸", 128), bio, "Down truncates to the old 256-code-point CHECK")
	require.Equal(t, strings.Repeat("🇺🇸", 32), displayName, "Down truncates to the old 64-code-point CHECK")

	testkit.MigrateUp(t, db)

	var description string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT description FROM users WHERE did = $1`, keptDID).Scan(&description))
	require.Equal(t, "kept", description)
}
