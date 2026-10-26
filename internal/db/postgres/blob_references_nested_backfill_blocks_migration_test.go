//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nestedBackfillBlock struct {
	OwnerDID sql.NullString
	BlobCID  string
	ActionID string
	Active   bool
}

func nestedBackfillBlocks(t *testing.T, db *sql.DB) []nestedBackfillBlock {
	t.Helper()
	rows, err := db.Query(`SELECT owner_did, blob_cid, action_id, active FROM moderation_media_blocks`)
	require.NoError(t, err)
	defer rows.Close()
	var blocks []nestedBackfillBlock
	for rows.Next() {
		var block nestedBackfillBlock
		require.NoError(t, rows.Scan(&block.OwnerDID, &block.BlobCID, &block.ActionID, &block.Active))
		blocks = append(blocks, block)
	}
	require.NoError(t, rows.Err())
	return blocks
}

func nestedBackfillRemoval(t *testing.T, db *sql.DB, uri string) string {
	t.Helper()
	seedModerationDecision(t, db, uri, "removal", "", true)
	var action string
	require.NoError(t, db.QueryRow(`SELECT active_action_id FROM moderation_decisions WHERE subject_uri = $1`, uri).Scan(&action))
	return action
}

type nestedBackfillRemovedSubjects struct {
	Author, Commenter, Post, Comment, PostAction, CommentAction string
}

func nestedBackfillSeedRemovedSubjects(t *testing.T, db *sql.DB) nestedBackfillRemovedSubjects {
	t.Helper()
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	fixture := nestedBackfillRemovedSubjects{Author: "did:plc:" + testkit.UniqueID(t), Commenter: "did:plc:" + testkit.UniqueID(t)}
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fixture.Post = seedVisibilityPost(t, db, community, fixture.Author, testkit.TID(), "removed nested post", createdAt)
	fixture.Comment = seedActorComment(t, db, fixture.Commenter, fixture.Post, testkit.TID(), createdAt)
	nestedBackfillSetEmbed(t, db, "posts", fixture.Post, nestedBackfillEmbed([]string{nestedBackfillFirstCID, nestedBackfillSecondCID},
		`{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/article","thumb":{"ref":{"$link":"`+nestedBackfillFirstCID+`"}},"images":`+contentBackfillImageEntries([]string{nestedBackfillThirdCID})+`}}`))
	nestedBackfillSetEmbed(t, db, "comments", fixture.Comment, nestedBackfillEmbed([]string{nestedBackfillSecondCID, nestedBackfillThirdCID}, nestedBackfillQuote()))
	fixture.PostAction = nestedBackfillRemoval(t, db, fixture.Post)
	fixture.CommentAction = nestedBackfillRemoval(t, db, fixture.Comment)
	return fixture
}

func nestedBackfillNoPurgeTargets(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM moderation_media_purges`).Scan(&count))
	assert.Zero(t, count, "065 has no runtime CDN configuration")
}

func TestNestedBlobReferencesBackfill065_LocalRemovalBlocksEveryCID(t *testing.T) {
	db := nestedBackfillDatabase(t)
	fixture := nestedBackfillSeedRemovedSubjects(t, db)
	require.Empty(t, nestedBackfillBlocks(t, db))

	testkit.MigrateUp(t, db)

	author := sql.NullString{String: fixture.Author, Valid: true}
	commenter := sql.NullString{String: fixture.Commenter, Valid: true}
	assert.ElementsMatch(t, []nestedBackfillBlock{
		{author, nestedBackfillFirstCID, fixture.PostAction, true},
		{author, nestedBackfillSecondCID, fixture.PostAction, true},
		{author, nestedBackfillThirdCID, fixture.PostAction, true},
		{commenter, nestedBackfillSecondCID, fixture.CommentAction, true},
		{commenter, nestedBackfillThirdCID, fixture.CommentAction, true},
	}, nestedBackfillBlocks(t, db))
	// The gallery and thumbnail share a CID: two reference kinds, one block.
	assert.ElementsMatch(t, []contentBackfillReference{
		{fixture.Author, nestedBackfillFirstCID, fixture.Post, "post_image", true},
		{fixture.Author, nestedBackfillFirstCID, fixture.Post, "external_thumb", true},
		{fixture.Author, nestedBackfillSecondCID, fixture.Post, "post_image", true},
		{fixture.Author, nestedBackfillThirdCID, fixture.Post, "post_image", true},
		{fixture.Commenter, nestedBackfillSecondCID, fixture.Comment, "comment_image", true},
		{fixture.Commenter, nestedBackfillThirdCID, fixture.Comment, "comment_image", true},
	}, contentBackfillRows(t, db, fixture.Post, fixture.Comment))
	nestedBackfillNoPurgeTargets(t, db)
}

func TestNestedBlobReferencesBackfill065_IgnoresOtherDecisionsAndOwners(t *testing.T) {
	db := nestedBackfillDatabase(t)
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author, otherOwner := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	removed := seedVisibilityPost(t, db, community, author, testkit.TID(), "local removal", createdAt)
	shared := seedVisibilityPost(t, db, community, otherOwner, testkit.TID(), "same CID another owner", createdAt)
	embed := nestedBackfillEmbed([]string{nestedBackfillFirstCID}, nestedBackfillQuote())
	for _, uri := range []string{removed, shared} {
		nestedBackfillSetEmbed(t, db, "posts", uri, embed)
	}
	_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active, created_at, updated_at)
		VALUES ($1, $2, $3, 'post_image', true, $4, $4)`, otherOwner, nestedBackfillFirstCID, shared, createdAt)
	require.NoError(t, err)
	sharedReferencesBefore := nestedBackfillSnapshot(t, db, "blob_references", "WHERE subject_uri = $1", shared)
	action := nestedBackfillRemoval(t, db, removed)
	for _, table := range []string{"posts", "comments"} {
		for _, decision := range []string{"inherited", "inactive", "community", "label"} {
			var uri string
			if table == "posts" {
				uri = seedVisibilityPost(t, db, community, author, testkit.TID(), decision, createdAt)
			} else {
				uri = seedActorComment(t, db, author, removed, testkit.TID(), createdAt)
			}
			nestedBackfillSetEmbed(t, db, table, uri, embed)
			kind, scope := "removal", ""
			if decision == "label" {
				kind = "label"
			}
			if decision == "community" {
				scope = community
			}
			seedModerationDecision(t, db, uri, kind, scope, decision != "inactive")
			if decision == "inherited" {
				_, err := db.Exec(`UPDATE moderation_actions SET origin = 'inherited' WHERE subject_uri = $1`, uri)
				require.NoError(t, err)
			}
		}
	}

	testkit.MigrateUp(t, db)

	assert.Equal(t, []nestedBackfillBlock{{sql.NullString{String: author, Valid: true}, nestedBackfillFirstCID, action, true}}, nestedBackfillBlocks(t, db))
	assert.Equal(t, []contentBackfillReference{{otherOwner, nestedBackfillFirstCID, shared, "post_image", true}}, contentBackfillRows(t, db, shared))
	assert.Equal(t, sharedReferencesBefore, nestedBackfillSnapshot(t, db, "blob_references", "WHERE subject_uri = $1", shared))
	assert.Equal(t, []contentBackfillReference{{author, nestedBackfillFirstCID, removed, "post_image", true}}, contentBackfillRows(t, db, removed))
	nestedBackfillNoPurgeTargets(t, db)
}

// JSONB snapshots include every stored column, including identity and timestamps.
func nestedBackfillSnapshot(t *testing.T, db *sql.DB, table, predicate string, arguments ...any) string {
	t.Helper()
	require.Contains(t, []string{"posts", "comments", "blob_references", "moderation_actions", "moderation_decisions", "moderation_subjects", "moderation_media_blocks", "moderation_media_purges"}, table)
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text), '[]'::jsonb)::text FROM `+table+` r `+predicate, arguments...).Scan(&snapshot))
	return snapshot
}

func TestNestedBlobReferencesBackfill065_InsertOnlyDownAndRerun(t *testing.T) {
	db := nestedBackfillDatabase(t)
	fixture := nestedBackfillSeedRemovedSubjects(t, db)
	const oldCreated = "2020-02-03T04:05:06Z"
	const oldUpdated = "2021-03-04T05:06:07Z"
	_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active, created_at, updated_at)
		VALUES ($1, $2, $3, 'post_image', false, $7, $8), ($4, $5, $6, 'comment_image', false, $7, $8)`,
		fixture.Author, nestedBackfillFirstCID, fixture.Post, fixture.Commenter, nestedBackfillSecondCID, fixture.Comment, oldCreated, oldUpdated)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO moderation_media_blocks (owner_did, blob_cid, action_id, active)
		VALUES ($1, $2, $3, false), ($4, $5, $6, false)`,
		fixture.Author, nestedBackfillFirstCID, fixture.PostAction, fixture.Commenter, nestedBackfillSecondCID, fixture.CommentAction)
	require.NoError(t, err)
	// Existing other embed types must not acquire references or blocks in 065.
	otherPost := seedVisibilityPost(t, db, visibilityCommunity(t, db, testkit.UniqueID(t)), fixture.Author, testkit.TID(), "ordinary images", time.Now().UTC())
	otherComment := seedActorComment(t, db, fixture.Commenter, otherPost, testkit.TID(), time.Now().UTC())
	for _, subject := range []struct{ table, uri string }{{"posts", otherPost}, {"comments", otherComment}} {
		nestedBackfillSetEmbed(t, db, subject.table, subject.uri, contentBackfillImagesEmbed([]string{nestedBackfillFirstCID}))
		nestedBackfillRemoval(t, db, subject.uri)
	}
	_, err = db.Exec(`INSERT INTO moderation_subjects (subject_uri, version, updated_at) VALUES ($1, 7, $3), ($2, 11, $3)`, fixture.Post, fixture.Comment, oldUpdated)
	require.NoError(t, err)

	unchanged := map[string]string{}
	for _, table := range []string{"posts", "comments", "moderation_actions", "moderation_decisions", "moderation_subjects", "moderation_media_purges"} {
		unchanged[table] = nestedBackfillSnapshot(t, db, table, "")
	}
	var blockID int64
	require.NoError(t, db.QueryRow(`SELECT MAX(id) FROM moderation_media_blocks`).Scan(&blockID))
	referencesBefore := nestedBackfillSnapshot(t, db, "blob_references", "")
	blocksBefore := nestedBackfillSnapshot(t, db, "moderation_media_blocks", "WHERE id <= $1", blockID)

	testkit.MigrateUp(t, db)

	assert.Equal(t, referencesBefore, nestedBackfillSnapshot(t, db, "blob_references", "WHERE created_at = $1", oldCreated))
	assert.Equal(t, blocksBefore, nestedBackfillSnapshot(t, db, "moderation_media_blocks", "WHERE id <= $1", blockID))
	assert.Empty(t, contentBackfillRows(t, db, otherPost, otherComment))
	author := sql.NullString{String: fixture.Author, Valid: true}
	commenter := sql.NullString{String: fixture.Commenter, Valid: true}
	assert.ElementsMatch(t, []nestedBackfillBlock{
		{author, nestedBackfillFirstCID, fixture.PostAction, false},
		{author, nestedBackfillSecondCID, fixture.PostAction, true},
		{author, nestedBackfillThirdCID, fixture.PostAction, true},
		{commenter, nestedBackfillSecondCID, fixture.CommentAction, false},
		{commenter, nestedBackfillThirdCID, fixture.CommentAction, true},
	}, nestedBackfillBlocks(t, db))
	assert.ElementsMatch(t, []contentBackfillReference{
		{fixture.Author, nestedBackfillFirstCID, fixture.Post, "post_image", false},
		{fixture.Author, nestedBackfillFirstCID, fixture.Post, "external_thumb", true},
		{fixture.Author, nestedBackfillSecondCID, fixture.Post, "post_image", true},
		{fixture.Author, nestedBackfillThirdCID, fixture.Post, "post_image", true},
		{fixture.Commenter, nestedBackfillSecondCID, fixture.Comment, "comment_image", false},
		{fixture.Commenter, nestedBackfillThirdCID, fixture.Comment, "comment_image", true},
	}, contentBackfillRows(t, db, fixture.Post, fixture.Comment))
	for table, before := range unchanged {
		assert.Equal(t, before, nestedBackfillSnapshot(t, db, table, ""), "%s must not be rewritten", table)
	}
	unchanged["blob_references"] = nestedBackfillSnapshot(t, db, "blob_references", "")
	unchanged["moderation_media_blocks"] = nestedBackfillSnapshot(t, db, "moderation_media_blocks", "")
	checkUnchanged := func() {
		t.Helper()
		for table, before := range unchanged {
			assert.Equal(t, before, nestedBackfillSnapshot(t, db, table, ""), "%s changed on Down or rerun", table)
		}
	}
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	checkUnchanged()
	testkit.MigrateUp(t, db)
	checkUnchanged()
}

func TestNestedBlobReferencesBackfill065_PaginatesPostsAndComments(t *testing.T) {
	db := nestedBackfillDatabase(t)
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author, commenter := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	prefix := testkit.UniqueIDWithPrefix(t, "nestedpages")
	embed := nestedBackfillEmbed([]string{nestedBackfillFirstCID}, nestedBackfillQuote())
	unrelated := contentBackfillImagesEmbed([]string{nestedBackfillSecondCID})
	// 600 image-free/unrelated leading rows precede 601 nested rows in each
	// table. This also crosses a page boundary when scanning only nested rows.
	_, err := db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at)
		SELECT 'at://' || $1 || '/social.coves.community.postv2/' || $3 || n,
		       'bafypagepost' || n, $3 || n, $1, $2, 'page fixture',
		       CASE WHEN n > 600 THEN $4::jsonb WHEN n % 2 = 0 THEN $5::jsonb ELSE NULL END, NOW()
		FROM generate_series(1, 1201) AS n ORDER BY n`, author, community, prefix, embed, unrelated)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, embed, created_at)
		SELECT 'at://' || $1 || '/social.coves.community.comment/' || $2 || n,
		       'bafypagecomment' || n, $2 || n, $1, 'at://did:plc:root/social.coves.community.postv2/root',
		       'bafyroot', 'at://did:plc:root/social.coves.community.postv2/root', 'bafyroot', 'page fixture',
		       CASE WHEN n > 600 THEN $3::jsonb WHEN n % 2 = 0 THEN $4::jsonb ELSE NULL END, NOW()
		FROM generate_series(1, 1201) AS n ORDER BY n`, commenter, prefix, embed, unrelated)
	require.NoError(t, err)
	// All rows are removed, so accidentally processing other embed types would
	// show up in both reference and block counts.
	_, err = db.Exec(`INSERT INTO moderation_actions (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, action, origin, created_at)
		SELECT $1 || uri, 'did:plc:moderator', 'did:plc:instance', 'instance', uri, collection, 'remove', 'local', NOW()
		FROM (SELECT uri, 'social.coves.community.postv2' AS collection FROM posts WHERE author_did = $2
		      UNION ALL SELECT uri, 'social.coves.community.comment' FROM comments WHERE commenter_did = $3) subjects`, prefix, author, commenter)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO moderation_decisions (authority_did, scope_kind, subject_uri, kind, active_action_id, active)
		SELECT authority_did, scope_kind, subject_uri, 'removal', id, true FROM moderation_actions WHERE id LIKE $1`, prefix+"%")
	require.NoError(t, err)

	testkit.MigrateUp(t, db)

	for _, subject := range []struct{ owner, collection, kind string }{
		{author, "social.coves.community.postv2", "post_image"},
		{commenter, "social.coves.community.comment", "comment_image"},
	} {
		pattern := "at://" + subject.owner + "/" + subject.collection + "/" + prefix + "%"
		var total, correct, distinctSubjects int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE owner_did = $2 AND kind = $3 AND active AND blob_cid = $4), COUNT(DISTINCT subject_uri)
			FROM blob_references WHERE subject_uri LIKE $1`, pattern, subject.owner, subject.kind, nestedBackfillFirstCID).Scan(&total, &correct, &distinctSubjects))
		assert.Equal(t, 601, total, "%s references", subject.collection)
		assert.Equal(t, 601, correct, "%s reference tuples", subject.collection)
		assert.Equal(t, 601, distinctSubjects, "%s subjects", subject.collection)
		require.NoError(t, db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE b.owner_did = $2 AND b.blob_cid = $3 AND b.active), COUNT(DISTINCT a.subject_uri)
			FROM moderation_media_blocks b JOIN moderation_actions a ON a.id = b.action_id WHERE a.subject_uri LIKE $1`, pattern, subject.owner, nestedBackfillFirstCID).Scan(&total, &correct, &distinctSubjects))
		assert.Equal(t, 601, total, "%s blocks", subject.collection)
		assert.Equal(t, 601, correct, "%s owner/action block tuples", subject.collection)
		assert.Equal(t, 601, distinctSubjects, "%s blocked subjects", subject.collection)
	}
	nestedBackfillNoPurgeTargets(t, db)
}
