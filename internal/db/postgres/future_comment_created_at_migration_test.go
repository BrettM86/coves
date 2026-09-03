//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration041_ClampsFutureCommentCreatedAt(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	require.EqualValues(t, 45, testkit.MigrateDownOne(t, db, 45),
		"045 (the community subscriber recount) sits on top of 044 and must be rolled back first")
	require.EqualValues(t, 44, testkit.MigrateDownOne(t, db, 44),
		"044 (the posts search vector column and index) sits on top of 043 and must be rolled back next")
	require.EqualValues(t, 43, testkit.MigrateDownOne(t, db, 43),
		"043 (the bridged-vote poll watermark) sits on top of 042 and must be rolled back next")
	require.EqualValues(t, 42, testkit.MigrateDownOne(t, db, 42),
		"042 (the dead-letter retention index) sits on top of 041 and must be rolled back next")
	require.EqualValues(t, 41, testkit.MigrateDownOne(t, db, 41),
		"this test seeds the state migration 041 repairs; rolling back a different migration would seed against the wrong schema")

	ctx := context.Background()
	authorLabel := testkit.UniqueIDWithPrefix(t, "m41author")
	authorDID := fixtures.DID(authorLabel)
	fixtures.User(t, db, authorLabel+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "m41community")
	ownerHandle := testkit.UniqueIDWithPrefix(t, "m41owner") + ".test"
	communityDID, err := fixtures.Community(ctx, db, communityName, ownerHandle)
	require.NoError(t, err)
	rootURI := fixtures.Post(t, db, communityDID, authorDID, "Migration 041 repair", 0, time.Now().UTC())

	seedComment := func(name string, createdAt, indexedAt time.Time) string {
		t.Helper()
		rkey := testkit.TID()
		uri := "at://" + authorDID + "/social.coves.community.comment/" + rkey
		_, err := db.ExecContext(ctx, `
			INSERT INTO comments (
				uri, cid, rkey, commenter_did, root_uri, root_cid,
				parent_uri, parent_cid, content, created_at, indexed_at
			) VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8, $9)
		`, uri, "bafym41"+name, rkey, authorDID, rootURI, "bafym41root",
			"migration 041 "+name, createdAt, indexedAt)
		require.NoErrorf(t, err, "seeding the %s comment fixture", name)
		return uri
	}

	now := time.Now().UTC().Truncate(time.Second)
	futureIndexedAt := now.Add(-time.Minute)
	futureURI := seedComment("future", now.Add(3*time.Hour), futureIndexedAt)
	pastCreatedAt := now.Add(-time.Hour)
	pastURI := seedComment("past", pastCreatedAt, now.Add(-30*time.Minute))
	skewedIndexedAt := now.Add(2 * time.Hour)
	skewedURI := seedComment("skewed", now.Add(3*time.Hour), skewedIndexedAt)

	testkit.MigrateUp(t, db)

	var storedFutureCreatedAt, storedFutureIndexedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT created_at, indexed_at FROM comments WHERE uri = $1
	`, futureURI).Scan(&storedFutureCreatedAt, &storedFutureIndexedAt))
	assert.True(t, storedFutureCreatedAt.UTC().Equal(storedFutureIndexedAt.UTC()),
		"a pre-fix future-dated row must be re-dated to when the AppView first saw it")

	var storedPastCreatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT created_at FROM comments WHERE uri = $1
	`, pastURI).Scan(&storedPastCreatedAt))
	assert.True(t, storedPastCreatedAt.UTC().Equal(pastCreatedAt.UTC()),
		"the repair must not change the created_at of an honest past-dated row")

	var storedSkewedCreatedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT created_at FROM comments WHERE uri = $1
	`, skewedURI).Scan(&storedSkewedCreatedAt))
	assert.False(t, storedSkewedCreatedAt.After(time.Now()),
		"watermark skew must not leave another future timestamp behind")
	assert.True(t, storedSkewedCreatedAt.UTC().Before(skewedIndexedAt.UTC()),
		"the repaired timestamp must be strictly earlier than a future-skewed indexed_at")
}
