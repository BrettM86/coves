//go:build integration

package postgres

import (
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationMigration053_BridgedUpvotePeakColumns(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	require.EqualValues(t, 55, testkit.MigrateDownOne(t, db, 55))
	require.EqualValues(t, 54, testkit.MigrateDownOne(t, db, 54))
	require.EqualValues(t, 53, testkit.MigrateDownOne(t, db, 53))
	for _, table := range []string{"posts", "comments"} {
		var columns int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = 'bridged_upvote_peak'`, table).Scan(&columns))
		require.Equal(t, 0, columns, "%s peak column must be absent after 053 Down", table)
	}

	const (
		userDID      = "did:plc:peakmigrationuser"
		communityDID = "did:plc:peakmigrationcommunity"
		postA        = "at://did:plc:peakmigrationuser/social.coves.community.postv2/a"
		postB        = "at://did:plc:peakmigrationuser/social.coves.community.postv2/b"
		postNew      = "at://did:plc:peakmigrationuser/social.coves.community.postv2/new"
		postNull     = "at://did:plc:peakmigrationuser/social.coves.community.postv2/null"
		commentC     = "at://did:plc:peakmigrationuser/social.coves.community.comment/c"
		commentNew   = "at://did:plc:peakmigrationuser/social.coves.community.comment/new"
		commentNull  = "at://did:plc:peakmigrationuser/social.coves.community.comment/null"
	)
	_, err := db.Exec(`INSERT INTO users (did, handle, pds_url) VALUES ($1, 'peakmigration.test', $2)`, userDID, bridgeAPDSURL)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, '!peakmigration@local.test', 'peakmigration', $1, $2, $1, NOW())`, communityDID, userDID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, bridged_upvote_count)
		VALUES ($1, 'bafypeakmigrationa', 'a', $3, $4, 'a', NOW(), 7),
		       ($2, 'bafypeakmigrationb', 'b', $3, $4, 'b', NOW(), 0)`, postA, postB, userDID, communityDID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO comments
		(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at, bridged_upvote_count)
		VALUES ($1, 'bafypeakmigrationc', 'c', $2, $3, 'bafypeakmigrationa', $3, 'bafypeakmigrationa', 'c', NOW(), 4)`,
		commentC, userDID, postA)
	require.NoError(t, err)

	testkit.MigrateUp(t, db)
	for _, row := range []struct {
		table string
		uri   string
		peak  int
	}{
		{"posts", postA, 7},
		{"posts", postB, 0},
		{"comments", commentC, 4},
	} {
		var peak int
		require.NoError(t, db.QueryRow(`SELECT bridged_upvote_peak FROM `+row.table+` WHERE uri = $1`, row.uri).Scan(&peak))
		require.Equal(t, row.peak, peak)
	}

	_, err = db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, 'bafypeakmigrationnewpost', 'new', $2, $3, 'new', NOW())`, postNew, userDID, communityDID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO comments
		(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafypeakmigrationnewcomment', 'new', $2, $3, 'bafypeakmigrationa', $3, 'bafypeakmigrationa', 'new', NOW())`,
		commentNew, userDID, postA)
	require.NoError(t, err)
	for _, row := range []struct {
		table string
		uri   string
	}{
		{"posts", postNew}, {"comments", commentNew},
	} {
		var peak int
		require.NoError(t, db.QueryRow(`SELECT bridged_upvote_peak FROM `+row.table+` WHERE uri = $1`, row.uri).Scan(&peak))
		require.Equal(t, 0, peak)
	}

	_, err = db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, bridged_upvote_peak)
		VALUES ($1, 'bafypeakmigrationnullpost', 'null', $2, $3, 'null', NOW(), NULL)`, postNull, userDID, communityDID)
	requireNotificationSQLState(t, err, "23502")
	_, err = db.Exec(`INSERT INTO comments
		(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at, bridged_upvote_peak)
		VALUES ($1, 'bafypeakmigrationnullcomment', 'null', $2, $3, 'bafypeakmigrationa', $3, 'bafypeakmigrationa', 'null', NOW(), NULL)`,
		commentNull, userDID, postA)
	requireNotificationSQLState(t, err, "23502")
	for _, row := range []struct {
		table string
		uri   string
	}{
		{"posts", postA}, {"comments", commentC},
	} {
		_, err = db.Exec(`UPDATE `+row.table+` SET bridged_upvote_peak = NULL WHERE uri = $1`, row.uri)
		requireNotificationSQLState(t, err, "23502")
		_, err = db.Exec(`UPDATE `+row.table+` SET bridged_upvote_peak = -1 WHERE uri = $1`, row.uri)
		requireNotificationSQLState(t, err, "23514")
	}
}
