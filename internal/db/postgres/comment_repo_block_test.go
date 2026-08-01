//go:build integration

package postgres

import (
	"Coves/internal/core/comments"
	"Coves/tests/testkit"
	"context"
	"testing"
	"time"

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
		list, _, err := repo.ListByParentWithHotRank(ctx, parentURI, "new", "", 50, nil, viewerDID)
		require.NoError(t, err)
		return commentURIs(list)
	}

	require.ElementsMatch(t, []string{blockedComment, thirdPartyComment}, read(t, cast.viewer),
		"the fixture is not visible before any block exists")

	insertUserBlock(t, db, cast.viewer, cast.blocked)

	assert.ElementsMatch(t, []string{thirdPartyComment}, read(t, cast.viewer),
		"the blocked commenter's comment must leave the thread and the third party's must stay")
}
