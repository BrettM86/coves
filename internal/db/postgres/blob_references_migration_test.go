//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/require"
)

func TestBlobReferencesMigration061_DownAndUpRoundTrip(t *testing.T) {
	db := testkit.DB(t)
	assertBlobReferencesMigration061(t, db)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	require.EqualValues(t, 61, testkit.MigrateDownOne(t, db, 61))
	var relation sql.NullString
	require.NoError(t, db.QueryRow(`SELECT to_regclass('blob_references')`).Scan(&relation))
	require.False(t, relation.Valid)
	testkit.MigrateUp(t, db)
	assertBlobReferencesMigration061(t, db)
}

func assertBlobReferencesMigration061(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var relation sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('blob_references')`).Scan(&relation))
	require.True(t, relation.Valid, "migration 061 must create blob_references")

	const (
		owner      = "did:plc:blobmigrationowner"
		otherOwner = "did:plc:blobmigrationother"
		uri        = "at://did:plc:blobmigrationowner/social.coves.community.postv2/one"
		otherURI   = "at://did:plc:blobmigrationowner/social.coves.community.postv2/two"
		cid        = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
		otherCID   = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	)
	for _, kind := range []string{"post_image", "comment_image", "external_thumb", "video_thumb", "avatar", "banner"} {
		t.Run(kind, func(t *testing.T) {
			var active bool
			var err error
			if kind == "post_image" {
				err = db.QueryRowContext(ctx, `INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind)
					VALUES ($1, $2, $3, $4) RETURNING active`, owner, cid, uri, kind).Scan(&active)
			} else {
				err = db.QueryRowContext(ctx, `INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
					VALUES ($1, $2, $3, $4, true) RETURNING active`, owner, otherCID, uri, kind).Scan(&active)
			}
			require.NoError(t, err)
			require.True(t, active)
		})
	}
	insert := func(ownerDID, blobCID, subjectURI, kind string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind)
			VALUES ($1, $2, $3, $4)`, ownerDID, blobCID, subjectURI, kind)
		return err
	}
	for _, tc := range []struct {
		name       string
		ownerDID   string
		subjectURI string
		kind       string
	}{
		{"different owner", otherOwner, uri, "post_image"},
		{"different subject", owner, otherURI, "post_image"},
		{"different kind", owner, uri, "comment_image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, insert(tc.ownerDID, cid, tc.subjectURI, tc.kind))
		})
	}
	for _, tc := range []struct {
		name string
		kind string
		code pqerror.Code
	}{
		{"unknown kind", "unknown", "23514"},
		{"duplicate key", "post_image", "23505"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(owner, cid, uri, tc.kind)
			require.Error(t, err)
			var databaseError *pq.Error
			require.ErrorAs(t, err, &databaseError)
			require.Equal(t, tc.code, databaseError.Code)
		})
	}

	var ownerCIDIndex, cidIndex bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COALESCE(bool_or(i.indnkeyatts >= 2 AND first.attname = 'owner_did' AND second.attname = 'blob_cid'), false),
		       COALESCE(bool_or(first.attname = 'blob_cid'), false)
		FROM pg_index i
		JOIN pg_class table_class ON table_class.oid = i.indrelid
		JOIN pg_namespace namespace ON namespace.oid = table_class.relnamespace
		JOIN pg_attribute first ON first.attrelid = i.indrelid AND first.attnum = i.indkey[0]
		LEFT JOIN pg_attribute second ON second.attrelid = i.indrelid AND second.attnum = i.indkey[1]
		WHERE namespace.nspname = current_schema() AND table_class.relname = 'blob_references'
		  AND i.indisvalid AND i.indisready AND i.indnkeyatts > 0
	`).Scan(&ownerCIDIndex, &cidIndex))
	require.True(t, ownerCIDIndex, "an index must lead with owner_did, blob_cid")
	require.True(t, cidIndex, "an index must lead with blob_cid")
}
