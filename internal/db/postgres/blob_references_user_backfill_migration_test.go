//go:build integration

package postgres

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/lib/pq"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserBlobReferencesBackfill063_PreservesExistingReferencesAndUsers(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	prefix := testkit.UniqueID(t)
	const (
		avatar = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
		banner = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	)
	// A valid CIDv1 stored under its base58btc alias must be backfilled as its
	// canonical base32 string, not as the raw column value.
	aliasHash, err := multihash.Sum([]byte("user backfill base58btc alias avatar"), multihash.SHA2_256, -1)
	require.NoError(t, err)
	aliasCID := cid.NewCidV1(cid.Raw, aliasHash)
	aliasAvatar, err := aliasCID.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	canonicalAliasAvatar := aliasCID.String()
	require.True(t, strings.HasPrefix(canonicalAliasAvatar, "bafkrei"), canonicalAliasAvatar)
	require.NotEqual(t, canonicalAliasAvatar, aliasAvatar)
	var dids, subjects []string
	for index, images := range []struct{ avatar, banner any }{
		{avatar, banner}, {avatar, nil}, {nil, nil}, {"", ""}, {"bafynotacid", banner}, {avatar, banner}, {aliasAvatar, nil},
	} {
		did := fmt.Sprintf("did:plc:%su%d", prefix, index+1)
		createTestUser(t, db, fmt.Sprintf("%su%d.test", prefix, index+1), did)
		_, err := db.Exec(`UPDATE users SET avatar_cid = $2, banner_cid = $3 WHERE did = $1`, did, images.avatar, images.banner)
		require.NoError(t, err)
		dids = append(dids, did)
		subjects = append(subjects, "at://"+did+"/social.coves.actor.profile/self")
	}
	const oldCreated = "2020-01-01T00:00:00Z"
	const oldUpdated = "2020-01-02T00:00:00Z"
	_, err = db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active, created_at, updated_at)
		VALUES ($1, $2, $3, 'avatar', false, $4, $5)`, dids[5], avatar, subjects[5], oldCreated, oldUpdated)
	require.NoError(t, err)
	usersSnapshot := func() string {
		t.Helper()
		var snapshot string
		require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(u) ORDER BY u.did), '[]'::jsonb)::text
			FROM users u WHERE did = ANY($1)`, pq.Array(dids)).Scan(&snapshot))
		return snapshot
	}
	referencesSnapshot := func() string {
		t.Helper()
		var snapshot string
		require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.subject_uri, r.blob_cid, r.kind), '[]'::jsonb)::text
			FROM blob_references r WHERE subject_uri = ANY($1)`, pq.Array(subjects)).Scan(&snapshot))
		return snapshot
	}
	usersBefore := usersSnapshot()
	require.NotEqual(t, "[]", usersBefore)
	var existingBefore string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(r)::text FROM blob_references r
		WHERE owner_did = $1 AND subject_uri = $2 AND kind = 'avatar'`, dids[5], subjects[5]).Scan(&existingBefore))

	var captured bytes.Buffer
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	testkit.MigrateUp(t, db)
	log.SetOutput(os.Stderr)

	var census string
	for _, line := range strings.Split(captured.String(), "\n") {
		if strings.Contains(line, "user blob references backfill:") {
			census = line
		}
	}
	require.NotEmpty(t, census, "the backfill must log its census line; captured log:\n%s", captured.String())
	assert.Contains(t, census, "user blob references backfill: users scanned=7 references inserted=6 malformed CIDs skipped=1")
	assert.NotContains(t, captured.String(), prefix, "the backfill log must not name any user")
	for _, did := range dids {
		assert.NotContains(t, captured.String(), did, "the backfill log must not name any user")
	}

	require.ElementsMatch(t, []contentBackfillReference{
		{dids[0], avatar, subjects[0], "avatar", true},
		{dids[0], banner, subjects[0], "banner", true},
		{dids[1], avatar, subjects[1], "avatar", true},
		{dids[4], banner, subjects[4], "banner", true},
		{dids[5], avatar, subjects[5], "avatar", false},
		{dids[5], banner, subjects[5], "banner", true},
		{dids[6], canonicalAliasAvatar, subjects[6], "avatar", true},
	}, contentBackfillRows(t, db, subjects...))
	require.Equal(t, usersBefore, usersSnapshot())
	var existingAfter string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(r)::text FROM blob_references r
		WHERE owner_did = $1 AND subject_uri = $2 AND kind = 'avatar'`, dids[5], subjects[5]).Scan(&existingAfter))
	require.Equal(t, existingBefore, existingAfter)
	var created, updated time.Time
	require.NoError(t, db.QueryRow(`SELECT created_at, updated_at FROM blob_references
		WHERE owner_did = $1 AND subject_uri = $2 AND kind = 'avatar'`, dids[5], subjects[5]).Scan(&created, &updated))
	require.Equal(t, oldCreated, created.UTC().Format(time.RFC3339))
	require.Equal(t, oldUpdated, updated.UTC().Format(time.RFC3339))
	firstRun := referencesSnapshot()
	require.NotEqual(t, "[]", firstRun)

	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.Equal(t, firstRun, referencesSnapshot())
	require.Equal(t, usersBefore, usersSnapshot())
	testkit.MigrateUp(t, db)
	require.Equal(t, firstRun, referencesSnapshot())
	require.Equal(t, usersBefore, usersSnapshot())
}

func TestUserBlobReferencesBackfill063_PagesPastUsersWithoutImages(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	prefix := testkit.UniqueIDWithPrefix(t, "usrbf")
	const avatar = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	_, err := db.Exec(`INSERT INTO users (did, handle, pds_url, avatar_cid)
		SELECT 'did:plc:' || $1 || 'u' || lpad(n::text, 4, '0'),
		       $1 || 'u' || lpad(n::text, 4, '0') || '.test', $2,
		       CASE WHEN n <= 600 THEN NULL ELSE $3 END
		FROM generate_series(1, 1201) AS n`, prefix, testkit.Endpoints().PDS.BaseURL, avatar)
	require.NoError(t, err)
	testkit.MigrateUp(t, db)
	var total, correct, subjects int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*),
		COUNT(*) FILTER (WHERE owner_did >= $2 AND blob_cid = $3 AND kind = 'avatar' AND active
			AND subject_uri = 'at://' || owner_did || '/social.coves.actor.profile/self'),
		COUNT(DISTINCT subject_uri)
		FROM blob_references WHERE owner_did LIKE $1`, "did:plc:"+prefix+"u%", "did:plc:"+prefix+"u0601", avatar).Scan(&total, &correct, &subjects))
	require.Equal(t, 601, total)
	require.Equal(t, 601, correct)
	require.Equal(t, 601, subjects)
}
