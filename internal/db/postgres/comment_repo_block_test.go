//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// User-block filtering in the comment listing. See
// viewer_block_fixtures_test.go for why there is one of these per read path.

// commentURIs renders a comment page as the URIs it returned.
func commentURIs(list []*comments.Comment) []string {
	uris := make([]string, 0, len(list))
	for _, comment := range list {
		uris = append(uris, comment.URI)
	}
	return uris
}

func TestCommentRepo_ViewerBlockFiltering(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	cast := seedBlockFilterCast(t, db, "cmt")
	repo := NewCommentRepository(db)

	// The parent needs no post row: the listing is keyed on parent_uri as a
	// string, and comments carry no foreign key to posts (see migration 016 —
	// out-of-order firehose indexing).
	const parentURI = "at://did:plc:blkcmtcommunity/social.coves.community.post/parent"

	seedComment := func(t *testing.T, commenterDID, rkey string, createdAt time.Time) string {
		t.Helper()
		uri := "at://" + commenterDID + "/social.coves.community.comment/" + rkey
		require.NoError(t, repo.Create(ctx, &comments.Comment{
			URI:          uri,
			CID:          "bafycmt" + rkey,
			RKey:         rkey,
			CommenterDID: commenterDID,
			RootURI:      parentURI,
			RootCID:      "bafyroot",
			ParentURI:    parentURI,
			ParentCID:    "bafyparent",
			Content:      "comment " + rkey,
			CreatedAt:    createdAt,
		}))
		return uri
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blockedComment := seedComment(t, cast.blocked, "cmtblocked", base)
	thirdPartyComment := seedComment(t, cast.thirdParty, "cmtthird", base.Add(time.Hour))

	read := func(t *testing.T, viewerDID string) []string {
		t.Helper()
		list, _, err := listByParentViaGenerator(ctx, repo, db, parentURI, "new", "", 50, nil, viewerDID)
		require.NoError(t, err)
		return commentURIs(list)
	}

	require.ElementsMatch(t, []string{blockedComment, thirdPartyComment}, read(t, cast.viewer),
		"the fixture is not visible before any block exists")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	assert.ElementsMatch(t, []string{thirdPartyComment}, read(t, cast.viewer),
		"the blocked commenter's comment must leave the thread and the third party's must stay")
}

// commentRKeys renders a comment page as the rkeys it returned, in order.
func commentRKeys(list []*comments.Comment) []string {
	rkeys := make([]string, 0, len(list))
	for _, comment := range list {
		rkeys = append(rkeys, comment.RKey)
	}
	return rkeys
}

// TestCommentGenerator_BlockedCommenterTakesNoPageSlot pins that the rank
// query itself drops a blocked commenter's comments. HydrateComments drops
// them too, so "no blocked author appears" alone cannot tell the two apart;
// what can is the page size: a rank query that let the blocked comment in
// would fill one of the limit slots with it and hydration would return one
// short.
func TestCommentGenerator_BlockedCommenterTakesNoPageSlot(t *testing.T) {
	t.Parallel()

	env := commentEnvFor(t)
	viewer := "did:plc:cmtviewer" + env.id
	blocked := "did:plc:cmtblocked" + env.id
	createTestUser(t, env.db, "cmtviewer-"+env.id+".test", viewer)
	createTestUser(t, env.db, "cmtblocked-"+env.id+".test", blocked)
	insertUserBlock(t, env.db, viewer, blocked)

	at := func(hours int) time.Time { return commentBaseTime.Add(time.Duration(hours) * time.Hour) }

	t.Run("single parent", func(t *testing.T) {
		// Newest first: slot b1 is the blocked commenter's, inside limit 2.
		env.seed(commentSpec{rkey: "slotsa1", createdAt: at(4)})
		env.seed(commentSpec{rkey: "slotsb1", author: blocked, createdAt: at(3)})
		env.seed(commentSpec{rkey: "slotsa2", createdAt: at(2)})
		env.seed(commentSpec{rkey: "slotsa3", createdAt: at(1)})

		list, next, err := listByParentViaGenerator(env.ctx, env.repo, env.db, env.root, "new", "", 2, nil, viewer)
		require.NoError(t, err)
		assert.Equal(t, []string{"slotsa1", "slotsa2"}, commentRKeys(list),
			"the blocked comment must not take one of the two slots")
		for _, comment := range list {
			assert.NotEqual(t, blocked, comment.CommenterDID)
		}
		assert.NotNil(t, next, "slotsa3 remains, so page one must carry a next cursor")
	})

	t.Run("several parents", func(t *testing.T) {
		parentA := env.seed(commentSpec{rkey: "slotspa", createdAt: at(10)})
		parentB := env.seed(commentSpec{rkey: "slotspb", createdAt: at(10)})
		// Parent A: LimitPerParent (2) + 1 replies, the blocked one second newest.
		env.seed(commentSpec{rkey: "slotsra1", parent: parentA, createdAt: at(14)})
		env.seed(commentSpec{rkey: "slotsrb1", parent: parentA, author: blocked, createdAt: at(13)})
		env.seed(commentSpec{rkey: "slotsra2", parent: parentA, createdAt: at(12)})
		env.seed(commentSpec{rkey: "slotsra3", parent: parentA, createdAt: at(11)})
		env.seed(commentSpec{rkey: "slotsrc1", parent: parentB, createdAt: at(12)})

		byParent, err := listByParentsBatchViaGenerator(env.ctx, env.repo, env.db, []string{parentA, parentB}, "new", 2, viewer)
		require.NoError(t, err)
		assert.Equal(t, []string{"slotsra1", "slotsra2"}, commentRKeys(byParent[parentA]),
			"parent A must keep both slots for non-blocked replies")
		assert.Equal(t, []string{"slotsrc1"}, commentRKeys(byParent[parentB]))
	})
}
