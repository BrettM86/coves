//go:build integration

package postgres

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/lib/pq"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/require"
)

func TestCommunityBlobReferencesBackfill064_PreservesExistingReferencesAndCommunities(t *testing.T) {
	db := testkit.DB(t)
	var existingCommunities int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM communities`).Scan(&existingCommunities))
	require.Zero(t, existingCommunities, "the cloned database must contain no communities")
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))

	parsed, err := cid.Decode(communityAvatarC)
	require.NoError(t, err)
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	require.NotEqual(t, communityAvatarC, alias)
	prefix := testkit.UniqueID(t)
	dids := make([]string, 0, 7)
	subjects := make([]string, 0, 7)
	for index, images := range []struct{ avatar, banner any }{
		{communityAvatarA, communityBannerB},
		{communityAvatarA, nil},
		{nil, nil},
		{"", ""},
		{"bafynotacid", communityBannerB},
		{alias, nil},
		{communityAvatarA, communityBannerB},
	} {
		did := fmt.Sprintf("did:plc:%sk%d", prefix, index+1)
		seedCommunityProfileRow(t, db, did, images.avatar, images.banner)
		dids = append(dids, did)
		subjects = append(subjects, communityProfileSubject(did))
	}
	const oldCreated = "2020-01-01T00:00:00Z"
	const oldUpdated = "2020-01-02T00:00:00Z"
	_, err = db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active, created_at, updated_at)
		VALUES ($1, $2, $3, 'avatar', false, $4, $5)`, dids[6], communityAvatarA, subjects[6], oldCreated, oldUpdated)
	require.NoError(t, err)
	communitiesSnapshot := func() string {
		t.Helper()
		var snapshot string
		require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.did), '[]'::jsonb)::text
			FROM communities c`).Scan(&snapshot))
		return snapshot
	}
	referencesSnapshot := func() string {
		t.Helper()
		var snapshot string
		require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.subject_uri, r.blob_cid, r.kind), '[]'::jsonb)::text
			FROM blob_references r WHERE subject_uri = ANY($1)`, pq.Array(subjects)).Scan(&snapshot))
		return snapshot
	}
	communitiesBefore := communitiesSnapshot()
	require.NotEqual(t, "[]", communitiesBefore)
	var existingBefore string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(r)::text FROM blob_references r
		WHERE owner_did = $1 AND subject_uri = $2 AND kind = 'avatar'`, dids[6], subjects[6]).Scan(&existingBefore))

	var captured bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(originalOutput) })
	testkit.MigrateUp(t, db)
	log.SetOutput(originalOutput)

	const census = "community blob references backfill: communities scanned=7 references inserted=6 malformed CIDs skipped=1"
	censusLines := 0
	for _, line := range strings.Split(captured.String(), "\n") {
		if strings.Contains(line, "community blob references backfill:") {
			censusLines++
			require.True(t, strings.HasSuffix(line, census), "unexpected census line: %s", line)
		}
	}
	require.Equal(t, 1, censusLines, "expected one community backfill census line")
	for _, did := range dids {
		require.NotContains(t, captured.String(), did)
	}
	require.ElementsMatch(t, []contentBackfillReference{
		{dids[0], communityAvatarA, subjects[0], "avatar", true},
		{dids[0], communityBannerB, subjects[0], "banner", true},
		{dids[1], communityAvatarA, subjects[1], "avatar", true},
		{dids[4], communityBannerB, subjects[4], "banner", true},
		{dids[5], communityAvatarC, subjects[5], "avatar", true},
		{dids[6], communityAvatarA, subjects[6], "avatar", false},
		{dids[6], communityBannerB, subjects[6], "banner", true},
	}, contentBackfillRows(t, db, subjects...))
	require.Equal(t, communitiesBefore, communitiesSnapshot())
	var existingAfter string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(r)::text FROM blob_references r
		WHERE owner_did = $1 AND subject_uri = $2 AND kind = 'avatar'`, dids[6], subjects[6]).Scan(&existingAfter))
	require.Equal(t, existingBefore, existingAfter)
	var created, updated time.Time
	require.NoError(t, db.QueryRow(`SELECT created_at, updated_at FROM blob_references
		WHERE owner_did = $1 AND subject_uri = $2 AND kind = 'avatar'`, dids[6], subjects[6]).Scan(&created, &updated))
	require.Equal(t, oldCreated, created.UTC().Format(time.RFC3339))
	require.Equal(t, oldUpdated, updated.UTC().Format(time.RFC3339))
	firstRun := referencesSnapshot()
	require.NotEqual(t, "[]", firstRun)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	testkit.MigrateUp(t, db)
	require.Equal(t, firstRun, referencesSnapshot())
	require.Equal(t, communitiesBefore, communitiesSnapshot())
}

func TestCommunityBlobReferencesBackfill064_PagesPastCommunitiesWithoutImages(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	prefix := testkit.UniqueIDWithPrefix(t, "cmbf")
	_, err := db.Exec(`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, avatar_cid, created_at)
		SELECT did, did || '.test', 'Community', did, did, did,
			CASE WHEN n <= 600 THEN NULL ELSE $2 END, NOW()
		FROM (SELECT 'did:plc:' || $1 || 'k' || lpad(n::text, 4, '0') AS did, n
			FROM generate_series(1, 1201) AS n) AS seed`, prefix, communityAvatarA)
	require.NoError(t, err)
	testkit.MigrateUp(t, db)
	var total, correct, subjects int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*),
		COUNT(*) FILTER (WHERE owner_did >= $2 AND blob_cid = $3 AND kind = 'avatar' AND active
			AND subject_uri = 'at://' || owner_did || '/social.coves.community.profile/self'),
		COUNT(DISTINCT subject_uri)
		FROM blob_references WHERE owner_did LIKE $1`, "did:plc:"+prefix+"k%", "did:plc:"+prefix+"k0601", communityAvatarA).Scan(&total, &correct, &subjects))
	require.Equal(t, 601, total)
	require.Equal(t, 601, correct)
	require.Equal(t, 601, subjects)
}
