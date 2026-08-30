//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

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

func newCommentConsumer(db *sql.DB) *CommentEventConsumer {
	return NewCommentEventConsumer(postgres.NewCommentRepository(db), db, WithCommentBridgeTrust(bridgeTrustForTests()))
}

func newVoteConsumer(db *sql.DB) *VoteEventConsumer {
	return NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
}

func commentCommitEvent(op, rkey, cid string, record map[string]interface{}) *JetstreamEvent {
	return &JetstreamEvent{
		Kind: "commit",
		Did:  bridgedTestCommenter,
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

// setupCommentThread indexes a postv2 parent through the real post consumer.
// It also indexes the commenter as a user hosted on the trusted bridge PDS, so the
// comment provenance gate (which resolves the commenter's pds_url) admits bridgedStats.
func setupCommentThread(t *testing.T, db *sql.DB) (postURI, postCID string) {
	t.Helper()
	insertBridgedUser(t, db, bridgedTestAuthor, "brauthor.test")
	insertBridgedUser(t, db, bridgedTestCommenter, "brcommenter.test")
	insertBridgedCommunity(t, db, bridgedTestCommunity, "brcommunity.test", bridgedTestAuthor)

	pc := NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), newMockUserService(), db,
		WithAdmissions(postgres.NewAdmissionRepository(db)),
	)
	const rkey = "cthread"
	postURI = pv2URI(bridgedTestAuthor, rkey)
	postCID = "bafcthread"
	require.NoError(t, pc.HandleEvent(context.Background(), pv2Event(
		bridgedTestAuthor, "create", rkey, testkit.TID(), postCID, time.Now().UnixMicro(),
		pv2Record(bridgedTestCommunity, "t", "c"),
	)))
	return postURI, postCID
}

func TestCommentConsumer_Create_WithBridgedStats(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	postURI, postCID := setupCommentThread(t, db)
	cc := newCommentConsumer(db)
	ctx := context.Background()
	uri := "at://" + bridgedTestCommenter + "/social.coves.community.comment/cc1"

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
	uri := "at://" + bridgedTestCommenter + "/social.coves.community.comment/cc2"

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

// --- provenance gate (fix 1a): untrusted repos cannot self-assert bridgedStats ---

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
