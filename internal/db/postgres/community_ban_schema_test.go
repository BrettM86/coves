//go:build integration

package postgres

import (
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// A ban lives in the community ban index, not on a membership: the migrated
// schema keeps no is_banned column on community_memberships.
func TestCommunityMembershipsHaveNoBanColumn(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	var columns int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'community_memberships'
		  AND column_name = 'is_banned'`).Scan(&columns))
	require.Equal(t, 0, columns, "community_memberships.is_banned must not exist after migrations")
}
