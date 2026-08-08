//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the bridged-vote-stats support end-to-end against a
// private testkit clone of the migrated template. They are strictly local-only:
// no public PLC/relay/PDS/image hosts are contacted.

// bridgeTrustForTests trusts only the bridge PDS host, so records from repos hosted
// there may assert bridgedStats while every other repo is default-denied.
func bridgeTrustForTests() *BridgeTrust {
	return NewBridgeTrust([]string{bridgedTestPDS})
}

// insertBridgedUser inserts a user hosted on the trusted bridge PDS.
func insertBridgedUser(t *testing.T, db *sql.DB, did, handle string) {
	t.Helper()
	insertBridgedUserOnPDS(t, db, did, handle, bridgedTestPDS)
}

// insertBridgedUserOnPDS inserts a user with an explicit pds_url, used to exercise the
// provenance gate (a non-bridge pds_url must cause bridgedStats to be ignored).
func insertBridgedUserOnPDS(t *testing.T, db *sql.DB, did, handle, pdsURL string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO UPDATE SET pds_url = EXCLUDED.pds_url`,
		did, handle, pdsURL)
	require.NoError(t, err)
}

func insertBridgedCommunity(t *testing.T, db *sql.DB, did, handle, ownerDID string) {
	t.Helper()
	insertBridgedCommunityOnPDS(t, db, did, handle, ownerDID, bridgedTestPDS)
}

func insertBridgedCommunityOnPDS(t *testing.T, db *sql.DB, did, handle, ownerDID, pdsURL string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, created_at)
		VALUES ($1, $2, $3, $4, $4, $4, $5, NOW()) ON CONFLICT (did) DO UPDATE SET pds_url = EXCLUDED.pds_url`,
		did, handle, "Bridged Test Community", ownerDID, pdsURL)
	require.NoError(t, err)
}

func newPostConsumer(t *testing.T, db *sql.DB) *PostEventConsumer {
	t.Helper()
	postRepo := postgres.NewPostRepository(db)
	communityRepo := postgres.NewCommunityRepository(db)
	us := newMockUserService()
	us.users[bridgedTestAuthor] = &users.User{DID: bridgedTestAuthor, Handle: "brauthor.test", PDSURL: bridgedTestPDS}
	// Register the alternate author so update-time validation passes and the
	// reassignment-rejection branch (not the author-not-found branch) is exercised.
	us.users[bridgedTestOther] = &users.User{DID: bridgedTestOther, Handle: "brother.test", PDSURL: bridgedTestPDS}
	return NewPostEventConsumer(postRepo, communityRepo, us, db, WithPostBridgeTrust(bridgeTrustForTests()))
}

func newCommentConsumer(db *sql.DB) *CommentEventConsumer {
	return NewCommentEventConsumer(postgres.NewCommentRepository(db), db, WithCommentBridgeTrust(bridgeTrustForTests()))
}

func newVoteConsumer(db *sql.DB) *VoteEventConsumer {
	return NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
}

func postCommitEvent(op, rkey, cid string, record map[string]interface{}) *JetstreamEvent {
	return &JetstreamEvent{
		Kind: "commit",
		Did:  bridgedTestCommunity,
		Commit: &CommitEvent{
			Operation:  op,
			Collection: "social.coves.community.post",
			RKey:       rkey,
			CID:        cid,
			Record:     record,
		},
	}
}

// readPostRow returns the stored native/bridged columns for assertions.
func readPostRow(t *testing.T, db *sql.DB, uri string) (up, down, bridgedUp, bridgedDown, score int, asOf *time.Time, deletedAt *time.Time, title string) {
	t.Helper()
	err := db.QueryRow(`SELECT upvote_count, downvote_count, bridged_upvote_count, bridged_downvote_count, score, bridged_stats_as_of, deleted_at, COALESCE(title,'')
		FROM posts WHERE uri = $1`, uri).
		Scan(&up, &down, &bridgedUp, &bridgedDown, &score, &asOf, &deletedAt, &title)
	require.NoError(t, err)
	return
}

func TestPostConsumer_Create_WithBridgedStats(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)

	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/create1"

	err := c.HandleEvent(ctx, postCommitEvent("create", "create1", "bafcreate1",
		postRecord("Hello", "world", bridgedStatsRecord(10, 3, asOfEarly))))
	require.NoError(t, err)

	up, down, bUp, bDown, score, asOf, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 0, up, "native upvotes untouched")
	assert.Equal(t, 0, down, "native downvotes untouched")
	assert.Equal(t, 10, bUp)
	assert.Equal(t, 3, bDown)
	assert.Equal(t, 7, score, "score = (0+10)-(0+3)")
	require.NotNil(t, asOf)

	// Read-path fold: displayed stats include bridged counts.
	repo := postgres.NewPostRepository(db)
	views, err := repo.GetViewsByURIs(ctx, []string{uri}, "")
	require.NoError(t, err)
	view := views[uri]
	require.NotNil(t, view)
	assert.Equal(t, 10, view.Stats.Upvotes, "displayed upvotes folded")
	assert.Equal(t, 3, view.Stats.Downvotes, "displayed downvotes folded")
	assert.Equal(t, 7, view.Stats.Score)
}

func TestPostConsumer_CreateBeforeAuthorProfile_IndexesTrustedBridgeAuthor(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	// The community is already indexed, but the post author's profile event
	// has not arrived yet. This is the cross-repo ordering BigSky permits.
	insertBridgedUser(t, db, bridgedTestOther, "community-owner.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestOther)

	resolver := &mockIdentityResolverForUser{identities: map[string]*identity.Identity{
		bridgedTestAuthor: {
			DID: bridgedTestAuthor, Handle: "author.lemmy.bridge.test", PDSURL: bridgedTestPDS,
		},
	}}
	userService := users.NewUserService(postgres.NewUserRepository(db), resolver, bridgedTestPDS, nil, "")
	consumer := NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), userService, db,
		WithPostBridgeTrust(bridgeTrustForTests()),
		WithPostIdentityResolver(resolver),
	)

	err := consumer.HandleEvent(context.Background(), postCommitEvent(
		"create", "beforeprofile", "bafbeforeprofile",
		postRecord("Post beat profile", "relay ordering", nil),
	))
	require.NoError(t, err)

	var authorPDS string
	require.NoError(t, db.QueryRow(`SELECT pds_url FROM users WHERE did = $1`, bridgedTestAuthor).Scan(&authorPDS))
	assert.Equal(t, bridgedTestPDS, authorPDS)

	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/beforeprofile"
	var storedAuthor string
	require.NoError(t, db.QueryRow(`SELECT author_did FROM posts WHERE uri = $1`, uri).Scan(&storedAuthor))
	assert.Equal(t, bridgedTestAuthor, storedAuthor)
}

func TestPostConsumer_Update_BridgedStats_NewerAsOfApplied(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/upd1"

	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "upd1", "bafupd1",
		postRecord("t", "c", bridgedStatsRecord(5, 1, asOfEarly)))))

	// Newer asOf -> applied, content + edited_at updated.
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("update", "upd1", "bafupd1b",
		postRecord("t2", "c2", bridgedStatsRecord(20, 4, asOfLate)))))

	_, _, bUp, bDown, score, _, _, title := readPostRow(t, db, uri)
	assert.Equal(t, 20, bUp)
	assert.Equal(t, 4, bDown)
	assert.Equal(t, 16, score, "score = (0+20)-(0+4)")
	assert.Equal(t, "t2", title, "content updated on edit")

	var editedAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT edited_at FROM posts WHERE uri=$1`, uri).Scan(&editedAt))
	assert.NotNil(t, editedAt, "edited_at set on update")
}

func TestPostConsumer_Update_StrictlyOlderIgnored_EqualApplied(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/upd2"

	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "upd2", "bafupd2",
		postRecord("t", "c", bridgedStatsRecord(20, 4, asOfLate)))))

	// Strictly-older asOf -> ignored (bridged counts unchanged).
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("update", "upd2", "bafupd2b",
		postRecord("t", "c", bridgedStatsRecord(1, 1, asOfEarly)))))
	_, _, bUp, bDown, score, _, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 20, bUp, "strictly-older asOf must not overwrite")
	assert.Equal(t, 4, bDown)
	assert.Equal(t, 16, score)

	// Equal asOf with DIFFERENT counts -> APPLIED (newer-or-equal guard). tidepool
	// truncates asOf, so equal-string collisions carrying fresher counts must not be
	// dropped; genuine replays carry identical counts and remain idempotent.
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("update", "upd2", "bafupd2c",
		postRecord("t", "c", bridgedStatsRecord(99, 5, asOfLate)))))
	_, _, bUp2, bDown2, score2, _, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 99, bUp2, "equal asOf with fresher counts must apply")
	assert.Equal(t, 5, bDown2)
	assert.Equal(t, 94, score2, "score = (0+99)-(0+5)")
}

func TestPostConsumer_Update_ReassignmentRejected(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedUser(t, db, bridgedTestOther, "brother.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/reassign1"

	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "reassign1", "bafr1",
		postRecord("orig", "c", nil))))

	// Update attempts to reassign author -> skipped (no error), row unchanged.
	rec := postRecord("changed", "c", nil)
	rec["author"] = bridgedTestOther
	err := c.HandleEvent(ctx, postCommitEvent("update", "reassign1", "bafr1b", rec))
	require.NoError(t, err, "reassignment is skipped, not errored")

	var storedAuthor, storedTitle string
	require.NoError(t, db.QueryRow(`SELECT author_did, COALESCE(title,'') FROM posts WHERE uri=$1`, uri).Scan(&storedAuthor, &storedTitle))
	assert.Equal(t, bridgedTestAuthor, storedAuthor, "author must not change")
	assert.Equal(t, "orig", storedTitle, "content must not change when reassignment rejected")
}

func TestPostConsumer_Update_SoftDeletedSkipped(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/del1"

	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "del1", "bafd1",
		postRecord("orig", "c", bridgedStatsRecord(5, 0, asOfEarly)))))
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("delete", "del1", "", nil)))

	// Update on soft-deleted row -> skipped, stays deleted, content unchanged.
	err := c.HandleEvent(ctx, postCommitEvent("update", "del1", "bafd1b",
		postRecord("changed", "c2", bridgedStatsRecord(99, 9, asOfLate))))
	require.NoError(t, err)

	_, _, bUp, _, _, _, deletedAt, title := readPostRow(t, db, uri)
	assert.NotNil(t, deletedAt, "post stays soft-deleted")
	assert.Equal(t, 5, bUp, "bridged counts untouched on soft-deleted row")
	assert.Equal(t, "orig", title, "content untouched on soft-deleted row")
}

func TestPostConsumer_Update_NonExistentSkipped(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/ghost1"

	err := c.HandleEvent(ctx, postCommitEvent("update", "ghost1", "bafg1",
		postRecord("t", "c", bridgedStatsRecord(1, 1, asOfLate))))
	require.NoError(t, err, "update for non-indexed post is a logged skip")

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE uri=$1`, uri).Scan(&count))
	assert.Equal(t, 0, count, "no row created by update")
}

func TestPostConsumer_InclusiveScore_NativeVotesStackOnBridged(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	pc := newPostConsumer(t, db)
	vc := newVoteConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/score1"

	require.NoError(t, pc.HandleEvent(ctx, postCommitEvent("create", "score1", "bafs1",
		postRecord("t", "c", bridgedStatsRecord(10, 2, asOfEarly)))))

	// A native upvote stacks on the bridged counts.
	voteEvent := &JetstreamEvent{
		Kind: "commit",
		Did:  bridgedTestVoter,
		Commit: &CommitEvent{
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       "v1",
			CID:        "bafvote1",
			Record: map[string]interface{}{
				"subject":   map[string]interface{}{"uri": uri, "cid": "bafs1"},
				"direction": "up",
				"createdAt": "2026-02-01T00:00:00Z",
			},
		},
	}
	require.NoError(t, vc.HandleEvent(ctx, voteEvent))

	up, down, bUp, bDown, score, _, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 1, up)
	assert.Equal(t, 0, down)
	assert.Equal(t, 10, bUp)
	assert.Equal(t, 2, bDown)
	assert.Equal(t, 9, score, "score = (1+10)-(0+2)")

	// A newer bridgedStats update must preserve the native vote.
	require.NoError(t, pc.HandleEvent(ctx, postCommitEvent("update", "score1", "bafs1b",
		postRecord("t", "c", bridgedStatsRecord(30, 2, asOfLate)))))
	up2, _, bUp2, _, score2, _, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 1, up2, "native upvote preserved across bridged update")
	assert.Equal(t, 30, bUp2)
	assert.Equal(t, 29, score2, "score = (1+30)-(0+2)")

	// Displayed stats fold native + bridged.
	repo := postgres.NewPostRepository(db)
	views, err := repo.GetViewsByURIs(ctx, []string{uri}, "")
	require.NoError(t, err)
	assert.Equal(t, 31, views[uri].Stats.Upvotes)
	assert.Equal(t, 2, views[uri].Stats.Downvotes)
	assert.Equal(t, 29, views[uri].Stats.Score)
}

// --- Comment consumer ---

func commentCommitEvent(op, rkey, cid string, record map[string]interface{}) *JetstreamEvent {
	return &JetstreamEvent{
		Kind: "commit",
		Did:  bridgedTestPrefix + "commenter",
		Commit: &CommitEvent{
			Operation:  op,
			Collection: CommentCollection,
			RKey:       rkey,
			CID:        cid,
			Record:     record,
		},
	}
}

func readCommentRow(t *testing.T, db *sql.DB, uri string) (up, down, bridgedUp, bridgedDown, score int, asOf *time.Time) {
	t.Helper()
	err := db.QueryRow(`SELECT upvote_count, downvote_count, bridged_upvote_count, bridged_downvote_count, score, bridged_stats_as_of
		FROM comments WHERE uri=$1`, uri).Scan(&up, &down, &bridgedUp, &bridgedDown, &score, &asOf)
	require.NoError(t, err)
	return
}

// setupCommentThread creates a post (so the comment has a valid parent) and returns its URI/CID.
// It also indexes the commenter as a user hosted on the trusted bridge PDS, so the
// comment provenance gate (which resolves the commenter's pds_url) admits bridgedStats.
func setupCommentThread(t *testing.T, db *sql.DB) (postURI, postCID string) {
	t.Helper()
	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedUser(t, db, bridgedTestCommenter, "brcommenter.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	pc := newPostConsumer(t, db)
	postURI = "at://" + bridgedTestCommunity + "/social.coves.community.post/cthread"
	postCID = "bafcthread"
	require.NoError(t, pc.HandleEvent(context.Background(),
		postCommitEvent("create", "cthread", postCID, postRecord("t", "c", nil))))
	return postURI, postCID
}

func TestCommentConsumer_Create_WithBridgedStats(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	postURI, postCID := setupCommentThread(t, db)
	cc := newCommentConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestPrefix + "commenter/social.coves.community.comment/cc1"

	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("create", "cc1", "bafcc1",
		commentRecord("hi", postURI, postCID, postURI, postCID, bridgedStatsRecord(5, 1, asOfEarly)))))

	up, down, bUp, bDown, score, asOf := readCommentRow(t, db, uri)
	assert.Equal(t, 0, up)
	assert.Equal(t, 0, down)
	assert.Equal(t, 5, bUp)
	assert.Equal(t, 1, bDown)
	assert.Equal(t, 4, score)
	require.NotNil(t, asOf)

	// Read-path fold via ListByParent.
	repo := postgres.NewCommentRepository(db)
	list, err := repo.ListByParent(ctx, postURI, 10, 0)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, 5, list[0].UpvoteCount, "displayed upvotes folded")
	assert.Equal(t, 1, list[0].DownvoteCount, "displayed downvotes folded")
	assert.Equal(t, 4, list[0].Score)
}

func TestCommentConsumer_Update_AsOfGuard_AndInclusiveScore(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	postURI, postCID := setupCommentThread(t, db)
	cc := newCommentConsumer(db)
	vc := newVoteConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestPrefix + "commenter/social.coves.community.comment/cc2"

	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("create", "cc2", "bafcc2",
		commentRecord("hi", postURI, postCID, postURI, postCID, bridgedStatsRecord(5, 1, asOfEarly)))))

	// Native upvote stacks on bridged.
	require.NoError(t, vc.HandleEvent(ctx, &JetstreamEvent{
		Kind: "commit", Did: bridgedTestVoter,
		Commit: &CommitEvent{Operation: "create", Collection: "social.coves.feed.vote", RKey: "cv1", CID: "bafcv1",
			Record: map[string]interface{}{
				"subject":   map[string]interface{}{"uri": uri, "cid": "bafcc2"},
				"direction": "up", "createdAt": "2026-02-01T00:00:00Z",
			}},
	}))
	up, _, _, _, score, _ := readCommentRow(t, db, uri)
	assert.Equal(t, 1, up)
	assert.Equal(t, 5, score, "score = (1+5)-(0+1)")

	// Stale asOf update -> ignored, native vote preserved.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("update", "cc2", "bafcc2b",
		commentRecord("edited", postURI, postCID, postURI, postCID, bridgedStatsRecord(0, 0, "2025-01-01T00:00:00Z")))))
	up2, _, bUp2, bDown2, score2, _ := readCommentRow(t, db, uri)
	assert.Equal(t, 1, up2, "native vote preserved")
	assert.Equal(t, 5, bUp2, "stale bridged ignored")
	assert.Equal(t, 1, bDown2)
	assert.Equal(t, 5, score2)

	// Newer asOf update -> applied, native vote still preserved.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("update", "cc2", "bafcc2c",
		commentRecord("edited2", postURI, postCID, postURI, postCID, bridgedStatsRecord(12, 3, asOfLate)))))
	up3, _, bUp3, bDown3, score3, _ := readCommentRow(t, db, uri)
	assert.Equal(t, 1, up3)
	assert.Equal(t, 12, bUp3)
	assert.Equal(t, 3, bDown3)
	assert.Equal(t, 10, score3, "score = (1+12)-(0+3)")

	// Absent bridgedStats on update -> stored bridged counts left alone.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("update", "cc2", "bafcc2d",
		commentRecord("edited3", postURI, postCID, postURI, postCID, nil))))
	_, _, bUp4, bDown4, _, _ := readCommentRow(t, db, uri)
	assert.Equal(t, 12, bUp4, "absent bridgedStats leaves stored counts")
	assert.Equal(t, 3, bDown4)

	// Equal asOf with DIFFERENT counts -> APPLIED (newer-or-equal guard), native vote
	// still preserved. Mirrors the post consumer's equal-asOf case.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("update", "cc2", "bafcc2e",
		commentRecord("edited4", postURI, postCID, postURI, postCID, bridgedStatsRecord(20, 2, asOfLate)))))
	up5, _, bUp5, bDown5, score5, _ := readCommentRow(t, db, uri)
	assert.Equal(t, 1, up5, "native vote preserved")
	assert.Equal(t, 20, bUp5, "equal asOf with fresher counts must apply")
	assert.Equal(t, 2, bDown5)
	assert.Equal(t, 19, score5, "score = (1+20)-(0+2)")
}

// --- edited_at churn (fix 4) ---

func TestPostConsumer_Update_StatsOnly_EditedAtUnchanged(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/edat1"

	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "edat1", "bafedat1",
		postRecord("title", "body", bridgedStatsRecord(5, 1, asOfEarly)))))

	// Stats-only refresh: identical content, newer bridgedStats. A debounced stats
	// refresh must NOT mark the post edited.
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("update", "edat1", "bafedat1b",
		postRecord("title", "body", bridgedStatsRecord(9, 1, asOfLate)))))
	var editedAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT edited_at FROM posts WHERE uri=$1`, uri).Scan(&editedAt))
	assert.Nil(t, editedAt, "stats-only update must leave edited_at NULL")
	_, _, bUp, _, _, _, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 9, bUp, "bridged counts still refreshed by the stats-only update")

	// A genuine content edit DOES set edited_at.
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("update", "edat1", "bafedat1c",
		postRecord("title changed", "body", bridgedStatsRecord(9, 1, asOfLate)))))
	require.NoError(t, db.QueryRow(`SELECT edited_at FROM posts WHERE uri=$1`, uri).Scan(&editedAt))
	assert.NotNil(t, editedAt, "content edit must set edited_at")
}

// --- provenance gate (fix 1a): untrusted repos cannot self-assert bridgedStats ---

func TestPostConsumer_Create_UntrustedCommunity_BridgedStatsIgnored(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	// Community hosted on a NON-bridge PDS -> provenance gate denies bridgedStats.
	insertBridgedCommunityOnPDS(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor, bridgedTestNativePDS)
	c := newPostConsumer(t, db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommunity + "/social.coves.community.post/unt1"

	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "unt1", "bafunt1",
		postRecord("t", "c", bridgedStatsRecord(50, 5, asOfEarly)))))

	// Post indexed normally, but bridgedStats ignored (default-deny).
	up, down, bUp, bDown, score, asOf, _, _ := readPostRow(t, db, uri)
	assert.Equal(t, 0, up)
	assert.Equal(t, 0, down)
	assert.Equal(t, 0, bUp, "untrusted community cannot self-assert bridged upvotes")
	assert.Equal(t, 0, bDown)
	assert.Equal(t, 0, score)
	assert.Nil(t, asOf, "no bridged asOf recorded for an ignored aggregate")
}

func TestCommentConsumer_Create_UntrustedCommenter_BridgedStatsIgnored(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	postURI, postCID := setupCommentThread(t, db)
	// Override the commenter to a non-bridge PDS -> provenance gate denies bridgedStats.
	insertBridgedUserOnPDS(t, db, bridgedTestCommenter, "brcommenter.test", bridgedTestNativePDS)
	cc := newCommentConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommenter + "/social.coves.community.comment/unt1"

	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("create", "unt1", "bafcunt1",
		commentRecord("hi", postURI, postCID, postURI, postCID, bridgedStatsRecord(7, 2, asOfEarly)))))

	up, down, bUp, bDown, score, asOf := readCommentRow(t, db, uri)
	assert.Equal(t, 0, up)
	assert.Equal(t, 0, down)
	assert.Equal(t, 0, bUp, "untrusted commenter cannot self-assert bridged upvotes")
	assert.Equal(t, 0, bDown)
	assert.Equal(t, 0, score)
	assert.Nil(t, asOf)
}

// --- input hygiene (fix 1b): negative / over-cap aggregates are ignored whole ---

func TestPostConsumer_Create_BridgedStatsHygiene(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)
	c := newPostConsumer(t, db)
	ctx := context.Background()

	// Negative count -> whole aggregate ignored.
	negURI := "at://" + bridgedTestCommunity + "/social.coves.community.post/neg1"
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "neg1", "bafneg1",
		postRecord("t", "c", bridgedStatsRecord(-5, 0, asOfEarly)))))
	_, _, bUp, bDown, score, asOf, _, _ := readPostRow(t, db, negURI)
	assert.Equal(t, 0, bUp, "negative bridged count ignored")
	assert.Equal(t, 0, bDown)
	assert.Equal(t, 0, score)
	assert.Nil(t, asOf)

	// Count above maxBridgedCount -> whole aggregate ignored.
	capURI := "at://" + bridgedTestCommunity + "/social.coves.community.post/cap1"
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "cap1", "bafcap1",
		postRecord("t", "c", bridgedStatsRecord(maxBridgedCount+1, 0, asOfEarly)))))
	_, _, bUp2, _, _, asOf2, _, _ := readPostRow(t, db, capURI)
	assert.Equal(t, 0, bUp2, "over-cap bridged count ignored")
	assert.Nil(t, asOf2)

	// Exactly at the cap -> accepted.
	okURI := "at://" + bridgedTestCommunity + "/social.coves.community.post/cap2"
	require.NoError(t, c.HandleEvent(ctx, postCommitEvent("create", "cap2", "bafcap2",
		postRecord("t", "c", bridgedStatsRecord(maxBridgedCount, 0, asOfEarly)))))
	_, _, bUp3, _, _, asOf3, _, _ := readPostRow(t, db, okURI)
	assert.Equal(t, maxBridgedCount, bUp3, "count exactly at the cap is accepted")
	require.NotNil(t, asOf3)
}

// --- comment update skips soft-deleted rows (fix 6) ---

func TestCommentConsumer_Update_SoftDeletedSkipped(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	postURI, postCID := setupCommentThread(t, db)
	cc := newCommentConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommenter + "/social.coves.community.comment/cdel1"

	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("create", "cdel1", "bafcdel1",
		commentRecord("original", postURI, postCID, postURI, postCID, bridgedStatsRecord(5, 1, asOfEarly)))))
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("delete", "cdel1", "", nil)))

	// Update on a soft-deleted comment must be skipped (no error, no resurrection, no
	// spurious ErrCommentNotFound), leaving the row deleted with content still blanked.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("update", "cdel1", "bafcdel1b",
		commentRecord("edited", postURI, postCID, postURI, postCID, bridgedStatsRecord(99, 9, asOfLate)))))

	var deletedAt *time.Time
	var content string
	var bUp int
	require.NoError(t, db.QueryRow(`SELECT deleted_at, content, bridged_upvote_count FROM comments WHERE uri=$1`, uri).
		Scan(&deletedAt, &content, &bUp))
	assert.NotNil(t, deletedAt, "comment stays soft-deleted")
	assert.Equal(t, "", content, "content stays blanked")
	assert.Equal(t, 5, bUp, "bridged counts untouched on soft-deleted row")
}

// --- comment resurrection score invariant (fix 5) ---

func TestCommentConsumer_Resurrection_ScoreIncludesSurvivingNativeVotes(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	postURI, postCID := setupCommentThread(t, db)
	cc := newCommentConsumer(db)
	vc := newVoteConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommenter + "/social.coves.community.comment/res1"

	// Create (no bridgedStats), then two native upvotes from distinct voters.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("create", "res1", "bafres1",
		commentRecord("hi", postURI, postCID, postURI, postCID, nil))))
	upvote := func(voter, rkey string) {
		require.NoError(t, vc.HandleEvent(ctx, &JetstreamEvent{
			Kind: "commit", Did: voter,
			Commit: &CommitEvent{Operation: "create", Collection: "social.coves.feed.vote", RKey: rkey, CID: "bafresv",
				Record: map[string]interface{}{
					"subject":   map[string]interface{}{"uri": uri, "cid": "bafres1"},
					"direction": "up", "createdAt": "2026-02-01T00:00:00Z",
				}},
		}))
	}
	upvote(bridgedTestVoter, "rv1")
	upvote(bridgedTestVoter+"2", "rv2")

	up, _, _, _, score, _ := readCommentRow(t, db, uri)
	require.Equal(t, 2, up, "two native upvotes recorded")
	require.Equal(t, 2, score)

	// Soft-delete, then resurrect (same rkey) WITH bridgedStats. The surviving native
	// upvotes must be reflected in the recomputed score.
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("delete", "res1", "", nil)))
	require.NoError(t, cc.HandleEvent(ctx, commentCommitEvent("create", "res1", "bafres1b",
		commentRecord("back", postURI, postCID, postURI, postCID, bridgedStatsRecord(4, 1, asOfLate)))))

	up2, down2, bUp2, bDown2, score2, _ := readCommentRow(t, db, uri)
	assert.Equal(t, 2, up2, "native upvotes survive resurrection")
	assert.Equal(t, 0, down2)
	assert.Equal(t, 4, bUp2)
	assert.Equal(t, 1, bDown2)
	assert.Equal(t, 5, score2, "score = (2+4)-(0+1) includes surviving native votes")
}
