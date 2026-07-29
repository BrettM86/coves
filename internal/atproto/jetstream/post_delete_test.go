//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a post DELETE event does to the row, which is the half of deletion that
// no serving endpoint can show.
//
// social.coves.community.post's ingestion contract (tests/e2e/post_contract_test.go)
// proves the observable half: after a delete the post is gone from
// social.coves.community.post.get and stays gone. It cannot distinguish the two
// ways of being gone, because both look identical through the endpoint —
// GetViewsByURIs filters `deleted_at IS NULL`, so a hard-deleted row and a
// soft-deleted one both come back as notFoundPost.
//
// The difference matters. deletePost sets deleted_at rather than removing the
// row (post_consumer.go), and that choice is load-bearing in three places:
// comment threads keep their parent, the rev gate has a row to hang a tombstone
// on so a replayed create cannot resurrect the post, and moderation can still
// see what was published. A refactor to a hard DELETE would keep every T2
// assertion green and quietly break all three.
//
// These are the assertions that were inside tests/integration/post_delete_test.go's
// TestPostDeletion_JetstreamConsumer, rewritten against the real database at the
// tier where the row is visible.

const (
	delTestPrefix    = "did:plc:jsdel"
	delTestCommunity = delTestPrefix + "community"
	delTestAuthor    = delTestPrefix + "author"

	// Successive commits of one repo. TIDs are lexicographically ordered, which
	// is what the rev gate compares.
	delRevCreate = "3ldeltestaa2a"
	delRevDelete = "3ldeltestaa2b"
	delRevLater  = "3ldeltestaa2c"
)

// newDeleteFixture indexes the user and community a post needs and returns a
// consumer wired to them.
func newDeleteFixture(t *testing.T, db *sql.DB) *PostEventConsumer {
	t.Helper()
	insertBridgedUser(t, db, delTestAuthor, "delauthor.test")
	insertBridgedCommunity(t, db, delTestCommunity, "delcommunity.test", delTestAuthor)

	us := newMockUserService()
	us.users[delTestAuthor] = &users.User{DID: delTestAuthor, Handle: "delauthor.test"}
	return NewPostEventConsumer(postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), us, db)
}

// delPostEvent builds a post commit for rkey with the given operation and rev.
// A delete carries no record, exactly as Jetstream delivers it.
func delPostEvent(op, rkey, rev, cid string, timeUS int64) *JetstreamEvent {
	var record map[string]interface{}
	if op != "delete" {
		record = map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": delTestCommunity,
			"author":    delTestAuthor,
			"title":     "delete target",
			"content":   "body that must survive the tombstone",
			"createdAt": "2026-03-01T00:00:00Z",
		}
	}
	return revCommitEvent(delTestCommunity, "social.coves.community.post", op, rkey, rev, cid, timeUS, record)
}

// readDeletedPost returns the row's tombstone and content. A missing row is a
// fatal error rather than a nil result: every caller here has just asserted the
// post exists, so its absence is the hard-delete regression these tests exist
// to catch, and it should say so at the point it happens.
func readDeletedPost(t *testing.T, db *sql.DB, uri string) (deletedAt *time.Time, title, content string) {
	t.Helper()
	err := db.QueryRow(
		`SELECT deleted_at, title, content FROM posts WHERE uri = $1`, uri,
	).Scan(&deletedAt, &title, &content)
	require.NoErrorf(t, err, "the post row for %s is gone: a delete must SOFT-delete (set deleted_at), "+
		"not remove the row — comment threads, the rev-gate tombstone and moderation all read it", uri)
	return deletedAt, title, content
}

func TestPostConsumer_Delete_IsSoftAndKeepsTheRow(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	c := newDeleteFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	uri := "at://" + delTestCommunity + "/social.coves.community.post/delsoft"

	require.NoError(t, c.HandleEvent(ctx, delPostEvent("create", "delsoft", delRevCreate, "bafdelsoft", base)))

	deletedAt, _, _ := readDeletedPost(t, db, uri)
	require.Nil(t, deletedAt, "fixture: a freshly indexed post is not deleted")

	require.NoError(t, c.HandleEvent(ctx, delPostEvent("delete", "delsoft", delRevDelete, "", base+1_000_000)))

	deletedAt, title, content := readDeletedPost(t, db, uri)
	require.NotNil(t, deletedAt, "the delete event must set deleted_at")
	assert.Equal(t, "delete target", title,
		"a soft delete must not blank the content: the row is what moderation and the thread view read")
	assert.Equal(t, "body that must survive the tombstone", content)
}

func TestPostConsumer_DuplicateDelete_TombstoneStaysAtTheFirstDelete(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	c := newDeleteFixture(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	uri := "at://" + delTestCommunity + "/social.coves.community.post/deldup"

	require.NoError(t, c.HandleEvent(ctx, delPostEvent("create", "deldup", delRevCreate, "bafdeldup", base)))

	deleteEvent := delPostEvent("delete", "deldup", delRevDelete, "", base+1_000_000)
	require.NoError(t, c.HandleEvent(ctx, deleteEvent))
	first, _, _ := readDeletedPost(t, db, uri)
	require.NotNil(t, first)

	// The rewind duplicate: the connector rewinds its cursor 5s after every
	// reconnect, so the IDENTICAL commit — same rev — is guaranteed to be
	// redelivered in production.
	require.NoError(t, c.HandleEvent(ctx, deleteEvent),
		"a redelivered delete must be a silent no-op, not an error the connector logs as a failure")

	// And the other shape of a repeat: a genuinely later delete commit for a
	// record that is already tombstoned. This one clears the rev gate (strictly
	// newer rev), so only the UPDATE's own `deleted_at IS NULL` guard stands
	// between it and a moved timestamp.
	require.NoError(t, c.HandleEvent(ctx, delPostEvent("delete", "deldup", delRevLater, "", base+2_000_000)))

	second, _, _ := readDeletedPost(t, db, uri)
	assert.Equal(t, first.UTC(), second.UTC(),
		"deleted_at must record when the post was FIRST deleted; a repeated delete that moves it "+
			"would keep re-dating the tombstone every time the connector rewinds")
}

func TestPostConsumer_DuplicateCreate_IndexesExactlyOnce(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	c := newDeleteFixture(t, db)
	ctx := context.Background()
	uri := "at://" + delTestCommunity + "/social.coves.community.post/dupcreate"

	// The post sibling of TestVoteConsumer_DuplicateCreate_IncrementsExactlyOnce
	// and TestCommentConsumer_DuplicateCreate_CountsExactlyOnce: the same commit
	// delivered twice by a cursor rewind.
	event := delPostEvent("create", "dupcreate", delRevCreate, "bafdupcreate", time.Now().UnixMicro())
	require.NoError(t, c.HandleEvent(ctx, event))
	require.NoError(t, c.HandleEvent(ctx, event),
		"a redelivered post create must be a silent no-op")

	var rows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE uri = $1`, uri).Scan(&rows))
	assert.Equal(t, 1, rows, "a duplicate create must not produce a second post row")
}
