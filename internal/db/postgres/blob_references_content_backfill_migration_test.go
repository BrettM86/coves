//go:build integration

package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/lib/pq"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type contentBackfillReference struct {
	OwnerDID   string
	BlobCID    string
	SubjectURI string
	Kind       string
	Active     bool
}

func contentBackfillRows(t *testing.T, db *sql.DB, subjects ...string) []contentBackfillReference {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT owner_did, blob_cid, subject_uri, kind, active
		FROM blob_references WHERE subject_uri = ANY($1)
		ORDER BY subject_uri, owner_did, blob_cid, kind, active
	`, pq.Array(subjects))
	require.NoError(t, err)
	defer rows.Close()
	result := []contentBackfillReference{}
	for rows.Next() {
		var reference contentBackfillReference
		require.NoError(t, rows.Scan(&reference.OwnerDID, &reference.BlobCID, &reference.SubjectURI, &reference.Kind, &reference.Active))
		result = append(result, reference)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestContentBlobReferencesBackfill062_PostEmbeds(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const firstCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	const secondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	const thirdCID = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
	images := seedVisibilityPost(t, db, community, author, testkit.TID(), "images", createdAt)
	external := seedVisibilityPost(t, db, community, author, testkit.TID(), "external", createdAt)
	video := seedVisibilityPost(t, db, community, author, testkit.TID(), "video", createdAt)
	for _, fixture := range []struct{ uri, embed string }{
		{images, fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, firstCID, secondCID)},
		{external, fmt.Sprintf(`{"$type":"social.coves.embed.external","external":{"thumb":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}}`, firstCID, firstCID, secondCID)},
		{video, fmt.Sprintf(`{"$type":"social.coves.embed.video","video":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"video/mp4","size":1},"thumbnail":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1}}`, thirdCID, secondCID)},
	} {
		_, err := db.Exec(`UPDATE posts SET embed = $2::jsonb WHERE uri = $1`, fixture.uri, fixture.embed)
		require.NoError(t, err)
	}
	testkit.MigrateUp(t, db)

	want := []contentBackfillReference{
		{author, firstCID, images, "post_image", true},
		{author, secondCID, images, "post_image", true},
		{author, firstCID, external, "external_thumb", true},
		{author, firstCID, external, "post_image", true},
		{author, secondCID, external, "post_image", true},
		{author, secondCID, video, "video_thumb", true},
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].SubjectURI != want[j].SubjectURI {
			return want[i].SubjectURI < want[j].SubjectURI
		}
		if want[i].BlobCID != want[j].BlobCID {
			return want[i].BlobCID < want[j].BlobCID
		}
		return want[i].Kind < want[j].Kind
	})
	require.Equal(t, want, contentBackfillRows(t, db, images, external, video))
}

func TestContentBlobReferencesBackfill062_LegacyPostCommunityOwnsImages(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	uri := seedLegacyPost(t, db, community, author, testkit.TID(), "legacy images", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	const imageCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	_, err := db.Exec(`UPDATE posts SET embed = $2::jsonb WHERE uri = $1`, uri,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, imageCID))
	require.NoError(t, err)
	testkit.MigrateUp(t, db)

	require.Equal(t, []contentBackfillReference{{community, imageCID, uri, "post_image", true}}, contentBackfillRows(t, db, uri))
}

func TestContentBlobReferencesBackfill062_CommentImages(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	commenter := "did:plc:" + testkit.UniqueID(t)
	root := "at://did:plc:" + testkit.UniqueID(t) + "/social.coves.community.postv2/" + testkit.TID()
	uri := seedActorComment(t, db, commenter, root, testkit.TID(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	const firstCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	const secondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	_, err := db.Exec(`UPDATE comments SET embed = $2::jsonb WHERE uri = $1`, uri,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, firstCID, secondCID))
	require.NoError(t, err)
	testkit.MigrateUp(t, db)

	require.Equal(t, []contentBackfillReference{
		{commenter, firstCID, uri, "comment_image", true},
		{commenter, secondCID, uri, "comment_image", true},
	}, contentBackfillRows(t, db, uri))
}

func TestContentBlobReferencesBackfill062_CanonicalDedupAndInvalidEmbeds(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const firstCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	const secondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	const duplicateCID = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	parsed, err := cid.Decode(firstCID)
	require.NoError(t, err)
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)

	images := seedVisibilityPost(t, db, community, author, testkit.TID(), "mixed images", createdAt)
	nullPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "null embed", createdAt)
	arrayPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "array embed", createdAt)
	nullComment := seedActorComment(t, db, commenter, images, testkit.TID(), createdAt)
	stringComment := seedActorComment(t, db, commenter, images, testkit.TID(), createdAt)
	_, err = db.Exec(`UPDATE posts SET embed = $2::jsonb WHERE uri = $1`, images,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","cid":"%s","mimeType":"image/png"},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"bafynotacid"},"mimeType":"image/png","size":1},"alt":""}]}`, secondCID, alias, duplicateCID, duplicateCID))
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE posts SET embed = '[]'::jsonb WHERE uri = $1`, arrayPost)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE comments SET embed = '"x"'::jsonb WHERE uri = $1`, stringComment)
	require.NoError(t, err)
	testkit.MigrateUp(t, db)

	require.Equal(t, []contentBackfillReference{
		{author, duplicateCID, images, "post_image", true},
		{author, firstCID, images, "post_image", true},
		{author, secondCID, images, "post_image", true},
	}, contentBackfillRows(t, db, images, nullPost, arrayPost, nullComment, stringComment))
}

func TestContentBlobReferencesBackfill062_EmptyBatchesBeforePopulatedBatches(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	postPrefix := testkit.UniqueIDWithPrefix(t, "backfillpost")
	commentPrefix := testkit.UniqueIDWithPrefix(t, "backfillcomment")
	const imageEmbed = `{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},"mimeType":"image/png","size":1},"alt":""}]}`

	// Each table's first 600 IDs carry no references. The next 601 span multiple
	// 500-row batches and must still all be visited after an empty batch.
	_, err := db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		SELECT 'at://' || $1 || '/social.coves.community.postv2/' || $3 || 'null' || n,
		       'bafynullpost' || n, $3 || 'null' || n, $1, $2, 'no embed', NOW()
		FROM generate_series(1, 600) AS n
	`, author, community, postPrefix)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at)
		SELECT 'at://' || $1 || '/social.coves.community.postv2/' || $3 || 'image' || n,
		       'bafyimagepost' || n, $3 || 'image' || n, $1, $2, 'image', $4::jsonb, NOW()
		FROM generate_series(1, 601) AS n
	`, author, community, postPrefix, imageEmbed)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		SELECT 'at://' || $1 || '/social.coves.community.comment/' || $2 || 'null' || n,
		       'bafynullcomment' || n, $2 || 'null' || n, $1, 'at://did:plc:root/social.coves.community.postv2/root',
		       'bafyroot', 'at://did:plc:root/social.coves.community.postv2/root', 'bafyroot', 'no embed', NOW()
		FROM generate_series(1, 600) AS n
	`, commenter, commentPrefix)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, embed, created_at)
		SELECT 'at://' || $1 || '/social.coves.community.comment/' || $2 || 'image' || n,
		       'bafyimagecomment' || n, $2 || 'image' || n, $1, 'at://did:plc:root/social.coves.community.postv2/root',
		       'bafyroot', 'at://did:plc:root/social.coves.community.postv2/root', 'bafyroot', 'image', $3::jsonb, NOW()
		FROM generate_series(1, 601) AS n
	`, commenter, commentPrefix, imageEmbed)
	require.NoError(t, err)
	testkit.MigrateUp(t, db)

	for _, fixture := range []struct{ name, pattern, imagePattern, owner, kind string }{
		{"posts", "at://" + author + "/social.coves.community.postv2/" + postPrefix + "%", "at://" + author + "/social.coves.community.postv2/" + postPrefix + "image%", author, "post_image"},
		{"comments", "at://" + commenter + "/social.coves.community.comment/" + commentPrefix + "%", "at://" + commenter + "/social.coves.community.comment/" + commentPrefix + "image%", commenter, "comment_image"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var total, correct, subjects int
			err := db.QueryRow(`
				SELECT COUNT(*),
				       COUNT(*) FILTER (WHERE owner_did = $2 AND kind = $3 AND active AND subject_uri LIKE $4
				                         AND blob_cid = 'bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm'),
				       COUNT(DISTINCT subject_uri)
				FROM blob_references WHERE subject_uri LIKE $1
			`, fixture.pattern, fixture.owner, fixture.kind, fixture.imagePattern).Scan(&total, &correct, &subjects)
			require.NoError(t, err)
			assert.Equal(t, 601, total)
			assert.Equal(t, 601, correct)
			assert.Equal(t, 601, subjects)
		})
	}
}

func TestContentBlobReferencesBackfill062_SoftDeletedContent(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	postURI := seedVisibilityPost(t, db, community, author, testkit.TID(), "deleted image", createdAt)
	commentURI := seedActorComment(t, db, commenter, postURI, testkit.TID(), createdAt)
	const postCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	const commentCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	_, err := db.Exec(`UPDATE posts SET embed = $2::jsonb, deleted_at = $3 WHERE uri = $1`, postURI,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, postCID), createdAt)
	require.NoError(t, err)
	// The normal soft-delete path clears comment.embed; retain it to exercise historical rows.
	_, err = db.Exec(`UPDATE comments SET embed = $2::jsonb, deleted_at = $3 WHERE uri = $1`, commentURI,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, commentCID), createdAt)
	require.NoError(t, err)
	testkit.MigrateUp(t, db)

	want := []contentBackfillReference{
		{commenter, commentCID, commentURI, "comment_image", false},
		{author, postCID, postURI, "post_image", false},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].SubjectURI < want[j].SubjectURI })
	require.Equal(t, want, contentBackfillRows(t, db, postURI, commentURI))
}

func TestContentBlobReferencesBackfill062_AdminRemovedPostRemainsActive(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", author)
	const imageCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	const postCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
	postURI := seedVisibilityPost(t, db, community, author, testkit.TID(), "removed image", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := db.Exec(`UPDATE posts SET cid = $2, embed = $3::jsonb WHERE uri = $1`, postURI, postCID,
		fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, imageCID))
	require.NoError(t, err)
	seedVisibilityAdmission(t, db, community, postURI, posts.AdmissionStatusAccepted, "", "")
	service := moderation.NewService(
		moderation.NewRepositorySubjectReader(NewPostRepository(db), NewCommentRepository(db)),
		NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	result, err := service.RemoveContent(t.Context(), fixtures.DID("postremovaladmin"), moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: postURI, CID: postCID},
		ExpectedVersion: "v0", IdempotencyKey: "remove-post",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	var decisionCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1 AND active`, postURI).Scan(&decisionCount))
	require.Equal(t, 1, decisionCount)
	var deletedAt sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM posts WHERE uri = $1`, postURI).Scan(&deletedAt))
	require.False(t, deletedAt.Valid)
	testkit.MigrateUp(t, db)

	require.Equal(t, []contentBackfillReference{{author, imageCID, postURI, "post_image", true}}, contentBackfillRows(t, db, postURI))
}

func TestContentBlobReferencesBackfill062_PreservesConsumerRowsAndContentOnDown(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	postURI := seedVisibilityPost(t, db, community, author, testkit.TID(), "existing references", createdAt)
	commentURI := seedActorComment(t, db, commenter, postURI, testkit.TID(), createdAt)
	const firstCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	const secondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	const extraCID = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
	embed := fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""},{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}]}`, firstCID, secondCID)
	_, err := db.Exec(`UPDATE posts SET embed = $2::jsonb WHERE uri = $1`, postURI, embed)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE comments SET embed = $2::jsonb WHERE uri = $1`, commentURI, embed)
	require.NoError(t, err)
	const oldCreated = "2020-02-03T04:05:06Z"
	const oldUpdated = "2021-03-04T05:06:07Z"
	for _, reference := range []contentBackfillReference{
		{author, firstCID, postURI, "post_image", false},
		{author, extraCID, postURI, "post_image", true},
		{commenter, firstCID, commentURI, "comment_image", false},
	} {
		_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, reference.OwnerDID, reference.BlobCID, reference.SubjectURI, reference.Kind, reference.Active, oldCreated, oldUpdated)
		require.NoError(t, err)
	}
	contentSnapshot := func(table, uri string) string {
		t.Helper()
		var snapshot string
		query := `SELECT to_jsonb(p)::text FROM posts p WHERE uri = $1`
		if table == "comments" {
			query = `SELECT to_jsonb(c)::text FROM comments c WHERE uri = $1`
		}
		require.NoError(t, db.QueryRow(query, uri).Scan(&snapshot))
		require.NotEmpty(t, snapshot)
		return snapshot
	}
	postBefore := contentSnapshot("posts", postURI)
	commentBefore := contentSnapshot("comments", commentURI)
	checkPreserved := func() {
		t.Helper()
		assert.Equal(t, postBefore, contentSnapshot("posts", postURI))
		assert.Equal(t, commentBefore, contentSnapshot("comments", commentURI))
		for _, reference := range []contentBackfillReference{
			{author, firstCID, postURI, "post_image", false},
			{author, extraCID, postURI, "post_image", true},
			{commenter, firstCID, commentURI, "comment_image", false},
		} {
			var created, updated time.Time
			err := db.QueryRow(`SELECT created_at, updated_at FROM blob_references
				WHERE owner_did = $1 AND blob_cid = $2 AND subject_uri = $3 AND kind = $4`,
				reference.OwnerDID, reference.BlobCID, reference.SubjectURI, reference.Kind).Scan(&created, &updated)
			require.NoError(t, err)
			assert.Equal(t, oldCreated, created.UTC().Format(time.RFC3339))
			assert.Equal(t, oldUpdated, updated.UTC().Format(time.RFC3339))
		}
	}
	want := []contentBackfillReference{
		{commenter, firstCID, commentURI, "comment_image", false},
		{commenter, secondCID, commentURI, "comment_image", true},
		{author, extraCID, postURI, "post_image", true},
		{author, firstCID, postURI, "post_image", false},
		{author, secondCID, postURI, "post_image", true},
	}
	testkit.MigrateUp(t, db)
	assert.ElementsMatch(t, want, contentBackfillRows(t, db, postURI, commentURI))
	checkPreserved()
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	assert.ElementsMatch(t, want, contentBackfillRows(t, db, postURI, commentURI))
	checkPreserved()
}

// contentBackfillCIDs returns count distinct sha2-256 blob CIDs.
func contentBackfillCIDs(t *testing.T, label string, count int) []string {
	t.Helper()
	result := make([]string, 0, count)
	for index := 0; index < count; index++ {
		hash, err := multihash.Sum([]byte(fmt.Sprintf("%s-%d", label, index)), multihash.SHA2_256, -1)
		require.NoError(t, err)
		result = append(result, cid.NewCidV1(cid.Raw, hash).String())
	}
	return result
}

func contentBackfillImageEntries(cids []string) string {
	entries := make([]string, 0, len(cids))
	for _, imageCID := range cids {
		entries = append(entries, fmt.Sprintf(`{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":1},"alt":""}`, imageCID))
	}
	return "[" + strings.Join(entries, ",") + "]"
}

func contentBackfillImagesEmbed(cids []string) string {
	return `{"$type":"social.coves.embed.images","images":` + contentBackfillImageEntries(cids) + `}`
}

func TestContentBlobReferencesBackfill062_SkipsRecordsOverImageLimit(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nine := contentBackfillCIDs(t, "nine", 9)
	eight := contentBackfillCIDs(t, "eight", 8)

	nineImagesPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "nine images", createdAt)
	nineExternalPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "nine external images", createdAt)
	eightImagesPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "eight images", createdAt)
	nineImagesComment := seedActorComment(t, db, commenter, eightImagesPost, testkit.TID(), createdAt)
	for _, fixture := range []struct{ table, uri, embed string }{
		{"posts", nineImagesPost, contentBackfillImagesEmbed(nine)},
		{"posts", nineExternalPost, `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com","images":` + contentBackfillImageEntries(nine) + `}}`},
		{"posts", eightImagesPost, contentBackfillImagesEmbed(eight)},
		{"comments", nineImagesComment, contentBackfillImagesEmbed(nine)},
	} {
		_, err := db.Exec(`UPDATE `+fixture.table+` SET embed = $2::jsonb WHERE uri = $1`, fixture.uri, fixture.embed)
		require.NoError(t, err)
	}
	testkit.MigrateUp(t, db)

	assert.Empty(t, contentBackfillRows(t, db, nineImagesPost, nineExternalPost, nineImagesComment),
		"the live consumers refuse a record with more than eight images, so the backfill must write no references for it")
	assert.Len(t, contentBackfillRows(t, db, eightImagesPost), 8, "exactly eight images is within the lexicon maximum")
}

func TestContentBlobReferencesBackfill062_CensusCountsEverySkip(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	require.EqualValues(t, 64, testkit.MigrateDownOne(t, db, 64))
	require.EqualValues(t, 63, testkit.MigrateDownOne(t, db, 63))
	require.EqualValues(t, 62, testkit.MigrateDownOne(t, db, 62))
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cids := contentBackfillCIDs(t, "census", 3)
	nine := contentBackfillCIDs(t, "census-nine", 9)

	// Posts: one with a duplicate and a malformed entry (one reference
	// extracted), one under an unknown collection, one non-object embed, one
	// over the image limit, and one whose reference already exists.
	droppedEntries := seedVisibilityPost(t, db, community, author, testkit.TID(), "dropped entries", createdAt)
	unknownCollection := "at://" + author + "/app.bsky.feed.post/" + testkit.TID()
	_, err := db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at)
		VALUES ($1, 'bafyunknowncollection', $2, $3, $4, 'unknown collection', $5::jsonb, NOW())
	`, unknownCollection, testkit.TID(), author, community, contentBackfillImagesEmbed(cids[:1]))
	require.NoError(t, err)
	arrayPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "array embed", createdAt)
	overLimitPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "over limit", createdAt)
	existingPost := seedVisibilityPost(t, db, community, author, testkit.TID(), "existing reference", createdAt)
	// Comments: one non-object embed, one over the image limit, one plain image.
	stringComment := seedActorComment(t, db, commenter, droppedEntries, testkit.TID(), createdAt)
	overLimitComment := seedActorComment(t, db, commenter, droppedEntries, testkit.TID(), createdAt)
	imageComment := seedActorComment(t, db, commenter, droppedEntries, testkit.TID(), createdAt)
	for _, fixture := range []struct{ table, uri, embed string }{
		{"posts", droppedEntries, `{"$type":"social.coves.embed.images","images":` + strings.TrimSuffix(contentBackfillImageEntries([]string{cids[0], cids[0]}), "]") +
			`,{"image":{"$type":"blob","ref":{"$link":"bafynotacid"},"mimeType":"image/png","size":1},"alt":""}]}`},
		{"posts", arrayPost, `[]`},
		{"posts", overLimitPost, contentBackfillImagesEmbed(nine)},
		{"posts", existingPost, contentBackfillImagesEmbed(cids[1:2])},
		{"comments", stringComment, `"x"`},
		{"comments", overLimitComment, contentBackfillImagesEmbed(nine)},
		{"comments", imageComment, contentBackfillImagesEmbed(cids[2:3])},
	} {
		_, err := db.Exec(`UPDATE `+fixture.table+` SET embed = $2::jsonb WHERE uri = $1`, fixture.uri, fixture.embed)
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active) VALUES ($1, $2, $3, 'post_image', true)`,
		author, cids[1], existingPost)
	require.NoError(t, err)

	var captured bytes.Buffer
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	testkit.MigrateUp(t, db)
	log.SetOutput(os.Stderr)

	var census string
	for _, line := range strings.Split(captured.String(), "\n") {
		if strings.Contains(line, "content blob references backfill:") {
			census = line
		}
	}
	require.NotEmpty(t, census, "the backfill must log its census line; captured log:\n%s", captured.String())
	assert.Contains(t, census, "content blob references backfill: posts scanned=5 comments scanned=3"+
		" posts skipped unknown collection=1 records non-object embed=2 records over image limit=2"+
		" records with skipped or duplicate blob entries=1 references extracted=3 references inserted=2")
}
