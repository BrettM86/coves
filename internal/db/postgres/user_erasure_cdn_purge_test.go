//go:build integration

package postgres

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/require"
)

const erasureBannerCID = "bafkreieuiawc7jwaczgogq6wxbs2gyuajst4lx3sx2np7z56ksfldxiw7a"

func seedErasureReference(t *testing.T, db *sql.DB, owner, blobCID, kind string, active bool) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO blob_references
		(owner_did, blob_cid, subject_uri, kind, active) VALUES ($1, $2, $3, $4, $5)`,
		owner, blobCID, "at://"+owner+"/social.coves.test/"+kind, kind, active)
	require.NoError(t, err)
}

// Complete rows include timestamps and purge fencing fields, so rollback and
// owner isolation cannot pass by preserving only counts or active flags.
func erasureAccountSnapshot(t *testing.T, db *sql.DB, owner string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, table := range []struct{ name, ownerColumn string }{
		{"users", "did"},
		{"blob_references", "owner_did"},
		{"moderation_media_purges", "owner_did"},
		{"deleted_accounts", "did"},
	} {
		var rows string
		require.NoError(t, db.QueryRowContext(t.Context(), fmt.Sprintf(`
			SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text), '[]'::jsonb)::text
			FROM %s r WHERE %s = $1`, table.name, table.ownerColumn), owner).Scan(&rows))
		snapshot[table.name] = rows
	}
	return snapshot
}

func erasurePurgeCIDs(t *testing.T, db *sql.DB, owner string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT blob_cid FROM moderation_media_purges
		WHERE owner_did = $1 ORDER BY blob_cid`, owner)
	require.NoError(t, err)
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var blobCID string
		require.NoError(t, rows.Scan(&blobCID))
		result = append(result, blobCID)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestUserRepoErasureCDNPurgeRecordsDistinctImagesWithoutConfiguredQueue(t *testing.T) {
	db := testkit.DB(t)
	owner, other := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", owner)
	createTestUser(t, db, testkit.UniqueID(t)+".test", other)
	for _, reference := range []struct {
		cid, kind string
		active    bool
	}{
		{communityAvatarA, "post_image", true},
		{communityBannerB, "comment_image", false},
		{communityAvatarC, "avatar", true},
		{erasureBannerCID, "banner", false},
		{communityAvatarA, "external_thumb", false},
	} {
		seedErasureReference(t, db, owner, reference.cid, reference.kind, reference.active)
	}
	seedErasureReference(t, db, other, communityAvatarA, "post_image", true)
	seedErasureReference(t, db, other, contentBackfillCIDs(t, "erasure control", 1)[0], "avatar", false)
	_, err := db.ExecContext(t.Context(), `INSERT INTO moderation_media_purges
		(owner_did, blob_cid, state, attempts, generation, claim)
		VALUES ($1, $2, 'completed', 3, 5, 7)`, other, communityAvatarA)
	require.NoError(t, err)
	otherBefore := erasureAccountSnapshot(t, db, other)

	// The ordinary repository has no queue dependency: targets must survive
	// even while runtime CDN purge is unconfigured.
	require.NoError(t, NewUserRepository(db).Delete(t.Context(), owner))
	require.ElementsMatch(t, []string{communityAvatarA, communityBannerB, communityAvatarC, erasureBannerCID},
		erasurePurgeCIDs(t, db, owner), "record every distinct active or inactive image before removing its references")
	var pending int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM moderation_media_purges
		WHERE owner_did = $1 AND state = 'pending'`, owner).Scan(&pending))
	require.Equal(t, 4, pending)
	after := erasureAccountSnapshot(t, db, owner)
	require.Equal(t, "[]", after["blob_references"])
	require.Equal(t, "[]", after["users"])
	require.Equal(t, otherBefore, erasureAccountSnapshot(t, db, other), "another owner's identical CID stays untouched")
}

func TestUserRepoErasureCDNPurgeResetsCompletedAndBackedOffPendingTargets(t *testing.T) {
	for _, state := range []string{"completed", "pending"} {
		t.Run(state, func(t *testing.T) {
			db := testkit.DB(t)
			owner := "did:plc:" + testkit.UniqueID(t)
			createTestUser(t, db, testkit.UniqueID(t)+".test", owner)
			seedErasureReference(t, db, owner, communityAvatarA, "post_image", true)
			_, err := db.ExecContext(t.Context(), `INSERT INTO moderation_media_purges
				(owner_did, blob_cid, state, attempts, next_attempt_at, earliest_completion_at,
				 last_failure_code, generation, claim)
				VALUES ($1, $2, $3, 4, NOW() + INTERVAL '1 day', NOW() + INTERVAL '1 hour', 'http_500', 7, 9)`,
				owner, communityAvatarA, state)
			require.NoError(t, err)

			require.NoError(t, NewUserRepository(db).Delete(t.Context(), owner))
			var target string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT jsonb_build_object(
				'state', state, 'attempts', attempts, 'dueImmediately', next_attempt_at = '-infinity',
				'earliestCompletionAt', earliest_completion_at, 'lastFailureCode', last_failure_code,
				'generation', generation, 'claim', claim)::text
				FROM moderation_media_purges WHERE owner_did = $1 AND blob_cid = $2`, owner, communityAvatarA).Scan(&target))
			require.JSONEq(t, `{"state":"pending","attempts":0,"dueImmediately":true,
				"earliestCompletionAt":null,"lastFailureCode":null,"generation":8,"claim":10}`, target,
				"erasure uses transaction recording semantics, including resetting pending backoff and fencing old claims")
		})
	}
}

func TestUserRepoErasureCDNPurgeRollsBackWithLateUserDeleteFailure(t *testing.T) {
	db := testkit.DB(t)
	owner := "did:plc:" + testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", owner)
	seedErasureReference(t, db, owner, communityAvatarA, "post_image", true)
	seedErasureReference(t, db, owner, communityBannerB, "comment_image", false)
	_, err := db.ExecContext(t.Context(), `INSERT INTO moderation_media_purges
		(owner_did, blob_cid, state, attempts, next_attempt_at, earliest_completion_at,
		 last_failure_code, generation, claim)
		VALUES ($1, $2, 'completed', 3, NOW() + INTERVAL '1 day', NOW(), 'http_500', 5, 8)`, owner, communityAvatarA)
	require.NoError(t, err)
	before := erasureAccountSnapshot(t, db, owner)
	require.ElementsMatch(t, []string{communityAvatarA}, erasurePurgeCIDs(t, db, owner), "second image has no target before erasure")
	_, err = db.ExecContext(t.Context(), `CREATE FUNCTION reject_erasure_user_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject erasure user delete' USING ERRCODE = 'P4343'; END; $$`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_erasure_user_delete BEFORE DELETE ON users
		FOR EACH ROW EXECUTE FUNCTION reject_erasure_user_delete()`)
	require.NoError(t, err)

	err = NewUserRepository(db).Delete(t.Context(), owner)
	var postgresError *pq.Error
	require.ErrorAs(t, err, &postgresError)
	require.Equal(t, pqerror.Code("P4343"), postgresError.Code, "reach the late users DELETE failure")
	require.Equal(t, before, erasureAccountSnapshot(t, db, owner),
		"rollback restores complete user/reference/target rows and leaves the missing target absent")
}

func TestUserRepoErasureCDNPurgeSkipsMalformedAndNoncanonicalReferences(t *testing.T) {
	db := testkit.DB(t)
	owner := "did:plc:" + testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", owner)
	parsed, err := cid.Decode(communityBannerB)
	require.NoError(t, err)
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	require.NotEqual(t, communityBannerB, alias)
	seedErasureReference(t, db, owner, communityAvatarA, "post_image", true)
	seedErasureReference(t, db, owner, "not-a-cid", "avatar", true)
	seedErasureReference(t, db, owner, alias, "banner", false)

	require.NoError(t, NewUserRepository(db).Delete(t.Context(), owner), "legacy bad references must not block erasure")
	require.Equal(t, []string{communityAvatarA}, erasurePurgeCIDs(t, db, owner),
		"skip malformed and alternate encodings; do not normalize the alias into its otherwise absent canonical target")
	require.Equal(t, "[]", erasureAccountSnapshot(t, db, owner)["blob_references"])
}

func TestUserRepoErasureCDNPurgeConfiguredSweepPurgesEveryPresetAndCompletes(t *testing.T) {
	db := testkit.DB(t)
	owner := "did:plc:" + testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", owner)
	seedErasureReference(t, db, owner, communityAvatarA, "post_image", true)
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	purger, err := imageproxy.NewCloudflarePurger(imageproxy.CloudflarePurgerConfig{
		APIBase: endpoint.URL(), ZoneID: "erasure-zone", APIToken: "erasure-test-credential",
		BaseURLs: []string{"https://img.example.test"}, Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	queue := moderation.NewCDNPurgeQueue(NewModerationRepository(db), purger,
		moderation.CDNPurgeQueueConfig{WriteTimeout: time.Second, Now: func() time.Time { return now }})
	t.Cleanup(queue.Close)

	require.NoError(t, NewUserRepository(db).Delete(t.Context(), owner))
	require.NoError(t, queue.Sweep(t.Context()))
	requests := endpoint.Requests()
	require.NotEmpty(t, requests, "the existing sweep must find the erased account's committed targets")
	var files []string
	for _, request := range requests {
		require.NoError(t, request.DecodeError)
		files = append(files, request.Files...)
	}
	base, suffix := "https://img.example.test/img/", "/plain/"+owner+"/"+communityAvatarA
	require.ElementsMatch(t, []string{
		base + "avatar" + suffix, base + "avatar_small" + suffix, base + "banner" + suffix,
		base + "content_preview" + suffix, base + "content_full" + suffix, base + "embed_thumbnail" + suffix,
	}, files)
	// The first acknowledgement precedes the queue's write-timeout window.
	// Advance the injected clock and let its existing follow-up sweep complete.
	now = now.Add(2 * time.Second)
	require.NoError(t, queue.Sweep(t.Context()))
	var state string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT state FROM moderation_media_purges
		WHERE owner_did = $1 AND blob_cid = $2`, owner, communityAvatarA).Scan(&state))
	require.Equal(t, "completed", state)
}
