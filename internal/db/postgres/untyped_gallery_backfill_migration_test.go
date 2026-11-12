//go:build integration

package postgres

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 078 stamps "$type":"social.coves.embed.images" on stored
// imagesWithAttachment galleries that omit the key, then writes the blob
// references (062/065 rules) and removal blocks (065 rules) those galleries
// were missing while their images were invisible to the readers.

const untypedGalleryCountQueryPath = "../../../scripts/sql/count_untyped_embed_galleries.sql"

func untypedGalleryCounts(t *testing.T, db *sql.DB) (posts, comments int) {
	t.Helper()
	query, err := os.ReadFile(untypedGalleryCountQueryPath)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(string(query)).Scan(&posts, &comments))
	return posts, comments
}

func untypedGalleryMigrationEmbed(galleryTypeMember string, cids []string) string {
	return `{"$type":"social.coves.embed.imagesWithAttachment","images":{` + galleryTypeMember +
		`"images":` + contentBackfillImageEntries(cids) + `},"attachment":` + nestedBackfillQuote() + `}`
}

func untypedGalleryStoredEmbed(t *testing.T, db *sql.DB, table, uri string) string {
	t.Helper()
	require.Contains(t, []string{"posts", "comments"}, table)
	var embed string
	require.NoError(t, db.QueryRow(`SELECT embed::text FROM `+table+` WHERE uri = $1`, uri).Scan(&embed))
	return embed
}

func TestUntypedGalleryBackfill078_StampsTypeAndBackfillsReferences(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),
		"080 (moderation community record writes) must be rolled back first")
	require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79),
		"079 (community provisioning accounts) must be rolled back first")
	require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78),
		"078 (untyped gallery backfill) must be rolled back first")

	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author, commenter := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nine := contentBackfillCIDs(t, "untyped-nine", 9)

	live := seedVisibilityPost(t, db, community, author, testkit.TID(), "untyped live", createdAt)
	legacy := seedLegacyPost(t, db, community, author, testkit.TID(), "untyped legacy", createdAt)
	comment := seedActorComment(t, db, commenter, live, testkit.TID(), createdAt)
	deleted := seedVisibilityPost(t, db, community, author, testkit.TID(), "untyped deleted", createdAt)
	overLimit := seedVisibilityPost(t, db, community, author, testkit.TID(), "untyped nine", createdAt)
	removed := seedVisibilityPost(t, db, community, author, testkit.TID(), "untyped removed", createdAt)
	removedOverLimit := seedVisibilityPost(t, db, community, author, testkit.TID(), "untyped removed nine", createdAt)
	removedComment := seedActorComment(t, db, commenter, live, testkit.TID(), createdAt)
	deletedComment := seedActorComment(t, db, commenter, live, testkit.TID(), createdAt)
	overLimitComment := seedActorComment(t, db, commenter, live, testkit.TID(), createdAt)
	nullType := seedVisibilityPost(t, db, community, author, testkit.TID(), "null gallery type", createdAt)
	typed := seedVisibilityPost(t, db, community, author, testkit.TID(), "typed composite", createdAt)
	images := seedVisibilityPost(t, db, community, author, testkit.TID(), "images", createdAt)

	twoImages := []string{nestedBackfillFirstCID, nestedBackfillSecondCID}
	oneImage := []string{nestedBackfillFirstCID}
	const stampedType = `"$type":"social.coves.embed.images",`
	untyped := map[string]struct {
		table   string
		gallery []string
	}{
		live:      {"posts", twoImages},
		legacy:    {"posts", oneImage},
		comment:   {"comments", oneImage},
		deleted:   {"posts", oneImage},
		overLimit: {"posts", nine},
		removed:   {"posts", oneImage},
		// Removal blocks never serve anything, so an over-limit gallery still
		// gets the blocks 065 would write, though no references.
		removedOverLimit: {"posts", nine},
		removedComment:   {"comments", twoImages},
		deletedComment:   {"comments", oneImage},
		overLimitComment: {"comments", nine},
	}
	for uri, fixture := range untyped {
		nestedBackfillSetEmbed(t, db, fixture.table, uri, untypedGalleryMigrationEmbed("", fixture.gallery))
	}
	untouched := map[string]string{
		nullType: untypedGalleryMigrationEmbed(`"$type":null,`, oneImage),
		typed:    untypedGalleryMigrationEmbed(stampedType, twoImages),
		images:   contentBackfillImagesEmbed(twoImages),
	}
	for uri, embed := range untouched {
		nestedBackfillSetEmbed(t, db, "posts", uri, embed)
	}
	_, err := db.Exec(`UPDATE posts SET deleted_at = $2 WHERE uri = $1`, deleted, createdAt)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE comments SET deleted_at = $2 WHERE uri = $1`, deletedComment, createdAt)
	require.NoError(t, err)
	removalAction := nestedBackfillRemoval(t, db, removed)
	removedOverLimitAction := nestedBackfillRemoval(t, db, removedOverLimit)
	removedCommentAction := nestedBackfillRemoval(t, db, removedComment)

	untouchedBefore := map[string]string{}
	for uri := range untouched {
		untouchedBefore[uri] = untypedGalleryStoredEmbed(t, db, "posts", uri)
	}
	require.Empty(t, nestedBackfillBlocks(t, db))
	postsUntyped, commentsUntyped := untypedGalleryCounts(t, db)
	assert.Equal(t, 6, postsUntyped, "live, legacy, deleted, nine-image, removed and removed nine-image posts; the null-typed gallery has the key")
	assert.Equal(t, 4, commentsUntyped, "live, removed, deleted and nine-image comments")

	testkit.MigrateUp(t, db)

	for uri, fixture := range untyped {
		assert.JSONEq(t, untypedGalleryMigrationEmbed(stampedType, fixture.gallery), untypedGalleryStoredEmbed(t, db, fixture.table, uri),
			"%s %s gallery must be stamped", fixture.table, uri)
	}
	for uri, before := range untouchedBefore {
		assert.Equal(t, before, untypedGalleryStoredEmbed(t, db, "posts", uri), "%s must be byte-identical", uri)
	}

	assert.ElementsMatch(t, []contentBackfillReference{
		{author, nestedBackfillFirstCID, live, "post_image", true},
		{author, nestedBackfillSecondCID, live, "post_image", true},
		{community, nestedBackfillFirstCID, legacy, "post_image", true},
		{commenter, nestedBackfillFirstCID, comment, "comment_image", true},
		{author, nestedBackfillFirstCID, deleted, "post_image", false},
		{author, nestedBackfillFirstCID, removed, "post_image", true},
		{commenter, nestedBackfillFirstCID, removedComment, "comment_image", true},
		{commenter, nestedBackfillSecondCID, removedComment, "comment_image", true},
		{commenter, nestedBackfillFirstCID, deletedComment, "comment_image", false},
	}, contentBackfillRows(t, db, live, legacy, comment, deleted, overLimit, removed, nullType,
		removedOverLimit, removedComment, deletedComment, overLimitComment))
	authorOwner := sql.NullString{String: author, Valid: true}
	commenterOwner := sql.NullString{String: commenter, Valid: true}
	assert.ElementsMatch(t, []nestedBackfillBlock{
		{authorOwner, nestedBackfillFirstCID, removalAction, true},
		{authorOwner, "bafkreif7w6pamqnqgs5iqd5o45zcksj4sf7ry6s4btznnkblgenzwhmo7e", removedOverLimitAction, true},
		{authorOwner, "bafkreielnhl4gmc3we4aaeee3fpbzw3jckri3fptajlgwd3jyhvlqga3ki", removedOverLimitAction, true},
		{authorOwner, "bafkreifzi2ak326ly3h4bh5qgqerwqvivzg3fekpeiwcalwxfv63iw36em", removedOverLimitAction, true},
		{authorOwner, "bafkreibts4vy2awiqfydadnc522g35b7ow52r2xp7v7jrj2rg5kq6cluqi", removedOverLimitAction, true},
		{authorOwner, "bafkreigxoi3y2f62vcvws5jyemgpihg6cdz6nfbbat66upwh4jphv27ek4", removedOverLimitAction, true},
		{authorOwner, "bafkreibuwwkntmkwqo5s3zjzx4b2ewke5bfvyddagt54lylfljhg3i5xya", removedOverLimitAction, true},
		{authorOwner, "bafkreigruh4lovkijxcvklqkyx5kbxaa3k4bitfvhrxt5pwrxl2cf35bbe", removedOverLimitAction, true},
		{authorOwner, "bafkreihalpxy2gylbvvuofl7bahg5unr2ym36fe25buw6lnyd2xithlrta", removedOverLimitAction, true},
		{authorOwner, "bafkreihrj4ia2upinbp7ej75dvxs4zdhpninlziyxbqzkyq4spauoxa6ha", removedOverLimitAction, true},
		{commenterOwner, nestedBackfillFirstCID, removedCommentAction, true},
		{commenterOwner, nestedBackfillSecondCID, removedCommentAction, true},
	}, nestedBackfillBlocks(t, db))
	postsUntyped, commentsUntyped = untypedGalleryCounts(t, db)
	assert.Equal(t, 0, postsUntyped)
	assert.Equal(t, 0, commentsUntyped)

	// Rolling 078 back and applying it again changes nothing.
	snapshots := map[string]string{}
	for _, table := range []string{"posts", "comments", "blob_references", "moderation_media_blocks"} {
		snapshots[table] = nestedBackfillSnapshot(t, db, table, "")
	}
	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80))
	require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79))
	require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78))
	testkit.MigrateUp(t, db)
	for table, before := range snapshots {
		assert.Equal(t, before, nestedBackfillSnapshot(t, db, table, ""), "%s changed on Down and rerun", table)
	}
}
