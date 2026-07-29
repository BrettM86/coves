//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Shared seeding for the four viewer-block filter suites.
//
// User blocks are enforced in SQL, once per read path: feed_repo.go,
// discover_repo.go, timeline_repo.go and comment_repo.go each carry their own
// NOT EXISTS subquery against user_blocks, and each builds its parameter index
// differently (the community feed's viewer is $3+, discover's is $2+, the
// timeline reuses $1). There is no shared helper to test, so there is one suite
// per query — feed_repo_block_test.go, discover_repo_block_test.go,
// timeline_repo_block_test.go, comment_repo_block_test.go — and this file holds
// what they all need to set up.
//
// They come from tests/integration/userblock_enforcement_test.go, which proved
// all four against the same shared database.

// blockFilterCast is the three parties every filter suite needs: the viewer who
// blocks, the author they block, and a third party who must stay visible.
//
// The third party is not decoration. A filter that dropped everything would
// satisfy "the blocked author is gone" perfectly, so every suite asserts on a
// post that must SURVIVE the same query.
type blockFilterCast struct {
	viewer     string
	blocked    string
	thirdParty string
}

// seedBlockFilterCast indexes the three users under DIDs derived from label, so
// a leftover row says which suite made it.
func seedBlockFilterCast(t *testing.T, db *sql.DB, label string) blockFilterCast {
	t.Helper()

	cast := blockFilterCast{
		viewer:     "did:plc:blk" + label + "viewer",
		blocked:    "did:plc:blk" + label + "blocked",
		thirdParty: "did:plc:blk" + label + "third",
	}
	for _, did := range []string{cast.viewer, cast.blocked, cast.thirdParty} {
		createTestUser(t, db, did+".test", did)
	}
	return cast
}

// insertUserBlock indexes blocker → blocked, as the firehose consumer would.
//
// It writes the row directly rather than through NewUserBlockRepository: what is
// under test is the reading query, and going through the writing one would make
// a filter suite fail when the block repository breaks.
func insertUserBlock(t *testing.T, db *sql.DB, blockerDID, blockedDID string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid)
		VALUES ($1, $2, $3, $4)
	`, blockerDID, blockedDID,
		"at://"+blockerDID+"/social.coves.actor.block/"+blockedDID,
		"bafyblock"+blockedDID)
	require.NoError(t, err, "indexing the block")
}

// seedFilterablePost inserts one post by authorDID in communityDID and returns
// its AT-URI. rkey doubles as the post's identity in failure messages.
func seedFilterablePost(t *testing.T, db *sql.DB, communityDID, authorDID, rkey string, createdAt time.Time) string {
	t.Helper()

	uri := "at://" + communityDID + "/social.coves.community.post/" + rkey
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, uri, "bafypost"+rkey, rkey, authorDID, communityDID, "post "+rkey, createdAt)
	require.NoErrorf(t, err, "seeding post %s", rkey)
	return uri
}
