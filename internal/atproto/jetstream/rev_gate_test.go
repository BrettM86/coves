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

// These tests prove the rev gate restores per-repo commit ordering across
// MULTIPLE Jetstream feeds carrying the same repos. The adversarial
// interleavings below cannot be produced by a single feed (which delivers a
// repo's events in commit order) — they model the lagging bsky.network feed
// replaying a repo's history HOURS after the self-hosted feed already
// processed newer events. Crucially, the replayed copies carry NEWER time_us
// values (each feed stamps its own emission time), so the pre-existing
// time-based recency guards cannot reject them; only rev — assigned by the
// repo itself and monotonic per repo — orders events across feeds.

const (
	revTestPrefix    = "did:plc:jsrev"
	revTestCommunity = revTestPrefix + "community"
	revTestAuthor    = revTestPrefix + "author"
	revTestCommenter = revTestPrefix + "commenter"
	revTestVoter     = revTestPrefix + "voter"

	// TIDs are lexicographically ordered strings; these model three
	// successive commits of one repo.
	revA = "3lrevtestaa2a"
	revB = "3lrevtestaa2b"
	revC = "3lrevtestaa2c"
)

// revCommitEvent builds a commit event carrying a rev, the one field the
// duplicate-delivery helpers omit.
func revCommitEvent(did, collection, op, rkey, rev, cid string, timeUS int64, record map[string]interface{}) *JetstreamEvent {
	return &JetstreamEvent{
		Kind:   "commit",
		Did:    did,
		TimeUS: timeUS,
		Commit: &CommitEvent{
			Rev:        rev,
			Operation:  op,
			Collection: collection,
			RKey:       rkey,
			CID:        cid,
			Record:     record,
		},
	}
}

// setupRevFixtures indexes the shared user/community fixtures plus one post,
// returning the post's URI/CID and a post consumer wired to the fixtures.
func setupRevFixtures(t *testing.T, db *sql.DB) (pc *PostEventConsumer, postURI, postCID string) {
	t.Helper()
	insertBridgedUser(t, db, revTestAuthor, "revauthor.test")
	insertBridgedUser(t, db, revTestCommenter, "revcommenter.test")
	insertBridgedUser(t, db, revTestVoter, "revvoter.test")
	insertBridgedCommunity(t, db, revTestCommunity, "revcommunity.test", revTestAuthor)

	us := newMockUserService()
	us.users[revTestAuthor] = &users.User{DID: revTestAuthor, Handle: "revauthor.test"}
	pc = NewPostEventConsumer(postgres.NewPostRepository(db), postgres.NewCommunityRepository(db), us, db)

	postURI = "at://" + revTestCommunity + "/social.coves.community.post/revpost1"
	postCID = "bafrevpost1"
	require.NoError(t, pc.HandleEvent(context.Background(), revCommitEvent(
		revTestCommunity, "social.coves.community.post", "create", "revpost1", revA, postCID,
		time.Now().UnixMicro(),
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": revTestCommunity,
			"author":    revTestAuthor,
			"title":     "rev target",
			"content":   "post v1",
			"createdAt": "2026-03-01T00:00:00Z",
		},
	)))
	return pc, postURI, postCID
}

func revCommentRecord(content, rootURI, rootCID, parentURI, parentCID string) map[string]interface{} {
	return map[string]interface{}{
		"$type":   CommentCollection,
		"content": content,
		"reply": map[string]interface{}{
			"root":   map[string]interface{}{"uri": rootURI, "cid": rootCID},
			"parent": map[string]interface{}{"uri": parentURI, "cid": parentCID},
		},
		"createdAt": "2026-03-01T02:00:00Z",
	}
}

func TestRevGate_AdvanceAndStalenessSemantics(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	uri := "at://" + revTestPrefix + "gate/social.coves.feed.vote/g1"

	// First writer wins.
	won, err := tryAdvanceRecordRev(ctx, db, uri, revB)
	require.NoError(t, err)
	assert.True(t, won, "first rev must win")

	// Equal rev = the same event replayed = no-op.
	won, err = tryAdvanceRecordRev(ctx, db, uri, revB)
	require.NoError(t, err)
	assert.False(t, won, "equal rev must be rejected (duplicate replay)")

	// Lower rev = stale cross-feed copy.
	won, err = tryAdvanceRecordRev(ctx, db, uri, revA)
	require.NoError(t, err)
	assert.False(t, won, "lower rev must be rejected (stale copy)")

	// Higher rev = genuinely newer commit.
	won, err = tryAdvanceRecordRev(ctx, db, uri, revC)
	require.NoError(t, err)
	assert.True(t, won, "higher rev must win")

	// Read-side check agrees.
	gate := NewRevGate(db)
	stale, err := gate.IsStale(ctx, uri, revB)
	require.NoError(t, err)
	assert.True(t, stale)
	stale, err = gate.IsStale(ctx, uri, revC)
	require.NoError(t, err)
	assert.True(t, stale, "equal rev is stale (already applied)")

	// Empty rev bypasses the gate entirely (synthetic/legacy events).
	won, err = tryAdvanceRecordRev(ctx, db, uri, "")
	require.NoError(t, err)
	assert.True(t, won, "rev-less events bypass the gate")
	stale, err = gate.IsStale(ctx, uri, "")
	require.NoError(t, err)
	assert.False(t, stale, "rev-less events are never stale")

	// A nil gate is inert.
	var nilGate *RevGate
	stale, err = nilGate.IsStale(ctx, uri, revA)
	require.NoError(t, err)
	assert.False(t, stale)
	require.NoError(t, nilGate.Advance(ctx, uri, revC))
}

// The zombie-resurrection interleaving: create → delete via the fast feed,
// then the lagging feed replays the original create with an older rev but a
// NEWER time_us. Without the gate, the resurrection branch restores the
// deleted comment permanently.
func TestCommentConsumer_StaleCreateReplayAfterDelete_DoesNotResurrect(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	_, postURI, postCID := setupRevFixtures(t, db)
	cc := NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/zomb1"
	record := revCommentRecord("hello", postURI, postCID, postURI, postCID)

	// Fast feed: create, then delete.
	require.NoError(t, cc.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", "zomb1", revA, "bafzomb1", base, record)))
	require.NoError(t, cc.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "delete", "zomb1", revB, "", base+1_000_000, nil)))

	var deletedAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM comments WHERE uri=$1`, commentURI).Scan(&deletedAt))
	require.NotNil(t, deletedAt, "fixture: comment soft-deleted")

	// Lagging feed: the original create replayed hours later — older rev,
	// NEWER time_us.
	require.NoError(t, cc.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", "zomb1", revA, "bafzomb1", base+2_000_000, record)))

	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM comments WHERE uri=$1`, commentURI).Scan(&deletedAt))
	assert.NotNil(t, deletedAt, "stale create replay must NOT resurrect the deleted comment")

	var commentCount int
	require.NoError(t, db.QueryRow(`SELECT comment_count FROM posts WHERE uri=$1`, postURI).Scan(&commentCount))
	assert.Equal(t, 1, commentCount, "stale create replay must not re-increment comment_count")
}

// A genuine re-creation of the same rkey carries a fresh, HIGHER rev and must
// still pass the gate and resurrect the row — proving the gate rejects only
// stale copies, not the legitimate atProto recreate-same-rkey flow.
func TestCommentConsumer_GenuineRecreateSameRKey_StillResurrects(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	_, postURI, postCID := setupRevFixtures(t, db)
	cc := NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/resur1"

	require.NoError(t, cc.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", "resur1", revA, "bafresur1", base,
		revCommentRecord("first life", postURI, postCID, postURI, postCID))))
	require.NoError(t, cc.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "delete", "resur1", revB, "", base+1_000_000, nil)))
	require.NoError(t, cc.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", "resur1", revC, "bafresur2", base+2_000_000,
		revCommentRecord("second life", postURI, postCID, postURI, postCID))))

	var deletedAt *time.Time
	var content string
	require.NoError(t, db.QueryRow(`SELECT deleted_at, content FROM comments WHERE uri=$1`, commentURI).Scan(&deletedAt, &content))
	assert.Nil(t, deletedAt, "genuine re-creation (higher rev) must resurrect the comment")
	assert.Equal(t, "second life", content)
}

// The stale-update-clobber interleaving for posts: two successive edits via
// the fast feed, then the lagging feed replays the FIRST edit with a newer
// time_us. The time-based recency guard passes it; only the rev gate rejects
// it. Without the gate the content regresses until the next organic edit.
func TestPostConsumer_StaleUpdateReplay_DoesNotClobberContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	pc, postURI, _ := setupRevFixtures(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	update := func(rev, content string, timeUS int64) *JetstreamEvent {
		return revCommitEvent(revTestCommunity, "social.coves.community.post", "update", "revpost1", rev, "baf"+rev,
			timeUS, map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": revTestCommunity,
				"author":    revTestAuthor,
				"title":     "rev target",
				"content":   content,
				"createdAt": "2026-03-01T00:00:00Z",
			})
	}

	// Fast feed: edit to v2, then to v3.
	require.NoError(t, pc.HandleEvent(ctx, update(revB, "post v2", base+1_000_000)))
	require.NoError(t, pc.HandleEvent(ctx, update(revC, "post v3", base+2_000_000)))

	// Lagging feed: the v2 edit replayed with a NEWER time_us.
	require.NoError(t, pc.HandleEvent(ctx, update(revB, "post v2", base+3_000_000)))

	var content string
	require.NoError(t, db.QueryRow(`SELECT content FROM posts WHERE uri=$1`, postURI).Scan(&content))
	assert.Equal(t, "post v3", content, "stale update replay must not regress post content")
}

// Same interleaving for comments.
func TestCommentConsumer_StaleUpdateReplay_DoesNotClobberContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	_, postURI, postCID := setupRevFixtures(t, db)
	cc := NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/edit1"
	edit := func(rev, content string, timeUS int64) *JetstreamEvent {
		op := "update"
		if rev == revA {
			op = "create"
		}
		return revCommitEvent(revTestCommenter, CommentCollection, op, "edit1", rev, "baf"+rev,
			timeUS, revCommentRecord(content, postURI, postCID, postURI, postCID))
	}

	require.NoError(t, cc.HandleEvent(ctx, edit(revA, "comment v1", base)))
	require.NoError(t, cc.HandleEvent(ctx, edit(revB, "comment v2", base+1_000_000)))
	require.NoError(t, cc.HandleEvent(ctx, edit(revC, "comment v3", base+2_000_000)))

	// Lagging feed replays the v2 edit with a NEWER time_us.
	replay := edit(revB, "comment v2", base+3_000_000)
	replay.Commit.Operation = "update"
	require.NoError(t, cc.HandleEvent(ctx, replay))

	var content string
	require.NoError(t, db.QueryRow(`SELECT content FROM comments WHERE uri=$1`, commentURI).Scan(&content))
	assert.Equal(t, "comment v3", content, "stale update replay must not regress comment content")
}

// The phantom-vote interleaving: vote → unvote via the fast feed, then the
// lagging feed replays the vote's create. Without the gate the vote row is
// re-indexed and the count re-incremented, permanently.
func TestVoteConsumer_StaleCreateReplayAfterDelete_NoPhantomVote(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	_, postURI, postCID := setupRevFixtures(t, db)
	vc := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	voteRecord := map[string]interface{}{
		"subject":   map[string]interface{}{"uri": postURI, "cid": postCID},
		"direction": "up",
		"createdAt": "2026-03-01T01:00:00Z",
	}

	require.NoError(t, vc.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.feed.vote", "create", "rv1", revA, "bafrv1", base, voteRecord)))
	require.NoError(t, vc.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.feed.vote", "delete", "rv1", revB, "", base+1_000_000, nil)))

	// Lagging feed: the vote's create replayed after the unvote.
	require.NoError(t, vc.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.feed.vote", "create", "rv1", revA, "bafrv1", base+2_000_000, voteRecord)))

	var upvotes, score int
	require.NoError(t, db.QueryRow(`SELECT upvote_count, score FROM posts WHERE uri=$1`, postURI).Scan(&upvotes, &score))
	assert.Equal(t, 0, upvotes, "stale vote-create replay must not restore a phantom vote")
	assert.Equal(t, 0, score)

	var activeVotes int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE voter_did=$1 AND deleted_at IS NULL`, revTestVoter).Scan(&activeVotes))
	assert.Equal(t, 0, activeVotes)
}

// Delete arriving for a never-indexed vote must still tombstone the record's
// rev, so the create's late copy cannot index a vote whose record no longer
// exists on the PDS.
func TestVoteConsumer_DeleteBeforeCreate_TombstoneRejectsLateCreate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	_, postURI, postCID := setupRevFixtures(t, db)
	vc := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	// Delete first (create was lost/never delivered on this feed).
	require.NoError(t, vc.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.feed.vote", "delete", "rv2", revB, "", base, nil)))

	// The create's copy arrives later from the lagging feed.
	require.NoError(t, vc.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.feed.vote", "create", "rv2", revA, "bafrv2", base+1_000_000,
		map[string]interface{}{
			"subject":   map[string]interface{}{"uri": postURI, "cid": postCID},
			"direction": "up",
			"createdAt": "2026-03-01T01:00:00Z",
		})))

	var voteRows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE voter_did=$1`, revTestVoter).Scan(&voteRows))
	assert.Equal(t, 0, voteRows, "create arriving after the record's delete must not be indexed")

	var upvotes int
	require.NoError(t, db.QueryRow(`SELECT upvote_count FROM posts WHERE uri=$1`, postURI).Scan(&upvotes))
	assert.Equal(t, 0, upvotes)
}

// deletePost coverage: a delete arriving for a never-indexed post (its create
// was lost or is still in flight on the other feed) must still tombstone the
// record's rev, so the create's late copy cannot index a post whose record no
// longer exists on the PDS.
func TestPostConsumer_DeleteBeforeCreate_TombstoneRejectsLateCreate(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	pc, _, _ := setupRevFixtures(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	postURI := "at://" + revTestCommunity + "/social.coves.community.post/revpost2"

	// Delete first (create never delivered on this feed).
	require.NoError(t, pc.HandleEvent(ctx, revCommitEvent(
		revTestCommunity, "social.coves.community.post", "delete", "revpost2", revB, "", base, nil)))

	// The create's copy arrives later from the lagging feed — older rev,
	// NEWER time_us.
	require.NoError(t, pc.HandleEvent(ctx, revCommitEvent(
		revTestCommunity, "social.coves.community.post", "create", "revpost2", revA, "bafrevpost2", base+1_000_000,
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": revTestCommunity,
			"author":    revTestAuthor,
			"title":     "late create",
			"content":   "must not be indexed",
			"createdAt": "2026-03-01T00:00:00Z",
		})))

	var postRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE uri=$1`, postURI).Scan(&postRows))
	assert.Equal(t, 0, postRows, "create arriving after the record's delete must not be indexed")
}

// The zombie-resurrection interleaving for posts: create → delete via the
// fast feed, then the lagging feed replays the original create with an older
// rev but a NEWER time_us. The tombstoned delete rev must keep the post dead.
func TestPostConsumer_StaleCreateReplayAfterDelete_DoesNotResurrect(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	// setupRevFixtures indexes revpost1 with revA / CID bafrevpost1.
	pc, postURI, _ := setupRevFixtures(t, db)
	ctx := context.Background()
	base := time.Now().UnixMicro()

	// Fast feed: delete the post.
	require.NoError(t, pc.HandleEvent(ctx, revCommitEvent(
		revTestCommunity, "social.coves.community.post", "delete", "revpost1", revB, "", base+1_000_000, nil)))

	var deletedAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM posts WHERE uri=$1`, postURI).Scan(&deletedAt))
	require.NotNil(t, deletedAt, "fixture: post soft-deleted")

	// Lagging feed: the original create replayed hours later — older rev,
	// NEWER time_us.
	require.NoError(t, pc.HandleEvent(ctx, revCommitEvent(
		revTestCommunity, "social.coves.community.post", "create", "revpost1", revA, "bafrevpost1", base+2_000_000,
		map[string]interface{}{
			"$type":     "social.coves.community.post",
			"community": revTestCommunity,
			"author":    revTestAuthor,
			"title":     "rev target",
			"content":   "post v1",
			"createdAt": "2026-03-01T00:00:00Z",
		})))

	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM posts WHERE uri=$1`, postURI).Scan(&deletedAt))
	assert.NotNil(t, deletedAt, "stale create replay must NOT resurrect the deleted post")
}

// The user consumer's profile gating is hand-rolled (check→write→advance in
// handleProfileCommit, not applyGated). This proves the gate rejects a stale
// cross-feed profile replay that the wall-clock recency guard cannot: the
// replay carries an OLDER rev but a NEWER time_us.
func TestUserConsumer_StaleProfileUpdateReplay_DoesNotRegressProfile(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	const did = revTestPrefix + "profileuser"
	profileURI := "at://" + did + "/social.coves.actor.profile/self"

	mockService := newMockUserService()
	mockService.users[did] = &users.User{DID: did, Handle: "revprofile.test"}
	consumer := NewUserEventConsumer(mockService, &mockIdentityResolverForUser{},
		WithUserRevGate(NewRevGate(db)))
	ctx := context.Background()
	base := time.Now().UnixMicro()

	profileEvent := func(rev, displayName string, timeUS int64) *JetstreamEvent {
		return revCommitEvent(did, CovesProfileCollection, "update", "self", rev, "baf"+rev, timeUS,
			map[string]interface{}{"displayName": displayName})
	}

	// Fast feed: the current profile state (v2).
	require.NoError(t, consumer.HandleEvent(ctx, profileEvent(revB, "v2", base)))
	require.Equal(t, "v2", mockService.users[did].DisplayName, "fixture: v2 applied")

	// Lagging feed: the pre-edit update replayed — older rev, NEWER time_us.
	require.NoError(t, consumer.HandleEvent(ctx, profileEvent(revA, "v1", base+2_000_000)))

	assert.Equal(t, "v2", mockService.users[did].DisplayName,
		"stale profile replay must not regress the profile")

	var storedRev string
	require.NoError(t, db.QueryRow(
		`SELECT rev FROM jetstream_record_revs WHERE record_uri=$1`, profileURI).Scan(&storedRev))
	assert.Equal(t, revB, storedRev, "gate row must still hold the newer applied rev")
}

// The zombie-subscription interleaving. Subscriptions are HARD-deleted, so
// only the gate's surviving row can reject the stale subscribe replay — this
// is the case a per-table rev column could never cover.
func TestCommunityConsumer_StaleSubscribeReplayAfterUnsubscribe_DoesNotResubscribe(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	insertBridgedUser(t, db, revTestVoter, "revsubscriber.test")
	insertBridgedUser(t, db, revTestAuthor, "revauthor.test")
	insertBridgedCommunity(t, db, revTestCommunity, "revcommunity.test", revTestAuthor)

	cec := NewCommunityEventConsumer(postgres.NewCommunityRepository(db), "did:web:test.local", true, nil,
		WithCommunityRevGate(NewRevGate(db)))
	ctx := context.Background()
	base := time.Now().UnixMicro()

	subRecord := map[string]interface{}{
		"$type":     "social.coves.community.subscription",
		"subject":   revTestCommunity,
		"createdAt": "2026-03-01T03:00:00Z",
	}

	// Fast feed: subscribe, then unsubscribe (hard delete).
	require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.community.subscription", "create", "rsub1", revA, "bafrsub1", base, subRecord)))

	var subscriptions int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM community_subscriptions WHERE user_did=$1 AND community_did=$2`,
		revTestVoter, revTestCommunity).Scan(&subscriptions))
	require.Equal(t, 1, subscriptions, "fixture: subscription indexed")

	require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.community.subscription", "delete", "rsub1", revB, "", base+1_000_000, nil)))

	// Lagging feed: the subscribe replayed after the unsubscribe.
	require.NoError(t, cec.HandleEvent(ctx, revCommitEvent(
		revTestVoter, "social.coves.community.subscription", "create", "rsub1", revA, "bafrsub1", base+2_000_000, subRecord)))

	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM community_subscriptions WHERE user_did=$1 AND community_did=$2`,
		revTestVoter, revTestCommunity).Scan(&subscriptions))
	assert.Equal(t, 0, subscriptions, "stale subscribe replay must not re-subscribe the user")

	var subscriberCount int
	require.NoError(t, db.QueryRow(
		`SELECT subscriber_count FROM communities WHERE did=$1`, revTestCommunity).Scan(&subscriberCount))
	assert.Equal(t, 0, subscriberCount, "subscriber_count must not be re-incremented by the stale replay")
}
