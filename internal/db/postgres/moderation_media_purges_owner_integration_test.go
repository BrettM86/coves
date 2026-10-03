//go:build integration

package postgres_test

import (
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// An empty owner DID means every owner on a media block, but a CDN purge
// target always names one owner's URLs, so the table refuses an empty owner.
func TestModerationMediaPurgesRejectEmptyOwnerDID(t *testing.T) {
	db := testkit.DB(t)
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO moderation_media_purges (owner_did, blob_cid) VALUES ('', $1)`, moderationImageCIDOne)
	require.ErrorContains(t, err, "moderation_media_purges_owner_did_check")
}
